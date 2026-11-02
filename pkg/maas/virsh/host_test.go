package virsh

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spectrocloud/maas-client-go/maasclient"
)

// maasclient.VMHost is a wide interface and the generated gomock doubles are wired for the LXD
// path, so a hand-rolled fake keeps every field that matters visible in the literal.

type fakeZone struct{ name string }

func (z *fakeZone) ID() int                                          { return 0 }
func (z *fakeZone) Name() string                                     { return z.name }
func (z *fakeZone) Description() string                              { return "" }
func (z *fakeZone) Get(ctx context.Context) (maasclient.Zone, error) { return z, nil }
func (z *fakeZone) Update(ctx context.Context, p maasclient.Params) (maasclient.Zone, error) {
	return z, nil
}
func (z *fakeZone) Delete(ctx context.Context) error { return nil }

type fakePool struct{ name string }

func (p *fakePool) ID() int                                                  { return 0 }
func (p *fakePool) Name() string                                             { return p.name }
func (p *fakePool) Description() string                                      { return "" }
func (p *fakePool) Get(ctx context.Context) (maasclient.ResourcePool, error) { return p, nil }
func (p *fakePool) Update(ctx context.Context, pr maasclient.Params) (maasclient.ResourcePool, error) {
	return p, nil
}
func (p *fakePool) Delete(ctx context.Context) error { return nil }

type fakeHost struct {
	name       string
	hostType   string
	zone       string
	pool       string
	tags       []string
	availCores int
	availMem   int
	storage    []maasclient.StoragePool
}

func (h *fakeHost) SystemID() string     { return "sys-" + h.name }
func (h *fakeHost) Name() string         { return h.name }
func (h *fakeHost) Type() string         { return h.hostType }
func (h *fakeHost) PowerAddress() string { return "" }
func (h *fakeHost) HostSystemID() string { return "" }
func (h *fakeHost) Zone() maasclient.Zone {
	if h.zone == "" {
		return nil
	}
	return &fakeZone{name: h.zone}
}
func (h *fakeHost) ResourcePool() maasclient.ResourcePool {
	if h.pool == "" {
		return nil
	}
	return &fakePool{name: h.pool}
}
func (h *fakeHost) TotalCores() int                                    { return h.availCores }
func (h *fakeHost) TotalMemory() int                                   { return h.availMem }
func (h *fakeHost) UsedCores() int                                     { return 0 }
func (h *fakeHost) UsedMemory() int                                    { return 0 }
func (h *fakeHost) AvailableCores() int                                { return h.availCores }
func (h *fakeHost) AvailableMemory() int                               { return h.availMem }
func (h *fakeHost) Capabilities() []string                             { return nil }
func (h *fakeHost) Projects() []string                                 { return nil }
func (h *fakeHost) StoragePools() []maasclient.StoragePool             { return h.storage }
func (h *fakeHost) Tags() []string                                     { return h.tags }
func (h *fakeHost) Composer() maasclient.VMComposer                    { return nil }
func (h *fakeHost) Machines() maasclient.VMHostMachines                { return nil }
func (h *fakeHost) Get(ctx context.Context) (maasclient.VMHost, error) { return h, nil }
func (h *fakeHost) Update(ctx context.Context, p maasclient.Params) (maasclient.VMHost, error) {
	return h, nil
}
func (h *fakeHost) Delete(ctx context.Context) error { return nil }

func virshHost(name, zone, pool string, cores, mem int, tags ...string) *fakeHost {
	return &fakeHost{name: name, hostType: "virsh", zone: zone, pool: pool, tags: tags, availCores: cores, availMem: mem}
}

// --- tests ----------------------------------------------------------------------------------

// Selection must key on the MAAS host type, not a name: a virsh host named "lxd-host-3" is
// still a virsh host, and an LXD one named "kvm1" is still LXD.
func TestSelectHostIgnoresLXDHosts(t *testing.T) {
	hosts := []maasclient.VMHost{
		&fakeHost{name: "lxd-host-1", hostType: "lxd", zone: "zone-a", availCores: 64, availMem: 262144},
		virshHost("kvm1", "zone-a", "pool-a", 32, 131072),
	}
	got, err := SelectHost(hosts, SelectOptions{})
	if err != nil {
		t.Fatalf("SelectHost() error = %v", err)
	}
	if got.Name() != "kvm1" {
		t.Fatalf("selected %q, want the virsh host kvm1", got.Name())
	}
}

