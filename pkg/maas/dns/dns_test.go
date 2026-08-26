package dns

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	"github.com/spectrocloud/maas-client-go/maasclient"
)

// fakeClientSet minimally satisfies maasclient.ClientSetInterface for tests
type fakeClientSet struct {
	dns               maasclient.DNSResources
	networkInterfaces maasclient.NetworkInterfaces
}

func (f *fakeClientSet) BootResources() maasclient.BootResources         { return nil }
func (f *fakeClientSet) DNSResources() maasclient.DNSResources           { return f.dns }
func (f *fakeClientSet) Domains() maasclient.Domains                     { return nil }
func (f *fakeClientSet) IPAddresses() maasclient.IPAddresses             { return nil }
func (f *fakeClientSet) Tags() maasclient.Tags                           { return nil }
func (f *fakeClientSet) Machines() maasclient.Machines                   { return nil }
func (f *fakeClientSet) NetworkInterfaces() maasclient.NetworkInterfaces { return f.networkInterfaces }
func (f *fakeClientSet) RackControllers() maasclient.RackControllers     { return nil }
func (f *fakeClientSet) ResourcePools() maasclient.ResourcePools         { return nil }
func (f *fakeClientSet) Spaces() maasclient.Spaces                       { return nil }
func (f *fakeClientSet) Users() maasclient.Users                         { return nil }
func (f *fakeClientSet) Zones() maasclient.Zones                         { return nil }
func (f *fakeClientSet) SSHKeys() maasclient.SSHKeys                     { return nil }
func (f *fakeClientSet) VMHosts() maasclient.VMHosts                     { return nil }
func (f *fakeClientSet) IPRanges() maasclient.IPRanges                   { return nil }
func (f *fakeClientSet) Subnets() maasclient.Subnets                     { return nil }

// fakeIPAddress satisfies maasclient.IPAddress for tests
type fakeIPAddress struct{ ip net.IP }

func (f *fakeIPAddress) IP() net.IP                                  { return f.ip }
func (f *fakeIPAddress) InterfaceSet() []maasclient.NetworkInterface { return nil }

type fakeNetworkInterfaces struct {
	interfaces []maasclient.NetworkInterface
	err        error
}

func (f *fakeNetworkInterfaces) Get(_ context.Context, _ string) ([]maasclient.NetworkInterface, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.interfaces, nil
}

func (f *fakeNetworkInterfaces) Interface(_, _ string) maasclient.NetworkInterface {
	return nil
}

func (f *fakeNetworkInterfaces) SetBootInterfaceStaticIP(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeNetworkInterfaces) SetStaticIPOnInterfaceID(_ context.Context, _, _, _ string) error {
	return nil
}

func (f *fakeNetworkInterfaces) CreateBridge(_ context.Context, _, _, _ string) (maasclient.NetworkInterface, error) {
	return nil, nil
}

func (f *fakeNetworkInterfaces) CreateBootInterfaceBridge(_ context.Context, _, _ string) (maasclient.NetworkInterface, error) {
	return nil, nil
}

