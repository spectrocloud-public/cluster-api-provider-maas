package lxd

import (
	"context"
	"net"
	"testing"

	"github.com/golang/mock/gomock"
	mock_clientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

// ---- Fakes for maasclient interfaces ----

type fakeZone struct {
	name string
	id   int
}

func (z *fakeZone) Name() string        { return z.name }
func (z *fakeZone) ID() int             { return z.id }
func (z *fakeZone) Description() string { return "" }

type fakeResourcePool struct {
	name string
	id   int
}

func (p *fakeResourcePool) Name() string        { return p.name }
func (p *fakeResourcePool) ID() int             { return p.id }
func (p *fakeResourcePool) Description() string { return "" }

type fakeVMHost struct {
	name            string
	systemID        string
	hostSystemID    string
	zone            *fakeZone
	pool            *fakeResourcePool
	tags            []string
	availableCores  int
	availableMemory int
	vmHostMachines  maasclient.VMHostMachines
}

func (h *fakeVMHost) Get(context.Context) (maasclient.VMHost, error) { return h, nil }
func (h *fakeVMHost) Update(context.Context, maasclient.Params) (maasclient.VMHost, error) {
	return h, nil
}
func (h *fakeVMHost) Delete(context.Context) error    { return nil }
func (h *fakeVMHost) Composer() maasclient.VMComposer { return nil }
func (h *fakeVMHost) Machines() maasclient.VMHostMachines {
	if h.vmHostMachines != nil {
		return h.vmHostMachines
	}
	return &fakeVMHostMachines{}
}
func (h *fakeVMHost) SystemID() string     { return h.systemID }
func (h *fakeVMHost) Name() string         { return h.name }
func (h *fakeVMHost) Type() string         { return "lxd" }
func (h *fakeVMHost) PowerAddress() string { return "" }
func (h *fakeVMHost) HostSystemID() string { return h.hostSystemID }
func (h *fakeVMHost) Zone() maasclient.Zone {
	if h.zone == nil {
		return nil
	}
	return h.zone
}
func (h *fakeVMHost) ResourcePool() maasclient.ResourcePool {
	if h.pool == nil {
		return nil
	}
	return h.pool
}
func (h *fakeVMHost) TotalCores() int  { return 0 }
func (h *fakeVMHost) TotalMemory() int { return 0 }
func (h *fakeVMHost) UsedCores() int   { return 0 }
func (h *fakeVMHost) UsedMemory() int  { return 0 }
func (h *fakeVMHost) AvailableCores() int {
	if h.availableCores == 0 {
		return 16 // default
	}
	return h.availableCores
}
func (h *fakeVMHost) AvailableMemory() int {
	if h.availableMemory == 0 {
		return 32768 // default 32GB
	}
	return h.availableMemory
}
func (h *fakeVMHost) Capabilities() []string                 { return nil }
func (h *fakeVMHost) Projects() []string                     { return nil }
func (h *fakeVMHost) StoragePools() []maasclient.StoragePool { return nil }
func (h *fakeVMHost) Tags() []string                         { return h.tags }

type fakeVMHostMachines struct {
	machines []maasclient.Machine
}

func (m *fakeVMHostMachines) List(ctx context.Context) ([]maasclient.Machine, error) {
	return m.machines, nil
}

type fakeMachine struct {
	systemID   string
	powerState string
	state      string
	tags       []string
}

func (m *fakeMachine) Get(ctx context.Context) (maasclient.Machine, error) { return m, nil }
func (m *fakeMachine) Delete(context.Context) error                        { return nil }
func (m *fakeMachine) Releaser() maasclient.MachineReleaser                { return nil }
func (m *fakeMachine) Modifier() maasclient.MachineModifier                { return nil }
func (m *fakeMachine) Deployer() maasclient.MachineDeployer                { return nil }
func (m *fakeMachine) SystemID() string                                    { return m.systemID }
func (m *fakeMachine) FQDN() string                                        { return "" }
func (m *fakeMachine) Zone() maasclient.Zone                               { return nil }
func (m *fakeMachine) PowerState() string                                  { return m.powerState }
func (m *fakeMachine) PowerType() string                                   { return "" }
func (m *fakeMachine) Hostname() string                                    { return "" }
func (m *fakeMachine) IPAddresses() []net.IP                               { return nil }
func (m *fakeMachine) State() string                                       { return m.state }
func (m *fakeMachine) OSSystem() string                                    { return "" }
func (m *fakeMachine) DeployedInMemory() bool                              { return false }
func (m *fakeMachine) DistroSeries() string                                { return "" }
func (m *fakeMachine) SwapSize() int                                       { return 0 }
func (m *fakeMachine) PowerManagerOn() maasclient.PowerManagerOn           { return nil }
func (m *fakeMachine) BootInterfaceID() string                             { return "" }
func (m *fakeMachine) TotalStorageGB() float64                             { return 0 }
func (m *fakeMachine) GetBootInterfaceType() string                        { return "" }
func (m *fakeMachine) ResourcePoolName() string                            { return "" }
func (m *fakeMachine) ZoneName() string                                    { return "" }
func (m *fakeMachine) BootInterfaceName() string                           { return "" }
func (m *fakeMachine) Tags() []string                                      { return m.tags }
func (m *fakeMachine) Parent() string                                      { return "" }

// fakeMachineWithParent extends fakeMachine to support Parent() for VM counting tests
type fakeMachineWithParent struct {
	fakeMachine
	parent string
}

func (m *fakeMachineWithParent) Get(ctx context.Context) (maasclient.Machine, error) { return m, nil }
func (m *fakeMachineWithParent) Parent() string                                      { return m.parent }

// ---- Full fake client for new interface ----

type fakeFullClient struct {
	machines maasclient.Machines
	vmHosts  maasclient.VMHosts
}

func (f *fakeFullClient) Machines() maasclient.Machines { return f.machines }
func (f *fakeFullClient) VMHosts() maasclient.VMHosts   { return f.vmHosts }

type fakeVMHosts struct {
	hosts map[string]maasclient.VMHost
}

func (v *fakeVMHosts) List(ctx context.Context, params maasclient.Params) ([]maasclient.VMHost, error) {
	var result []maasclient.VMHost
	for _, h := range v.hosts {
		result = append(result, h)
	}
	return result, nil
}
func (v *fakeVMHosts) Create(ctx context.Context, params maasclient.Params) (maasclient.VMHost, error) {
	return nil, nil
}
func (v *fakeVMHosts) VMHost(systemID string) maasclient.VMHost {
	if h, ok := v.hosts[systemID]; ok {
		return h
	}
	return &fakeVMHost{systemID: systemID}
}

// ---- Tests ----

// U1: No hosts returns error
func TestSelectLXDHost_NoHosts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{}},
	}

	_, err := SelectLXDHostWithMaasClient(client, nil, SelectOptions{})
	if err == nil {
		t.Fatal("expected error when no hosts provided")
	}

	_, err = SelectLXDHostWithMaasClient(client, []maasclient.VMHost{}, SelectOptions{})
	if err == nil {
		t.Fatal("expected error when empty hosts provided")
	}
}

