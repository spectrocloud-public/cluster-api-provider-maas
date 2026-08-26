package machine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"reflect"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/lxd"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/conditions"
)

// Service manages the MaaS machine
var (
	ErrBrokenMachine = errors.New("broken machine encountered")
	ErrVMComposing   = errors.New("vm composing/commissioning")
	reHostID         = regexp.MustCompile(`host (\d+)`)
	reMachineID      = regexp.MustCompile(`machine[s]? ([a-z0-9]{4,6})`)
)

const (
	clusterNamespacePrefix    = "cluster-"
	clusterNamespacePrefixLen = len(clusterNamespacePrefix)
	hashIDLength              = 8 // Length of hash-based cluster ID

	// customOSSystem is the MAAS osystem for legacy non-prefixed custom images.
	customOSSystem = "custom"
)

// splitImage derives the MAAS osystem and distro_series from a boot-resource
// image name. A prefixed name ("<osystem>/<series>", e.g. "rhel/rocky-92-0-k-1285-0")
// deploys under that osystem so MAAS handles the OS correctly; a legacy
// non-prefixed name keeps osystem "custom" (no behavior change for existing
// Ubuntu images).
func splitImage(image string) (osystem, distroSeries string) {
	if os, series, found := strings.Cut(image, "/"); found {
		return os, series
	}
	return customOSSystem, image
}

type Service struct {
	scope      *scope.MachineScope
	maasClient maasclient.ClientSetInterface
}

// DNS service returns a new helper for managing a MaaS "DNS" (DNS client loadbalancing)
func NewService(machineScope *scope.MachineScope) *Service {
	return &Service{
		scope:      machineScope,
		maasClient: scope.NewMaasClient(machineScope.ClusterScope),
	}
}

// logVMHostDiagnostics attempts to extract the VM host (pod) id from a MAAS error message
// and, if found, fetches its details and prints status information to the controller log.
func logVMHostDiagnostics(s *Service, err error) {
	// First, check for machine id in the error and force-release it if found
	if m := reMachineID.FindStringSubmatch(err.Error()); len(m) == 2 {
		sys := m[1]
		s.scope.Info("Releasing broken machine", "system-id", sys)
		ctx := context.TODO()
		_, _ = s.maasClient.Machines().Machine(sys).Releaser().WithForce().Release(ctx)
	}

	matches := reHostID.FindStringSubmatch(err.Error())
	if len(matches) != 2 {
		return // no host id in message
	}
	podID := matches[1]
	s.scope.Info("Broken VM host detected", "pod-id", podID)
	ctx := context.TODO()
	if vmHost, e := s.maasClient.VMHosts().VMHost(podID).Get(ctx); e == nil {
		s.scope.Info("VM host status", "pod-id", podID, "name", vmHost.Name(), "status", vmHost.Type(), "availCores", vmHost.AvailableCores(), "availMem", vmHost.AvailableMemory())
	} else {
		s.scope.Error(e, "failed to fetch VM host details", "pod-id", podID)
	}
}

func (s *Service) GetMachine(systemID string) (*infrav1beta1.Machine, error) {

	if systemID == "" {
		return nil, nil
	}

	m, err := s.maasClient.Machines().Machine(systemID).Get(context.Background())
	if err != nil {
		// Treat MAAS 404 as not found (already deleted)
		msg := err.Error()
		if strings.Contains(msg, "status: 404") || strings.Contains(strings.ToLower(msg), "no machine matches") {
			return nil, nil
		}
		return nil, err
	}

	machine := fromSDKTypeToMachine(m)

	return machine, nil
}

func (s *Service) ReleaseMachine(systemID string) error {
	ctx := context.TODO()

	// WithErase is required for MAAS to wipe disks; WithQuickErase selects a fast
	// wipe (start/end of each disk) so leftover data (e.g. rook-ceph OSD signatures)
	// does not block reuse on the next cluster deploy.
	_, err := s.maasClient.Machines().
		Machine(systemID).
		Releaser().
		WithErase().
		WithQuickErase().
		Release(ctx)
	if err != nil {
		return errors.Wrapf(err, "Unable to release machine")
	}

	return nil
}