type fakeNetworkInterface struct {
	name     string
	tags     []string
	links    []maasclient.NetworkInterfaceLink
	children []string
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
func (f *fakeNetworkInterface) ID() string                                    { return "" }
func (f *fakeNetworkInterface) Name() string                                  { return f.name }
func (f *fakeNetworkInterface) Tags() []string                                { return f.tags }
func (f *fakeNetworkInterface) Type() string                                  { return "" }
func (f *fakeNetworkInterface) Enabled() bool                                 { return true }
func (f *fakeNetworkInterface) MACAddress() string                            { return "" }
func (f *fakeNetworkInterface) Links() []maasclient.NetworkInterfaceLink      { return f.links }
func (f *fakeNetworkInterface) Children() []string                            { return f.children }
func (f *fakeNetworkInterface) VLAN() maasclient.VLAN                         { return nil }

type fakeNetworkInterfaceLink struct {
	ip net.IP
}

func (f *fakeNetworkInterfaceLink) ID() string                { return "" }
func (f *fakeNetworkInterfaceLink) Mode() string              { return "" }
func (f *fakeNetworkInterfaceLink) Subnet() maasclient.Subnet { return nil }
func (f *fakeNetworkInterfaceLink) IPAddress() net.IP         { return f.ip }

func TestDNS(t *testing.T) {
	log := klogr.New()
	cluster := &clusterv1.Cluster{
		ObjectMeta: v1.ObjectMeta{
			Name: "a",
		},
	}
	maasCluster := &infrav1beta1.MaasCluster{
		Spec: infrav1beta1.MaasClusterSpec{
			DNSDomain: "b.com",
		},
	}

	t.Run("reconcile dns", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockDNSResources := mockclientset.NewMockDNSResources(ctrl)
		mockDNSResourceBuilder := mockclientset.NewMockDNSResourceBuilder(ctrl)
		s := &Service{
			scope: &scope.ClusterScope{
				Logger:      log,
				Cluster:     cluster,
				MaasCluster: maasCluster,
			},
			maasClient: mockClientSetInterface,
		}
		mockClientSetInterface.EXPECT().DNSResources().Return(mockDNSResources)
		mockDNSResources.EXPECT().List(context.Background(), gomock.Any()).Return(nil, nil)
		mockClientSetInterface.EXPECT().DNSResources().Return(mockDNSResources)
		mockDNSResources.EXPECT().Builder().Return(mockDNSResourceBuilder)
		mockDNSResourceBuilder.EXPECT().WithFQDN(gomock.Any()).Return(mockDNSResourceBuilder)
		mockDNSResourceBuilder.EXPECT().WithAddressTTL("10").Return(mockDNSResourceBuilder)
		mockDNSResourceBuilder.EXPECT().WithIPAddresses(nil).Return(mockDNSResourceBuilder)
		mockDNSResourceBuilder.EXPECT().Create(context.TODO())
		err := s.ReconcileDNS()

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(s.scope.GetDNSName()).To(ContainSubstring(cluster.Name))
		g.Expect(s.scope.GetDNSName()).To(ContainSubstring(maasCluster.Spec.DNSDomain))
	})

	t.Run("update dns attachment", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockDNSResources := mockclientset.NewMockDNSResources(ctrl)
		mockDNSResource := mockclientset.NewMockDNSResource(ctrl)
		mockDNSResourceModifier := mockclientset.NewMockDNSResourceModifier(ctrl)
		s := &Service{
			scope: &scope.ClusterScope{
				Logger:      log,
				Cluster:     cluster,
				MaasCluster: maasCluster,
			},
			maasClient: mockClientSetInterface,
		}

		mockClientSetInterface.EXPECT().DNSResources().Return(mockDNSResources)
		mockDNSResources.EXPECT().List(context.Background(), gomock.Any()).Return([]maasclient.DNSResource{mockDNSResource}, nil)
		mockDNSResource.EXPECT().Modifier().Return(mockDNSResourceModifier)
		mockDNSResourceModifier.EXPECT().SetIPAddresses([]string{"1.1.1.1", "8.8.8.8"}).Return(mockDNSResourceModifier)
		mockDNSResourceModifier.EXPECT().Modify(context.TODO()).Return(mockDNSResource, nil)

		err := s.UpdateDNSAttachments([]string{"1.1.1.1", "8.8.8.8"})

		g.Expect(err).ToNot(HaveOccurred())
	})

	t.Run("machine is registered", func(t *testing.T) {
		g := NewGomegaWithT(t)
		ctrl := gomock.NewController(t)
		mockClientSetInterface := mockclientset.NewMockClientSetInterface(ctrl)
		mockDNSResources := mockclientset.NewMockDNSResources(ctrl)
		mockDNSResource := mockclientset.NewMockDNSResource(ctrl)
		mockIPAddress := mockclientset.NewMockIPAddress(ctrl)
		s := &Service{
			scope: &scope.ClusterScope{
				Logger:      log,
				Cluster:     cluster,
				MaasCluster: maasCluster,
			},
			maasClient: mockClientSetInterface,
		}
		mockClientSetInterface.EXPECT().DNSResources().Return(mockDNSResources)
		mockDNSResources.EXPECT().List(context.Background(), gomock.Any()).Return([]maasclient.DNSResource{mockDNSResource}, nil)
		mockDNSResource.EXPECT().IPAddresses().Return([]maasclient.IPAddress{mockIPAddress})
		mockIPAddress.EXPECT().IP().Return(net.ParseIP("1.1.1.1"))
		mockIPAddress.EXPECT().IP().Return(net.ParseIP("8.8.8.8"))

		res, err := s.MachineIsRegisteredWithAPIServerDNS(&infrav1beta1.Machine{
			Addresses: []clusterv1.MachineAddress{
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "1.1.1.1",
				},
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "8.8.8.8",
				},
			},
		})

		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(res).To(BeTrue())
	})
}

