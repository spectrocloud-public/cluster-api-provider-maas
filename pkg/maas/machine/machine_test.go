package machine

import (
	"context"
	"net"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"
	"k8s.io/utils/pointer"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

func TestMachine(t *testing.T) {
	log := klogr.New()
	cluster := &clusterv1.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: "a",
		},
	}

	//intPlaceholder := 400

	//maasMachine := &infrav1beta1.MaasMachine{
	//	TypeMeta:   v1.TypeMeta{},
	//	ObjectMeta: v1.ObjectMeta{},
	//	Spec: infrav1beta1.MaasMachineSpec{
	//		FailureDomain: pointer.String("zone1"),
	//		ResourcePool:  pointer.String("rp1"),
	//		MinCPU:        &intPlaceholder,
	//		MinMemoryInMB: &intPlaceholder,
	//		Image:         "custom-image",
	//	},
	//	Status: infrav1beta1.MaasMachineStatus{},
	//}

	t.Run("get machine with fqdn", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)
		mockZone := mockclientset.NewMockZone(ctrl)

		s := &Service{
			scope: &scope.MachineScope{
				Logger:  log,
				Cluster: cluster,
			},
			maasClient: mockClientSetInterface,
		}

		mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("abc123").Return(mockMachine)
		mockMachine.EXPECT().Get(context.Background()).Return(mockMachine, nil)

		mockMachine.EXPECT().SystemID().Return("abc123")
		mockMachine.EXPECT().Hostname().Return("abc.hostanme")
		mockMachine.EXPECT().State().Return("Deployed")
		mockMachine.EXPECT().PowerState().Return("on")
		mockMachine.EXPECT().Zone().Return(mockZone)
		mockMachine.EXPECT().DeployedInMemory().Return(false)

		mockZone.EXPECT().Name().Return("zone1")

		mockMachine.EXPECT().FQDN().AnyTimes().Return("abc123.domain.local")
		mockMachine.EXPECT().IPAddresses().Return([]net.IP{net.ParseIP("1.2.3.4")})

		machine, err := s.GetMachine("abc123")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(machine).ToNot(BeNil())
		g.Expect(machine.Hostname).To(BeEquivalentTo("abc.hostanme"))
		g.Expect(machine.ID).To(BeEquivalentTo("abc123"))
		g.Expect(machine.Powered).To(BeTrue())
		g.Expect(machine.State).To(BeEquivalentTo("Deployed"))
		g.Expect(machine.AvailabilityZone).To(BeEquivalentTo("zone1"))
		g.Expect(machine.Addresses).To(ContainElements(clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalDNS,
			Address: "abc123.domain.local",
		}, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalIP,
			Address: "1.2.3.4",
		}))
	})

	t.Run("get machine with deployed in memory true", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)
		mockZone := mockclientset.NewMockZone(ctrl)

		s := &Service{
			scope: &scope.MachineScope{
				Logger:  log,
				Cluster: cluster,
			},
			maasClient: mockClientSetInterface,
		}

		mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("abc456").Return(mockMachine)
		mockMachine.EXPECT().Get(context.Background()).Return(mockMachine, nil)

		mockMachine.EXPECT().SystemID().Return("abc456")
		mockMachine.EXPECT().Hostname().Return("deployed.hostname")
		mockMachine.EXPECT().State().Return("Deployed")
		mockMachine.EXPECT().PowerState().Return("on")
		mockMachine.EXPECT().Zone().Return(mockZone)
		mockMachine.EXPECT().DeployedInMemory().Return(true)

		mockZone.EXPECT().Name().Return("zone2")

		mockMachine.EXPECT().FQDN().AnyTimes().Return("abc456.domain.local")
		mockMachine.EXPECT().IPAddresses().Return([]net.IP{net.ParseIP("5.6.7.8")})

		machine, err := s.GetMachine("abc456")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(machine).ToNot(BeNil())
		g.Expect(machine.Hostname).To(BeEquivalentTo("deployed.hostname"))
		g.Expect(machine.ID).To(BeEquivalentTo("abc456"))
		g.Expect(machine.Powered).To(BeTrue())
		g.Expect(machine.State).To(BeEquivalentTo("Deployed"))
		g.Expect(machine.AvailabilityZone).To(BeEquivalentTo("zone2"))
		g.Expect(machine.Addresses).To(ContainElements(clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalDNS,
			Address: "abc456.domain.local",
		}, clusterv1.MachineAddress{
			Type:    clusterv1.MachineExternalIP,
			Address: "5.6.7.8",
		}))
	})

	t.Run("release machine", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)
		mockMachineReleaser := mockclientset.NewMockMachineReleaser(ctrl)

		s := &Service{
			scope: &scope.MachineScope{
				Logger:  log,
				Cluster: cluster,
			},
			maasClient: mockClientSetInterface,
		}

		mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("abc123").Return(mockMachine)
		mockMachine.EXPECT().Releaser().Return(mockMachineReleaser)
		mockMachineReleaser.EXPECT().Release(gomock.Any()).Return(mockMachine, nil)

		err := s.ReleaseMachine("abc123")
		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("deploy machine with existing provider id", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)
		mockMachineModifier := mockclientset.NewMockMachineModifier(ctrl)
		mockMachineDeployer := mockclientset.NewMockMachineDeployer(ctrl)
		mockZone := mockclientset.NewMockZone(ctrl)

		maasMachine := &infrav1beta1.MaasMachine{
			ObjectMeta: v1.ObjectMeta{
				Name: "test-machine",
			},
			Spec: infrav1beta1.MaasMachineSpec{
				ProviderID:     pointer.String("maas:///zone1/abc789"),
				MinCPU:         pointer.Int(2),
				MinMemoryInMB:  pointer.Int(4096),
				Image:          "custom-image",
				DeployInMemory: true,
			},
		}

		s := &Service{
			scope: &scope.MachineScope{
				Logger:      log,
				Cluster:     cluster,
				ClusterScope: &scope.ClusterScope{
					MaasCluster: &infrav1beta1.MaasCluster{},
				},
				MaasMachine: maasMachine,
				Machine: &clusterv1.Machine{
					Spec: clusterv1.MachineSpec{},
				},
			},
			maasClient: mockClientSetInterface,
		}

		mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("abc789").Return(mockMachine)
		mockMachine.EXPECT().Get(context.TODO()).Return(mockMachine, nil)

		mockMachine.EXPECT().SystemID().Times(5).Return("abc789")
		mockMachine.EXPECT().Zone().AnyTimes().Return(mockZone)
		mockZone.EXPECT().Name().Return("zone1")

		mockMachine.EXPECT().Modifier().Return(mockMachineModifier)
		mockMachineModifier.EXPECT().SetSwapSize(0).Return(mockMachineModifier)
		mockMachineModifier.EXPECT().Update(context.TODO()).Return(mockMachine, nil)

		mockMachine.EXPECT().Deployer().Return(mockMachineDeployer)
		mockMachineDeployer.EXPECT().SetUserData("userdata").Return(mockMachineDeployer)
		mockMachineDeployer.EXPECT().SetOSSystem("custom").Return(mockMachineDeployer)
		mockMachineDeployer.EXPECT().SetEphemeralDeploy(true).Return(mockMachineDeployer)
		mockMachineDeployer.EXPECT().SetDistroSeries("custom-image").Return(mockMachineDeployer)
		mockMachineDeployer.EXPECT().Deploy(context.TODO()).Return(mockMachine, nil)

		mockMachine.EXPECT().Hostname().Return("existing-hostname")
		mockMachine.EXPECT().State().Return("Deployed")
		mockMachine.EXPECT().PowerState().Return("on")
		mockMachine.EXPECT().FQDN().AnyTimes().Return("existing-hostname.domain.local")
		mockMachine.EXPECT().IPAddresses().AnyTimes().Return([]net.IP{net.ParseIP("10.0.0.2")})
		mockMachine.EXPECT().DeployedInMemory().Return(true)

		machine, err := s.DeployMachine("userdata")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(machine).ToNot(BeNil())
		g.Expect(machine.Hostname).To(BeEquivalentTo("existing-hostname"))
		g.Expect(machine.ID).To(BeEquivalentTo("abc789"))
		g.Expect(machine.Powered).To(BeTrue())
		g.Expect(machine.State).To(BeEquivalentTo("Deployed"))
		g.Expect(machine.AvailabilityZone).To(BeEquivalentTo("zone1"))
		g.Expect(machine.DeployedInMemory).To(BeTrue())
	})
}

