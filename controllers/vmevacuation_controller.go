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

// Package controllers implements the VM Evacuation Controller (VEC) for workload clusters.
//
// VEC runs in each workload cluster (WLC) and coordinates control-plane VM relocation
// when the Host Control Plane (HCP) marks a physical host for maintenance.
//
// Note: VEC is only registered when --cluster-role=wlc is set. WLC clusters do NOT have
// LXD enabled (isLXDEnabled=false); HCP clusters have LXD enabled (isLXDEnabled=true).
//
// Key responsibilities:
//  1. Detect CP Machines on hosts marked with maintenance tags (maas-lxd-host-maintenance, maas-lxd-host-noschedule)
//  2. Derive operation ID (opID) from host tag maas-lxd-hcp-op-<uuid>
//  3. Identify CP Machine on source host via providerID/host mapping
//  4. For 3-CP clusters: delete targeted CP Machine; KCP replaces one
//  5. For 1-CP clusters: KCP template swap with maxSurge=1 (handled in another repo)

package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
)

// VMEvacuationReconciler reconciles control-plane VM evacuation for workload clusters.
// It runs in each WLC and watches for host maintenance tags from the HCP.
type VMEvacuationReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

const (
	// VECMaintenanceConfigMapPrefix is the prefix for ConfigMap names storing maintenance session state
	VECMaintenanceConfigMapPrefix = "vec-maintenance-"
)