func (s *Service) DeployMachine(userDataB64 string) (_ *infrav1beta1.Machine, rerr error) {
	ctx := context.TODO()
	log := textlogger.NewLogger(textlogger.NewConfig())

	mm := s.scope.MaasMachine

	// Decide if we should create a VM via MAAS (LXD) based on user input or node-pool policy.
	// Machine-level enablement (preferred) or node-pool policy (fallback)
	if s.scope.GetDynamicLXD() {
		s.scope.Info("Using LXD VM creation path (unified)", "machine", mm.Name)
		return s.createVMViaMAAS(ctx, userDataB64)
	}

	// Standard MAAS machine allocation path
	s.scope.Info("Using standard MAAS machine allocation path", "machine", mm.Name)

	failureDomain := mm.Spec.FailureDomain
	if failureDomain == nil {
		if s.scope.Machine.Spec.FailureDomain != nil && *s.scope.Machine.Spec.FailureDomain != "" {
			failureDomain = s.scope.Machine.Spec.FailureDomain
		}
	}

	var m maasclient.Machine
	var err error

	if s.scope.GetProviderID() == "" {
		allocator := s.maasClient.
			Machines().
			Allocator().
			WithCPUCount(*mm.Spec.MinCPU).
			WithMemory(*mm.Spec.MinMemoryInMB)

		if failureDomain != nil {
			allocator.WithZone(*failureDomain)
		}

		if mm.Spec.ResourcePool != nil {
			allocator.WithResourcePool(*mm.Spec.ResourcePool)
		}

		// For HCP clusters, both control-plane and worker nodes can be LXD hosts
		if s.scope.ClusterScope.IsLXDHostEnabled() {
			allocator.WithNotPod(true)
			allocator.WithNotPodType("lxd")
			s.scope.Info("Allocating machine for LXD host under HCP", "machine", mm.Name, "isControlPlane", s.scope.IsControlPlane())
			// Allow both bare metal and LXD VM hosts for LXD-enabled clusters
		}

		if len(mm.Spec.Tags) > 0 {
			allocator.WithTags(mm.Spec.Tags)
		}

		s.scope.Info("Requesting MAAS allocation")
		m, err = allocator.Allocate(ctx)
		if err != nil {
			if strings.Contains(err.Error(), "Invalid transition: Broken") {
				logVMHostDiagnostics(s, err)
				s.scope.Info("Broken machine encountered; will retry")
				return nil, ErrBrokenMachine
			}
			return nil, errors.Wrapf(err, "Unable to allocate machine")
		}

		// Backstop: If MAAS still returned a VM host, reject it for HCP control-plane
		if s.scope.ClusterScope.IsLXDHostEnabled() {
			pt := strings.ToLower(m.PowerType())
			if pt == "lxd" || pt == "lxdvm" || pt == "virsh" {
				s.scope.Info("Rejecting VM host allocation for node(s) under HCP; releasing and retrying",
					"system-id", m.SystemID(), "powerType", pt, "zone", m.ZoneName(), "pool", m.ResourcePoolName())
				_, _ = m.Releaser().WithForce().Release(ctx)
				return nil, ErrBrokenMachine
			}
		}

		s.scope.SetProviderID(m.SystemID(), m.Zone().Name())
		err = s.scope.PatchObject()
		if err != nil {
			return nil, errors.Wrapf(err, "unable to pathc machine with provider id")
		}
	} else {
		m, err = s.maasClient.Machines().Machine(*s.scope.GetInstanceID()).Get(ctx)
		if err != nil {
			return nil, errors.Wrapf(err, "unable to find machine %s", *s.scope.GetInstanceID())
		}

		// Backstop for reuse path: if previous reconcile captured a VM host, reject for HCP CP
		if s.scope.ClusterScope.IsLXDHostEnabled() {
			pt := strings.ToLower(m.PowerType())
			if pt == "lxd" || pt == "lxdvm" || pt == "virsh" {
				s.scope.Info("Releasing previously selected VM host for node(s) under HCP; will re-allocate BM",
					"system-id", m.SystemID(), "powerType", pt, "zone", m.ZoneName(), "pool", m.ResourcePoolName())
				_, _ = m.Releaser().WithForce().Release(ctx)
				// Clear IDs so next reconcile re-allocates
				s.scope.MaasMachine.Spec.ProviderID = nil
				s.scope.MaasMachine.Spec.SystemID = nil
				_ = s.scope.PatchObject()
				return nil, ErrBrokenMachine
			}
		}
	}

	s.scope.Info("Allocated machine", "system-id", m.SystemID())

	// Create boot interface bridge if needed
	if s.scope.ClusterScope.IsLXDHostEnabled() {
		if err := s.createBootInterfaceBridge(ctx, m.SystemID()); err != nil {
			s.scope.Error(err, "failed to create boot interface bridge", "system-id", m.SystemID())
			// Continue despite bridge creation failure as it's not critical for basic functionality
		}
	}

	defer func() {
		if rerr != nil {
			s.scope.Info("Attempting to release machine which failed to deploy")
			_, err := m.Releaser().Release(ctx)
			if err != nil {
				// Is it right to NOT set rerr so we can see the original issue?
				log.Error(err, "Unable to release properly")
			}

			// Clear IDs so the next reconcile can allocate a different machine instead of
			// getting stuck trying to reuse a bad one (e.g., no network link/config).
			s.scope.MaasMachine.Spec.ProviderID = nil
			s.scope.MaasMachine.Spec.SystemID = nil
			_ = s.scope.PatchObject()
		}
	}()

	// TODO need to revisit if we need to set the hostname OR not
	//Hostname: &mm.Name,
	noSwap := 0
	if _, err := m.Modifier().SetSwapSize(noSwap).Update(ctx); err != nil {
		return nil, errors.Wrapf(err, "Unable to disable swap")
	}

	s.scope.Info("Swap disabled", "system-id", m.SystemID())

	// Configure static IP before deployment (control-plane only)
	if s.scope.IsControlPlane() {
		if staticIP := s.scope.GetStaticIP(); staticIP != "" {
			staticIPConfig := s.scope.GetStaticIPConfig()
			if staticIPConfig != nil {
				err := s.setMachineStaticIP(m.SystemID(), staticIPConfig)
				if err != nil {
					return nil, errors.Wrapf(err, "failed to configure static IP")
				}
			}
		}
	}

	s.scope.Info("Starting deployment", "system-id", m.SystemID())
	osystem, distroSeries := splitImage(mm.Spec.Image)
	deployingM, err := m.Deployer().
		SetUserData(userDataB64).
		SetOSSystem(osystem).
		SetDistroSeries(distroSeries).Deploy(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "Unable to deploy machine")
	}

	return fromSDKTypeToMachine(deployingM), nil
}

// createVMViaMAAS performs a unified VM creation flow using the MAAS API.
// It consolidates previous createLXDVM* variants. VM placement is derived from
// MaasMachine spec first, then (if applicable) workload node-pool mappings.
func (s *Service) createVMViaMAAS(ctx context.Context, userDataB64 string) (*infrav1beta1.Machine, error) {

	mm := s.scope.MaasMachine

	// If a VM was already composed earlier (providerID/system-id present), reuse it and only deploy
	if id := s.scope.GetInstanceID(); id != nil && *id != "" {
		m, err := s.maasClient.Machines().Machine(*id).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get existing VM by system-id")
		}
		// Verify and fix network interfaces (eth0/eth1 subnets) before deploy so that even if MAAS
		// selected the wrong subnet for eth0 after commissioning/acquire, we correct it before deploy.
		if err := s.VerifyVMNetworkInterfaces(ctx, m.SystemID()); err != nil {
			return nil, errors.Wrap(err, "failed to verify VM network interfaces before deploy")
		}
		// Best-effort: set hostname and static IP before deploy
		machineName := s.scope.Machine.Name
		vmName := fmt.Sprintf("vm-%s", machineName)
		_, _ = m.Modifier().SetHostname(vmName).Update(ctx)

		if s.scope.IsControlPlane() {
			if staticIP := s.scope.GetStaticIP(); staticIP != "" {
				if err := s.setMachineStaticIP(m.SystemID(), &infrav1beta1.StaticIPConfig{IP: staticIP}); err != nil {
					// Fail fast so we don't attempt Deploy without a network link configured
					return nil, errors.Wrap(err, "failed to configure static IP before deploy")
				}
			}
		}
		osystem, distroSeries := splitImage(mm.Spec.Image)
		deployingM, err := m.Deployer().
			SetUserData(userDataB64).
			SetOSSystem(osystem).
			SetDistroSeries(distroSeries).Deploy(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to deploy existing VM")
		}
		// Determine fallback zone
		fallbackZone := ""
		if deployingM.Zone() != nil {
			fallbackZone = deployingM.Zone().Name()
		}
		if fallbackZone == "" {
			if mm.Spec.FailureDomain != nil && *mm.Spec.FailureDomain != "" {
				fallbackZone = *mm.Spec.FailureDomain
			} else if s.scope.Machine.Spec.FailureDomain != nil && *s.scope.Machine.Spec.FailureDomain != "" {
				fallbackZone = *s.scope.Machine.Spec.FailureDomain
			}
		}
		s.scope.SetSystemID(deployingM.SystemID())
		s.scope.SetProviderID(deployingM.SystemID(), fallbackZone)
		if fallbackZone != "" {
			s.scope.SetFailureDomain(fallbackZone)
		}
		_ = s.scope.PatchObject()

		// Tag CP VMs for anti-affinity placement and maintenance operations
		if s.scope.IsControlPlane() {
			s.tagCPVM(ctx, deployingM.SystemID())
			// If there's an active maintenance session, update the VEC ConfigMap with
			// this replacement VM's systemID so VEC can tag it with ready-op and complete.
			s.tagVMIfMaintenanceActive(ctx, deployingM.SystemID())
		}

		res := fromSDKTypeToMachine(deployingM)
		if res.AvailabilityZone == "" {
			res.AvailabilityZone = fallbackZone
		}
		return res, nil
	}

	// No composed VM yet; wait for PrepareLXDVM/commissioning to complete
	if _, err := s.PrepareLXDVM(ctx); err != nil {
		return nil, errors.Wrap(err, "compose failed prior to deploy")
	}
	conditions.MarkFalse(s.scope.MaasMachine, infrav1beta1.MachineDeployedCondition, infrav1beta1.MachineDeployingReason, clusterv1.ConditionSeverityInfo, "VM composed; commissioning")
	_ = s.scope.PatchObject()
	return nil, ErrVMComposing
}