// ---- Fakes for network interface tests ----

type fakeNetworkInterfaces struct {
	ifaces   []maasclient.NetworkInterface
	fetchErr error
	captured *captureNetworkInterface // returned by Interface()
}

func (f *fakeNetworkInterfaces) Get(_ context.Context, _ string) ([]maasclient.NetworkInterface, error) {
	return f.ifaces, f.fetchErr
}
func (f *fakeNetworkInterfaces) Interface(_, _ string) maasclient.NetworkInterface {
	if f.captured != nil {
		return f.captured
	}
	return &captureNetworkInterface{}
}

func (f *fakeNetworkInterfaces) CreateBridge(_ context.Context, _, _, _ string) (maasclient.NetworkInterface, error) {
	return nil, nil
}
func (f *fakeNetworkInterfaces) CreateBootInterfaceBridge(_ context.Context, _, _ string) (maasclient.NetworkInterface, error) {
	return nil, nil
}

// captureNetworkInterface records the mode passed to LinkSubnetWithMode so tests can assert on it.
type captureNetworkInterface struct {
	capturedMode     string
	capturedSubnetID string
	linkErr          error
}

func (c *captureNetworkInterface) Get(_ context.Context) (maasclient.NetworkInterface, error) {
	return c, nil
}
func (c *captureNetworkInterface) Update(_ context.Context, _ maasclient.Params) error { return nil }
func (c *captureNetworkInterface) LinkSubnet(_ context.Context, _ string, _ string) error {
	return c.linkErr
}
func (c *captureNetworkInterface) LinkSubnetWithMode(_ context.Context, subnetID, mode, _ string) error {
	c.capturedSubnetID = subnetID
	c.capturedMode = mode
	return c.linkErr
}
func (c *captureNetworkInterface) LinkSubnetWithForce(_ context.Context, subnetID, mode, _ string) error {
	c.capturedSubnetID = subnetID
	c.capturedMode = mode
	return c.linkErr
}
func (c *captureNetworkInterface) UnlinkSubnet(_ context.Context, _ string) error { return nil }
func (c *captureNetworkInterface) UpdateIPConfiguration(_ context.Context, _ maasclient.IPConfigurationUpdate) error {
	return nil
}
func (c *captureNetworkInterface) SetStaticIP(_ context.Context, _ string) error { return nil }
func (c *captureNetworkInterface) SetDHCP(_ context.Context, _ string) error     { return nil }
func (c *captureNetworkInterface) ID() string                                    { return "" }
func (c *captureNetworkInterface) Name() string                                  { return "" }
func (c *captureNetworkInterface) Type() string                                  { return "" }
func (c *captureNetworkInterface) Enabled() bool                                 { return true }
func (c *captureNetworkInterface) MACAddress() string                            { return "" }
func (c *captureNetworkInterface) Links() []maasclient.NetworkInterfaceLink      { return nil }
func (c *captureNetworkInterface) Children() []string                            { return nil }
func (c *captureNetworkInterface) Tags() []string                                { return nil }
func (c *captureNetworkInterface) VLAN() maasclient.VLAN                         { return nil }

