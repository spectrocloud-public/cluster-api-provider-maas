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

package lxd

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/maas-client-go/maasclient"
	"k8s.io/klog/v2/textlogger"
)

// HostConfig contains the configuration for setting up an LXD host
type HostConfig struct {
	NodeIP          string
	HostName        string
	MaasAPIKey      string
	MaasAPIEndpoint string
	StorageBackend  string
	StorageSize     string
	NetworkBridge   string
	Zone            string
	ResourcePool    string
	TrustPassword   string
}

// validateHostConfig validates the host configuration
func validateHostConfig(config HostConfig) error {
	if config.NodeIP == "" {
		return fmt.Errorf("node IP is required")
	}

	if config.MaasAPIKey == "" {
		return fmt.Errorf("MAAS API key is required")
	}

	if config.MaasAPIEndpoint == "" {
		return fmt.Errorf("MAAS API endpoint is required")
	}

	return nil
}

// SetupLXDHostWithMaasClient sets up an LXD host on a node using the official MAAS client
// This function assumes that LXD initialization is handled by the DaemonSet
// It only checks if the host is registered with MAAS and registers it if not
func SetupLXDHostWithMaasClient(config HostConfig) error {
	log := textlogger.NewLogger(textlogger.NewConfig())
	log.Info("Setting up LXD host with official MAAS client", "node", config.NodeIP)

	// Validate configuration
	if err := validateHostConfig(config); err != nil {
		return fmt.Errorf("invalid host configuration: %w", err)
	}

	// Create MAAS client
	client := maasclient.NewAuthenticatedClientSet(config.MaasAPIEndpoint, config.MaasAPIKey)

	// Check if the host is already registered with MAAS (by systemID, desired name, or power address)
	hn := strings.TrimSpace(config.HostName)
	desiredName := fmt.Sprintf("lxd-host-%s", hn)
	if hn == "" {
		desiredName = fmt.Sprintf("lxd-host-%s", config.NodeIP)
	}
	isRegistered, err := isHostRegisteredWithMaasClientAdvanced(client, "", desiredName, config.NodeIP)
	if err != nil {
		return fmt.Errorf("failed to check if host is registered: %w", err)
	}

	if isRegistered {
		log.Info("LXD host is already registered with MAAS", "node", config.NodeIP)
		return nil
	}

	// Register the host with MAAS as a KVM host
	if err := registerWithMaasClient(client, config); err != nil {
		return fmt.Errorf("failed to register with MAAS: %w", err)
	}

	log.Info("Successfully set up LXD host", "node", config.NodeIP)
	return nil
}

// normalizeHost extracts the host part from a MAAS power_address or raw string
func normalizeHost(s string) string {
	if s == "" {
		return ""
	}
	// If there is no scheme, add one so url.Parse works.
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return s
	}
	h := u.Host
	if h == "" {
		h = u.Path // fallback when parse put everything into Path
	}
	if hp, _, err2 := net.SplitHostPort(h); err2 == nil {
		h = hp
	}
	return h
}

// isHostRegisteredWithMaasClient checks if a host is already registered with MAAS as a VM host
// isHostRegisteredWithMaasClientAdvanced returns true if a VM host exists matching systemID, desired name, or power address host
func isHostRegisteredWithMaasClientAdvanced(client maasclient.ClientSetInterface, systemID, desiredName, nodeIP string) (bool, error) {
	ctx := context.Background()

	vmHosts, err := client.VMHosts().List(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("failed to get VM hosts: %w", err)
	}

	wantName := strings.TrimSpace(desiredName)
	wantHost := normalizeHost(strings.TrimSpace(nodeIP))

	for _, host := range vmHosts {
		// 1) Prefer exact hostSystemID match when provided
		if strings.TrimSpace(systemID) != "" && host.HostSystemID() == strings.TrimSpace(systemID) {
			return true, nil
		}
		// 2) Compare by desired name
		if wantName != "" && host.Name() == wantName {
			return true, nil
		}
		// 3) Legacy power address host match
		if wantHost != "" && normalizeHost(host.PowerAddress()) == wantHost {
			return true, nil
		}
	}
	return false, nil
}

