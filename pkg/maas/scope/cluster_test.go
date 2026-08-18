/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package scope

import (
	"testing"

	"github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
)

func TestNewCluster(t *testing.T) {
	cluster := &clusterv1.Cluster{
		TypeMeta:   metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{},
		Spec:       clusterv1.ClusterSpec{},
		Status:     clusterv1.ClusterStatus{},
	}

	maasCluster := &infrav1beta1.MaasCluster{
		TypeMeta:   metav1.TypeMeta{},
		ObjectMeta: metav1.ObjectMeta{},
		Spec:       infrav1beta1.MaasClusterSpec{},
		Status:     infrav1beta1.MaasClusterStatus{},
	}

	t.Run("new cluster scope", func(t *testing.T) {
		g := gomega.NewGomegaWithT(t)
		scheme := runtime.NewScheme()
		_ = infrav1beta1.AddToScheme(scheme)
		client := fake.NewClientBuilder().WithScheme(scheme).Build()

		log := klogr.New()
		scope, err := NewClusterScope(ClusterScopeParams{
			Client:      client,
			Logger:      log,
			Cluster:     cluster,
			MaasCluster: maasCluster,
		})

		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(scope).ToNot(gomega.BeNil())

	})

	t.Run("new dns name", func(t *testing.T) {
		g := gomega.NewGomegaWithT(t)
		scheme := runtime.NewScheme()
		_ = infrav1beta1.AddToScheme(scheme)
		client := fake.NewClientBuilder().WithScheme(scheme).Build()
		clusterCopy := cluster.DeepCopy()
		clusterCopy.Name = "dns-test"
		maasClusterCopy := maasCluster.DeepCopy()
		maasClusterCopy.Spec.DNSDomain = "maas.com"
		log := klogr.New()
		scope, err := NewClusterScope(ClusterScopeParams{
			Client:      client,
			Logger:      log,
			Cluster:     clusterCopy,
			MaasCluster: maasClusterCopy,
		})

		g.Expect(err).ToNot(gomega.HaveOccurred())
		g.Expect(scope.GetDNSName()).ToNot(gomega.BeNil())
		g.Expect(scope.GetDNSName()).To(gomega.ContainSubstring(clusterCopy.Name))
		g.Expect(scope.GetDNSName()).To(gomega.ContainSubstring(maasClusterCopy.Spec.DNSDomain))
		dnsLengh := len("dns-test-") + DnsSuffixLength + len(".maas.com")
		g.Expect(len(scope.GetDNSName())).To(gomega.Equal(dnsLengh))
	})
}
