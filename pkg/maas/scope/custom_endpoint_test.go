package scope

import (
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/textlogger"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	infrautil "github.com/spectrocloud/cluster-api-provider-maas/pkg/util"
)

const customHost = "api.example.com"

func newCustomEndpointScope(annotated bool, host string, port int) *ClusterScope {
	mc := &infrav1beta1.MaasCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "default"},
		Spec: infrav1beta1.MaasClusterSpec{
			DNSDomain:            "maas.example.com",
			ControlPlaneEndpoint: infrav1beta1.APIEndpoint{Host: host, Port: port},
		},
	}
	if annotated {
		mc.Annotations = map[string]string{infrautil.CustomEndpointProvidedAnnotation: "true"}
	}
	return &ClusterScope{
		Logger:      textlogger.NewLogger(textlogger.NewConfig()),
		Cluster:     &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
		MaasCluster: mc,
	}
}

func TestCustomEndpointDNSName(t *testing.T) {
	tests := []struct {
		name       string
		annotated  bool
		wantCustom bool
		wantStatus string
	}{
		{name: "annotated uses the MaasCluster endpoint and sets status", annotated: true, wantCustom: true, wantStatus: customHost},
		{name: "unannotated is not a custom endpoint", annotated: false, wantCustom: false, wantStatus: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			s := newCustomEndpointScope(tt.annotated, customHost, 6443)

			g.Expect(s.IsCustomEndpoint()).To(Equal(tt.wantCustom))
			g.Expect(s.MaasCluster.Status.Network.DNSName).To(Equal(tt.wantStatus))

			name := s.GetDNSName()
			if tt.annotated {
				g.Expect(name).To(Equal(customHost))
			} else {
				g.Expect(name).ToNot(Equal(customHost))
				g.Expect(strings.HasSuffix(name, ".maas.example.com")).To(BeTrue(), name)
			}
		})
	}
}

func TestCustomEndpointAPIServerPort(t *testing.T) {
	tests := []struct {
		name        string
		annotated   bool
		port        int
		clusterPort int32
		want        int
	}{
		{name: "annotated uses the MaasCluster port", annotated: true, port: 7443, want: 7443},
		{name: "annotated without a port falls back to the default", annotated: true, port: 0, want: 6443},
		{name: "unannotated ignores the MaasCluster port", annotated: false, port: 7443, want: 6443},
		{name: "unannotated uses the Cluster port", annotated: false, clusterPort: 9443, want: 9443},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			s := newCustomEndpointScope(tt.annotated, customHost, tt.port)
			s.Cluster.Spec.ClusterNetwork.APIServerPort = tt.clusterPort

			g.Expect(s.APIServerPort()).To(Equal(tt.want))
		})
	}
}