// fakeNetworkInterface is an in-memory NetworkInterface with a configurable name, ID, and links.
type fakeNetworkInterface struct {
	id    string
	name  string
	links []maasclient.NetworkInterfaceLink
}

func (f *fakeNetworkInterface) Get(_ context.Context) (maasclient.NetworkInterface, error) {
	return f, nil
}
func (f *fakeNetworkInterface) Update(_ context.Context, _ maasclient.Params) error { return nil }
func (f *fakeNetworkInterface) LinkSubnet(_ context.Context, _, _ string) error     { return nil }
func (f *fakeNetworkInterface) LinkSubnetWithMode(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeNetworkInterface) LinkSubnetWithForce(_ context.Context, _, _, _ string) error {
	return nil
}
func (f *fakeNetworkInterface) UnlinkSubnet(_ context.Context, _ string) error { return nil }
func (f *fakeNetworkInterface) UpdateIPConfiguration(_ context.Context, _ maasclient.IPConfigurationUpdate) error {
	return nil
}
func (f *fakeNetworkInterface) SetStaticIP(_ context.Context, _ string) error { return nil }
func (f *fakeNetworkInterface) SetDHCP(_ context.Context, _ string) error     { return nil }
func (f *fakeNetworkInterface) ID() string                                    { return f.id }
func (f *fakeNetworkInterface) Name() string                                  { return f.name }
func (f *fakeNetworkInterface) Type() string                                  { return "" }
func (f *fakeNetworkInterface) Enabled() bool                                 { return true }
func (f *fakeNetworkInterface) MACAddress() string                            { return "" }
func (f *fakeNetworkInterface) Links() []maasclient.NetworkInterfaceLink      { return f.links }
func (f *fakeNetworkInterface) Children() []string                            { return nil }
func (f *fakeNetworkInterface) Tags() []string                                { return nil }
func (f *fakeNetworkInterface) VLAN() maasclient.VLAN                         { return nil }

// fakeNetworkInterfaceLink is a minimal NetworkInterfaceLink with a configurable subnet.
type fakeNetworkInterfaceLink struct {
	id     string
	subnet maasclient.Subnet
}

func (l *fakeNetworkInterfaceLink) ID() string                { return l.id }
func (l *fakeNetworkInterfaceLink) Mode() string              { return "" }
func (l *fakeNetworkInterfaceLink) Subnet() maasclient.Subnet { return l.subnet }
func (l *fakeNetworkInterfaceLink) IPAddress() net.IP         { return nil }

// fakeSubnet is a minimal Subnet whose CIDR is used for subnet matching.
type fakeSubnet struct {
	cidr string
}

func (s *fakeSubnet) ID() int               { return 0 }
func (s *fakeSubnet) Name() string          { return s.cidr }
func (s *fakeSubnet) Space() string         { return "" }
func (s *fakeSubnet) VLAN() maasclient.VLAN { return nil }
func (s *fakeSubnet) CIDR() string          { return s.cidr }