// U2: Zone set, one host matches zone
func TestSelectLXDHost_ZoneMatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-z1", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{Zone: "z1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-z1" {
		t.Fatalf("expected host-z1, got %s", selected.Name())
	}
}

// U3: Zone set, no host matches
func TestSelectLXDHost_ZoneNoMatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-z2", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z2", id: 2}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	_, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{Zone: "z1"})
	if err == nil {
		t.Fatal("expected error when zone doesn't match")
	}
}

// U4: Zone empty, pool set, host matches pool
func TestSelectLXDHost_PoolMatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-p1", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{ResourcePool: "p1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-p1" {
		t.Fatalf("expected host-p1, got %s", selected.Name())
	}
}

// U5: Tags set, host has all tags
func TestSelectLXDHost_TagsMatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-tagged", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			tags: []string{"t1", "t2", "t3"},
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{Tags: []string{"t1", "t2"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-tagged" {
		t.Fatalf("expected host-tagged, got %s", selected.Name())
	}
}

// U6: Tags set, host missing one tag
func TestSelectLXDHost_TagsMissing(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-partial", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			tags: []string{"t1"}, // missing t2
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	_, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{Tags: []string{"t1", "t2"}})
	if err == nil {
		t.Fatal("expected error when host is missing required tags")
	}
}

