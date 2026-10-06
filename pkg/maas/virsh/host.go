// Package virsh composes worker VMs on MAAS-registered libvirt/KVM ("virsh") VM hosts.
//
// Unlike pkg/maas/lxd, this package does not register hosts: they must already be registered
// in MAAS, and are only discovered, filtered and composed on here.
package virsh

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spectrocloud/maas-client-go/maasclient"
)

// HostType is the MAAS VM host type this package composes on.
const HostType = "virsh"

// SelectOptions constrains which virsh host may run a VM. Every field is a filter, not a
// preference: a host failing any of them is ineligible.
type SelectOptions struct {
	// Zone restricts to a MAAS zone (a rack, and therefore the failure domain). Empty = any.
	Zone string
	// ResourcePool restricts to a MAAS resource pool. Empty = any.
	ResourcePool string
	// HostTags requires ALL of these tags on the host. Empty = any. Tags rather than a host
	// name prefix: renaming a host must not change whether it is eligible.
	HostTags []string
	// MinCores is the minimum number of cores that must still be AVAILABLE on the host.
	MinCores int
	// MinMemoryMB is the minimum memory in MB that must still be AVAILABLE on the host.
	MinMemoryMB int
}

// ErrNoEligibleHost is returned when no registered virsh host satisfies the constraints.
// Distinguishable so callers can requeue instead of failing the Machine: the usual cause is
// every hypervisor being momentarily full, not a bad spec.
type ErrNoEligibleHost struct {
	Considered int
	Reason     string
}

func (e *ErrNoEligibleHost) Error() string {
	return fmt.Sprintf("no eligible virsh VM host among %d considered: %s", e.Considered, e.Reason)
}

// IsVirshHost reports whether a MAAS VM host is a libvirt/KVM host.
func IsVirshHost(h maasclient.VMHost) bool {
	return strings.EqualFold(strings.TrimSpace(h.Type()), HostType)
}

func hostHasAllTags(h maasclient.VMHost, required []string) bool {
	if len(required) == 0 {
		return true
	}
	have := make(map[string]struct{}, len(h.Tags()))
	for _, t := range h.Tags() {
		have[strings.ToLower(strings.TrimSpace(t))] = struct{}{}
	}
	for _, want := range required {
		if _, ok := have[strings.ToLower(strings.TrimSpace(want))]; !ok {
			return false
		}
	}
	return true
}

// headroom scores a host by the fraction of capacity left after placing the VM, taking the
// scarcer of cores and memory so a host that is roomy on one and nearly full on the other
// does not outrank a balanced one.
func headroom(h maasclient.VMHost, cores, memoryMB int) float64 {
	availCores, availMem := h.AvailableCores(), h.AvailableMemory()
	if availCores <= 0 || availMem <= 0 {
		return 0
	}
	coreFrac := float64(availCores-cores) / float64(availCores)
	memFrac := float64(availMem-memoryMB) / float64(availMem)
	if coreFrac < memFrac {
		return coreFrac
	}
	return memFrac
}

// SelectHost picks the virsh host with the most headroom that satisfies every constraint.
// Ties break on name: equal hosts must not make consecutive reconciles oscillate.
func SelectHost(hosts []maasclient.VMHost, opts SelectOptions) (maasclient.VMHost, error) {
	considered := len(hosts)
	if considered == 0 {
		return nil, &ErrNoEligibleHost{Considered: 0, Reason: "MAAS returned no VM hosts at all"}
	}

	var (
		eligible   []maasclient.VMHost
		sawVirsh   bool
		lastWhyNot string
	)

	for _, h := range hosts {
		if !IsVirshHost(h) {
			lastWhyNot = fmt.Sprintf("host %q is type %q, not %q", h.Name(), h.Type(), HostType)
			continue
		}
		sawVirsh = true

		if opts.Zone != "" && !strings.EqualFold(zoneName(h), opts.Zone) {
			lastWhyNot = fmt.Sprintf("host %q is in zone %q, wanted %q", h.Name(), zoneName(h), opts.Zone)
			continue
		}
		if opts.ResourcePool != "" && !strings.EqualFold(poolName(h), opts.ResourcePool) {
			lastWhyNot = fmt.Sprintf("host %q is in pool %q, wanted %q", h.Name(), poolName(h), opts.ResourcePool)
			continue
		}
		if !hostHasAllTags(h, opts.HostTags) {
			lastWhyNot = fmt.Sprintf("host %q lacks required tags %v (has %v)", h.Name(), opts.HostTags, h.Tags())
			continue
		}
		if opts.MinCores > 0 && h.AvailableCores() < opts.MinCores {
			lastWhyNot = fmt.Sprintf("host %q has %d cores available, need %d", h.Name(), h.AvailableCores(), opts.MinCores)
			continue
		}
		if opts.MinMemoryMB > 0 && h.AvailableMemory() < opts.MinMemoryMB {
			lastWhyNot = fmt.Sprintf("host %q has %d MB available, need %d", h.Name(), h.AvailableMemory(), opts.MinMemoryMB)
			continue
		}
		eligible = append(eligible, h)
	}

	if len(eligible) == 0 {
		reason := lastWhyNot
		if !sawVirsh {
			// A setup problem, not a capacity problem: waiting will not fix it.
			reason = fmt.Sprintf("no VM host of type %q is registered in MAAS (%s)", HostType, lastWhyNot)
		}
		return nil, &ErrNoEligibleHost{Considered: considered, Reason: reason}
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		hi := headroom(eligible[i], opts.MinCores, opts.MinMemoryMB)
		hj := headroom(eligible[j], opts.MinCores, opts.MinMemoryMB)
		if hi != hj {
			return hi > hj
		}
		return eligible[i].Name() < eligible[j].Name()
	})

	return eligible[0], nil
}

func zoneName(h maasclient.VMHost) string {
	if z := h.Zone(); z != nil {
		return z.Name()
	}
	return ""
}

func poolName(h maasclient.VMHost) string {
	if p := h.ResourcePool(); p != nil {
		return p.Name()
	}
	return ""
}

// Placement carries the machine-level placement inputs that override cluster defaults.
type Placement struct {
	// FailureDomain is the CAPI failure domain for this machine, if set.
	FailureDomain string
	// ResourcePool overrides the cluster resource pool, if set.
	ResourcePool string
	// HostTags are layered on top of the cluster-level tags, never replacing them.
	HostTags []string
	// Cores and MemoryMB are the VM's shape, and double as the host's minimum free capacity.
	Cores    int
	MemoryMB int
}

// ClusterDefaults carries the cluster-level virsh placement defaults.
type ClusterDefaults struct {
	Zone         string
	ResourcePool string
	HostTags     []string
	StoragePool  string
}

// MergeSelectOptions resolves cluster defaults against machine-level placement.
//
// FailureDomain wins over the cluster zone: a MachineDeployment spreads replicas across racks
// by setting it per Machine, so a cluster-wide zone must not collapse them onto one rack.
// Host tags are the exception to override semantics and accumulate, so a machine can narrow
// the cluster's host restriction but never escape it.
func MergeSelectOptions(defaults ClusterDefaults, p Placement) SelectOptions {
	opts := SelectOptions{
		Zone:         defaults.Zone,
		ResourcePool: defaults.ResourcePool,
		MinCores:     p.Cores,
		MinMemoryMB:  p.MemoryMB,
	}
	if p.FailureDomain != "" {
		opts.Zone = p.FailureDomain
	}
	if p.ResourcePool != "" {
		opts.ResourcePool = p.ResourcePool
	}
	opts.HostTags = append(append([]string{}, defaults.HostTags...), p.HostTags...)
	return opts
}