// registerWithMaasClient registers a host with MAAS as a VM host
func registerWithMaasClient(client maasclient.ClientSetInterface, config HostConfig) error {
	ctx := context.Background()

	// Create registration parameters
	name := strings.TrimSpace(config.HostName)
	if name != "" {
		name = fmt.Sprintf("lxd-host-%s", name)
	} else {
		name = fmt.Sprintf("lxd-host-%s", config.NodeIP)
	}
	params := maasclient.ParamsBuilder().
		Set("type", "lxd").
		Set("power_address", fmt.Sprintf("https://%s:8443", config.NodeIP)).
		Set("name", name)

	if config.Zone != "" {
		// Pass the zone name directly. MAAS API expects the zone name, not ID.
		params.Set("zone", config.Zone)
	}

	if config.ResourcePool != "" {
		// Pass pool name directly.
		params.Set("pool", config.ResourcePool)
	}

	if config.TrustPassword != "" {
		params.Set("password", config.TrustPassword)
	}

	log := textlogger.NewLogger(textlogger.NewConfig())
	log.Info("register params", "zone", params.Values().Get("zone"), "pool", params.Values().Get("pool"), "name", name)

	// Register the host with MAAS
	_, err := client.VMHosts().Create(ctx, params)
	if err != nil {
		return fmt.Errorf("failed to register host with MAAS: %w", err)
	}

	return nil
}

// GetAvailableLXDHostsWithMaasClient returns a list of available LXD hosts from MAAS
func GetAvailableLXDHostsWithMaasClient(apiKey, apiEndpoint string) ([]maasclient.VMHost, error) {
	// Create MAAS client
	client := maasclient.NewAuthenticatedClientSet(apiEndpoint, apiKey)

	// Get all VM hosts
	ctx := context.Background()
	vmHosts, err := client.VMHosts().List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get VM hosts: %w", err)
	}

	return vmHosts, nil
}

// UnregisterLXDHostByNameWithMaasClient removes a VM host registration from MAAS by matching the exact host name
func UnregisterLXDHostByNameWithMaasClient(apiKey, apiEndpoint, hostName string) error {
	client := maasclient.NewAuthenticatedClientSet(apiEndpoint, apiKey)

	ctx := context.Background()
	vmHosts, err := client.VMHosts().List(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to get VM hosts: %w", err)
	}

	for _, host := range vmHosts {
		if host.Name() == hostName {
			if derr := client.VMHosts().VMHost(host.SystemID()).Delete(ctx); derr != nil {
				return fmt.Errorf("failed to delete VM host %s (id=%s): %w", host.Name(), host.SystemID(), derr)
			}
			log := textlogger.NewLogger(textlogger.NewConfig())
			log.Info("Successfully unregistered LXD host", "name", hostName, "id", host.SystemID())
			return nil
		}
	}
	return nil
}

// isHostUnderMaintenance checks if a host has maintenance tags
func isHostUnderMaintenance(client machineGetter, hostSystemID string, log logr.Logger) bool {
	if hostSystemID == "" {
		return false
	}

	ctx := context.Background()
	machine, err := client.Machines().Machine(hostSystemID).Get(ctx)
	if err != nil {
		log.Info("Failed to get machine details for maintenance check", "system-id", hostSystemID, "error", err.Error())
		return false // Assume not under maintenance if we can't check
	}

	tags := machine.Tags()
	for _, tag := range tags {
		if tag == maintenance.TagHostMaintenance || tag == maintenance.TagHostNoSchedule {
			return true
		}
	}
	return false
}

// machineGetter narrows the client interface to only what is needed here
type machineGetter interface {
	Machines() maasclient.Machines
}

// lxdHostSelectorClient extends machineGetter with VMHosts for CP distribution counting
type lxdHostSelectorClient interface {
	machineGetter
	VMHosts() maasclient.VMHosts
}

// SelectOptions holds placement constraints and hints for LXD host selection.
// Semantics align with MAAS allocator params for BM: each field is optional;
// when set, it is enforced strictly; when zero/empty, that dimension is unconstrained.
type SelectOptions struct {
	Zone          string   // FailureDomain / AZ; empty = any
	ResourcePool  string   // MAAS resource pool; empty = any
	Tags          []string // All required on VM host (host.Tags()); nil/empty = any
	MinCores      int      // Minimum available cores on host; 0 = no minimum
	MinMemory     int      // Minimum available memory (MB) on host; 0 = no minimum
	MinDiskSizeGB int      // Minimum uncommitted disk space (GB) on host; 0 = no minimum

	// ClusterID When set (non-empty), the selector counts how many control-plane VMs for this
	// cluster already exist on each eligible host. Hosts with fewer existing CP VMs
	// are preferred, distributing CP VMs across multiple physical hosts.
	ClusterID string
}

// candidateHost holds a VM host along with computed metrics for ranking.
// The cpCount field is used for anti-affinity: hosts with lower cpCount are preferred
// to spread control-plane VMs across multiple physical hosts.
// The resourceScore combines available cores and memory into a single metric for ranking.
type candidateHost struct {
	host          maasclient.VMHost
	cpCount       int     // number of CP VMs for the target cluster already on this host (0 = ideal for anti-affinity)
	resourceScore float64 // combined resource availability score (higher = more resources)
}

