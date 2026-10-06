package machine

import (
	"context"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/virsh"
	"github.com/spectrocloud/maas-client-go/maasclient"
	"sigs.k8s.io/cluster-api/util/conditions"
)

// vmNameAnnotation records the composed VM's MAAS hostname, so a reconcile interrupted between
// compose and the status write can adopt the orphan instead of composing a second VM.
// See virsh.ErrHostnameTaken.
const vmNameAnnotation = "maas.spectrocloud.com/vm-name"

// virshVMName returns a stable MAAS hostname for this machine's VM, persisting it on first
// use. It is the only handle on a VM that was composed but not yet recorded, so it is derived
// from the Machine name and UID, both fixed for the life of the Machine.
func (s *Service) virshVMName() string {
	mm := s.scope.MaasMachine
	if n := mm.Annotations[vmNameAnnotation]; n != "" {
		return n
	}

	uid := string(s.scope.Machine.UID)
	short := uid
	if len(uid) > 5 {
		short = uid[:5]
	}
	name := fmt.Sprintf("vm-%s-%s", s.scope.Machine.Name, short)
	// Lowercase so adopt-by-hostname cannot miss on case alone.
	name = strings.ToLower(name)

	if mm.Annotations == nil {
		mm.Annotations = map[string]string{}
	}
	mm.Annotations[vmNameAnnotation] = name
	return name
}

// virshSelectOptions builds host constraints from the cluster and machine specs.
//
// The precedence rules themselves live in virsh.MergeSelectOptions, where they are unit
// tested; this function only adapts the CRD shapes to that call.
func (s *Service) virshSelectOptions() virsh.SelectOptions {
	mm := s.scope.MaasMachine

	defaults := virsh.ClusterDefaults{}
	if cfg := s.scope.ClusterScope.GetVirshConfig(); cfg != nil {
		defaults.Zone = cfg.Zone
		defaults.ResourcePool = cfg.ResourcePool
		defaults.HostTags = cfg.HostTags
		defaults.StoragePool = cfg.StoragePool
	}

	p := virsh.Placement{HostTags: s.scope.VirshHostTags()}

	if mm.Spec.FailureDomain != nil && *mm.Spec.FailureDomain != "" {
		p.FailureDomain = *mm.Spec.FailureDomain
	} else if s.scope.Machine.Spec.FailureDomain != "" {
		p.FailureDomain = s.scope.Machine.Spec.FailureDomain
	}
	if mm.Spec.ResourcePool != nil && *mm.Spec.ResourcePool != "" {
		p.ResourcePool = *mm.Spec.ResourcePool
	}
	if mm.Spec.MinCPU != nil {
		p.Cores = *mm.Spec.MinCPU
	}
	if mm.Spec.MinMemoryInMB != nil {
		p.MemoryMB = *mm.Spec.MinMemoryInMB
	}

	return virsh.MergeSelectOptions(defaults, p)
}

