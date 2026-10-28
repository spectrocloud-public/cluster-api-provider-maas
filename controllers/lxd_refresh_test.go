package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
)

var defaultLXDPolicy = lxdVersionPolicy{AutoRefresh: true, Floor: lxdMinServerVersion, Approved: lxdTestedVersions}

func TestLXDVersionBelowFloor(t *testing.T) {
	g := NewGomegaWithT(t)
	tests := []struct {
		name    string
		version string
		floor   string
		below   bool
	}{
		{name: "missing version needs refresh", version: "", floor: lxdMinServerVersion, below: true},
		{name: "customer snap is below the fix", version: "5.0.3", floor: lxdMinServerVersion, below: true},
		{name: "5.0.6 does not prove the interim fix", version: "5.0.6", floor: lxdMinServerVersion, below: true},
		{name: "5.0.7 is the first proven fix", version: "5.0.7", floor: lxdMinServerVersion, below: false},
		{name: "newer patch on the same track is enough", version: "5.0.10", floor: lxdMinServerVersion, below: false},
		{name: "two component version below the floor", version: "5.0", floor: lxdMinServerVersion, below: true},
		{name: "raised floor catches an older patch", version: "5.0.8", floor: "5.0.10", below: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdVersionBelowFloor(tt.version, tt.floor)).To(Equal(tt.below))
		})
	}
	g.Expect(compareDotVersions("5.0.10", "5.0.6")).To(Equal(1))
}

func TestLXDPolicyFor(t *testing.T) {
	annotated := func(a map[string]string) *infrav1beta1.MaasCluster {
		return &infrav1beta1.MaasCluster{ObjectMeta: metav1.ObjectMeta{Annotations: a}}
	}
	withSpec := func(cfg infrav1beta1.LXDConfig, a map[string]string) *infrav1beta1.MaasCluster {
		c := annotated(a)
		c.Spec.LXDConfig = &cfg
		return c
	}

	tests := []struct {
		name         string
		cluster      *infrav1beta1.MaasCluster
		wantFloor    string
		wantApproved []string
	}{
		{name: "defaults are the built-in floor and tested versions", cluster: &infrav1beta1.MaasCluster{}, wantFloor: "5.0.7", wantApproved: []string{"5.0.10"}},
		{name: "nil cluster uses defaults", cluster: nil, wantFloor: "5.0.7", wantApproved: []string{"5.0.10"}},
		{name: "pack annotation raises the floor", cluster: annotated(map[string]string{lxdMinVersionAnnotation: "5.0.10"}), wantFloor: "5.0.10", wantApproved: []string{"5.0.10"}},
		{name: "pack annotation cannot lower the floor", cluster: annotated(map[string]string{lxdMinVersionAnnotation: "5.0.3"}), wantFloor: "5.0.7", wantApproved: []string{"5.0.10"}},
		{name: "malformed floor is ignored", cluster: annotated(map[string]string{lxdMinVersionAnnotation: "latest"}), wantFloor: "5.0.7", wantApproved: []string{"5.0.10"}},
		{name: "pack annotation adds approved versions", cluster: annotated(map[string]string{lxdApprovedAnnotation: " 5.0.11 ,5.0.10"}), wantFloor: "5.0.7", wantApproved: []string{"5.0.10", "5.0.11"}},
		{name: "malformed approved entries are dropped", cluster: annotated(map[string]string{lxdApprovedAnnotation: "5.0.11;reboot,,5.0.12"}), wantFloor: "5.0.7", wantApproved: []string{"5.0.10", "5.0.12"}},
		{name: "spec wins over the annotations", cluster: withSpec(
			infrav1beta1.LXDConfig{MinVersion: "5.0.9", ApprovedVersions: []string{"5.0.11"}},
			map[string]string{lxdMinVersionAnnotation: "5.0.12", lxdApprovedAnnotation: "5.0.12"},
		), wantFloor: "5.0.9", wantApproved: []string{"5.0.10", "5.0.11"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			p := lxdPolicyFor(tt.cluster)
			g.Expect(p.AutoRefresh).To(BeTrue())
			g.Expect(p.Floor).To(Equal(tt.wantFloor))
			g.Expect(p.Approved).To(Equal(tt.wantApproved))
		})
	}
}