// SelectLXDHostWithMaasClient selects an LXD host based on SelectOptions.
// It implements a filter-then-rank approach:
//   - Filter: zone, pool, tags (all must match when set), resources, health, maintenance
//   - Rank: fewer CP VMs for cluster → combined resource score (cores + memory) → managed host
//
// No fallback or constraint relaxation: if no host passes all filters, an error is returned.
func SelectLXDHostWithMaasClient(client lxdHostSelectorClient, hosts []maasclient.VMHost, opts SelectOptions) (maasclient.VMHost, error) {
	log := textlogger.NewLogger(textlogger.NewConfig())
	ctx := context.Background()

	if len(hosts) == 0 {
		return nil, fmt.Errorf("no LXD hosts available")
	}

	// Build cluster tag for CP distribution counting.
	// When ClusterID is set, we look for VMs tagged with both:
	//   - TagVMControlPlane ("maas-lxd-wlc-cp"): marks VM as a control-plane node
	//   - TagVMClusterPrefix + clusterID: identifies which cluster the VM belongs to
	// This allows us to count how many CP VMs from THIS cluster are already on each host.
	clusterTag := ""
	if opts.ClusterID != "" {
		clusterTag = maintenance.TagVMClusterPrefix + maintenance.SanitizeID(opts.ClusterID)
	}

	cpCountByHost := make(map[string]int)
	if clusterTag != "" {
		allMachines, err := client.Machines().List(ctx, nil)
		if err != nil {
			log.Info("Warning: failed to list machines for CP counting, proceeding without anti-affinity", "error", err.Error())
		} else {
			cpCountByHost = computeCPCountsByHost(allMachines, clusterTag)
		}
	}

	// Filter phase: collect eligible hosts
	log.Info("Starting host selection", "totalHosts", len(hosts), "opts", fmt.Sprintf("%+v", opts))
	var candidates []candidateHost
	for _, host := range hosts {
		hostZone := ""
		if host.Zone() != nil {
			hostZone = host.Zone().Name()
		}
		hostPool := ""
		if host.ResourcePool() != nil {
			hostPool = host.ResourcePool().Name()
		}
		log.Info("Evaluating host", "name", host.Name(), "zone", hostZone, "pool", hostPool,
			"tags", host.Tags(), "cores", host.AvailableCores(), "memory", host.AvailableMemory(),
			"hostSystemID", host.HostSystemID())

		// 1. Zone filter
		if opts.Zone != "" {
			if hostZone != opts.Zone {
				log.Info("Skipping host: zone mismatch", "host", host.Name(), "hostZone", hostZone, "requiredZone", opts.Zone)
				continue
			}
		}

		// 2. Resource pool filter
		if opts.ResourcePool != "" {
			if hostPool != opts.ResourcePool {
				log.Info("Skipping host: pool mismatch", "host", host.Name(), "hostPool", hostPool, "requiredPool", opts.ResourcePool)
				continue
			}
		}

		// 3. Tags filter: host must have ALL specified tags
		if len(opts.Tags) > 0 {
			if !hostHasAllTags(host, opts.Tags) {
				log.Info("Skipping host: missing required tags", "host", host.Name(), "hostTags", host.Tags(), "requiredTags", opts.Tags)
				continue
			}
		}

		// 4. Resource filter: check available cores and memory
		if opts.MinCores > 0 && host.AvailableCores() < opts.MinCores {
			log.Info("Skipping host: insufficient cores", "host", host.Name(),
				"available", host.AvailableCores(), "required", opts.MinCores)
			continue
		}
		if opts.MinMemory > 0 && host.AvailableMemory() < opts.MinMemory {
			log.Info("Skipping host: insufficient memory", "host", host.Name(),
				"available", host.AvailableMemory(), "required", opts.MinMemory)
			continue
		}

		// 5. Storage filter: ensure at least one local storage pool has enough uncommitted space.
		// MAAS allows over-commitment by counting pending (allocated but not yet used) storage
		// as available. We subtract pending to prevent composing VMs onto a host that is
		// already over-committed at the storage layer.
		if opts.MinDiskSizeGB > 0 {
			// MAAS reports storage in decimal bytes (1 GB = 1,000,000,000 bytes),
			// matching what the MAAS UI displays — use 1000^3, not 1024^3.
			requiredBytes := int64(opts.MinDiskSizeGB) * 1000 * 1000 * 1000
			hasStorage := false
			for _, pool := range host.StoragePools() {
				if pool.Remote {
					continue
				}
				uncommitted := pool.Available - pool.Pending
				if uncommitted >= requiredBytes {
					hasStorage = true
					break
				}
			}
			if !hasStorage {
				log.Info("Skipping host: insufficient uncommitted storage", "host", host.Name(),
					"requiredGB", opts.MinDiskSizeGB, "pools", host.StoragePools())
				continue
			}
		}

		// 7. Health check: backing machine must be powered on and Deployed
		hostSystemID := host.HostSystemID()
		if hostSystemID == "" {
			continue
		}
		machine, err := client.Machines().Machine(hostSystemID).Get(ctx)
		if err != nil {
			log.Info("Failed to get backing machine", "host", host.Name(), "system-id", hostSystemID, "error", err.Error())
			continue
		}
		if machine.PowerState() != "on" || machine.State() != "Deployed" {
			log.Info("Skipping unhealthy host", "host", host.Name(),
				"power", machine.PowerState(), "state", machine.State())
			continue
		}

		// 8. Maintenance check
		if hasMaintenanceTag(machine.Tags()) {
			log.Info("Skipping host under maintenance", "host", host.Name())
			continue
		}

		// Look up pre-computed CP count for this host
		cpCount := cpCountByHost[hostSystemID]

		// Compute combined resource score.
		// Score = (availableCores / minCores) + (availableMemory / minMemory)
		// This balances both resources: a host with more headroom on both resources scores higher.
		// If minCores or minMemory is 0, that dimension contributes 0 to the score.
		resourceScore := computeResourceScore(host.AvailableCores(), host.AvailableMemory(), opts.MinCores, opts.MinMemory)

		candidates = append(candidates, candidateHost{
			host:          host,
			cpCount:       cpCount,
			resourceScore: resourceScore,
		})
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("no eligible LXD host found (zone=%q, pool=%q, tags=%v, minCores=%d, minMemory=%d)",
			opts.Zone, opts.ResourcePool, opts.Tags, opts.MinCores, opts.MinMemory)
	}

	// Rank phase: sort candidates by preference to select the best host.
	//
	// Priority order (highest to lowest):
	//   1. Anti-affinity: Prefer hosts with fewer CP VMs for this cluster.
	//   2. Combined resource score: Prefer hosts with higher combined resource availability.
	//      The score considers both cores and memory together, not sequentially.
	//   3. Managed preference: Prefer Palette-managed hosts (lxd-host-*) over OOB hosts.
	//      Tie-breaker when all else is equal.
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]

		// 1. Anti-affinity: fewer CP VMs wins
		if a.cpCount != b.cpCount {
			return a.cpCount < b.cpCount
		}

		// 2. Combined resource score: higher score wins
		if a.resourceScore != b.resourceScore {
			return a.resourceScore > b.resourceScore
		}

		// 3. Prefer managed host over OOB
		return isManagedHost(a.host) && !isManagedHost(b.host)
	})

	selected := candidates[0].host
	log.Info("Selected LXD host", "host", selected.Name(), "host-id", selected.SystemID(),
		"zone", opts.Zone, "pool", opts.ResourcePool, "cpCount", candidates[0].cpCount,
		"resourceScore", fmt.Sprintf("%.2f", candidates[0].resourceScore),
		"availableCores", selected.AvailableCores(), "availableMemory", selected.AvailableMemory())
	return selected, nil
}