// U7: Tags empty, no tag filter applied
func TestSelectLXDHost_TagsEmpty(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-notags", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{Tags: nil})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-notags" {
		t.Fatalf("expected host-notags, got %s", selected.Name())
	}
}

// U8: MinCores set, host has insufficient cores
func TestSelectLXDHost_InsufficientCores(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-lowcpu", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableCores: 4, // only 4 cores
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	_, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{MinCores: 8})
	if err == nil {
		t.Fatal("expected error when host has insufficient cores")
	}
}

// U9: MinCores set, host has sufficient cores
func TestSelectLXDHost_SufficientCores(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-goodcpu", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableCores: 8,
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{MinCores: 4})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-goodcpu" {
		t.Fatalf("expected host-goodcpu, got %s", selected.Name())
	}
}

// U10: MinMemory set, host has insufficient memory
func TestSelectLXDHost_InsufficientMemory(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-lowmem", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableMemory: 4096, // 4GB
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	_, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{MinMemory: 8192})
	if err == nil {
		t.Fatal("expected error when host has insufficient memory")
	}
}

// U11: Maintenance host excluded
func TestSelectLXDHost_MaintenanceExcluded(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: []string{maintenance.TagHostMaintenance}}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-maint", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
		&fakeVMHost{name: "host-healthy", systemID: "2", hostSystemID: "H2", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0], "2": hosts[1]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-healthy" {
		t.Fatalf("expected host-healthy, got %s", selected.Name())
	}
}

// U12: Unhealthy host excluded (power off or not Deployed)
func TestSelectLXDHost_UnhealthyExcluded(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "off", state: "Deployed", tags: nil}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Ready", tags: nil} // Not deployed
	m3 := &fakeMachine{systemID: "H3", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()
	machines.EXPECT().Machine("H3").Return(m3).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-off", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
		&fakeVMHost{name: "host-notdeployed", systemID: "2", hostSystemID: "H2", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
		&fakeVMHost{name: "host-healthy", systemID: "3", hostSystemID: "H3", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0], "2": hosts[1], "3": hosts[2]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-healthy" {
		t.Fatalf("expected host-healthy, got %s", selected.Name())
	}
}

// U13: Multiple eligible, prefer 0 CP count
func TestSelectLXDHost_PreferLessCPCount(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()

	// Create a CP VM parented to H1 (host1's backing machine)
	clusterTag := maintenance.TagVMClusterPrefix + maintenance.SanitizeID("test-cluster")
	cpVM := &fakeMachineWithParent{
		fakeMachine: fakeMachine{systemID: "VM1", powerState: "on", state: "Deployed", tags: []string{maintenance.TagVMControlPlane, clusterTag}},
		parent:      "H1", // Parented to host1's backing machine
	}

	// Mock Machines().List() to return all machines including the CP VM
	machines.EXPECT().List(gomock.Any(), gomock.Any()).Return([]maasclient.Machine{cpVM}, nil).AnyTimes()
	machines.EXPECT().Machine("VM1").Return(cpVM).AnyTimes()

	host1 := &fakeVMHost{
		name: "host-with-cp", systemID: "1", hostSystemID: "H1",
		zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
	}
	host2 := &fakeVMHost{
		name: "host-no-cp", systemID: "2", hostSystemID: "H2",
		zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
	}

	hosts := []maasclient.VMHost{host1, host2}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": host1, "2": host2}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{ClusterID: "test-cluster"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-no-cp" {
		t.Fatalf("expected host-no-cp (0 CP VMs), got %s", selected.Name())
	}
}