// PrepareLXDVM composes an LXD VM and sets providerID; it does not deploy/boot the VM.
func (s *Service) PrepareLXDVM(ctx context.Context) (*infrav1beta1.Machine, error) {

	mm := s.scope.MaasMachine

	// If already composed (system-id or providerID present), reuse
	if mm.Spec.SystemID != nil && *mm.Spec.SystemID != "" {
		m, err := s.maasClient.Machines().Machine(*mm.Spec.SystemID).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get existing VM by system-id (pre-bootstrap)")
		}
		return fromSDKTypeToMachine(m), nil
	}

	if id := s.scope.GetInstanceID(); id != nil && *id != "" {
		m, err := s.maasClient.Machines().Machine(*id).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "failed to get existing VM by system-id (pre-bootstrap)")
		}
		return fromSDKTypeToMachine(m), nil
	}

	// Determine placement inputs using only machine-level fields
	var zone string
	if mm.Spec.FailureDomain != nil && *mm.Spec.FailureDomain != "" {
		zone = *mm.Spec.FailureDomain
	} else if s.scope.Machine.Spec.FailureDomain != nil && *s.scope.Machine.Spec.FailureDomain != "" {
		zone = *s.scope.Machine.Spec.FailureDomain
	}

	var resourcePool string
	if mm.Spec.ResourcePool != nil && *mm.Spec.ResourcePool != "" {
		resourcePool = *mm.Spec.ResourcePool
	}

	// VM name and minimal resources
	vmName := mm.Annotations["maas.spectrocloud.com/vm-name"]
	if vmName == "" {
		uid := string(s.scope.Machine.UID)
		short := uid
		if len(uid) >= 5 {
			short = uid[:5]
		}
		vmName = fmt.Sprintf("vm-%s-%s", s.scope.Machine.Name, short)
		if mm.Annotations == nil {
			mm.Annotations = map[string]string{}
		}
		mm.Annotations["maas.spectrocloud.com/vm-name"] = vmName
		_ = s.scope.PatchObject()
	}

	var cpu, mem, diskSizeGB int
	if mm.Spec.MinCPU != nil && *mm.Spec.MinCPU > 0 {
		cpu = *mm.Spec.MinCPU
	}
	if mm.Spec.MinMemoryInMB != nil && *mm.Spec.MinMemoryInMB > 0 {
		mem = *mm.Spec.MinMemoryInMB
	}

	if mm.Spec.MinDiskSizeInGB != nil && *mm.Spec.MinDiskSizeInGB > 0 {
		diskSizeGB = *mm.Spec.MinDiskSizeInGB
	}

	// Enforce minimum 60GB storage
	if mm.Spec.LXD != nil && mm.Spec.LXD.VMConfig != nil && mm.Spec.LXD.VMConfig.DiskSize != nil && *mm.Spec.LXD.VMConfig.DiskSize > diskSizeGB {
		diskSizeGB = *mm.Spec.LXD.VMConfig.DiskSize
	}

	// Select an LXD VM host based on zone, resource pool, tags, resources, and distribution
	hosts, err := s.maasClient.VMHosts().List(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list LXD VM hosts")
	}

	// Build SelectOptions from MaasMachine spec
	selectOpts := lxd.SelectOptions{
		Zone:          zone,
		ResourcePool:  resourcePool,
		Tags:          mm.Spec.Tags,
		MinCores:      cpu,
		MinMemory:     mem,
		MinDiskSizeGB: diskSizeGB,
	}

	if s.scope.IsControlPlane() {
		selectOpts.ClusterID = s.deriveClusterID()
	}

	selectedHost, err := lxd.SelectLXDHostWithMaasClient(s.maasClient, hosts, selectOpts)
	if err != nil {
		return nil, errors.Wrap(err, "failed to select LXD VM host")
	}

	s.scope.Info("Selected LXD host for VM", "host-name", selectedHost.Name(), "host-id", selectedHost.SystemID(),
		"zone", zone, "resource-pool", resourcePool, "tags", mm.Spec.Tags,
		"isControlPlane", s.scope.IsControlPlane())

	zoneID := selectedHost.Zone().ID()
	rp := selectedHost.ResourcePool()
	if rp == nil {
		return nil, errors.New("selected LXD host has no resource pool; shouldn't use default resource pool")
	}
	poolID := rp.ID()

	params := maasclient.ParamsBuilder().
		Set("hostname", vmName).
		Set("cores", fmt.Sprintf("%d", cpu)).
		Set("memory", fmt.Sprintf("%d", mem)).
		Set("storage", fmt.Sprintf("%d", diskSizeGB)).
		Set("zone", fmt.Sprintf("%d", zoneID)).
		Set("pool", fmt.Sprintf("%d", poolID))

	// If spec.LXD.VMConfig.Network is present, check if its values are separated by ","
	// The values are subnet names that will be passed to compose LXD VMs
	// Maximum number of subnets is 2
	if mm.Spec.LXD != nil && mm.Spec.LXD.VMConfig != nil && mm.Spec.LXD.VMConfig.Network != "" {
		networkStr := strings.TrimSpace(mm.Spec.LXD.VMConfig.Network)
		// Split by comma to get individual subnet names
		subnets := strings.Split(networkStr, ",")
		// Only set interfaces when there are exactly 2 subnets
		if len(subnets) == 2 {
			subnet0 := strings.TrimSpace(subnets[0])
			subnet1 := strings.TrimSpace(subnets[1])
			if subnet0 == "" || subnet1 == "" {
				s.scope.Info("Skipping setting network interfaces due to empty subnet name(s)", "subnet0", subnet0, "subnet1", subnet1)
			} else {
				// Build interfaces parameter: eth0 = first subnet (typically PXE), eth1 = second subnet (typically management).
				// eth0 is sent without ip= so it uses DHCP; eth1 gets static IP when configured. Using DHCP on eth0
				// can cause DHCP-provided routes (e.g. to MAAS controller) to be installed on eth0; if the controller
				// is on the eth1 network, ensure PXE subnet DHCP does not push those routes, or use static IP for eth0.
				// Format: "eth0:subnet=<subnet-name>;eth1:subnet=<subnet-name>" or
				//         "eth0:subnet=<subnet-name>;eth1:subnet=<subnet-name>,ip=<static-ip>" if static IP is configured
				interfacesParam := fmt.Sprintf("eth0:subnet=%s;eth1:subnet=%s", subnet0, subnet1)

				// Check if static IP is configured for control-plane - include it in compose parameter
				staticIP := ""
				if s.scope.IsControlPlane() && s.scope.GetStaticIP() != "" {
					staticIP = s.scope.GetStaticIP()
					// Include static IP in the compose parameter: eth1:subnet=<subnet>,ip=<ip>
					interfacesParam = fmt.Sprintf("eth0:subnet=%s;eth1:subnet=%s,ip=%s", subnet0, subnet1, staticIP)
				}

				params.Set("interfaces", interfacesParam)
				if staticIP != "" {
					s.scope.Info("Setting network interfaces for VM composition with static IP", "interfaces", interfacesParam, "static-ip", staticIP)
				} else {
					s.scope.Info("Setting network interfaces for VM composition", "interfaces", interfacesParam)
				}
			}
		} else {
			s.scope.Info("Network configuration ignored: expected exactly 2 subnets, got", "count", len(subnets), "network", networkStr)
		}
	}

	// Create the VM on the selected host
	m, err := selectedHost.Composer().Compose(ctx, params)
	if err != nil {
		errStr := err.Error()

		// Check for network mismatch error - the selected host doesn't have access to requested networks
		if strings.Contains(errStr, "does not match the specified networks") || strings.Contains(errStr, "pod does not match") {
			requestedNetworks := ""
			if mm.Spec.LXD != nil && mm.Spec.LXD.VMConfig != nil && mm.Spec.LXD.VMConfig.Network != "" {
				requestedNetworks = mm.Spec.LXD.VMConfig.Network
			}
			return nil, fmt.Errorf("selected LXD host %q (ID: %s) does not have access to the requested networks %q. "+
				"This host may be in a different zone/fabric or the networks may not be configured on this host. "+
				"Zone: %s, Resource Pool: %s. Error: %w",
				selectedHost.Name(), selectedHost.SystemID(), requestedNetworks, zone, resourcePool, err)
		}

		// Check for instance/hostname already exists errors - try to reuse existing VM
		isHostnameExists := strings.Contains(strings.ToLower(errStr), "hostname") && strings.Contains(strings.ToLower(errStr), "already exists")
		isInstanceExists := strings.Contains(errStr, "Instance") && strings.Contains(errStr, "already exists")

		if isHostnameExists || isInstanceExists {
			if existingVM, findErr := s.findExistingVMByHostname(ctx, vmName, selectedHost, zone); findErr == nil && existingVM != nil {
				return existingVM, nil
			}

			// If LXD instance exists but not found in MAAS, provide helpful error
			if isInstanceExists {
				return nil, fmt.Errorf("LXD instance %q already exists on host %q but is not registered in MAAS. "+
					"This may indicate a stale LXD instance. Manual cleanup may be required. Error: %w", vmName, selectedHost.Name(), err)
			}

			return nil, errors.Wrap(err, "failed to compose VM on LXD host")
		}

		return nil, errors.Wrap(err, "failed to compose VM on LXD host")
	}

	// Set IDs early so system-id/providerID are recorded
	if m.SystemID() != "" {
		s.scope.SetSystemID(m.SystemID())
		s.scope.SetProviderID(m.SystemID(), zone)
		if zone != "" {
			s.scope.SetFailureDomain(zone)
		}
		_ = s.scope.PatchObject()

		// Tag CP VMs for anti-affinity placement and maintenance operations
		if s.scope.IsControlPlane() {
			s.tagCPVM(ctx, m.SystemID())
			// If there's an active maintenance session, update the VEC ConfigMap with
			// this replacement VM's systemID so VEC can tag it with ready-op and complete.
			s.tagVMIfMaintenanceActive(ctx, m.SystemID())
		}
	}
	s.scope.Info("Composed VM (pre-bootstrap)", "system-id", m.SystemID())

	res := fromSDKTypeToMachine(m)
	if res.AvailabilityZone == "" {
		res.AvailabilityZone = zone
	}
	return res, nil
}