// computeResourceScore calculates a combined resource availability score.
// The score normalizes cores and memory relative to the requested minimums,
// then sums them so that hosts with more headroom on both resources score higher.
//
// Formula: score = (availableCores / minCores) + (availableMemory / minMemory)
//
// Examples (minCores=4, minMemory=8192):
//
//	Host A: 8 cores, 16GB  → score = 8/4 + 16384/8192 = 2.0 + 2.0 = 4.0
//	Host B: 12 cores, 8GB  → score = 12/4 + 8192/8192 = 3.0 + 1.0 = 4.0
//	Host C: 4 cores, 32GB  → score = 4/4 + 32768/8192 = 1.0 + 4.0 = 5.0
//
// If minCores or minMemory is 0, that dimension contributes 0 to the score.
func computeResourceScore(availableCores, availableMemory, minCores, minMemory int) float64 {
	var coreRatio, memRatio float64

	if minCores > 0 {
		coreRatio = float64(availableCores) / float64(minCores)
	}
	if minMemory > 0 {
		memRatio = float64(availableMemory) / float64(minMemory)
	}

	return coreRatio + memRatio
}

// hostHasAllTags checks if the VM host has all the required tags
func hostHasAllTags(host maasclient.VMHost, requiredTags []string) bool {
	hostTags := host.Tags()
	hostTagSet := make(map[string]struct{}, len(hostTags))
	for _, t := range hostTags {
		hostTagSet[t] = struct{}{}
	}
	for _, required := range requiredTags {
		if _, ok := hostTagSet[required]; !ok {
			return false
		}
	}
	return true
}

