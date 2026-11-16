/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-logr/logr"
	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// HostEvacuationFinalizer is the finalizer that blocks deletion until evacuation criteria are met
	HostEvacuationFinalizer = "maas.lxd.io/host-evacuation"

	// EvacuationCheckInterval is the interval for checking evacuation gates
	EvacuationCheckInterval = 10 * time.Second
)

// HostMaintenanceService provides evacuation logic for MaasMachine controller
type HostMaintenanceService struct {
	client           client.Client
	namespace        string
	tagService       maint.TagService
	inventoryService maint.InventoryService
}

// NewHostMaintenanceService creates a new host maintenance service.
func NewHostMaintenanceService(k8sClient client.Client, namespace string) (*HostMaintenanceService, error) {
	maasClient, err := maint.NewMAASClient(k8sClient, namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to create MAAS client: %w", err)
	}
	return &HostMaintenanceService{
		client:           k8sClient,
		namespace:        namespace,
		tagService:       maint.NewTagService(maasClient),
		inventoryService: maint.NewInventoryService(maasClient),
	}, nil
}

// AddEvacuationFinalizer adds the evacuation finalizer to a host machine
func (s *HostMaintenanceService) AddEvacuationFinalizer(ctx context.Context, maasMachine *infrav1beta1.MaasMachine, log logr.Logger) error {
	// Only add finalizer to host machines (not VMs)
	if !s.isHostMachine(maasMachine) {
		return nil
	}

	if !containsString(maasMachine.Finalizers, HostEvacuationFinalizer) {
		log.Info("Adding host evacuation finalizer")
		maasMachine.Finalizers = append(maasMachine.Finalizers, HostEvacuationFinalizer)
		return s.client.Update(ctx, maasMachine)
	}
	return nil
}

// CheckEvacuationGates checks if evacuation criteria are met for host deletion
func (s *HostMaintenanceService) CheckEvacuationGates(ctx context.Context, maasMachine *infrav1beta1.MaasMachine, log logr.Logger) (bool, error) {
	if maasMachine.Spec.SystemID == nil {
		return false, fmt.Errorf("maasMachine has no systemID")
	}
	hostSystemID := *maasMachine.Spec.SystemID

	// Gate 1: Check if host is empty (no VMs running).
	vms, err := s.inventoryService.ListHostVMs(hostSystemID)
	if err != nil {
		return false, fmt.Errorf("failed to list host VMs: %w", err)
	}
	if len(vms) > 0 {
		log.Info("Host not empty, evacuation blocked", "host", hostSystemID, "vmCount", len(vms))
		return false, nil
	}
	log.Info("Host is empty (no VMs)", "host", hostSystemID)

	// Gate 2: Check per-WLC ready-op-<uuid> tags on CP VMs.
	wlcReady, err := s.checkWLCReadyTags(ctx, hostSystemID, log)
	if err != nil {
		return false, fmt.Errorf("failed to check WLC ready tags: %w", err)
	}
	if !wlcReady {
		log.Info("WLC ready tags not met, evacuation blocked", "host", hostSystemID)
		return false, nil
	}

	log.Info("All evacuation gates met", "host", hostSystemID)
	return true, nil
}

// ListHostVMs exposes the host VM inventory to HMC for condition reporting.
func (s *HostMaintenanceService) ListHostVMs(hostSystemID string) ([]maint.Machine, error) {
	return s.inventoryService.ListHostVMs(hostSystemID)
}

// ClearMaintenanceTagsAndRemoveFinalizer clears maintenance tags and removes the evacuation finalizer
func (s *HostMaintenanceService) ClearMaintenanceTagsAndRemoveFinalizer(ctx context.Context, maasMachine *infrav1beta1.MaasMachine, log logr.Logger) error {
	if maasMachine.Spec.SystemID == nil {
		return fmt.Errorf("maasMachine has no systemID")
	}
	hostSystemID := *maasMachine.Spec.SystemID

	// Clear maintenance tags
	if err := s.clearMaintenanceTags(ctx, hostSystemID, log); err != nil {
		log.Error(err, "failed to clear maintenance tags")
		return err
	}

	// Remove finalizer
	log.Info("Removing host evacuation finalizer")
	maasMachine.Finalizers = removeString(maasMachine.Finalizers, HostEvacuationFinalizer)
	if err := s.client.Update(ctx, maasMachine); err != nil {
		log.Error(err, "failed to remove finalizer")
		return err
	}

	log.Info("Host evacuation completed successfully")
	return nil
}

