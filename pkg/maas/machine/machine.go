package machine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/pkg/errors"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
	"k8s.io/klog/v2/textlogger"

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

	_, err := s.maasClient.Machines().
		Machine(systemID).
		Releaser().
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

		// For HCP clusters, control-plane must be bare metal: exclude pod-backed VM hosts
		s.scope.Info("Allocating bare metal machine for CP under HCP", "machine", mm.Name)
		if s.scope.IsControlPlane() && s.scope.ClusterScope.IsLXDHostEnabled() {
			allocator.WithNotPod(true)
			allocator.WithNotPodType("lxd")
			s.scope.Info("Allocating bare metal machine for CP under HCP", "machine", mm.Name)
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
		if s.scope.IsControlPlane() && s.scope.ClusterScope.IsLXDHostEnabled() {
			pt := strings.ToLower(m.PowerType())
			if pt == "lxd" || pt == "lxdvm" || pt == "virsh" {
				s.scope.Info("Rejecting VM host allocation for CP under HCP; releasing and retrying",
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
		if s.scope.IsControlPlane() && s.scope.ClusterScope.IsLXDHostEnabled() {
			pt := strings.ToLower(m.PowerType())
			if pt == "lxd" || pt == "lxdvm" || pt == "virsh" {
				s.scope.Info("Releasing previously selected VM host for CP under HCP; will re-allocate BM",
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
	deployingM, err := m.Deployer().
		SetUserData(userDataB64).
		SetOSSystem("custom").
		SetDistroSeries(mm.Spec.Image).Deploy(ctx)
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
		deployingM, err := m.Deployer().
			SetUserData(userDataB64).
			SetOSSystem("custom").
			SetDistroSeries(mm.Spec.Image).Deploy(ctx)
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

	// Select an LXD VM host based on zone and resource pool
	hosts, err := s.maasClient.VMHosts().List(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to list LXD VM hosts")
	}
	selectedHost, err := lxd.SelectLXDHostWithMaasClient(s.maasClient, hosts, zone, resourcePool)
	if err != nil {
		return nil, errors.Wrap(err, "failed to select LXD VM host")
	}

	s.scope.Info("Selected LXD host for VM", "host-name", selectedHost.Name(), "host-id", selectedHost.SystemID(), "zone", zone, "resource-pool", resourcePool)

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

	// Create the VM on the selected host
	m, err := selectedHost.Composer().Compose(ctx, params)
	if err != nil {
		// If hostname already exists, reuse that VM
		errStr := err.Error()
		if strings.Contains(strings.ToLower(errStr), "hostname") && strings.Contains(strings.ToLower(errStr), "already exists") {
			// First try global machines list
			if all, aerr := s.maasClient.Machines().List(ctx, nil); aerr == nil {
				for _, cand := range all {
					cid := cand.SystemID()
					if cid == "" {
						continue
					}
					cDet, cg := s.maasClient.Machines().Machine(cid).Get(ctx)
					if cg == nil && strings.EqualFold(cDet.Hostname(), vmName) {
						s.scope.SetSystemID(cDet.SystemID())
						s.scope.SetProviderID(cDet.SystemID(), zone)
						if zone != "" {
							s.scope.SetFailureDomain(zone)
						}
						_ = s.scope.PatchObject()
						s.scope.Info("Reusing existing VM by hostname (pre-bootstrap)", "system-id", cDet.SystemID())
						res := fromSDKTypeToMachine(cDet)
						if res.AvailabilityZone == "" {
							res.AvailabilityZone = zone
						}
						return res, nil
					}
				}
			}
			// Then try host-local list
			if list, lerr := selectedHost.Machines().List(ctx); lerr == nil {
				for _, ex := range list {
					exID := ex.SystemID()
					if exID == "" {
						continue
					}
					// fetch details to get hostname
					exDet, gerr := s.maasClient.Machines().Machine(exID).Get(ctx)
					if gerr != nil {
						continue
					}
					if strings.EqualFold(exDet.Hostname(), vmName) {
						s.scope.SetSystemID(exDet.SystemID())
						s.scope.SetProviderID(exDet.SystemID(), zone)
						if zone != "" {
							s.scope.SetFailureDomain(zone)
						}
						_ = s.scope.PatchObject()
						s.scope.Info("Reusing existing VM by hostname (pre-bootstrap)", "system-id", exDet.SystemID())
						res := fromSDKTypeToMachine(exDet)
						if res.AvailabilityZone == "" {
							res.AvailabilityZone = zone
						}
						return res, nil
					}
				}
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
	}
	s.scope.Info("Composed VM (pre-bootstrap)", "system-id", m.SystemID())

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

	// Now set the static IP on the target machine's boot interface
	s.scope.Info("Setting static IP on boot interface", "ip", config.IP, "systemID", systemID)
	err = s.maasClient.NetworkInterfaces().SetBootInterfaceStaticIP(ctx, systemID, config.IP)
	if err != nil {
		return fmt.Errorf("failed to set static IP %s on boot interface for machine %s: %w", config.IP, systemID, err)
	}

	s.scope.Info("Static IP configured successfully", "ip", config.IP, "systemID", systemID)
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

func fromSDKTypeToMachine(m maasclient.Machine) *infrav1beta1.Machine {
	machine := &infrav1beta1.Machine{
		ID:               m.SystemID(),
		Hostname:         m.Hostname(),
		State:            infrav1beta1.MachineState(m.State()),
		Powered:          m.PowerState() == "on",
		AvailabilityZone: m.Zone().Name(),
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

//// ReconcileDNS reconciles the load balancers for the given cluster.
//func (s *Service) ReconcileDNS() error {
//	s.scope.V(2).Info("Reconciling DNS")
//
//	s.scope.SetDNSName("cluster1.maas")
//	return nil
//}
//