// getSubnetCIDR safely extracts CIDR from a subnet, handling nil pointer panics
func (s *Service) getSubnetCIDR(subnet maasclient.Subnet) string {
	if subnet == nil {
		return ""
	}
	// Use recover to handle typed nil interfaces that pass != nil check
	defer func() {
		if r := recover(); r != nil {
			s.scope.V(1).Info("Panic while getting subnet CIDR", "panic", r)
		}
	}()
	return subnet.CIDR()
}

// resolveLinkMode returns the MAAS link mode for an interface: from VMConfig.InterfaceLinkModes, else default (eth0=auto, others=dhcp).
func (s *Service) resolveLinkMode(ifaceName string) string {
	defaultMode := maasclient.ModeDHCP
	if ifaceName == "eth0" {
		defaultMode = maasclient.ModeAuto
	}
	mm := s.scope.MaasMachine
	if mm.Spec.LXD == nil || mm.Spec.LXD.VMConfig == nil || len(mm.Spec.LXD.VMConfig.InterfaceLinkModes) == 0 {
		return defaultMode
	}
	m := strings.TrimSpace(mm.Spec.LXD.VMConfig.InterfaceLinkModes[ifaceName])
	allowed := map[string]bool{maasclient.ModeAuto: true, maasclient.ModeDHCP: true, maasclient.ModeStatic: true, maasclient.ModeLinkUp: true}
	if m != "" && allowed[m] {
		return m
	}
	return defaultMode
}