// U14: Multiple eligible, same CP count, prefer higher combined resource score
func TestSelectLXDHost_PreferMoreMemory(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()

	// Both hosts have same cores (8), different memory
	// host-lowmem: score = 8/4 + 16384/8192 = 2 + 2 = 4
	// host-highmem: score = 8/4 + 32768/8192 = 2 + 4 = 6 (wins)
	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-lowmem", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableCores:  8,
			availableMemory: 16384, // 16GB
		},
		&fakeVMHost{
			name: "host-highmem", systemID: "2", hostSystemID: "H2",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableCores:  8,
			availableMemory: 32768, // 32GB
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0], "2": hosts[1]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{MinCores: 4, MinMemory: 8192})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-highmem" {
		t.Fatalf("expected host-highmem, got %s", selected.Name())
	}
}

// U15: Multiple eligible, same CP count and memory, prefer managed
func TestSelectLXDHost_PreferManaged(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "oob-host", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableMemory: 32768,
			availableCores:  16,
		},
		&fakeVMHost{
			name: "lxd-host-managed", systemID: "2", hostSystemID: "H2",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			availableMemory: 32768,
			availableCores:  16,
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0], "2": hosts[1]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "lxd-host-managed" {
		t.Fatalf("expected lxd-host-managed, got %s", selected.Name())
	}
}

// U16: No eligible after all filters
func TestSelectLXDHost_NoEligible(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "off", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{name: "host-off", systemID: "1", hostSystemID: "H1", zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1}},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0]}},
	}

	_, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{})
	if err == nil {
		t.Fatal("expected error when no eligible hosts")
	}
}

// Combined test: zone + pool + tags
func TestSelectLXDHost_CombinedFilters(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	machines := mock_clientset.NewMockMachines(ctrl)
	m1 := &fakeMachine{systemID: "H1", powerState: "on", state: "Deployed", tags: nil}
	m2 := &fakeMachine{systemID: "H2", powerState: "on", state: "Deployed", tags: nil}
	m3 := &fakeMachine{systemID: "H3", powerState: "on", state: "Deployed", tags: nil}
	machines.EXPECT().Machine("H1").Return(m1).AnyTimes()
	machines.EXPECT().Machine("H2").Return(m2).AnyTimes()
	machines.EXPECT().Machine("H3").Return(m3).AnyTimes()

	hosts := []maasclient.VMHost{
		&fakeVMHost{
			name: "host-wrong-zone", systemID: "1", hostSystemID: "H1",
			zone: &fakeZone{name: "z2", id: 2}, pool: &fakeResourcePool{name: "p1", id: 1},
			tags: []string{"gpu"},
		},
		&fakeVMHost{
			name: "host-wrong-pool", systemID: "2", hostSystemID: "H2",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p2", id: 2},
			tags: []string{"gpu"},
		},
		&fakeVMHost{
			name: "host-match-all", systemID: "3", hostSystemID: "H3",
			zone: &fakeZone{name: "z1", id: 1}, pool: &fakeResourcePool{name: "p1", id: 1},
			tags: []string{"gpu", "ssd"},
		},
	}

	client := &fakeFullClient{
		machines: machines,
		vmHosts:  &fakeVMHosts{hosts: map[string]maasclient.VMHost{"1": hosts[0], "2": hosts[1], "3": hosts[2]}},
	}

	selected, err := SelectLXDHostWithMaasClient(client, hosts, SelectOptions{
		Zone:         "z1",
		ResourcePool: "p1",
		Tags:         []string{"gpu"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if selected.Name() != "host-match-all" {
		t.Fatalf("expected host-match-all, got %s", selected.Name())
	}
}
