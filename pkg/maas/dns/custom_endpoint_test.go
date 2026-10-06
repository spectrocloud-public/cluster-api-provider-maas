package dns

import (
	"testing"

	"github.com/golang/mock/gomock"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/textlogger"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	mockclientset "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/client/mock"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
)

func newDNSService(annotated bool, client *mockclientset.MockClientSetInterface) (*Service, *infrav1beta1.MaasCluster) {
	mc := &infrav1beta1.MaasCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec: infrav1beta1.MaasClusterSpec{
			DNSDomain:            "maas.example.com",
			ControlPlaneEndpoint: infrav1beta1.APIEndpoint{Host: "api.example.com", Port: 6443},
		},
	}
	if annotated {
		mc.Annotations = map[string]string{infrautil.CustomEndpointProvidedAnnotation: "true"}
	}
	return &Service{
		scope: &scope.ClusterScope{
			Logger:      textlogger.NewLogger(textlogger.NewConfig()),
			Cluster:     &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			MaasCluster: mc,
		},
		maasClient: client,
	}, mc
}

// The mock client has no expectations, so any MAAS call fails the test.
func TestCustomEndpointSkipsMaasDNS(t *testing.T) {
	tests := []struct {
		name string
		call func(g Gomega, s *Service)
	}{
		{
			name: "ReconcileDNS does not create a DNS resource",
			call: func(g Gomega, s *Service) { g.Expect(s.ReconcileDNS()).To(Succeed()) },
		},
		{
			name: "UpdateDNSAttachments with no control-plane IPs does not clear the record",
			call: func(g Gomega, s *Service) { g.Expect(s.UpdateDNSAttachments(nil)).To(Succeed()) },
		},
		{
			name: "UpdateDNSAttachments with IPs does not modify the record",
			call: func(g Gomega, s *Service) { g.Expect(s.UpdateDNSAttachments([]string{"192.0.2.10"})).To(Succeed()) },
		},
		{
			name: "GetDNSResource returns nothing",
			call: func(g Gomega, s *Service) {
				res, err := s.GetDNSResource()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(res).To(BeNil())
			},
		},
		{
			name: "GetAPIServerDNSRecords returns an empty set",
			call: func(g Gomega, s *Service) {
				ips, err := s.GetAPIServerDNSRecords()
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(ips.Has("192.0.2.10")).To(BeFalse())
			},
		},
		{
			name: "MachineIsRegisteredWithAPIServerDNS reports registered",
			call: func(g Gomega, s *Service) {
				ok, err := s.MachineIsRegisteredWithAPIServerDNS(&infrav1beta1.Machine{
					Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineExternalIP, Address: "192.0.2.10"}},
				})
				g.Expect(err).ToNot(HaveOccurred())
				g.Expect(ok).To(BeTrue())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			s, mc := newDNSService(true, mockclientset.NewMockClientSetInterface(gomock.NewController(t)))

			tt.call(g, s)
			g.Expect(mc.Status.Network.DNSName).To(Equal("api.example.com"))
		})
	}
}

func TestWithoutCustomEndpointQueriesMaasDNS(t *testing.T) {
	g := NewWithT(t)
	ctrl := gomock.NewController(t)
	client := mockclientset.NewMockClientSetInterface(ctrl)
	dnsResources := mockclientset.NewMockDNSResources(ctrl)
	s, _ := newDNSService(false, client)

	client.EXPECT().DNSResources().Return(dnsResources)
	dnsResources.EXPECT().List(gomock.Any(), gomock.Any()).Return(nil, nil)

	_, err := s.GetDNSResource()
	g.Expect(err).To(MatchError(ErrNotFound))
}