func TestGetMachineIPForInterfaceTag(t *testing.T) {
	t.Run("returns ip from selected interface tag", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						&fakeNetworkInterface{
							name:  "eno1",
							tags:  []string{"storage"},
							links: []maasclient.NetworkInterfaceLink{
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.10")},
							},
						},
						&fakeNetworkInterface{
							name:  "eno2",
							tags:  []string{"control-plane"},
							links: []maasclient.NetworkInterfaceLink{
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.20")},
							},
						},
					},
				},
			},
		}

		ip, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(ip).To(Equal("10.0.0.20"))
	})

	t.Run("fails when multiple interfaces share the same tag", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						&fakeNetworkInterface{
							name:  "eno1",
							tags:  []string{"control-plane"},
							links: []maasclient.NetworkInterfaceLink{
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.10")},
							},
						},
						&fakeNetworkInterface{
							name:  "eno2",
							tags:  []string{"control-plane"},
							links: []maasclient.NetworkInterfaceLink{
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.20")},
							},
						},
					},
				},
			},
		}

		_, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("tag \"control-plane\" is assigned to multiple interfaces"))
		g.Expect(err.Error()).To(ContainSubstring("eno1"))
		g.Expect(err.Error()).To(ContainSubstring("eno2"))
	})

	t.Run("fails when no interface has the tag", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						&fakeNetworkInterface{name: "eno1", tags: []string{"storage"}},
					},
				},
			},
		}

		_, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no interface with tag \"control-plane\" found"))
	})

	t.Run("fails when selected interface has no ip", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						&fakeNetworkInterface{
							name:  "eno2",
							tags:  []string{"control-plane"},
							links: []maasclient.NetworkInterfaceLink{},
						},
					},
				},
			},
		}

		_, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("no IPv4 address found on interface"))
	})

	t.Run("returns ip from bridge child when tagged NIC has no links", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						// Tagged NIC – MAAS moved its links to the bridge child.
						&fakeNetworkInterface{
							name:     "eno1",
							tags:     []string{"control-plane"},
							links:    []maasclient.NetworkInterfaceLink{},
							children: []string{"br0"},
						},
						// Bridge child that carries the actual IP.
						&fakeNetworkInterface{
							name: "br0",
							links: []maasclient.NetworkInterfaceLink{
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.30")},
							},
						},
					},
				},
			},
		}

		ip, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(ip).To(Equal("10.0.0.30"))
	})

	t.Run("returns lowest IPv4 when interface has multiple addresses", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					interfaces: []maasclient.NetworkInterface{
						&fakeNetworkInterface{
							name: "eno1",
							tags: []string{"control-plane"},
							links: []maasclient.NetworkInterfaceLink{
								// IPv6 – must be ignored.
								&fakeNetworkInterfaceLink{ip: net.ParseIP("2001:db8::1")},
								// Two IPv4 – lowest sorted address wins.
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.20")},
								&fakeNetworkInterfaceLink{ip: net.ParseIP("10.0.0.10")},
							},
						},
					},
				},
			},
		}

		ip, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).ToNot(HaveOccurred())
		g.Expect(ip).To(Equal("10.0.0.10"))
	})

	t.Run("fails when listing interfaces fails", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{
			maasClient: &fakeClientSet{
				networkInterfaces: &fakeNetworkInterfaces{
					err: errors.New("boom"),
				},
			},
		}

		_, err := s.GetMachineIPForInterfaceTag("abc123", "control-plane")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("failed to list interfaces"))
	})

	t.Run("validates required params", func(t *testing.T) {
		g := NewGomegaWithT(t)
		s := &Service{maasClient: &fakeClientSet{}}

		_, err := s.GetMachineIPForInterfaceTag("", "control-plane")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("systemID is required"))

		_, err = s.GetMachineIPForInterfaceTag("abc123", "")
		g.Expect(err).To(HaveOccurred())
		g.Expect(err.Error()).To(ContainSubstring("interfaceTag is required"))
	})
}