// isHostMachine checks if the MaasMachine is a host (not a VM)
func (s *HostMaintenanceService) isHostMachine(maasMachine *infrav1beta1.MaasMachine) bool {
	// Check if this is a host machine (not a VM)
	// VMs typically have a parent reference, hosts don't
	return maasMachine.Spec.Parent == nil || *maasMachine.Spec.Parent == ""
}

// isHostEmpty checks if the host has no VMs
func (s *HostMaintenanceService) isHostEmpty(ctx context.Context, hostSystemID string, log logr.Logger) (bool, error) {
	vms, err := s.inventoryService.ListHostVMs(hostSystemID)
	if err != nil {
		return false, fmt.Errorf("failed to list host VMs: %w", err)
	}

	// Simple check: if VMs list is empty, host is empty
	if len(vms) == 0 {
		log.Info("Host is empty (no VMs)", "host", hostSystemID)
		return true, nil
	}

	log.Info("Host has VMs", "host", hostSystemID, "vmCount", len(vms))
	return false, nil
}

// checkWLCReadyTags checks if WLC evacuation is ready by looking for replacement VMs
// Logic: Replacement VMs will be created on different hosts in the same resource pool and zone.
// We need to check all VMs in the inventory with the same resource pool and zone, filtering for
// CP VMs with cluster tags and ready-op tags.
func (s *HostMaintenanceService) checkWLCReadyTags(ctx context.Context, hostSystemID string, log logr.Logger) (bool, error) {
	// Load session to get opID and track affected clusters
	session, cm, err := maint.LoadSession(ctx, s.client, s.namespace)
	if err != nil {
		log.Error(err, "failed to load session, continuing without session data")
		// Continue without session data - not critical
	}

	log.Info("Loaded session state",
		"opID", session.OpID,
		"status", session.Status,
		"currentHost", session.CurrentHost,
		"activeSessions", session.ActiveSessions,
		"affectedWLCClusters", session.AffectedWLCClusters,
		"pendingReadyVMReplacements", session.PendingReadyVMReplacements,
		"configMapExists", cm != nil,
		"namespace", s.namespace)

	// Get the current host details to know its resource pool and zone
	hostDetails, err := s.inventoryService.GetHost(hostSystemID)
	if err != nil {
		return false, fmt.Errorf("failed to get host details: %w", err)
	}

	log.Info("Checking for replacement VMs in same resource pool and zone",
		"host", hostSystemID,
		"resourcePool", hostDetails.ResourcePool,
		"zone", hostDetails.Zone,
		"sessionOpID", session.OpID,
		"sessionActiveSince", session.StartedAt)

	// Get all VMs currently on the host being evacuated
	vmsOnCurrentHost, err := s.inventoryService.ListHostVMs(hostSystemID)
	if err != nil {
		return false, fmt.Errorf("failed to list host VMs: %w", err)
	}

	// If host is empty, evacuation can proceed
	if len(vmsOnCurrentHost) == 0 {
		// Update session to indicate no VMs to evacuate
		if session.OpID != "" {
			session.AffectedWLCClusters = []string{}
			session.PendingReadyVMReplacements = []string{}
			if err := maint.SaveSession(ctx, s.client, s.namespace, session); err != nil {
				log.Error(err, "failed to update session for empty host")
				// Continue - not critical
			} else {
				log.Info("Updated session: host is empty, no VMs to evacuate", "host", hostSystemID)
			}
		}
		log.Info("Host is empty, evacuation can proceed", "host", hostSystemID)
		return true, nil
	}

	// Identify CP VMs with cluster tags on the current host
	// These are the VMs that need replacements
	type vmToReplace struct {
		systemID   string
		clusterTag string // maas-lxd-wlc-<cluster-id>
	}
	var vmsNeedingReplacement []vmToReplace

	for _, vm := range vmsOnCurrentHost {
		// Check if this is a CP VM
		isCPVM := false
		var clusterTag string

		for _, tag := range vm.Tags {
			if tag == maint.TagVMControlPlane {
				isCPVM = true
			}
			// Look for cluster tag (maas-lxd-wlc-*)
			if strings.HasPrefix(tag, maint.TagVMClusterPrefix) {
				clusterTag = tag
			}
		}

		// If it's a CP VM with a cluster tag, it needs a replacement
		if isCPVM && clusterTag != "" {
			vmsNeedingReplacement = append(vmsNeedingReplacement, vmToReplace{
				systemID:   vm.SystemID,
				clusterTag: clusterTag,
			})
			log.Info("Found CP VM needing replacement", "vm", vm.SystemID, "clusterTag", clusterTag)
		}
	}

	// If no CP VMs need replacement, evacuation can proceed
	if len(vmsNeedingReplacement) == 0 {
		// Update session to indicate no CP VMs to evacuate
		if session.OpID != "" {
			session.AffectedWLCClusters = []string{}
			session.PendingReadyVMReplacements = []string{}
			if err := maint.SaveSession(ctx, s.client, s.namespace, session); err != nil {
				log.Error(err, "failed to update session for no CP VMs")
				// Continue - not critical
			} else {
				log.Info("Updated session: no CP VMs need replacement", "host", hostSystemID, "totalVMs", len(vmsOnCurrentHost))
			}
		}
		log.Info("No CP VMs need replacement, evacuation can proceed", "host", hostSystemID)
		return true, nil
	}

	// Track affected clusters and pending VMs for session update
	affectedClusters := make(map[string]bool)
	pendingVMs := []string{}
	for _, vm := range vmsNeedingReplacement {
		affectedClusters[vm.clusterTag] = true
		pendingVMs = append(pendingVMs, vm.systemID)
	}

	// Update session with affected clusters and pending VMs
	if session.OpID != "" {
		session.AffectedWLCClusters = make([]string, 0, len(affectedClusters))
		for cluster := range affectedClusters {
			session.AffectedWLCClusters = append(session.AffectedWLCClusters, cluster)
		}
		session.PendingReadyVMReplacements = pendingVMs

		log.Info("Attempting to save session with evacuation data",
			"opID", session.OpID,
			"affectedClusters", session.AffectedWLCClusters,
			"pendingVMs", pendingVMs,
			"pendingVMCount", len(pendingVMs),
			"activeSessions", session.ActiveSessions,
			"currentHost", session.CurrentHost)

		if err := maint.SaveSession(ctx, s.client, s.namespace, session); err != nil {
			log.Error(err, "❌ FAILED to update session with affected clusters",
				"opID", session.OpID,
				"affectedClusters", session.AffectedWLCClusters,
				"pendingVMCount", len(pendingVMs))
			// Continue - not critical
		} else {
			log.Info("✅ Successfully updated session with evacuation tracking data",
				"opID", session.OpID,
				"affectedClusters", session.AffectedWLCClusters,
				"pendingVMCount", len(pendingVMs),
				"namespace", s.namespace)
		}
	} else {
		log.Info("⚠️ Session OpID is empty, cannot save session data")
	}

	// Get all VMs in the inventory
	allVMs, err := s.inventoryService.ListAllVMs()
	if err != nil {
		return false, fmt.Errorf("failed to list all VMs: %w", err)
	}

	// Track replacement progress
	replacementsFound := 0
	replacementDetails := []string{}

	// For each VM needing replacement, check if there's a replacement VM in the same
	// resource pool and zone with the same cluster tag and a ready-op tag
	for _, vmToRepl := range vmsNeedingReplacement {
		foundReplacement := false

		for _, candidateVM := range allVMs {
			// Skip VMs not in the same resource pool and zone
			if candidateVM.ResourcePool != hostDetails.ResourcePool || candidateVM.Zone != hostDetails.Zone {
				continue
			}

			// Skip the VM being replaced itself
			if candidateVM.SystemID == vmToRepl.systemID {
				continue
			}

			// Check if this candidate has the CP tag and same cluster tag
			hasCPTag := false
			hasClusterTag := false
			hasReadyOpTag := false

			for _, tag := range candidateVM.Tags {
				if tag == maint.TagVMControlPlane {
					hasCPTag = true
				}
				if tag == vmToRepl.clusterTag {
					hasClusterTag = true
				}
				// Check for ready-op-<uuid> tag
				if strings.HasPrefix(tag, maint.TagVMReadyOpPrefix) {
					hasReadyOpTag = true
				}
			}

			// If this VM has all required tags, it's a valid replacement
			if hasCPTag && hasClusterTag && hasReadyOpTag {
				foundReplacement = true
				replacementsFound++
				replacementDetails = append(replacementDetails,
					fmt.Sprintf("%s→%s(%s)", vmToRepl.systemID, candidateVM.SystemID, vmToRepl.clusterTag))
				log.Info("Found replacement VM",
					"originalVM", vmToRepl.systemID,
					"replacementVM", candidateVM.SystemID,
					"clusterTag", vmToRepl.clusterTag,
					"resourcePool", candidateVM.ResourcePool,
					"zone", candidateVM.Zone,
					"sessionOpID", session.OpID,
					"progress", fmt.Sprintf("%d/%d", replacementsFound, len(vmsNeedingReplacement)))
				break
			}
		}

		if !foundReplacement {
			// Calculate time elapsed since session start for timeout tracking
			timeElapsed := time.Since(session.StartedAt)
			log.Info("No replacement VM found yet - evacuation blocked",
				"vmNeedingReplacement", vmToRepl.systemID,
				"clusterTag", vmToRepl.clusterTag,
				"requiredResourcePool", hostDetails.ResourcePool,
				"requiredZone", hostDetails.Zone,
				"sessionOpID", session.OpID,
				"timeElapsed", timeElapsed,
				"progress", fmt.Sprintf("%d/%d replacements found", replacementsFound, len(vmsNeedingReplacement)),
				"affectedClusters", session.AffectedWLCClusters)
			return false, nil
		}
	}

	// All replacements found - log comprehensive success summary
	log.Info("✅ All CP VMs have replacement VMs ready - evacuation can proceed",
		"host", hostSystemID,
		"sessionOpID", session.OpID,
		"totalReplacements", replacementsFound,
		"replacementDetails", replacementDetails,
		"affectedClusters", session.AffectedWLCClusters,
		"timeElapsed", time.Since(session.StartedAt),
		"resourcePool", hostDetails.ResourcePool,
		"zone", hostDetails.Zone)
	return true, nil
}