// PrepareVirshVM composes a VM on a virsh host and records its system ID; it does not deploy.
// Adopts an existing VM when one is found, so repeated calls are safe.
func (s *Service) PrepareVirshVM(ctx context.Context) (*infrav1beta1.Machine, error) {
	mm := s.scope.MaasMachine

	if mm.Spec.SystemID != nil && *mm.Spec.SystemID != "" {
		m, err := s.maasClient.Machines().Machine(*mm.Spec.SystemID).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "virsh: failed to get composed VM by system-id")
		}
		return fromSDKTypeToMachine(m), nil
	}
	if id := s.scope.GetInstanceID(); id != nil && *id != "" {
		m, err := s.maasClient.Machines().Machine(*id).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "virsh: failed to get composed VM by provider id")
		}
		return fromSDKTypeToMachine(m), nil
	}

	vmName := s.virshVMName()
	// Persist the name BEFORE composing: it is the only way back to an orphaned VM.
	_ = s.scope.PatchObject()
	if existing, err := virsh.FindByHostname(ctx, s.maasClient, vmName); err != nil {
		return nil, err
	} else if existing != nil {
		s.scope.Info("Adopting a previously composed virsh VM", "hostname", vmName, "system-id", existing.SystemID())
		return s.recordComposedVM(existing)
	}

	hosts, err := s.maasClient.VMHosts().List(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "virsh: unable to list MAAS VM hosts")
	}

	opts := s.virshSelectOptions()
	host, err := virsh.SelectHost(hosts, opts)
	if err != nil {
		// A condition rather than a hard failure: usually every hypervisor is momentarily
		// full, which resolves without intervention.
		conditions.Set(mm, metav1.Condition{
			Type:    infrav1beta1.MachineDeployedCondition,
			Status:  metav1.ConditionFalse,
			Reason:  infrav1beta1.MachineDeployingReason,
			Message: fmt.Sprintf("waiting for a virsh VM host: %v", err),
		})
		return nil, err
	}

	diskGB := 0
	if mm.Spec.MinDiskSizeInGB != nil {
		diskGB = *mm.Spec.MinDiskSizeInGB
	}
	storagePool := s.scope.VirshStoragePool()
	if storagePool == "" {
		if cfg := s.scope.ClusterScope.GetVirshConfig(); cfg != nil {
			storagePool = cfg.StoragePool
		}
	}

	req := virsh.ComposeRequest{
		Hostname:    vmName,
		Cores:       opts.MinCores,
		MemoryMB:    opts.MinMemoryMB,
		DiskGB:      diskGB,
		StoragePool: storagePool,
	}

	s.scope.Info("Composing virsh VM",
		"host", host.Name(), "host-id", host.SystemID(), "hostname", vmName,
		"cores", req.Cores, "memoryMB", req.MemoryMB, "diskGB", req.DiskGB)

	composed, err := virsh.Compose(ctx, host, req)
	if err != nil {
		// A racing reconcile composed it first. Adopt: the VM is real, and failing strands it.
		var taken *virsh.ErrHostnameTaken
		if errors.As(err, &taken) {
			if existing, ferr := virsh.FindByHostname(ctx, s.maasClient, vmName); ferr == nil && existing != nil {
				s.scope.Info("Compose raced; adopting the existing VM", "hostname", vmName)
				return s.recordComposedVM(existing)
			}
		}
		return nil, err
	}

	return s.recordComposedVM(composed)
}

// recordComposedVM persists the composed VM's identity onto the MaasMachine.
func (s *Service) recordComposedVM(m maasclient.Machine) (*infrav1beta1.Machine, error) {
	zone := ""
	if z := m.Zone(); z != nil {
		zone = z.Name()
	}
	s.scope.SetSystemID(m.SystemID())
	s.scope.SetProviderID(m.SystemID(), zone)
	if zone != "" {
		s.scope.SetFailureDomain(zone)
	}
	if err := s.scope.PatchObject(); err != nil {
		// The VM exists but is unrecorded; the vm-name annotation is what lets the next
		// reconcile adopt it.
		return nil, errors.Wrapf(err, "virsh: composed VM %s but failed to record it", m.SystemID())
	}
	return fromSDKTypeToMachine(m), nil
}

// createVirshVM composes on the first pass and deploys once the VM exists.
func (s *Service) createVirshVM(ctx context.Context, userDataB64 string) (*infrav1beta1.Machine, error) {
	mm := s.scope.MaasMachine

	if id := s.scope.GetInstanceID(); id != nil && *id != "" {
		m, err := s.maasClient.Machines().Machine(*id).Get(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "virsh: failed to get composed VM for deploy")
		}

		osystem, distroSeries := splitImage(mm.Spec.Image)
		deploying, err := m.Deployer().
			SetUserData(userDataB64).
			SetOSSystem(osystem).
			SetDistroSeries(distroSeries).Deploy(ctx)
		if err != nil {
			return nil, errors.Wrap(err, "virsh: failed to deploy composed VM")
		}

		zone := ""
		if deploying.Zone() != nil {
			zone = deploying.Zone().Name()
		}
		s.scope.SetSystemID(deploying.SystemID())
		s.scope.SetProviderID(deploying.SystemID(), zone)
		if zone != "" {
			s.scope.SetFailureDomain(zone)
		}
		_ = s.scope.PatchObject()

		res := fromSDKTypeToMachine(deploying)
		if res.AvailabilityZone == "" {
			res.AvailabilityZone = zone
		}
		return res, nil
	}

	if _, err := s.PrepareVirshVM(ctx); err != nil {
		return nil, errors.Wrap(err, "virsh: compose failed prior to deploy")
	}
	conditions.Set(mm, metav1.Condition{
		Type:    infrav1beta1.MachineDeployedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  infrav1beta1.MachineDeployingReason,
		Message: "virsh VM composed; commissioning",
	})
	_ = s.scope.PatchObject()
	return nil, ErrVMComposing
}