// getMaintenanceConfigMapName returns the ConfigMap name for a given opID
func getMaintenanceConfigMapName(opID string) string {
	return VECMaintenanceConfigMapPrefix + opID
}

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasmachines,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=maasmachinetemplates,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=kubeadmcontrolplanes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles the VM evacuation process for workload clusters
func (r *VMEvacuationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("maascluster", req.Name, "namespace", req.Namespace)

	// Fetch the MaasCluster instance
	maasCluster := &infrav1beta1.MaasCluster{}
	if err := r.Get(ctx, req.NamespacedName, maasCluster); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Get the Cluster
	cluster, err := util.GetOwnerCluster(ctx, r.Client, maasCluster.ObjectMeta)
	if err != nil || cluster == nil {
		log.Info("Cluster not found or not ready")
		return ctrl.Result{}, nil
	}

	// Create cluster scope
	clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:         r.Client,
		Logger:         log,
		Cluster:        cluster,
		MaasCluster:    maasCluster,
		ControllerName: "vmevacuation",
	})
	if err != nil {
		return ctrl.Result{}, errors.Wrap(err, "failed to create cluster scope")
	}

	// Get MAAS client
	clientIdentity := clusterScope.GetMaasClientIdentity()
	maasClient := maasclient.NewAuthenticatedClientSet(clientIdentity.URL, clientIdentity.Token)

	// Step 1: Check for existing maintenance sessions in ConfigMaps
	activeSessions, err := r.listMaintenanceSessions(ctx, cluster.Namespace)
	if err != nil {
		log.Error(err, "failed to list maintenance sessions")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 2: If there's any active session, wait (only one CP maintenance at a time)
	if len(activeSessions) > 0 {
		// Get the first active session
		var activeOpID string
		for opID := range activeSessions {
			activeOpID = opID
			break
		}
		log.Info("Active maintenance session in progress", "opID", activeOpID, "count", len(activeSessions))

		// Check if KCP is stable
		kcp, err := r.getKubeadmControlPlane(ctx, cluster)
		if err != nil {
			// Log at Error level so RBAC or connectivity issues are immediately
			// visible instead of silently looping. This was the root cause of
			// PCP-XXXX where missing RBAC caused VEC to loop for hours.
			log.Error(err, "failed to get KubeadmControlPlane during active session — check RBAC for controlplane.cluster.x-k8s.io", "opID", activeOpID)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if !r.isKCPStable(kcp, log) {
			log.V(1).Info("KCP not yet stable, waiting", "opID", activeOpID)
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}

		// KCP is stable - tag replacement VM with ready-op and complete session
		if err := r.tagReplacementVMAndCompleteSession(ctx, maasClient, cluster, activeOpID, log); err != nil {
			log.Error(err, "failed to tag replacement VM and complete session", "opID", activeOpID)
		}

		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Step 3: No active session - check for new maintenance hosts
	cpMachinesOnMaintenanceHosts, err := r.findCPMachinesOnMaintenanceHosts(ctx, maasClient, cluster, cluster.Namespace, log)
	if err != nil {
		log.Error(err, "failed to find CP machines on maintenance hosts")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	if len(cpMachinesOnMaintenanceHosts) == 0 {
		// No CP machines on maintenance hosts
		return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
	}

	log.Info("Found CP machines on maintenance hosts", "count", len(cpMachinesOnMaintenanceHosts))

	// Step 4: Process only the first CP maintenance (one at a time)
	cpInfo := cpMachinesOnMaintenanceHosts[0]
	log.Info("Starting new maintenance session (processing one at a time)",
		"machine", cpInfo.Machine.Name,
		"hostSystemID", cpInfo.HostSystemID,
		"opID", cpInfo.OpID,
		"totalFound", len(cpMachinesOnMaintenanceHosts))

	// Get KCP and check if we can proceed
	kcp, err := r.getKubeadmControlPlane(ctx, cluster)
	if err != nil {
		log.Error(err, "failed to get KubeadmControlPlane")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if KCP is stable before proceeding
	if !r.isKCPStable(kcp, log) {
		log.Info("KCP not stable, waiting", "kcp", kcp.GetName())
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Check if another CP is being deleted
	if r.hasOtherCPBeingDeleted(ctx, cluster, cpInfo.Machine, log) {
		log.Info("Another CP is being deleted, waiting")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Determine replica count and evacuation strategy
	replicas := int32(3) // default
	if kcp != nil {
		if r, found, _ := unstructured.NestedInt64(kcp.Object, "spec", "replicas"); found {
			// Validate range to prevent integer overflow
			if r < 0 || r > 2147483647 {
				log.Info("Invalid replica count, using default", "replicas", r)
			} else {
				replicas = int32(r)
			}
		}
	}

	// Execute evacuation based on replica count
	if replicas >= 3 {
		// 3-CP: delete CP Machine; KCP will replace it
		log.Info("Executing 3-CP evacuation strategy", "machine", cpInfo.Machine.Name)

		// Delete the CP machine
		if err := r.deleteCPMachine(ctx, cpInfo.Machine, log); err != nil {
			log.Error(err, "failed to delete CP machine")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		log.Info("Successfully deleted CP machine", "machine", cpInfo.Machine.Name)
	} else if replicas == 1 {
		// 1-CP: template swap with maxSurge=1 (new CP first, then old CP removed)
		log.Info("Executing 1-CP evacuation strategy (template swap with maxSurge=1)", "machine", cpInfo.Machine.Name)

		// Create new MaasMachineTemplate with different name
		newTemplateName, err := r.createNewMaasMachineTemplate(ctx, kcp, log)
		if err != nil {
			log.Error(err, "failed to create new MaasMachineTemplate")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		log.Info("Created new MaasMachineTemplate", "template", newTemplateName)

		// Update KCP to reference new template and set maxSurge=1
		if err := r.swapKCPTemplateWithMaxSurge(ctx, kcp, newTemplateName, log); err != nil {
			log.Error(err, "failed to swap KCP template")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		log.Info("Successfully swapped KCP template with maxSurge=1", "newTemplate", newTemplateName)
	}

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
}

// cpMachineOnMaintenanceHost holds information about a CP Machine on a maintenance host
type cpMachineOnMaintenanceHost struct {
	Machine      *clusterv1.Machine
	HostSystemID string
	OpID         string
}

// findCPMachinesOnMaintenanceHosts finds all CP Machines in this cluster whose LXD hosts are under maintenance
// and creates ConfigMaps to track the maintenance sessions
func (r *VMEvacuationReconciler) findCPMachinesOnMaintenanceHosts(ctx context.Context, maasClient maasclient.ClientSetInterface, cluster *clusterv1.Cluster, namespace string, log logr.Logger) ([]cpMachineOnMaintenanceHost, error) {
	var result []cpMachineOnMaintenanceHost

	// List all CP Machines in this cluster
	machineList := &clusterv1.MachineList{}
	labels := map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}

	if err := r.List(ctx, machineList, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, errors.Wrap(err, "failed to list machines")
	}

	// For each CP Machine, check if its host is under maintenance
	for i := range machineList.Items {
		machine := &machineList.Items[i]

		// Skip if being deleted
		if !machine.DeletionTimestamp.IsZero() {
			continue
		}

		// Get the MaasMachine
		if machine.Spec.InfrastructureRef.Name == "" {
			continue
		}

		maasMachine := &infrav1beta1.MaasMachine{}
		key := client.ObjectKey{
			Namespace: machine.Namespace,
			Name:      machine.Spec.InfrastructureRef.Name,
		}

		if err := r.Get(ctx, key, maasMachine); err != nil {
			log.Error(err, "failed to get MaasMachine", "name", key.Name)
			continue
		}

		// Parse providerID to get VM systemID
		if maasMachine.Spec.ProviderID == nil || *maasMachine.Spec.ProviderID == "" {
			continue
		}

		vmSystemID := extractSystemIDFromProviderID(*maasMachine.Spec.ProviderID)
		if vmSystemID == "" {
			continue
		}

		// Find which LXD host this VM is on
		hostSystemID, err := r.getVMHostSystemID(ctx, maasClient, vmSystemID, log)
		if err != nil {
			log.V(1).Error(err, "failed to get VM host", "vmSystemID", vmSystemID, "machine", machine.Name)
			continue
		}

		// Get the LXD host details to check for maintenance tags
		hostDetails, err := maasClient.Machines().Machine(hostSystemID).Get(ctx)
		if err != nil {
			log.V(1).Error(err, "failed to get host details", "hostSystemID", hostSystemID)
			continue
		}

		tags := hostDetails.Tags()
		if len(tags) == 0 {
			continue
		}

		// Check if this host has all 3 required maintenance tags
		hasMaintenance := false
		hasNoSchedule := false
		opID := ""

		for _, tag := range tags {
			if tag == maintenance.TagHostMaintenance {
				hasMaintenance = true
			}
			if tag == maintenance.TagHostNoSchedule {
				hasNoSchedule = true
			}
			if strings.HasPrefix(tag, maintenance.TagHostOpPrefix) {
				opID = strings.TrimPrefix(tag, maintenance.TagHostOpPrefix)
			}
		}

		// Host is in maintenance mode ONLY when all 3 tags are present
		if hasMaintenance && hasNoSchedule && opID != "" {
			log.Info("Found CP machine on maintenance host",
				"machine", machine.Name,
				"vmSystemID", vmSystemID,
				"hostSystemID", hostSystemID,
				"hostname", hostDetails.Hostname(),
				"opID", opID)

			// Create maintenance session ConfigMap immediately with status Active
			if err := r.saveMaintenanceSession(ctx, namespace, opID, hostSystemID, string(maintenance.StatusActive)); err != nil {
				log.Error(err, "failed to save maintenance session ConfigMap", "opID", opID)
				// Continue even if ConfigMap creation fails
			} else {
				log.Info("Created maintenance session ConfigMap", "opID", opID, "hostSystemID", hostSystemID)
			}

			result = append(result, cpMachineOnMaintenanceHost{
				Machine:      machine,
				HostSystemID: hostSystemID,
				OpID:         opID,
			})
		}
	}

	return result, nil
}

// getVMHostSystemID gets the parent host systemID for an LXD VM
func (r *VMEvacuationReconciler) getVMHostSystemID(ctx context.Context, maasClient maasclient.ClientSetInterface, vmSystemID string, log logr.Logger) (string, error) {
	// Get the VM's machine details from MAAS
	vmMachine, err := maasClient.Machines().Machine(vmSystemID).Get(ctx)
	if err != nil {
		return "", errors.Wrap(err, "failed to get VM machine details")
	}

	// Get the parent systemID (bare metal host's systemID)
	hostSystemID := vmMachine.Parent()
	if hostSystemID == "" {
		return "", fmt.Errorf("VM %s has no parent host", vmSystemID)
	}

	log.V(1).Info("Found VM parent host",
		"vmSystemID", vmSystemID,
		"hostSystemID", hostSystemID)

	return hostSystemID, nil
}

// extractSystemIDFromProviderID extracts the system ID from a MAAS providerID
func extractSystemIDFromProviderID(providerID string) string {
	// Format: maas:///<zone>/<systemID>
	parts := strings.Split(providerID, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-1]
}

// getKubeadmControlPlane retrieves the KubeadmControlPlane for the cluster
func (r *VMEvacuationReconciler) getKubeadmControlPlane(ctx context.Context, cluster *clusterv1.Cluster) (*unstructured.Unstructured, error) {
	if cluster.Spec.ControlPlaneRef.Name == "" {
		return nil, fmt.Errorf("cluster has no controlPlaneRef")
	}

	kcp := &unstructured.Unstructured{}
	kcp.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "controlplane.cluster.x-k8s.io",
		Version: "v1beta2",
		Kind:    "KubeadmControlPlane",
	})

	key := client.ObjectKey{
		Namespace: cluster.Namespace,
		Name:      cluster.Spec.ControlPlaneRef.Name,
	}

	if err := r.Get(ctx, key, kcp); err != nil {
		return nil, errors.Wrap(err, "failed to get KubeadmControlPlane")
	}

	return kcp, nil
}

// isKCPStable checks if the KubeadmControlPlane is in a stable state
func (r *VMEvacuationReconciler) isKCPStable(kcp *unstructured.Unstructured, log logr.Logger) bool {
	if kcp == nil {
		return false
	}

	// Check if KCP is paused
	if paused, hasPausedField, _ := unstructured.NestedBool(kcp.Object, "spec", "paused"); hasPausedField && paused {
		log.Info("KCP is paused")
		return false
	}

	// Get replica counts from status
	specReplicas, hasSpecReplicas, _ := unstructured.NestedInt64(kcp.Object, "spec", "replicas")
	readyReplicas, hasReadyReplicas, _ := unstructured.NestedInt64(kcp.Object, "status", "readyReplicas")
	updatedReplicas, hasUpdatedReplicas, _ := unstructured.NestedInt64(kcp.Object, "status", "updatedReplicas")
	replicas, hasStatusReplicas, _ := unstructured.NestedInt64(kcp.Object, "status", "replicas")

	if !hasSpecReplicas || !hasReadyReplicas || !hasUpdatedReplicas || !hasStatusReplicas {
		log.Info("KCP status fields not found")
		return false
	}

	// KCP is stable when:
	// readyReplicas == updatedReplicas == replicas == spec.replicas
	stable := readyReplicas == updatedReplicas && updatedReplicas == replicas && replicas == specReplicas
	if !stable {
		log.Info("KCP not stable",
			"specReplicas", specReplicas,
			"readyReplicas", readyReplicas,
			"updatedReplicas", updatedReplicas,
			"replicas", replicas)
	}

	return stable
}

// hasOtherCPBeingDeleted checks if another CP Machine is being deleted
func (r *VMEvacuationReconciler) hasOtherCPBeingDeleted(ctx context.Context, cluster *clusterv1.Cluster, currentMachine *clusterv1.Machine, log logr.Logger) bool {
	machineList := &clusterv1.MachineList{}
	labels := map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}

	if err := r.List(ctx, machineList, client.InNamespace(cluster.Namespace), client.MatchingLabels(labels)); err != nil {
		log.Error(err, "failed to list machines")
		return true // Assume blocked on error
	}

	for i := range machineList.Items {
		machine := &machineList.Items[i]
		// Skip self
		if machine.UID == currentMachine.UID {
			continue
		}

		// If any other CP has deletionTimestamp set, block
		if !machine.DeletionTimestamp.IsZero() {
			log.Info("Another CP is being deleted",
				"currentMachine", currentMachine.Name,
				"otherMachine", machine.Name)
			return true
		}
	}

	return false
}

// deleteCPMachine deletes a control-plane Machine
func (r *VMEvacuationReconciler) deleteCPMachine(ctx context.Context, machine *clusterv1.Machine, log logr.Logger) error {
	log.Info("Deleting CP machine", "machine", machine.Name)

	// Delete the Machine object
	if err := r.Delete(ctx, machine); err != nil {
		if apierrors.IsNotFound(err) {
			// Already deleted
			return nil
		}
		return errors.Wrap(err, "failed to delete machine")
	}

	return nil
}

// createNewMaasMachineTemplate clones the current MaasMachineTemplate with a new name
func (r *VMEvacuationReconciler) createNewMaasMachineTemplate(ctx context.Context, kcp *unstructured.Unstructured, log logr.Logger) (string, error) {
	// Get current template reference from KCP
	templateRef, found, err := unstructured.NestedMap(kcp.Object, "spec", "machineTemplate", "infrastructureRef")
	if err != nil || !found {
		return "", fmt.Errorf("failed to get machineTemplate.infrastructureRef from KCP")
	}

	currentTemplateName, ok := templateRef["name"].(string)
	if !ok || currentTemplateName == "" {
		return "", fmt.Errorf("invalid template name in KCP")
	}

	templateNamespace, ok := templateRef["namespace"].(string)
	if !ok || templateNamespace == "" {
		templateNamespace = kcp.GetNamespace()
	}

	// Get the current MaasMachineTemplate
	currentTemplate := &infrav1beta1.MaasMachineTemplate{}
	key := client.ObjectKey{
		Namespace: templateNamespace,
		Name:      currentTemplateName,
	}

	if err := r.Get(ctx, key, currentTemplate); err != nil {
		return "", errors.Wrap(err, "failed to get current MaasMachineTemplate")
	}

	// Create new template with UUID suffix to avoid collisions
	newTemplateName := fmt.Sprintf("%s-evac-%s", currentTemplateName, uuid.New().String()[:8])
	newTemplate := &infrav1beta1.MaasMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      newTemplateName,
			Namespace: templateNamespace,
			Labels:    currentTemplate.Labels,
		},
		Spec: currentTemplate.Spec,
	}

	// Create the new template
	if err := r.Create(ctx, newTemplate); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", errors.Wrap(err, "failed to create new MaasMachineTemplate")
		}
		// Already exists, validate that it has the correct spec
		existingTemplate := &infrav1beta1.MaasMachineTemplate{}
		existingKey := client.ObjectKey{
			Namespace: templateNamespace,
			Name:      newTemplateName,
		}
		if err := r.Get(ctx, existingKey, existingTemplate); err != nil {
			return "", errors.Wrap(err, "failed to get existing MaasMachineTemplate for validation")
		}

		// Compare specs to ensure they match
		if !reflect.DeepEqual(existingTemplate.Spec, currentTemplate.Spec) {
			return "", fmt.Errorf("existing MaasMachineTemplate %s has different spec than expected, this could lead to unexpected behavior", newTemplateName)
		}

		log.Info("New MaasMachineTemplate already exists with correct spec", "template", newTemplateName)
	}

	return newTemplateName, nil
}

// swapKCPTemplateWithMaxSurge updates KCP to reference new template and sets maxSurge=1
func (r *VMEvacuationReconciler) swapKCPTemplateWithMaxSurge(ctx context.Context, kcp *unstructured.Unstructured, newTemplateName string, log logr.Logger) error {
	if kcp == nil {
		return fmt.Errorf("KCP is nil")
	}

	// Set new template name
	if err := unstructured.SetNestedField(kcp.Object, newTemplateName, "spec", "machineTemplate", "infrastructureRef", "name"); err != nil {
		return errors.Wrap(err, "failed to set new template name")
	}

	// Set maxSurge=1
	if err := unstructured.SetNestedField(kcp.Object, int64(1), "spec", "rolloutStrategy", "rollingUpdate", "maxSurge"); err != nil {
		return errors.Wrap(err, "failed to set maxSurge")
	}

	// Update KCP
	if err := r.Update(ctx, kcp); err != nil {
		return errors.Wrap(err, "failed to update KCP")
	}

	log.Info("Successfully swapped KCP template and set maxSurge=1", "newTemplate", newTemplateName)
	return nil
}

// listMaintenanceSessions returns all active maintenance sessions in the namespace
func (r *VMEvacuationReconciler) listMaintenanceSessions(ctx context.Context, namespace string) (map[string]string, error) {
	cmList := &corev1.ConfigMapList{}
	if err := r.List(ctx, cmList, client.InNamespace(namespace)); err != nil {
		return nil, errors.Wrap(err, "failed to list ConfigMaps")
	}

	sessions := make(map[string]string) // opID -> status
	for _, cm := range cmList.Items {
		if strings.HasPrefix(cm.Name, VECMaintenanceConfigMapPrefix) {
			opID := cm.Data[maintenance.CmKeyOpID]
			status := cm.Data[maintenance.CmKeyStatus]
			if opID != "" && status == string(maintenance.StatusActive) {
				sessions[opID] = status
			}
		}
	}

	return sessions, nil
}

// saveMaintenanceSession creates or updates the maintenance session ConfigMap
func (r *VMEvacuationReconciler) saveMaintenanceSession(ctx context.Context, namespace, opID, hostSystemID, status string) error {
	cmName := getMaintenanceConfigMapName(opID)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		Data: map[string]string{
			maintenance.CmKeyOpID:        opID,
			maintenance.CmKeyStatus:      status,
			maintenance.CmKeyCurrentHost: hostSystemID,
			maintenance.CmKeyStartedAt:   time.Now().UTC().Format(time.RFC3339),
		},
	}

	// Try to create, if exists, update
	if err := r.Create(ctx, cm); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return errors.Wrap(err, "failed to create maintenance session ConfigMap")
		}

		// Update existing
		existing := &corev1.ConfigMap{}
		key := types.NamespacedName{
			Namespace: namespace,
			Name:      cmName,
		}
		if err := r.Get(ctx, key, existing); err != nil {
			return errors.Wrap(err, "failed to get existing ConfigMap")
		}

		existing.Data = cm.Data
		if err := r.Update(ctx, existing); err != nil {
			return errors.Wrap(err, "failed to update maintenance session ConfigMap")
		}
	}

	return nil
}