func TestSelectHostNoVirshHostsIsDistinguishable(t *testing.T) {
	hosts := []maasclient.VMHost{
		&fakeHost{name: "lxd-host-1", hostType: "lxd", availCores: 64, availMem: 262144},
	}
	_, err := SelectHost(hosts, SelectOptions{})
	var noHost *NoEligibleHostError
	if !errors.As(err, &noHost) {
		t.Fatalf("expected *NoEligibleHostError, got %T: %v", err, err)
	}
	// An operator setup problem must not read like a transient capacity problem.
	if want := `no VM host of type "virsh" is registered`; !strings.Contains(noHost.Reason, want) {
		t.Fatalf("reason %q should explain that no virsh host is registered", noHost.Reason)
	}
}

func TestSelectHostFilters(t *testing.T) {
	all := []maasclient.VMHost{
		virshHost("kvm1", "zone-a", "pool-a", 32, 131072, "k8s-worker"),
		virshHost("kvm2", "zone-b", "pool-a", 64, 262144, "k8s-worker"),
		virshHost("kvm3", "zone-a", "pool-b", 64, 262144, "k8s-worker"),
		virshHost("kvm4", "zone-a", "pool-a", 64, 262144),
	}

	for _, tc := range []struct {
		name string
		opts SelectOptions
		want string
	}{
		{"zone", SelectOptions{Zone: "zone-b"}, "kvm2"},
		{"resource pool", SelectOptions{Zone: "zone-a", ResourcePool: "pool-b"}, "kvm3"},
		{"host tags", SelectOptions{Zone: "zone-a", ResourcePool: "pool-a", HostTags: []string{"k8s-worker"}}, "kvm1"},
		{"tags are case-insensitive", SelectOptions{Zone: "zone-a", ResourcePool: "pool-a", HostTags: []string{"K8S-Worker"}}, "kvm1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectHost(all, tc.opts)
			if err != nil {
				t.Fatalf("SelectHost() error = %v", err)
			}
			if got.Name() != tc.want {
				t.Fatalf("selected %q, want %q", got.Name(), tc.want)
			}
		})
	}
}

func TestSelectHostRejectsInsufficientCapacity(t *testing.T) {
	hosts := []maasclient.VMHost{virshHost("kvm1", "zone-a", "pool-a", 4, 8192)}

	if _, err := SelectHost(hosts, SelectOptions{MinCores: 8}); err == nil {
		t.Fatal("expected a capacity rejection on cores")
	}
	if _, err := SelectHost(hosts, SelectOptions{MinMemoryMB: 16384}); err == nil {
		t.Fatal("expected a capacity rejection on memory")
	}
	if _, err := SelectHost(hosts, SelectOptions{MinCores: 4, MinMemoryMB: 8192}); err != nil {
		t.Fatalf("exactly-fitting request should be accepted, got %v", err)
	}
}

// TestSelectHostSpreadsAcrossHosts pins the placement property that makes a burst pool usable:
// repeated composes must land on the roomiest host, not pile onto the first match.
func TestSelectHostSpreadsAcrossHosts(t *testing.T) {
	hosts := []maasclient.VMHost{
		virshHost("kvm1", "zone-a", "pool-a", 8, 16384),
		virshHost("kvm2", "zone-a", "pool-a", 64, 262144),
		virshHost("kvm3", "zone-a", "pool-a", 16, 65536),
	}
	got, err := SelectHost(hosts, SelectOptions{MinCores: 4, MinMemoryMB: 8192})
	if err != nil {
		t.Fatalf("SelectHost() error = %v", err)
	}
	if got.Name() != "kvm2" {
		t.Fatalf("selected %q, want the roomiest host kvm2", got.Name())
	}
}

// TestHeadroomUsesTheScarcerDimension: a host with plenty of cores but almost no memory must
// not outrank a balanced one. Scoring on cores alone would pick the host that fails next.
func TestHeadroomUsesTheScarcerDimension(t *testing.T) {
	lopsided := virshHost("lopsided", "zone-a", "p", 128, 9000)
	balanced := virshHost("balanced", "zone-a", "p", 32, 131072)

	got, err := SelectHost([]maasclient.VMHost{lopsided, balanced}, SelectOptions{MinCores: 4, MinMemoryMB: 8192})
	if err != nil {
		t.Fatalf("SelectHost() error = %v", err)
	}
	if got.Name() != "balanced" {
		t.Fatalf("selected %q; a host with 9000MB free must not win on core count alone", got.Name())
	}
}