// clearMaintenanceTags clears maintenance-related tags from the host
func (s *HostMaintenanceService) clearMaintenanceTags(ctx context.Context, hostSystemID string, log logr.Logger) error {
	// Get current machine details to check existing tags
	hostDetails, err := s.inventoryService.GetHost(hostSystemID)
	if err != nil {
		log.Error(err, "failed to get host details for tag cleanup", "host", hostSystemID)
		return err
	}

	// Clear static maintenance tags
	maintenanceTags := []string{
		maint.TagHostMaintenance,
		maint.TagHostNoSchedule,
	}

	for _, tag := range maintenanceTags {
		if err := s.tagService.RemoveTagFromHost(hostSystemID, tag); err != nil {
			log.Error(err, "failed to remove maintenance tag", "tag", tag, "host", hostSystemID)
			return err
		}
		log.Info("Cleared maintenance tag", "tag", tag, "host", hostSystemID)
	}

	// Clear dynamic operation ID tags (maas.lxd-hcp-op-*)
	for _, tag := range hostDetails.Tags {
		if strings.HasPrefix(tag, maint.TagHostOpPrefix) {
			if err := s.tagService.RemoveTagFromHost(hostSystemID, tag); err != nil {
				log.Error(err, "failed to remove operation ID tag", "tag", tag, "host", hostSystemID)
				return err
			}
			log.Info("Cleared operation ID tag", "tag", tag, "host", hostSystemID)
		}
	}

	return nil
}

// Helper functions
func containsString(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}

func removeString(slice []string, s string) []string {
	var result []string
	for _, item := range slice {
		if item != s {
			result = append(result, item)
		}
	}
	return result
}