// VerifyVMNetworkInterfaces verifies and fixes LXD VM network interfaces to have expected subnets after commissioning.
func (s *Service) VerifyVMNetworkInterfaces(ctx context.Context, systemID string) error {
	mm := s.scope.MaasMachine
	if mm.Spec.LXD == nil || mm.Spec.LXD.VMConfig == nil || mm.Spec.LXD.VMConfig.Network == "" {
		return nil
	}

	subnets := strings.Split(strings.TrimSpace(mm.Spec.LXD.VMConfig.Network), ",")
	if len(subnets) != 2 {
		s.scope.Info("VMConfig.Network should contain exactly 2 comma-separated subnets", "network", mm.Spec.LXD.VMConfig.Network, "count", len(subnets))
		return nil
	}

	expected0, expected1 := strings.TrimSpace(subnets[0]), strings.TrimSpace(subnets[1])
	if expected0 == "" || expected1 == "" {
		return nil
	}

	machine, err := s.maasClient.Machines().Machine(systemID).Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get machine state: %w", err)
	}

	machineState := machine.State()
	allowedStates := map[string]bool{"New": true, "Ready": true, "Allocated": true, "Broken": true}
	if !allowedStates[machineState] {
		s.scope.Info("Skipping VM network interface verification - machine not in allowed state", "system-id", systemID, "state", machineState, "allowed-states", []string{"New", "Ready", "Allocated", "Broken"})
		return nil
	}

	s.scope.Info("Verifying VM network interfaces", "system-id", systemID, "state", machineState, "expected-subnets", fmt.Sprintf("%s,%s", expected0, expected1), "eth0-mode", s.resolveLinkMode("eth0"), "eth1-mode", s.resolveLinkMode("eth1"))

	// Fetch network interfaces - refetch if we encounter issues with subnet data
	interfaces, err := s.maasClient.NetworkInterfaces().Get(ctx, systemID)
	if err != nil {
		return fmt.Errorf("failed to get network interfaces: %w", err)
	}

	var eth0Iface, eth1Iface maasclient.NetworkInterface
	var eth0Subnet, eth1Subnet string
	needsRefetch := false

	// First pass: try to extract subnet information
	for _, iface := range interfaces {
		name := iface.Name()
		if name == "eth0" {
			eth0Iface = iface
			for _, link := range iface.Links() {
				subnet := link.Subnet()
				if subnet != nil {
					if cidr := s.getSubnetCIDR(subnet); cidr != "" {
						eth0Subnet = cidr
						break
					} else {
						needsRefetch = true
					}
				}
			}
		} else if name == "eth1" {
			eth1Iface = iface
			for _, link := range iface.Links() {
				subnet := link.Subnet()
				if subnet != nil {
					if cidr := s.getSubnetCIDR(subnet); cidr != "" {
						eth1Subnet = cidr
						break
					} else {
						needsRefetch = true
					}
				}
			}
		}
	}

	// If we couldn't get subnet info and suspect stale data, refetch interfaces once
	if needsRefetch && (eth0Subnet == "" || eth1Subnet == "") {
		s.scope.Info("Refetching network interfaces due to incomplete subnet data", "system-id", systemID)
		interfaces, err = s.maasClient.NetworkInterfaces().Get(ctx, systemID)
		if err != nil {
			return fmt.Errorf("failed to refetch network interfaces: %w", err)
		}

		// Reset and retry extraction
		eth0Subnet, eth1Subnet = "", ""
		for _, iface := range interfaces {
			name := iface.Name()
			if name == "eth0" && eth0Subnet == "" {
				eth0Iface = iface
				for _, link := range iface.Links() {
					subnet := link.Subnet()
					if subnet != nil {
						if cidr := s.getSubnetCIDR(subnet); cidr != "" {
							eth0Subnet = cidr
							break
						}
					}
				}
			} else if name == "eth1" && eth1Subnet == "" {
				eth1Iface = iface
				for _, link := range iface.Links() {
					subnet := link.Subnet()
					if subnet != nil {
						if cidr := s.getSubnetCIDR(subnet); cidr != "" {
							eth1Subnet = cidr
							break
						}
					}
				}
			}
		}
	}

	// subnetsMatch returns true if actual and expected represent the same subnet (CIDR or name).
	subnetsMatch := func(actual, expected string) bool {
		if actual == "" || expected == "" {
			return false
		}
		if strings.EqualFold(actual, expected) {
			return true
		}
		// If both parse as CIDR, compare by network equality (robust to formatting)
		_, actualNet, err1 := net.ParseCIDR(actual)
		_, expectedNet, err2 := net.ParseCIDR(expected)
		if err1 == nil && err2 == nil && actualNet != nil && expectedNet != nil {
			return actualNet.IP.Equal(expectedNet.IP) && actualNet.Mask != nil && expectedNet.Mask != nil &&
				bytes.Equal(actualNet.Mask, expectedNet.Mask)
		}
		return false
	}

	var aggErr error
	if eth0Iface != nil {
		if eth0Subnet == "" || !subnetsMatch(eth0Subnet, expected0) {
			s.scope.Info("Fixing eth0 subnet mismatch", "system-id", systemID, "expected", expected0, "actual", eth0Subnet)
			if err := s.fixInterfaceSubnet(ctx, systemID, eth0Iface, expected0, "eth0", s.resolveLinkMode("eth0")); err != nil {
				s.scope.Error(err, "Failed to fix eth0 subnet", "system-id", systemID, "expected", expected0, "actual", eth0Subnet)
				aggErr = errors.Wrap(err, "eth0 subnet correction failed")
			} else {
				s.scope.Info("Successfully fixed eth0 subnet", "system-id", systemID, "subnet", expected0)
			}
		} else {
			s.scope.V(1).Info("eth0 subnet is correct", "system-id", systemID, "subnet", eth0Subnet)
		}
	} else {
		s.scope.Info("eth0 interface not found, skipping verification", "system-id", systemID)
	}

	if eth1Iface != nil {
		if eth1Subnet == "" || !subnetsMatch(eth1Subnet, expected1) {
			s.scope.Info("Fixing eth1 subnet mismatch", "system-id", systemID, "expected", expected1, "actual", eth1Subnet)
			if err := s.fixInterfaceSubnet(ctx, systemID, eth1Iface, expected1, "eth1", s.resolveLinkMode("eth1")); err != nil {
				s.scope.Error(err, "Failed to fix eth1 subnet", "system-id", systemID, "expected", expected1, "actual", eth1Subnet)
				if aggErr != nil {
					aggErr = errors.Wrap(aggErr, err.Error())
				} else {
					aggErr = errors.Wrap(err, "eth1 subnet correction failed")
				}
			} else {
				s.scope.Info("Successfully fixed eth1 subnet", "system-id", systemID, "subnet", expected1)
			}
		} else {
			s.scope.V(1).Info("eth1 subnet is correct", "system-id", systemID, "subnet", eth1Subnet)
		}
	} else {
		s.scope.Info("eth1 interface not found, skipping verification", "system-id", systemID)
	}

	if aggErr == nil {
		s.scope.Info("VM network interfaces verified successfully", "system-id", systemID)
	}

	return aggErr
}