func TestNodeNeedsLXDWork(t *testing.T) {
	ready := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1", Labels: map[string]string{
		lxdInitializedLabel: "true",
		lxdVersionLabel:     "5.0.10",
	}}}
	old := ready.DeepCopy()
	old.Labels[lxdVersionLabel] = "5.0.3"
	unprovenFix := ready.DeepCopy()
	unprovenFix.Labels[lxdVersionLabel] = "5.0.6"
	failed := ready.DeepCopy()
	failed.Labels[lxdUpgradeStatusLabel] = "failed-vm-restarted"
	deferred := old.DeepCopy()
	deferred.Labels[lxdDeferRefreshLabel] = "true"
	blocked := old.DeepCopy()
	blocked.Labels[lxdUpgradeStatusLabel] = lxdBlockedStatus
	fresh := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "new"}}
	optOut := defaultLXDPolicy
	optOut.AutoRefresh = false

	tests := []struct {
		name   string
		node   corev1.Node
		policy lxdVersionPolicy
		want   bool
	}{
		{name: "unlabeled node still needs install", node: fresh, policy: defaultLXDPolicy, want: true},
		{name: "current server needs nothing", node: ready, policy: defaultLXDPolicy, want: false},
		{name: "held old server needs refresh", node: *old, policy: defaultLXDPolicy, want: true},
		{name: "5.0.6 needs refresh because it may lack the fix", node: *unprovenFix, policy: defaultLXDPolicy, want: true},
		{name: "opt-out leaves a held old server alone", node: *old, policy: optOut, want: false},
		{name: "opt-out still installs a new node", node: fresh, policy: optOut, want: true},
		{name: "failed verification keeps the daemonset", node: *failed, policy: defaultLXDPolicy, want: true},
		{name: "repave keeps the daemonset until the host is back", node: *deferred, policy: defaultLXDPolicy, want: true},
		{name: "blocked refresh keeps the daemonset to pick up approvals", node: *blocked, policy: defaultLXDPolicy, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(nodeNeedsLXDWork(tt.node, tt.policy)).To(Equal(tt.want))
		})
	}
}

func TestLXDAutoRefreshEnabled(t *testing.T) {
	f := false
	tr := true
	tests := []struct {
		name    string
		cluster *infrav1beta1.MaasCluster
		want    bool
	}{
		{name: "default is refresh", cluster: &infrav1beta1.MaasCluster{}, want: true},
		{name: "spec false wins", cluster: &infrav1beta1.MaasCluster{Spec: infrav1beta1.MaasClusterSpec{LXDConfig: &infrav1beta1.LXDConfig{AutoRefresh: &f}}}, want: false},
		{name: "spec true wins over the annotation", cluster: &infrav1beta1.MaasCluster{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lxdAutoRefreshAnnotation: "false"}},
			Spec:       infrav1beta1.MaasClusterSpec{LXDConfig: &infrav1beta1.LXDConfig{AutoRefresh: &tr}},
		}, want: true},
		{name: "pack annotation opts out", cluster: &infrav1beta1.MaasCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lxdAutoRefreshAnnotation: "false"}}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdAutoRefreshEnabled(tt.cluster)).To(Equal(tt.want))
		})
	}
}

func TestLXDRefreshSettled(t *testing.T) {
	readyNode := func(version string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			lxdInitializedLabel: "true",
			lxdVersionLabel:     version,
		}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}}}
	}
	optOut := defaultLXDPolicy
	optOut.AutoRefresh = false
	raised := defaultLXDPolicy
	raised.Floor = "5.0.11"

	tests := []struct {
		name   string
		nodes  []corev1.Node
		policy lxdVersionPolicy
		want   bool
	}{
		{name: "patched node lets the daemonset go", nodes: []corev1.Node{readyNode("5.0.10")}, policy: defaultLXDPolicy, want: true},
		{name: "old node keeps the daemonset", nodes: []corev1.Node{readyNode("5.0.3")}, policy: defaultLXDPolicy, want: false},
		{name: "5.0.6 keeps the daemonset", nodes: []corev1.Node{readyNode("5.0.6")}, policy: defaultLXDPolicy, want: false},
		{name: "opt-out ignores the floor", nodes: []corev1.Node{readyNode("5.0.3")}, policy: optOut, want: true},
		{name: "raised floor brings the daemonset back", nodes: []corev1.Node{readyNode("5.0.10")}, policy: raised, want: false},
		{name: "no nodes is not settled", nodes: nil, policy: defaultLXDPolicy, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdRefreshSettled(tt.nodes, tt.policy)).To(Equal(tt.want))
		})
	}
}

