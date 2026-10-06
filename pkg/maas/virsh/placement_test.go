package virsh

import (
	"reflect"
	"testing"
)

// TestFailureDomainBeatsClusterZone is the property that keeps a MachineDeployment spread
// across racks. If the cluster zone won instead, every replica would land in one zone and the
// cluster would silently lose the rack-level fault tolerance it was sized for.
func TestFailureDomainBeatsClusterZone(t *testing.T) {
	got := MergeSelectOptions(
		ClusterDefaults{Zone: "zone-a"},
		Placement{FailureDomain: "zone-b"},
	)
	if got.Zone != "zone-b" {
		t.Fatalf("Zone = %q, want the machine's failure domain %q", got.Zone, "zone-b")
	}
}

func TestClusterZoneUsedWhenMachineHasNoFailureDomain(t *testing.T) {
	got := MergeSelectOptions(ClusterDefaults{Zone: "zone-a"}, Placement{})
	if got.Zone != "zone-a" {
		t.Fatalf("Zone = %q, want the cluster default %q", got.Zone, "zone-a")
	}
}

func TestMachineResourcePoolOverridesCluster(t *testing.T) {
	got := MergeSelectOptions(
		ClusterDefaults{ResourcePool: "pool-a"},
		Placement{ResourcePool: "pool-b"},
	)
	if got.ResourcePool != "pool-b" {
		t.Fatalf("ResourcePool = %q, want %q", got.ResourcePool, "pool-b")
	}
}

// TestHostTagsAccumulate: cluster tags say which hypervisors may host cluster workloads at
// all. If a machine could REPLACE that set, one MachineDeployment could escape the fleet-wide
// restriction and place VMs on hosts reserved for another tenant.
func TestHostTagsAccumulate(t *testing.T) {
	got := MergeSelectOptions(
		ClusterDefaults{HostTags: []string{"k8s-worker"}},
		Placement{HostTags: []string{"gpu"}},
	)
	want := []string{"k8s-worker", "gpu"}
	if !reflect.DeepEqual(got.HostTags, want) {
		t.Fatalf("HostTags = %v, want %v (machine tags narrow, they do not replace)", got.HostTags, want)
	}
}

// TestMergeDoesNotAliasClusterDefaults: appending to a shared backing array would let one
// machine's tags leak into the next machine's selection.
func TestMergeDoesNotAliasClusterDefaults(t *testing.T) {
	defaults := ClusterDefaults{HostTags: []string{"k8s-worker"}}

	a := MergeSelectOptions(defaults, Placement{HostTags: []string{"gpu"}})
	b := MergeSelectOptions(defaults, Placement{HostTags: []string{"nvme"}})

	if len(defaults.HostTags) != 1 {
		t.Fatalf("cluster defaults were mutated: %v", defaults.HostTags)
	}
	if reflect.DeepEqual(a.HostTags, b.HostTags) {
		t.Fatalf("tag sets leaked between machines: %v vs %v", a.HostTags, b.HostTags)
	}
	if got := a.HostTags[len(a.HostTags)-1]; got != "gpu" {
		t.Fatalf("first machine's tags corrupted: %v", a.HostTags)
	}
}

func TestShapeBecomesHostMinimum(t *testing.T) {
	got := MergeSelectOptions(ClusterDefaults{}, Placement{Cores: 8, MemoryMB: 16384})
	if got.MinCores != 8 || got.MinMemoryMB != 16384 {
		t.Fatalf("VM shape must become the host capacity floor, got cores=%d mem=%d", got.MinCores, got.MinMemoryMB)
	}
}