func (s *Service) fixInterfaceSubnet(ctx context.Context, systemID string, iface maasclient.NetworkInterface, expectedSubnetIdentifier, ifaceName string, linkMode string) error {
	interfaceID := iface.ID()
	ifaceClient := s.maasClient.NetworkInterfaces().Interface(systemID, interfaceID)

	// Unlink all existing subnets from this interface
	for _, link := range iface.Links() {
		if link.Subnet() != nil {
			if err := ifaceClient.UnlinkSubnet(ctx, link.ID()); err != nil {
				return fmt.Errorf("failed to unlink subnet from %s: %w", ifaceName, err)
			}
		}
	}

	// Link the subnet with the given mode (from VMConfig.Eth0LinkMode/Eth1LinkMode or defaults).
	// When linkMode is empty we use LinkSubnet (client default: auto when no IP).
	if linkMode != "" {
		if err := ifaceClient.LinkSubnetWithMode(ctx, expectedSubnetIdentifier, linkMode, ""); err != nil {
			return fmt.Errorf("failed to link subnet %s to %s (mode=%s): %w", expectedSubnetIdentifier, ifaceName, linkMode, err)
		}
	} else {
		if err := ifaceClient.LinkSubnet(ctx, expectedSubnetIdentifier, ""); err != nil {
			return fmt.Errorf("failed to link subnet %s to %s: %w", expectedSubnetIdentifier, ifaceName, err)
		}
	}

	s.scope.Info("Fixed subnet on interface", "system-id", systemID, "interface", ifaceName, "subnet", expectedSubnetIdentifier, "link-mode", linkMode)
	return nil
}

// findExistingVMByHostname searches for an existing VM in MAAS by hostname
// Returns the machine if found, nil otherwise
func (s *Service) findExistingVMByHostname(ctx context.Context, vmName string, selectedHost maasclient.VMHost, zone string) (*infrav1beta1.Machine, error) {
	// First try global machines list
	if all, err := s.maasClient.Machines().List(ctx, nil); err == nil {
		for _, cand := range all {
			cid := cand.SystemID()
			if cid == "" {
				continue
			}
			cDet, err := s.maasClient.Machines().Machine(cid).Get(ctx)
			if err == nil && strings.EqualFold(cDet.Hostname(), vmName) {
				return s.reuseExistingVM(cDet, zone)
			}
		}
	}

	// Then try host-local list
	if list, err := selectedHost.Machines().List(ctx); err == nil {
		for _, ex := range list {
			exID := ex.SystemID()
			if exID == "" {
				continue
			}
			exDet, err := s.maasClient.Machines().Machine(exID).Get(ctx)
			if err == nil && strings.EqualFold(exDet.Hostname(), vmName) {
				return s.reuseExistingVM(exDet, zone)
			}
		}
	}

	return nil, nil
}

// reuseExistingVM sets the system ID and provider ID for an existing VM and returns it
func (s *Service) reuseExistingVM(m maasclient.Machine, zone string) (*infrav1beta1.Machine, error) {
	s.scope.SetSystemID(m.SystemID())
	s.scope.SetProviderID(m.SystemID(), zone)
	if zone != "" {
		s.scope.SetFailureDomain(zone)
	}
	_ = s.scope.PatchObject()
	s.scope.Info("Reusing existing VM by hostname", "system-id", m.SystemID())
	res := fromSDKTypeToMachine(m)
	if res.AvailabilityZone == "" {
		res.AvailabilityZone = zone
	}
	return res, nil
}