func TestLXDRefreshNodeReporting(t *testing.T) {
	failed := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{lxdUpgradeStatusLabel: "failed-refresh"}}}
	blocked := corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "b",
		Labels:      map[string]string{lxdUpgradeStatusLabel: lxdBlockedStatus},
		Annotations: map[string]string{lxdUpgradeDetailAnnot: "5.0/stable offers 5.0.11; approved: 5.0.10"},
	}}
	ok := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "c", Labels: map[string]string{lxdUpgradeStatusLabel: "ok"}}}

	tests := []struct {
		name        string
		nodes       []corev1.Node
		wantFailed  []string
		wantBlocked []string
		wantDetail  string
	}{
		{name: "healthy nodes report nothing", nodes: []corev1.Node{ok}},
		{name: "failed and blocked are reported separately", nodes: []corev1.Node{failed, blocked, ok},
			wantFailed: []string{"a"}, wantBlocked: []string{"b"}, wantDetail: "5.0/stable offers 5.0.11; approved: 5.0.10"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdRefreshFailedNodeNames(tt.nodes)).To(Equal(tt.wantFailed))
			names, detail := lxdRefreshBlockedNodes(tt.nodes)
			g.Expect(names).To(Equal(tt.wantBlocked))
			g.Expect(detail).To(Equal(tt.wantDetail))
		})
	}
}

func TestSyncLXDDeferRefresh(t *testing.T) {
	const ns, clusterName = "hcp", "hcp-1"
	deleting := metav1.NewTime(time.Now())

	machine := func(name, node string, deleted bool) *clusterv1.Machine {
		m := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
		m.Status.NodeRef.Name = node
		if deleted {
			m.DeletionTimestamp = &deleting
			m.Finalizers = []string{"test/hold"}
		}
		return m
	}
	maasMachine := func(name, owner string, deleted bool, finalizers ...string) *infrav1beta1.MaasMachine {
		mm := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  ns,
			Labels:     map[string]string{clusterv1.ClusterNameLabel: clusterName},
			Finalizers: finalizers,
		}}
		if owner != "" {
			mm.OwnerReferences = []metav1.OwnerReference{{APIVersion: clusterv1.GroupVersion.String(), Kind: "Machine", Name: owner, UID: "uid"}}
		}
		if deleted {
			mm.DeletionTimestamp = &deleting
			if len(mm.Finalizers) == 0 {
				mm.Finalizers = []string{"test/hold"}
			}
		}
		return mm
	}
	node := func(name string, deferred bool) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if deferred {
			n.Labels[lxdDeferRefreshLabel] = "true"
		}
		return n
	}

	tests := []struct {
		name      string
		mgmt      []client.Object
		nodes     []client.Object
		wantDefer map[string]bool
	}{
		{
			name: "defers the node of a Machine being deleted under a differently named MaasMachine",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", true),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", false, HostEvacuationFinalizer),
			},
			nodes:     []client.Object{node("node-a", false)},
			wantDefer: map[string]bool{"node-a": true},
		},
		{
			name: "does not defer a healthy host that carries the evacuation finalizer",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", false),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", false, HostEvacuationFinalizer, maintenance.HostEvacuationFinalizer),
			},
			nodes:     []client.Object{node("node-a", false)},
			wantDefer: map[string]bool{"node-a": false},
		},
		{
			name: "defers the node when the MaasMachine itself is being deleted",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", false),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", true),
			},
			nodes:     []client.Object{node("node-a", false)},
			wantDefer: map[string]bool{"node-a": true},
		},
		{
			name: "clears a stale label once the host is no longer being repaved",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", false),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", false),
			},
			nodes:     []client.Object{node("node-a", true)},
			wantDefer: map[string]bool{"node-a": false},
		},
		{
			name: "ignores a MaasMachine without an owning Machine",
			mgmt: []client.Object{
				maasMachine("hcp-1-cp-xyz89", "", true),
			},
			nodes:     []client.Object{node("node-a", false)},
			wantDefer: map[string]bool{"node-a": false},
		},
	}

	scheme := runtime.NewScheme()
	_ = k8sscheme.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = infrav1beta1.AddToScheme(scheme)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.mgmt...).Build()
			remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.nodes...).Build()
			r := &MaasClusterReconciler{Client: mgmt, Log: logr.Discard()}

			g.Expect(r.syncLXDDeferRefresh(context.Background(), ns, clusterName, remote)).To(Succeed())

			for name, want := range tt.wantDefer {
				n := &corev1.Node{}
				g.Expect(remote.Get(context.Background(), client.ObjectKey{Name: name}, n)).To(Succeed())
				g.Expect(n.Labels[lxdDeferRefreshLabel] == "true").To(Equal(want), "node %s", name)
			}
		})
	}
}
