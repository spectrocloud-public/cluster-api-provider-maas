package machine

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"
	"sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

func TestMachine(t *testing.T) {
	log := klogr.New()
	cluster := &v1beta1.Cluster{
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
		g.Expect(machine.Addresses).To(ContainElements(v1beta1.MachineAddress{
			Type:    v1beta1.MachineExternalDNS,
			Address: "abc123.domain.local",
		}, v1beta1.MachineAddress{
			Type:    v1beta1.MachineExternalIP,
			Address: "1.2.3.4",
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

	//t.Run("deploy machine with success", func(t *testing.T) {
	//	g := NewGomegaWithT(t)
	//	ctrl := gomock.NewController(t)
	//	mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
	//	mockMachines := mockclientset.NewMockMachines(ctrl)
	//	mockMachine := mockclientset.NewMockMachine(ctrl)
	//	mockMachineAllocator := mockclientset.NewMockMachineAllocator(ctrl)
	//	mockMachineModifier := mockclientset.NewMockMachineModifier(ctrl)
	//	mockMachineDeployer := mockclientset.NewMockMachineDeployer(ctrl)
	//	mockZone := mockclientset.NewMockZone(ctrl)
	//
	//	s := &Service{
	//		scope: &scope.MachineScope{
	//			Logger:      log,
	//			Cluster:     cluster,
	//			MaasMachine: maasMachine,
	//		},
	//		maasClient: mockClientSetInterface,
	//	}
	//
	//	mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
	//	mockMachines.EXPECT().Allocator().Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithZone("zone1").Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithResourcePool("rp1").Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithCPUCount(intPlaceholder).Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithMemory(intPlaceholder).Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().Allocate(context.Background()).Return(mockMachine, nil)
	//
	//	mockMachine.EXPECT().SystemID().AnyTimes().Return("abc123")
	//
	//	mockMachine.EXPECT().Modifier().Return(mockMachineModifier)
	//	mockMachineModifier.EXPECT().SetSwapSize(0).Return(mockMachineModifier)
	//	mockMachineModifier.EXPECT().Update(context.Background()).Return(mockMachine, nil)
	//
	//	mockMachine.EXPECT().Deployer().Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetUserData("userdata").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetDistroSeries("custom-image").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetOSSystem("custom").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().Deploy(context.Background()).Return(mockMachine, nil)
	//
	//	mockMachine.EXPECT().Hostname().Return("abc.hostanme")
	//	mockMachine.EXPECT().State().Return("Deployed")
	//	mockMachine.EXPECT().PowerState().Return("on")
	//	mockMachine.EXPECT().Zone().Return(mockZone)
	//
	//	mockZone.EXPECT().Name().Return("zone1")
	//
	//	mockMachine.EXPECT().FQDN().AnyTimes().Return("abc123.domain.local")
	//	mockMachine.EXPECT().IPAddresses().Return([]net.IP{net.ParseIP("1.2.3.4")})
	//	//mockMachines.EXPECT().Machine("abc123").Return(mockMachine)
	//	//mockMachine.EXPECT().Releaser().Return(mockMachineReleaser)
	//	//mockMachineReleaser.EXPECT().Release(context.Background()).Return(mockMachine, nil)
	//
	//	machine, err := s.DeployMachine("userdata")
	//	g.Expect(err).ToNot(HaveOccurred())
	//	g.Expect(machine).ToNot(BeNil())
	//	g.Expect(machine.Hostname).To(BeEquivalentTo("abc.hostanme"))
	//	g.Expect(machine.ID).To(BeEquivalentTo("abc123"))
	//	g.Expect(machine.Powered).To(BeTrue())
	//	g.Expect(machine.State).To(BeEquivalentTo("Deployed"))
	//	g.Expect(machine.AvailabilityZone).To(BeEquivalentTo("zone1"))
	//	g.Expect(machine.Addresses).To(ContainElements(v1beta1.MachineAddress{
	//		Type:    v1beta1.MachineExternalDNS,
	//		Address: "abc123.domain.local",
	//	}, v1beta1.MachineAddress{
	//		Type:    v1beta1.MachineExternalIP,
	//		Address: "1.2.3.4",
	//	}))
	//})
	//
	//t.Run("deploy machine with failure should release machine", func(t *testing.T) {
	//	g := NewGomegaWithT(t)
	//	ctrl := gomock.NewController(t)
	//	mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
	//	mockMachines := mockclientset.NewMockMachines(ctrl)
	//	mockMachine := mockclientset.NewMockMachine(ctrl)
	//	mockMachineAllocator := mockclientset.NewMockMachineAllocator(ctrl)
	//	mockMachineModifier := mockclientset.NewMockMachineModifier(ctrl)
	//	mockMachineDeployer := mockclientset.NewMockMachineDeployer(ctrl)
	//	mockMachineReleaser := mockclientset.NewMockMachineReleaser(ctrl)
	//
	//	s := &Service{
	//		scope: &scope.MachineScope{
	//			Logger:      log,
	//			Cluster:     cluster,
	//			MaasMachine: maasMachine,
	//		},
	//		maasClient: mockClientSetInterface,
	//	}
	//
	//	mockClientSetInterface.EXPECT().Machines().Return(mockMachines)
	//	mockMachines.EXPECT().Allocator().Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithZone("zone1").Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithResourcePool("rp1").Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithCPUCount(intPlaceholder).Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().WithMemory(intPlaceholder).Return(mockMachineAllocator)
	//	mockMachineAllocator.EXPECT().Allocate(context.Background()).Return(mockMachine, nil)
	//
	//	mockMachine.EXPECT().SystemID().AnyTimes().Return("abc123")
	//
	//	mockMachine.EXPECT().Modifier().Return(mockMachineModifier)
	//	mockMachineModifier.EXPECT().SetSwapSize(0).Return(mockMachineModifier)
	//	mockMachineModifier.EXPECT().Update(context.Background()).Return(mockMachine, nil)
	//
	//	mockMachine.EXPECT().Deployer().Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetUserData("userdata").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetDistroSeries("custom-image").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().SetOSSystem("custom").Return(mockMachineDeployer)
	//	mockMachineDeployer.EXPECT().Deploy(context.Background()).Return(nil, errors.New("mock failure deploy"))
	//
	//	mockMachine.EXPECT().Releaser().Return(mockMachineReleaser)
	//	mockMachineReleaser.EXPECT().Release(context.Background()).Return(mockMachine, nil)
	//
	//	machine, err := s.DeployMachine("userdata")
	//	g.Expect(err).To(HaveOccurred())
	//	g.Expect(machine).To(BeNil())
	//})
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
func (f *fakeNetworkInterfaces) SetBootInterfaceStaticIP(_ context.Context, _, _ string) error { return nil }
func (f *fakeNetworkInterfaces) SetStaticIPOnInterfaceID(_ context.Context, _, _, _ string) error {
	return nil
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

func (c *captureNetworkInterface) Get(_ context.Context) (maasclient.NetworkInterface, error) { return c, nil }
func (c *captureNetworkInterface) Update(_ context.Context, _ maasclient.Params) error         { return nil }
func (c *captureNetworkInterface) LinkSubnet(_ context.Context, _ string, _ string) error      { return c.linkErr }
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
func (c *captureNetworkInterface) SetStaticIP(_ context.Context, _ string) error          { return nil }
func (c *captureNetworkInterface) SetDHCP(_ context.Context, _ string) error              { return nil }
func (c *captureNetworkInterface) ID() string                                             { return "" }
func (c *captureNetworkInterface) Name() string                                           { return "" }
func (c *captureNetworkInterface) Type() string                                           { return "" }
func (c *captureNetworkInterface) Enabled() bool                                          { return true }
func (c *captureNetworkInterface) MACAddress() string                                     { return "" }
func (c *captureNetworkInterface) Links() []maasclient.NetworkInterfaceLink                { return nil }
func (c *captureNetworkInterface) Children() []string                                     { return nil }
func (c *captureNetworkInterface) VLAN() maasclient.VLAN                                  { return nil }

// fakeNetworkInterface is an in-memory NetworkInterface with a configurable name, ID, and links.
type fakeNetworkInterface struct {
	id    string
	name  string
	links []maasclient.NetworkInterfaceLink
}

func (f *fakeNetworkInterface) Get(_ context.Context) (maasclient.NetworkInterface, error) { return f, nil }
func (f *fakeNetworkInterface) Update(_ context.Context, _ maasclient.Params) error         { return nil }
func (f *fakeNetworkInterface) LinkSubnet(_ context.Context, _, _ string) error             { return nil }
func (f *fakeNetworkInterface) LinkSubnetWithMode(_ context.Context, _, _, _ string) error  { return nil }
func (f *fakeNetworkInterface) LinkSubnetWithForce(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeNetworkInterface) UnlinkSubnet(_ context.Context, _ string) error              { return nil }
func (f *fakeNetworkInterface) UpdateIPConfiguration(_ context.Context, _ maasclient.IPConfigurationUpdate) error {
	return nil
}
func (f *fakeNetworkInterface) SetStaticIP(_ context.Context, _ string) error          { return nil }
func (f *fakeNetworkInterface) SetDHCP(_ context.Context, _ string) error              { return nil }
func (f *fakeNetworkInterface) ID() string                                             { return f.id }
func (f *fakeNetworkInterface) Name() string                                           { return f.name }
func (f *fakeNetworkInterface) Type() string                                           { return "" }
func (f *fakeNetworkInterface) Enabled() bool                                          { return true }
func (f *fakeNetworkInterface) MACAddress() string                                     { return "" }
func (f *fakeNetworkInterface) Links() []maasclient.NetworkInterfaceLink               { return f.links }
func (f *fakeNetworkInterface) Children() []string                                     { return nil }
func (f *fakeNetworkInterface) VLAN() maasclient.VLAN                                  { return nil }

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

// ---- TestResolveLinkMode ----

func TestResolveLinkMode(t *testing.T) {
	cases := []struct {
		name      string
		lxd       *infrav1beta1.MachineLXDConfig
		ifaceName string
		expected  string
	}{
		// Bullet 1: default mode selection
		{
			name:      "default eth0 no lxd config",
			ifaceName: "eth0",
			expected:  maasclient.ModeAuto,
		},
		{
			name:      "default eth1 no lxd config",
			ifaceName: "eth1",
			expected:  maasclient.ModeDHCP,
		},
		{
			name:      "default eth2 no lxd config",
			ifaceName: "eth2",
			expected:  maasclient.ModeDHCP,
		},
		// Bullet 2: custom mode from InterfaceLinkModes map
		{
			name:      "custom static on eth0",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth0": maasclient.ModeStatic}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeStatic,
		},
		{
			name:      "custom link_up on eth1",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth1": maasclient.ModeLinkUp}}},
			ifaceName: "eth1",
			expected:  maasclient.ModeLinkUp,
		},
		{
			name:      "custom dhcp on eth0 overrides auto default",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth0": maasclient.ModeDHCP}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeDHCP,
		},
		// Bullet 3: all four allowed modes are accepted
		{
			name:      "valid auto on eth1",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth1": maasclient.ModeAuto}}},
			ifaceName: "eth1",
			expected:  maasclient.ModeAuto,
		},
		{
			name:      "valid dhcp on eth1",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth1": maasclient.ModeDHCP}}},
			ifaceName: "eth1",
			expected:  maasclient.ModeDHCP,
		},
		{
			name:      "valid static on eth1",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth1": maasclient.ModeStatic}}},
			ifaceName: "eth1",
			expected:  maasclient.ModeStatic,
		},
		// Bullet 4: invalid/empty modes fall back to the interface default
		{
			name:      "invalid mode eth0 falls back to auto",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth0": "bogus"}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeAuto,
		},
		{
			name:      "invalid mode eth1 falls back to dhcp",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth1": "bogus"}}},
			ifaceName: "eth1",
			expected:  maasclient.ModeDHCP,
		},
		{
			name:      "empty mode eth0 falls back to auto",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth0": ""}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeAuto,
		},
		{
			name:      "whitespace-only mode falls back to auto",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{"eth0": "  "}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeAuto,
		},
		// Nil guards
		{
			name:      "lxd present vmconfig nil",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: nil},
			ifaceName: "eth1",
			expected:  maasclient.ModeDHCP,
		},
		{
			name:      "interface link modes empty map",
			lxd:       &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{InterfaceLinkModes: map[string]string{}}},
			ifaceName: "eth0",
			expected:  maasclient.ModeAuto,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Service{
				scope: &scope.MachineScope{
					MaasMachine: &infrav1beta1.MaasMachine{
						Spec: infrav1beta1.MaasMachineSpec{LXD: tc.lxd},
					},
				},
			}
			got := s.resolveLinkMode(tc.ifaceName)
			if got != tc.expected {
				t.Errorf("resolveLinkMode(%q) = %q, want %q", tc.ifaceName, got, tc.expected)
			}
		})
	}
}

// ---- TestVerifyVMNetworkInterfaces ----

func TestVerifyVMNetworkInterfaces(t *testing.T) {
	log := klogr.New()

	// helpers to build fake interfaces with a single link pointing to a named subnet
	eth0With := func(cidr string) *fakeNetworkInterface {
		return &fakeNetworkInterface{
			id:   "iface-0",
			name: "eth0",
			links: []maasclient.NetworkInterfaceLink{
				&fakeNetworkInterfaceLink{id: "link-0", subnet: &fakeSubnet{cidr: cidr}},
			},
		}
	}
	eth1With := func(cidr string) *fakeNetworkInterface {
		return &fakeNetworkInterface{
			id:   "iface-1",
			name: "eth1",
			links: []maasclient.NetworkInterfaceLink{
				&fakeNetworkInterfaceLink{id: "link-1", subnet: &fakeSubnet{cidr: cidr}},
			},
		}
	}

	t.Run("lxd nil returns nil immediately", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			scope: &scope.MachineScope{
				Logger:      log,
				MaasMachine: &infrav1beta1.MaasMachine{Spec: infrav1beta1.MaasMachineSpec{LXD: nil}},
			},
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("vmconfig nil returns nil immediately", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: nil},
					},
				},
			},
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("network empty returns nil immediately", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: ""}},
					},
				},
			},
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("network with one subnet skips verification", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: "only-one-subnet"}},
					},
				},
			},
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("machine in Deployed state is skipped", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockCS := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)

		mockCS.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("sys-1").Return(mockMachine)
		mockMachine.EXPECT().Get(gomock.Any()).Return(mockMachine, nil)
		mockMachine.EXPECT().State().Return("Deployed")

		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: "sn0,sn1"}},
					},
				},
			},
			maasClient: mockCS,
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("subnets already match no fixes applied", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockCS := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)

		mockCS.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("sys-1").Return(mockMachine)
		mockMachine.EXPECT().Get(gomock.Any()).Return(mockMachine, nil)
		mockMachine.EXPECT().State().Return("Ready")
		mockCS.EXPECT().NetworkInterfaces().Return(&fakeNetworkInterfaces{
			ifaces: []maasclient.NetworkInterface{eth0With("sn0"), eth1With("sn1")},
		})

		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: "sn0,sn1"}},
					},
				},
			},
			maasClient: mockCS,
		}
		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
	})

	t.Run("eth0 subnet mismatch fixed with default auto mode", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockCS := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)

		captureIface := &captureNetworkInterface{}
		fakeNI := &fakeNetworkInterfaces{
			ifaces:   []maasclient.NetworkInterface{eth0With("10.0.0.0/24"), eth1With("sn1")},
			captured: captureIface,
		}

		mockCS.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("sys-1").Return(mockMachine)
		mockMachine.EXPECT().Get(gomock.Any()).Return(mockMachine, nil)
		mockMachine.EXPECT().State().Return("New")
		mockCS.EXPECT().NetworkInterfaces().Return(fakeNI).AnyTimes()

		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: "sn0,sn1"}},
					},
				},
			},
			maasClient: mockCS,
		}

		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
		if captureIface.capturedMode != maasclient.ModeAuto {
			t.Errorf("link mode = %q, want %q", captureIface.capturedMode, maasclient.ModeAuto)
		}
		if captureIface.capturedSubnetID != "sn0" {
			t.Errorf("subnet ID = %q, want %q", captureIface.capturedSubnetID, "sn0")
		}
	})

	t.Run("eth0 subnet mismatch fixed with custom static mode", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockCS := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)

		captureIface := &captureNetworkInterface{}
		fakeNI := &fakeNetworkInterfaces{
			ifaces:   []maasclient.NetworkInterface{eth0With("10.0.0.0/24"), eth1With("sn1")},
			captured: captureIface,
		}

		mockCS.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("sys-1").Return(mockMachine)
		mockMachine.EXPECT().Get(gomock.Any()).Return(mockMachine, nil)
		mockMachine.EXPECT().State().Return("New")
		mockCS.EXPECT().NetworkInterfaces().Return(fakeNI).AnyTimes()

		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{
							VMConfig: &infrav1beta1.VMConfig{
								Network:            "sn0,sn1",
								InterfaceLinkModes: map[string]string{"eth0": maasclient.ModeStatic},
							},
						},
					},
				},
			},
			maasClient: mockCS,
		}

		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(Succeed())
		if captureIface.capturedMode != maasclient.ModeStatic {
			t.Errorf("link mode = %q, want %q", captureIface.capturedMode, maasclient.ModeStatic)
		}
	})

	t.Run("fixInterfaceSubnet error is propagated", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		defer ctrl.Finish()

		mockCS := mockclientset.NewMockClientSetInterface(ctrl)
		mockMachines := mockclientset.NewMockMachines(ctrl)
		mockMachine := mockclientset.NewMockMachine(ctrl)

		captureIface := &captureNetworkInterface{linkErr: fmt.Errorf("link failed")}
		fakeNI := &fakeNetworkInterfaces{
			ifaces:   []maasclient.NetworkInterface{eth0With("10.0.0.0/24"), eth1With("sn1")},
			captured: captureIface,
		}

		mockCS.EXPECT().Machines().Return(mockMachines)
		mockMachines.EXPECT().Machine("sys-1").Return(mockMachine)
		mockMachine.EXPECT().Get(gomock.Any()).Return(mockMachine, nil)
		mockMachine.EXPECT().State().Return("New")
		mockCS.EXPECT().NetworkInterfaces().Return(fakeNI).AnyTimes()

		s := &Service{
			scope: &scope.MachineScope{
				Logger: log,
				MaasMachine: &infrav1beta1.MaasMachine{
					Spec: infrav1beta1.MaasMachineSpec{
						LXD: &infrav1beta1.MachineLXDConfig{VMConfig: &infrav1beta1.VMConfig{Network: "sn0,sn1"}},
					},
				},
			},
			maasClient: mockCS,
		}

		g.Expect(s.VerifyVMNetworkInterfaces(context.Background(), "sys-1")).To(HaveOccurred())
	})
}