// hasMaintenanceTag checks if tags contain maintenance or no-schedule tags
func hasMaintenanceTag(tags []string) bool {
	for _, tag := range tags {
		if tag == maintenance.TagHostMaintenance || tag == maintenance.TagHostNoSchedule {
			return true
		}
	}
	return false
}

// isManagedHost checks if the host is a Palette-managed LXD host by naming convention
func isManagedHost(h maasclient.VMHost) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(h.Name())), "lxd-host-")
}

// computeCPCountsByHost builds a map of hostSystemID → CP VM count for a given cluster.
// It processes all machines in a single pass, avoiding O(N_hosts × N_machines) API calls.
//
// The function:
//   - Filters machines by Parent() to identify which host they belong to
//   - Checks for TagVMControlPlane to identify control-plane VMs
//   - Checks for the cluster-specific tag to count only THIS cluster's CPs
//
// Example scenario with 3 LXD hosts creating a 3-node control plane:
//
//	Host A (H1): 1 CP VM (tagged maas-lxd-wlc-cp + maas-lxd-wlc-<cluster-id>)
//	Host B (H2): 1 CP VM (same tags)
//	Host C (H3): 0 CP VMs
//
// Returns: {"H1": 1, "H2": 1} (H3 not in map means 0)
//
// The caller uses this to prefer hosts with fewer existing CP VMs (anti-affinity).
func computeCPCountsByHost(machines []maasclient.Machine, clusterTag string) map[string]int {
	counts := make(map[string]int)

	for _, m := range machines {
		// Parent() returns the host's system ID for LXD VMs, empty for bare metal
		parentID := m.Parent()
		if parentID == "" {
			continue
		}

		tags := m.Tags()

		// Check if this VM is a control-plane node for the target cluster.
		// Both tags must be present:
		//   - TagVMControlPlane: identifies VM as a control-plane node
		//   - clusterTag: identifies which cluster this CP node belongs to
		hasCP := false
		hasCluster := false
		for _, tag := range tags {
			if tag == maintenance.TagVMControlPlane {
				hasCP = true
			}
			if tag == clusterTag {
				hasCluster = true
			}
		}
		if hasCP && hasCluster {
			counts[parentID]++
		}
	}
	return counts
}

// CreateLXDVMWithMaasClient creates a VM on an LXD host using MAAS API
func CreateLXDVMWithMaasClient(apiKey, apiEndpoint, vmHostID, vmName, vmCores, vmMemory, vmDisk, staticIP string) (string, error) {
	// Create MAAS client
	client := maasclient.NewAuthenticatedClientSet(apiEndpoint, apiKey)

	// Get the VM host
	vmHost := client.VMHosts().VMHost(vmHostID)

	// Create VM parameters
	params := maasclient.ParamsBuilder().
		Set("hostname", vmName).
		Set("cores", vmCores).
		Set("memory", vmMemory).
		Set("storage", vmDisk)

	if staticIP != "" {
		params.Set("interfaces", fmt.Sprintf("name=eth0,ip=%s", staticIP))
	}

	// Create the VM
	ctx := context.Background()
	machine, err := vmHost.Composer().Compose(ctx, params)
	if err != nil {
		return "", fmt.Errorf("failed to create VM: %w", err)
	}

	return machine.SystemID(), nil
}

// DeleteLXDVMWithMaasClient deletes a VM using MAAS API if it's an LXD VM
func DeleteLXDVMWithMaasClient(apiKey, apiEndpoint, systemID string) error {
	// Create MAAS client
	client := maasclient.NewAuthenticatedClientSet(apiEndpoint, apiKey)

	// Get the machine
	machine := client.Machines().Machine(systemID)
	ctx := context.Background()

	// Get machine details to check power type
	m, err := machine.Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get machine %s: %w", systemID, err)
	}

	// Only delete if this is an LXD VM
	if m.PowerType() != "lxd" {
		return fmt.Errorf("machine %s is not an LXD VM (power_type: %s)", systemID, m.PowerType())
	}

	// Delete the LXD VM
	if err := machine.Delete(ctx); err != nil {
		return fmt.Errorf("failed to delete LXD VM: %w", err)
	}

	return nil
}
