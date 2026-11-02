package machine

import (
	"context"
	"net"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/spectrocloud/maas-client-go/maasclient"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

// Hand-rolled fakes: the generated mocks do not cover VM hosts or resource pools, and the
// flow under test makes many incidental property calls. Embedded interfaces are left nil so
// any call the test did not anticipate panics instead of passing silently.

type fakeZone struct {
	id   int
	name string
}

func (z *fakeZone) ID() int             { return z.id }
func (z *fakeZone) Name() string        { return z.name }
func (z *fakeZone) Description() string { return "" }

type fakePool struct {
	id   int
	name string
}

func (p *fakePool) ID() int             { return p.id }
func (p *fakePool) Name() string        { return p.name }
func (p *fakePool) Description() string { return "" }

type fakeDeployer struct {
	maasclient.MachineDeployer
	m         *fakeMachine
	ephemeral bool
	deployed  bool
}

func (d *fakeDeployer) SetOSSystem(string) maasclient.MachineDeployer     { return d }
func (d *fakeDeployer) SetUserData(string) maasclient.MachineDeployer     { return d }
func (d *fakeDeployer) SetDistroSeries(string) maasclient.MachineDeployer { return d }
func (d *fakeDeployer) SetEphemeralDeploy(v bool) maasclient.MachineDeployer {
	d.ephemeral = v
	return d
}
func (d *fakeDeployer) Deploy(context.Context) (maasclient.Machine, error) {
	d.deployed = true
	d.m.state = "Deploying"
	return d.m, nil
}

type fakeMachine struct {
	maasclient.Machine
	systemID  string
	hostname  string
	state     string
	powerType string
	// zone is a pointer so a test can reproduce the SDK's typed-nil Zone() on an unfetched
	// machine handle.
	zone      *fakeZone
	deployer  *fakeDeployer
	deleted   bool
	deleteErr error
}

func (m *fakeMachine) Get(context.Context) (maasclient.Machine, error) { return m, nil }
func (m *fakeMachine) Delete(context.Context) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	m.deleted = true
	return nil
}
func (m *fakeMachine) Deployer() maasclient.MachineDeployer {
	if m.deployer == nil {
		m.deployer = &fakeDeployer{m: m}
	}
	return m.deployer
}
func (m *fakeMachine) SystemID() string         { return m.systemID }
func (m *fakeMachine) Hostname() string         { return m.hostname }
func (m *fakeMachine) State() string            { return m.state }
func (m *fakeMachine) PowerState() string       { return "off" }
func (m *fakeMachine) PowerType() string        { return m.powerType }
func (m *fakeMachine) DeployedInMemory() bool   { return false }
func (m *fakeMachine) FQDN() string             { return "" }
func (m *fakeMachine) IPAddresses() []net.IP    { return nil }
func (m *fakeMachine) Zone() maasclient.Zone    { return m.zone }
func (m *fakeMachine) ZoneName() string         { return "" }
func (m *fakeMachine) ResourcePoolName() string { return "" }

type fakeMachines struct {
	maasclient.Machines
	byID map[string]*fakeMachine
}

func (f *fakeMachines) List(context.Context, maasclient.Params) ([]maasclient.Machine, error) {
	var out []maasclient.Machine
	for _, m := range f.byID {
		out = append(out, m)
	}
	return out, nil
}

func (f *fakeMachines) Machine(id string) maasclient.Machine {
	if m, ok := f.byID[id]; ok {
		return m
	}
	return &fakeMachine{systemID: id}
}

type fakeComposer struct {
	params maasclient.Params
	result *fakeMachine
	calls  int
}

func (c *fakeComposer) Compose(_ context.Context, p maasclient.Params) (maasclient.Machine, error) {
	c.calls++
	c.params = p
	return c.result, nil
}

type fakeVMHost struct {
	maasclient.VMHost
	name     string
	zone     *fakeZone
	pool     *fakePool
	cores    int
	memory   int
	storage  []maasclient.StoragePool
	composer *fakeComposer
}

func (h *fakeVMHost) SystemID() string { return "host-" + h.name }
func (h *fakeVMHost) Name() string     { return h.name }
func (h *fakeVMHost) Type() string     { return "virsh" }
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
func (h *fakeVMHost) AvailableCores() int                    { return h.cores }
func (h *fakeVMHost) AvailableMemory() int                   { return h.memory }
func (h *fakeVMHost) Tags() []string                         { return nil }
func (h *fakeVMHost) StoragePools() []maasclient.StoragePool { return h.storage }
func (h *fakeVMHost) Composer() maasclient.VMComposer        { return h.composer }

type fakeVMHosts struct {
	maasclient.VMHosts
	hosts []maasclient.VMHost
}

func (f *fakeVMHosts) List(context.Context, maasclient.Params) ([]maasclient.VMHost, error) {
	return f.hosts, nil
}