// setMachineStaticIP configures static IP for a machine using the simplified networkInterfaceImpl branch API
// It first checks if the IP is already allocated elsewhere and releases it if necessary
func (s *Service) setMachineStaticIP(systemID string, config *infrav1beta1.StaticIPConfig) error {
	ctx := context.TODO()

	// Check machine state - MAAS only allows network interface changes in specific states
	machine, err := s.maasClient.Machines().Machine(systemID).Get(ctx)
	if err != nil {
		return fmt.Errorf("failed to get machine state: %w", err)
	}
	machineState := machine.State()

	// MAAS allows unlinking subnet interfaces only when machine is in: New, Ready, Allocated, or Broken
	allowedStates := map[string]bool{
		"New":       true,
		"Ready":     true,
		"Allocated": true,
		"Broken":    true,
	}

	if !allowedStates[machineState] {
		s.scope.Info("Machine is not in a state that allows network configuration changes", "state", machineState, "systemID", systemID)

		// Special handling for Commissioning state: skip static IP configuration to avoid blocking commissioning
		if machineState == "Commissioning" {
			s.scope.Info("Machine is commissioning, skipping static IP configuration to avoid interfering with commissioning process. Will configure after commissioning completes", "systemID", systemID)
			// Return error to requeue - static IP will be configured after commissioning completes
			return fmt.Errorf("machine is commissioning, static IP configuration will be retried after commissioning completes")
		}

		// For other non-allowed states, check if static IP is already correctly configured
		// If already configured, we can skip the update
		interfaces, err := s.maasClient.NetworkInterfaces().Get(ctx, systemID)
		if err == nil {
			staticIP := net.ParseIP(config.IP)
			if staticIP != nil {
				for _, iface := range interfaces {
					links := iface.Links()
					for _, link := range links {
						if link.Mode() == "static" && link.IPAddress() != nil {
							existingIP := link.IPAddress().String()
							if existingIP == config.IP {
								if link.Subnet() != nil {
									subnetCIDR := link.Subnet().CIDR()
									if subnetCIDR != "" {
										_, subnetIPNet, err := net.ParseCIDR(subnetCIDR)
										if err == nil && subnetIPNet.Contains(staticIP) {
											s.scope.Info("Static IP already correctly configured, skipping update due to machine state", "ip", config.IP, "state", machineState, "systemID", systemID)
											return nil
										}
									}
								}
							}
						}
					}
				}
			}
		}
		// If not already configured, return error to requeue and retry when machine reaches allowed state
		return fmt.Errorf("machine is in state %s which does not allow network interface changes (allowed states: New, Ready, Allocated, Broken). Will retry when machine reaches an allowed state", machineState)
	}

	// Check if the IP is already allocated elsewhere
	s.scope.V(1).Info("Checking existing IP allocation", "ip", config.IP)
	existingIP, err := s.maasClient.IPAddresses().Get(ctx, config.IP)
	if err == nil {
		// IP exists - check if it's actually allocated to any interfaces
		interfaces := existingIP.InterfaceSet()
		if len(interfaces) > 0 {
			s.scope.Info("Found existing IP allocation with interfaces, skipping release", "ip", config.IP, "interfaceCount", len(interfaces))
		} else {
			s.scope.Info("Found IP with no interfaces, releasing to clean up stale state", "ip", config.IP)

			// Try normal release only - no force release to avoid risky operations
			if releaseErr := s.maasClient.IPAddresses().Release(ctx, config.IP); releaseErr != nil {
				return fmt.Errorf("failed to release existing IP allocation %s: %w (manual intervention may be required)", config.IP, releaseErr)
			}
			s.scope.Info("Successfully released IP", "ip", config.IP)
		}
	} else {
		// IP doesn't exist or GetAll failed - this is fine, we can proceed with assignment
		s.scope.V(1).Info("IP not found in existing allocations (expected for new assignments)", "ip", config.IP)
	}

	// Parse the static IP address
	staticIP := net.ParseIP(config.IP)
	if staticIP == nil {
		return fmt.Errorf("invalid IP address: %s", config.IP)
	}

	// Find the correct interface with matching subnet CIDR, or fallback to boot interface
	var targetInterfaceID string
	var useBootInterface bool

	// Always fetch all interfaces to find the one with matching subnet
	interfaces, err := s.maasClient.NetworkInterfaces().Get(ctx, systemID)
	if err != nil {
		s.scope.Error(err, "Failed to get interfaces, falling back to boot interface", "systemID", systemID)
		useBootInterface = true
	} else {
		// Find interface with subnet that contains the static IP
		found := false
		for _, iface := range interfaces {
			links := iface.Links()
			for _, link := range links {
				if link.Subnet() != nil {
					subnetCIDR := link.Subnet().CIDR()
					if subnetCIDR != "" {
						_, subnetIPNet, err := net.ParseCIDR(subnetCIDR)
						if err == nil {
							// Check if static IP is within this subnet
							if subnetIPNet.Contains(staticIP) {
								targetInterfaceID = iface.ID()
								found = true
								s.scope.Info("Found interface with matching subnet for static IP", "interface-id", targetInterfaceID, "interface-name", iface.Name(), "subnet-cidr", subnetCIDR, "static-ip", config.IP)
								break
							}
						}
					}
				}
			}
			if found {
				break
			}
		}

		if !found {
			// If CIDR was provided, also check for exact CIDR match
			if config.CIDR != "" {
				// Validate CIDR format
				if _, _, err := net.ParseCIDR(config.CIDR); err == nil {
					for _, iface := range interfaces {
						links := iface.Links()
						for _, link := range links {
							if link.Subnet() != nil {
								subnetCIDR := link.Subnet().CIDR()
								if subnetCIDR == config.CIDR {
									targetInterfaceID = iface.ID()
									found = true
									s.scope.Info("Found interface with exact CIDR match", "interface-id", targetInterfaceID, "interface-name", iface.Name(), "subnet-cidr", subnetCIDR, "target-cidr", config.CIDR)
									break
								}
							}
						}
						if found {
							break
						}
					}
				}
			}

			if !found {
				s.scope.Info("No interface found with matching subnet for static IP, falling back to boot interface", "static-ip", config.IP, "target-cidr", config.CIDR)
				useBootInterface = true
			}
		}
	}

	// Set static IP on the selected interface
	if useBootInterface {
		s.scope.Info("Setting static IP on boot interface", "ip", config.IP, "systemID", systemID)
		err = s.maasClient.NetworkInterfaces().SetBootInterfaceStaticIP(ctx, systemID, config.IP)
		if err != nil {
			return fmt.Errorf("failed to set static IP %s on boot interface for machine %s: %w", config.IP, systemID, err)
		}
		s.scope.Info("Static IP configured successfully on boot interface", "ip", config.IP, "systemID", systemID)
	} else {
		// Get the target interface and check if static IP is already correctly configured
		targetInterface := s.maasClient.NetworkInterfaces().Interface(systemID, targetInterfaceID)
		targetInterface, err = targetInterface.Get(ctx)
		if err != nil {
			return fmt.Errorf("failed to get interface %s for machine %s: %w", targetInterfaceID, systemID, err)
		}

		// Check if the static IP is already correctly configured on this interface
		alreadyConfigured := false
		links := targetInterface.Links()
		for _, link := range links {
			if link.Mode() == "static" && link.IPAddress() != nil {
				existingIP := link.IPAddress().String()
				if existingIP == config.IP {
					// Check if the subnet matches
					if link.Subnet() != nil {
						subnetCIDR := link.Subnet().CIDR()
						if subnetCIDR != "" {
							_, subnetIPNet, err := net.ParseCIDR(subnetCIDR)
							if err == nil && subnetIPNet.Contains(staticIP) {
								alreadyConfigured = true
								s.scope.Info("Static IP already correctly configured on interface", "ip", config.IP, "interface-id", targetInterfaceID, "interface-name", targetInterface.Name(), "systemID", systemID)
								break
							}
						}
					}
				}
			}
		}

		if !alreadyConfigured {
			s.scope.Info("Setting static IP on interface", "ip", config.IP, "systemID", systemID, "interface-id", targetInterfaceID)
			err = targetInterface.SetStaticIP(ctx, config.IP)
			if err != nil {
				return fmt.Errorf("failed to set static IP %s on interface %s for machine %s: %w", config.IP, targetInterfaceID, systemID, err)
			}
			s.scope.Info("Static IP configured successfully on interface", "ip", config.IP, "systemID", systemID, "interface-id", targetInterfaceID)
		}
	}

	return nil
}

// createBootInterfaceBridge creates a bridge on the boot interface using maas-client-go
// First checks if the boot interface type is "physical" before attempting to create a bridge
func (s *Service) createBootInterfaceBridge(ctx context.Context, systemID string) error {
	s.scope.Info("Checking boot interface type", "systemID", systemID)

	// First, check if the boot interface is physical using GetBootInterfaceType
	machine, err := s.maasClient.Machines().Machine(systemID).Get(ctx)
	if err != nil {
		s.scope.Error(err, "Failed to get machine details")
	}
	interfaceType := machine.GetBootInterfaceType()
	s.scope.Info("Boot interface type", "systemID", systemID, "interfaceType", interfaceType)

	// Only create bridge if the boot interface is physical
	if interfaceType != "physical" {
		s.scope.Info("Boot interface is not physical, skipping bridge creation",
			"systemID", systemID, "interfaceType", interfaceType)
		return nil
	}

	s.scope.Info("Creating bridge for physical boot interface", "systemID", systemID, "interfaceType", interfaceType)

	// Now create the bridge since we know it's physical
	_, err = s.maasClient.NetworkInterfaces().CreateBootInterfaceBridge(ctx, systemID, "br0")
	if err != nil {
		// Handle expected errors gracefully (e.g., bridge already exists)
		if strings.Contains(err.Error(), "already bridged") ||
			strings.Contains(err.Error(), "already exists") {
			s.scope.V(1).Info("Boot interface bridge creation skipped", "systemID", systemID, "reason", err.Error())
			return nil
		}
		return fmt.Errorf("failed to create boot interface bridge for machine %s: %w", systemID, err)
	}

	s.scope.Info("Boot interface bridge created successfully", "systemID", systemID, "bridgeName", "br0")
	return nil
}