// tagReplacementVMAndCompleteSession tags the replacement VM with ready-op after KCP is stable
func (r *VMEvacuationReconciler) tagReplacementVMAndCompleteSession(ctx context.Context, maasClient maasclient.ClientSetInterface, cluster *clusterv1.Cluster, opID string, log logr.Logger) error {
	// Get the maintenance session ConfigMap
	cm := &corev1.ConfigMap{}
	cmKey := types.NamespacedName{
		Namespace: cluster.Namespace,
		Name:      getMaintenanceConfigMapName(opID),
	}

	if err := r.Get(ctx, cmKey, cm); err != nil {
		return errors.Wrap(err, "failed to get maintenance session ConfigMap")
	}

	// Check if newVMSystemID has been set by provisioning
	newVMSystemID := cm.Data[maintenance.CmKeyNewVMSystemID]
	if newVMSystemID == "" {
		log.V(1).Info("Replacement VM not yet provisioned (newVMSystemID not set)", "opID", opID)
		return nil
	}

	log.Info("Found replacement VM in ConfigMap, tagging with ready-op", "opID", opID, "systemID", newVMSystemID)

	// Tag the VM with maas-lxd-ready-op-<opID>
	if err := maintenance.TagVMReadyOp(ctx, maasClient, newVMSystemID, opID); err != nil {
		return errors.Wrap(err, "failed to tag replacement VM with ready-op")
	}

	log.Info("Successfully tagged replacement VM with ready-op", "opID", opID, "systemID", newVMSystemID)

	// Mark ConfigMap as completed
	if cm.Data == nil {
		cm.Data = make(map[string]string)
	}
	cm.Data[maintenance.CmKeyStatus] = string(maintenance.StatusCompleted)
	if err := r.Update(ctx, cm); err != nil {
		return errors.Wrap(err, "failed to mark session as completed")
	}

	log.Info("Marked maintenance session as completed", "opID", opID)
	return nil
}

// SetupWithManager sets up the controller with the Manager
func (r *VMEvacuationReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, options controller.Options) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("vec").
		For(&infrav1beta1.MaasCluster{}).
		WithOptions(options).
		Complete(r)
}