type fakeResourcePools struct {
	maasclient.ResourcePools
	pools []maasclient.ResourcePool
}

func (f *fakeResourcePools) List(context.Context, maasclient.Params) ([]maasclient.ResourcePool, error) {
	return f.pools, nil
}

type fakeClientSet struct {
	maasclient.ClientSetInterface
	machines *fakeMachines
	vmHosts  *fakeVMHosts
	pools    *fakeResourcePools
}

func (c *fakeClientSet) Machines() maasclient.Machines           { return c.machines }
func (c *fakeClientSet) VMHosts() maasclient.VMHosts             { return c.vmHosts }
func (c *fakeClientSet) ResourcePools() maasclient.ResourcePools { return c.pools }

func newFakeClientSet(machines ...*fakeMachine) *fakeClientSet {
	byID := map[string]*fakeMachine{}
	for _, m := range machines {
		byID[m.systemID] = m
	}
	return &fakeClientSet{
		machines: &fakeMachines{byID: byID},
		vmHosts:  &fakeVMHosts{},
		pools:    &fakeResourcePools{},
	}
}

func newVirshService(t *testing.T, c maasclient.ClientSetInterface, mutate func(*infrav1beta1.MaasMachine)) *Service {
	t.Helper()
	g := NewWithT(t)

	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-0", Namespace: "default"},
		Spec: infrav1beta1.MaasMachineSpec{
			MinCPU:        ptr.To(2),
			MinMemoryInMB: ptr.To(4096),
			Image:         "custom-image",
			Virsh:         &infrav1beta1.MachineVirshConfig{Enabled: ptr.To(true)},
		},
	}
	if mutate != nil {
		mutate(mm)
	}

	s := runtime.NewScheme()
	g.Expect(infrav1beta1.AddToScheme(s)).To(Succeed())
	k8s := fake.NewClientBuilder().WithScheme(s).WithObjects(mm).WithStatusSubresource(mm).Build()

	ms, err := scope.NewMachineScope(scope.MachineScopeParams{
		Client:  k8s,
		Logger:  textlogger.NewLogger(textlogger.NewConfig()),
		Cluster: &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"}},
		ClusterScope: &scope.ClusterScope{
			MaasCluster: &infrav1beta1.MaasCluster{Spec: infrav1beta1.MaasClusterSpec{
				Virsh: &infrav1beta1.VirshConfig{Enabled: ptr.To(true)},
			}},
		},
		Machine: &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{
			Name: "worker-0", Namespace: "default", UID: types.UID("abcdef12-3456"),
		}},
		MaasMachine: mm,
	})
	g.Expect(err).ToNot(HaveOccurred())

	return &Service{scope: ms, maasClient: c}
}

func TestVirshComposeRecordsVMWhoseZoneIsNotFetched(t *testing.T) {
	g := NewWithT(t)

	// Compose returns a handle without fetched details: its Zone() is a typed nil.
	composed := &fakeMachine{systemID: "vm1", zone: nil}
	host := &fakeVMHost{
		name: "kvm1", zone: &fakeZone{id: 3, name: "zone-a"}, pool: &fakePool{id: 1, name: "pool-a"},
		cores: 32, memory: 65536, composer: &fakeComposer{result: composed},
	}
	c := newFakeClientSet()
	c.vmHosts.hosts = []maasclient.VMHost{host}
	s := newVirshService(t, c, nil)

	var err error
	g.Expect(func() { _, err = s.DeployMachine("userdata") }).ToNot(Panic())
	g.Expect(err).To(MatchError(ErrVMComposing))
	g.Expect(host.composer.calls).To(Equal(1))
	g.Expect(s.scope.MaasMachine.Spec.SystemID).To(Equal(ptr.To("vm1")))
	g.Expect(s.scope.MaasMachine.Spec.ProviderID).To(Equal(ptr.To("maas:///zone-a/vm1")))
}

func TestVirshAdoptsAnOrphanedVMByHostname(t *testing.T) {
	g := NewWithT(t)

	orphan := &fakeMachine{systemID: "vm9", hostname: "vm-worker-0-abcde", state: "Commissioning", zone: &fakeZone{id: 3, name: "zone-a"}}
	host := &fakeVMHost{name: "kvm1", cores: 32, memory: 65536, composer: &fakeComposer{}}
	c := newFakeClientSet(orphan)
	c.vmHosts.hosts = []maasclient.VMHost{host}
	s := newVirshService(t, c, nil)

	_, err := s.DeployMachine("userdata")
	g.Expect(err).To(MatchError(ErrVMComposing))
	g.Expect(host.composer.calls).To(BeZero(), "an existing VM must be adopted, not composed again")
	g.Expect(s.scope.MaasMachine.Spec.ProviderID).To(Equal(ptr.To("maas:///zone-a/vm9")))
}