func TestSelectHostIsDeterministic(t *testing.T) {
	hosts := []maasclient.VMHost{
		virshHost("kvm-b", "zone-a", "p", 32, 131072),
		virshHost("kvm-a", "zone-a", "p", 32, 131072),
	}
	first, err := SelectHost(hosts, SelectOptions{})
	if err != nil {
		t.Fatalf("SelectHost() error = %v", err)
	}
	for i := 0; i < 20; i++ {
		again, err := SelectHost(hosts, SelectOptions{})
		if err != nil {
			t.Fatalf("SelectHost() error = %v", err)
		}
		if again.Name() != first.Name() {
			t.Fatalf("selection is not deterministic: got %q then %q", first.Name(), again.Name())
		}
	}
	if first.Name() != "kvm-a" {
		t.Fatalf("equal hosts should tie-break by name, got %q", first.Name())
	}
}

func TestIsVirshHost(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"virsh", true}, {"VIRSH", true}, {" virsh ", true},
		{"lxd", false}, {"lxdvm", false}, {"", false},
	} {
		if got := IsVirshHost(&fakeHost{hostType: tc.typ}); got != tc.want {
			t.Fatalf("IsVirshHost(%q) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func withStorage(h *fakeHost, pools ...maasclient.StoragePool) *fakeHost {
	h.storage = pools
	return h
}

const gb = int64(1000 * 1000 * 1000)

// A host the VM's disk cannot fit on must be skipped at selection; otherwise it stays the top
// pick on every retry and compose keeps failing while another host has room.
func TestSelectHostFiltersOnStorage(t *testing.T) {
	hosts := []maasclient.VMHost{
		withStorage(virshHost("kvm1", "zone-a", "p", 64, 262144),
			maasclient.StoragePool{Name: "default", Available: 500 * gb},
			maasclient.StoragePool{Name: "fast", Available: 10 * gb}),
		withStorage(virshHost("kvm2", "zone-a", "p", 32, 131072),
			maasclient.StoragePool{Name: "default", Available: 20 * gb},
			maasclient.StoragePool{Name: "fast", Available: 200 * gb}),
		withStorage(virshHost("kvm3", "zone-a", "p", 128, 524288),
			maasclient.StoragePool{Name: "default", Available: 30 * gb}),
	}

	for _, tc := range []struct {
		name string
		opts SelectOptions
		want string
	}{
		{"named pool with room", SelectOptions{StoragePool: "fast", MinDiskGB: 100}, "kvm2"},
		{"named pool must exist on the host", SelectOptions{StoragePool: "fast", MinCores: 4, MinMemoryMB: 8192}, "kvm1"},
		{"no pool named: any pool with room", SelectOptions{MinDiskGB: 100, MinCores: 4, MinMemoryMB: 8192}, "kvm1"},
		{"no disk requested: roomiest host", SelectOptions{MinCores: 4, MinMemoryMB: 8192}, "kvm3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SelectHost(hosts, tc.opts)
			if err != nil {
				t.Fatalf("SelectHost() error = %v", err)
			}
			if got.Name() != tc.want {
				t.Fatalf("selected %q, want %q", got.Name(), tc.want)
			}
		})
	}

	_, err := SelectHost(hosts, SelectOptions{StoragePool: "fast", MinDiskGB: 1000})
	var noHost *NoEligibleHostError
	if !errors.As(err, &noHost) {
		t.Fatalf("expected *NoEligibleHostError when no pool has room, got %v", err)
	}
	if _, err := SelectHost(hosts, SelectOptions{StoragePool: "missing"}); err == nil {
		t.Fatal("a storage pool no host has must not match")
	}
}

// Hosts that report no storage pools are not filtered on disk size: there is nothing to
// compare against, and MAAS still validates the compose request.
func TestSelectHostKeepsHostsWithoutStorageData(t *testing.T) {
	hosts := []maasclient.VMHost{virshHost("kvm1", "zone-a", "p", 64, 262144)}
	if _, err := SelectHost(hosts, SelectOptions{MinDiskGB: 100}); err != nil {
		t.Fatalf("SelectHost() error = %v", err)
	}
}