// hasNilValue checks if an interface value holds a nil concrete value (typed nil).
// In Go, an interface is only nil if both its type and value are nil.
// This function returns true if the interface has a type but the underlying value is nil.
func hasNilValue(i interface{}) bool {
	if i == nil {
		return false
	}
	v := reflect.ValueOf(i)
	return v.Kind() == reflect.Ptr && v.IsNil()
}

func fromSDKTypeToMachine(m maasclient.Machine) *infrav1beta1.Machine {
	az := ""
	zone := m.Zone()
	if zone != nil && !hasNilValue(zone) {
		az = zone.Name()
	}
	ps := m.PowerState()
	machine := &infrav1beta1.Machine{
		ID:               m.SystemID(),
		Hostname:         m.Hostname(),
		State:            infrav1beta1.MachineState(m.State()),
		Powered:          ps == infrav1beta1.MachinePowerStateOn,
		PowerState:       ps,
		AvailabilityZone: az,
	}

	if m.FQDN() != "" {
		machine.Addresses = append(machine.Addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalDNS,
			Address: m.FQDN(),
		})
		machine.Addresses = append(machine.Addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalDNS,
			Address: m.FQDN(),
		})
	}

	for _, v := range m.IPAddresses() {
		machine.Addresses = append(machine.Addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalIP,
			Address: v.String(),
		})
		machine.Addresses = append(machine.Addresses, clusterv1.MachineAddress{
			Type:    clusterv1.MachineInternalIP,
			Address: v.String(),
		})
	}

	return machine
}

func (s *Service) PowerOnMachine() error {
	_, err := s.maasClient.Machines().Machine(s.scope.GetSystemID()).PowerManagerOn().WithPowerOnComment("maas provider power on").PowerOn(context.Background())
	return err
}

// tagCPVM tags a control-plane VM with identity tags for anti-affinity placement and maintenance.
//
// Tags applied:
//   - TagVMControlPlane ("maas-lxd-wlc-cp"): identifies VM as a control-plane node
//   - TagVMClusterPrefix + clusterID: identifies which cluster the VM belongs to
func (s *Service) tagCPVM(ctx context.Context, systemID string) {
	if systemID == "" {
		return
	}

	clusterId := s.deriveClusterID()
	if clusterId == "" {
		s.scope.V(1).Info("Could not derive cluster ID for CP tagging", "systemID", systemID)
		return
	}

	clusterTag := maintenance.TagVMClusterPrefix + maintenance.SanitizeID(clusterId)

	s.tagVM(ctx, systemID, []string{maintenance.TagVMControlPlane, clusterTag})
	s.scope.Info("Tagged CP VM for anti-affinity and maintenance", "systemID", systemID, "clusterTag", clusterTag)
}

// tagVMIfMaintenanceActive checks for active maintenance ConfigMaps and tags the VM with cluster identity
func (s *Service) tagVMIfMaintenanceActive(ctx context.Context, systemID string) {
	if systemID == "" || s.scope == nil || s.scope.ClusterScope == nil {
		return
	}

	namespace := s.scope.Cluster.Namespace

	// List all ConfigMaps in the namespace looking for active maintenance sessions
	cmList := &corev1.ConfigMapList{}
	k8sClient := s.scope.ClusterScope.Client()
	if k8sClient == nil {
		return
	}

	if err := k8sClient.List(ctx, cmList, client.InNamespace(namespace)); err != nil {
		s.scope.V(1).Info("Failed to list ConfigMaps for maintenance check", "error", err)
		return
	}

	// Look for vec-maintenance-* ConfigMaps with Active status
	for _, cm := range cmList.Items {
		if !strings.HasPrefix(cm.Name, "vec-maintenance-") {
			continue
		}

		opID := cm.Data[maintenance.CmKeyOpID]
		status := cm.Data[maintenance.CmKeyStatus]

		if opID == "" || status != string(maintenance.StatusActive) {
			continue
		}

		s.scope.Info("Found active maintenance session, updating ConfigMap", "opID", opID, "systemID", systemID)

		// Reuse common tagging function (idempotent, safe if already tagged by tagCPVM)
		clusterId := s.deriveClusterID()
		if clusterId != "" {
			clusterTag := maintenance.TagVMClusterPrefix + maintenance.SanitizeID(clusterId)
			s.tagVM(ctx, systemID, []string{maintenance.TagVMControlPlane, clusterTag})
		}

		// Update ConfigMap with the new VM systemID (maintenance-specific logic)
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		cm.Data[maintenance.CmKeyNewVMSystemID] = systemID
		if err := k8sClient.Update(ctx, &cm); err != nil {
			s.scope.V(1).Info("Failed to update ConfigMap with new VM systemID", "error", err, "opID", opID)
		}

		// Only process the first active session
		break
	}
}

// deriveClusterID extracts cluster ID from cluster name or hashes namespace
func (s *Service) deriveClusterID() string {
	if s.scope == nil || s.scope.Cluster == nil {
		return ""
	}

	// Try to extract UID from "cluster-<uid>" format in namespace
	namespace := s.scope.Cluster.Namespace
	if strings.HasPrefix(namespace, clusterNamespacePrefix) && len(namespace) > clusterNamespacePrefixLen {
		uid := namespace[clusterNamespacePrefixLen:] // Extract after "cluster-"
		if uid != "" {
			return uid
		}
	}

	// Fallback: hash the namespace to get a short identifier
	hash := sha256.Sum256([]byte(namespace))
	hashStr := hex.EncodeToString(hash[:])
	// Take first hashIDLength characters of hash for brevity
	if len(hashStr) > hashIDLength {
		return hashStr[:hashIDLength]
	}
	return hashStr
}

// tagVM applies the specified tags to a VM in MAAS.
func (s *Service) tagVM(ctx context.Context, systemID string, tags []string) {
	if systemID == "" || len(tags) == 0 {
		return
	}

	tagsClient := s.maasClient.Tags()
	if tagsClient == nil {
		s.scope.V(1).Info("Tags client not available", "systemID", systemID)
		return
	}

	for _, tag := range tags {
		if tag == "" {
			continue
		}
		// Create is idempotent - ignores error if tag already exists
		_ = tagsClient.Create(ctx, tag)
		if err := tagsClient.Assign(ctx, tag, systemID); err != nil {
			s.scope.Error(err, "Failed to tag VM", "tag", tag, "systemID", systemID)
		} else {
			s.scope.V(1).Info("Tagged VM", "tag", tag, "systemID", systemID)
		}
	}
}
