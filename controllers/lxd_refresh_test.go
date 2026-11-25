package controllers

import (
	"context"
	"errors"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
)

var defaultLXDPolicy = lxdVersionPolicy{AutoRefresh: true, Target: lxdRecommendedVersion, RefreshBelow: lxdRecommendedVersion}

func TestLXDVersionBelow(t *testing.T) {
	g := NewGomegaWithT(t)
	tests := []struct {
		name    string
		version string
		target  string
		below   bool
	}{
		{name: "missing version needs refresh", version: "", target: lxdRecommendedVersion, below: true},
		{name: "customer snap is below the target", version: "5.0.3", target: lxdRecommendedVersion, below: true},
		{name: "5.0.7 is still below the recommended target", version: "5.0.7", target: lxdRecommendedVersion, below: true},
		{name: "target version needs nothing", version: "5.0.10", target: lxdRecommendedVersion, below: false},
		{name: "newer than the target is never downgraded", version: "5.0.11", target: lxdRecommendedVersion, below: false},
		{name: "two component version is below", version: "5.0", target: lxdRecommendedVersion, below: true},
		{name: "customer target catches the recommended version", version: "5.0.10", target: "5.0.11", below: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdVersionBelow(tt.version, tt.target)).To(Equal(tt.below))
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
	on := map[string]string{lxdAutoRefreshAnnotation: "true"}
	onWith := func(target string) map[string]string {
		return map[string]string{lxdAutoRefreshAnnotation: "true", lxdTargetAnnotation: target}
	}

	tests := []struct {
		name          string
		cluster       *infrav1beta1.MaasCluster
		wantAuto      bool
		wantTarget    string
		wantBelow     string
		wantRevisions string
		wantError     string
	}{
		{name: "default is off with the recommended target", cluster: &infrav1beta1.MaasCluster{},
			wantTarget: "5.0.10", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912"},
		{name: "nil cluster uses defaults", cluster: nil, wantTarget: "5.0.10", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912"},
		{name: "enabled without a target uses the recommended version", cluster: annotated(on),
			wantAuto: true, wantTarget: "5.0.10", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912"},
		{name: "specific newer version with known revisions", cluster: annotated(onWith(" 5.0.11 ")),
			wantAuto: true, wantTarget: "5.0.11", wantBelow: "5.0.11", wantRevisions: "amd64=41283,arm64=41286"},
		{name: "specific version without known revisions resolves from the channel", cluster: annotated(onWith("5.0.12")),
			wantAuto: true, wantTarget: "5.0.12", wantBelow: "5.0.12"},
		{name: "latest refreshes hosts below the recommended version", cluster: annotated(onWith("Latest")),
			wantAuto: true, wantTarget: "latest", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912"},
		{name: "specific version at the security minimum is allowed", cluster: annotated(onWith("5.0.7")),
			wantAuto: true, wantTarget: "5.0.7", wantBelow: "5.0.7"},
		{name: "version below the security minimum turns the refresh off", cluster: annotated(onWith("5.0.6")),
			wantTarget: "5.0.10", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912", wantError: "below 5.0.7"},
		{name: "malformed version turns the refresh off", cluster: annotated(onWith("5.0.11;reboot")),
			wantTarget: "5.0.10", wantBelow: "5.0.10", wantRevisions: "amd64=40911,arm64=40912", wantError: "not a version"},
		{name: "spec wins over the annotation", cluster: withSpec(
			infrav1beta1.LXDConfig{TargetVersion: "5.0.11"}, onWith("5.0.12"),
		), wantAuto: true, wantTarget: "5.0.11", wantBelow: "5.0.11", wantRevisions: "amd64=41283,arm64=41286"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			p := lxdPolicyFor(tt.cluster)
			g.Expect(p.AutoRefresh).To(Equal(tt.wantAuto))
			g.Expect(p.Target).To(Equal(tt.wantTarget))
			g.Expect(p.RefreshBelow).To(Equal(tt.wantBelow))
			g.Expect(p.targetRevisions()).To(Equal(tt.wantRevisions))
			if tt.wantError == "" {
				g.Expect(p.TargetError).To(BeEmpty())
			} else {
				g.Expect(p.TargetError).To(ContainSubstring(tt.wantError))
			}
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
	newer := ready.DeepCopy()
	newer.Labels[lxdVersionLabel] = "5.0.11"
	unchecked := ready.DeepCopy()
	delete(unchecked.Labels, lxdVersionLabel)
	failed := ready.DeepCopy()
	failed.Labels[lxdUpgradeStatusLabel] = "failed-vm-restarted"
	deferred := old.DeepCopy()
	deferred.Labels[lxdDeferRefreshLabel] = "true"
	blocked := old.DeepCopy()
	blocked.Labels[lxdUpgradeStatusLabel] = lxdBlockedStatus
	fresh := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "new"}}
	optOut := defaultLXDPolicy
	optOut.AutoRefresh = false
	target511 := defaultLXDPolicy
	target511.Target, target511.RefreshBelow = "5.0.11", "5.0.11"

	tests := []struct {
		name   string
		node   corev1.Node
		policy lxdVersionPolicy
		want   bool
	}{
		{name: "unlabeled node still needs install", node: fresh, policy: defaultLXDPolicy, want: true},
		{name: "server at the target needs nothing", node: ready, policy: defaultLXDPolicy, want: false},
		{name: "server newer than the target is not downgraded", node: *newer, policy: defaultLXDPolicy, want: false},
		{name: "held 5.0.3 server needs refresh", node: *old, policy: defaultLXDPolicy, want: true},
		{name: "unchecked server needs a check", node: *unchecked, policy: defaultLXDPolicy, want: true},
		{name: "customer target refreshes a host on the recommended version", node: ready, policy: target511, want: true},
		{name: "autoRefresh off leaves a held old server alone", node: *old, policy: optOut, want: false},
		{name: "autoRefresh off still installs a new node", node: fresh, policy: optOut, want: true},
		{name: "failed verification keeps the daemonset", node: *failed, policy: defaultLXDPolicy, want: true},
		{name: "repave keeps the daemonset until the host is back", node: *deferred, policy: defaultLXDPolicy, want: true},
		{name: "blocked refresh keeps the daemonset to pick up a new target", node: *blocked, policy: defaultLXDPolicy, want: true},
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
		{name: "default is off", cluster: &infrav1beta1.MaasCluster{}, want: false},
		{name: "nil cluster is off", cluster: nil, want: false},
		{name: "pack annotation opts in", cluster: &infrav1beta1.MaasCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lxdAutoRefreshAnnotation: " True "}}}, want: true},
		{name: "pack annotation false stays off", cluster: &infrav1beta1.MaasCluster{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lxdAutoRefreshAnnotation: "false"}}}, want: false},
		{name: "spec true opts in", cluster: &infrav1beta1.MaasCluster{Spec: infrav1beta1.MaasClusterSpec{LXDConfig: &infrav1beta1.LXDConfig{AutoRefresh: &tr}}}, want: true},
		{name: "spec false wins over the annotation", cluster: &infrav1beta1.MaasCluster{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{lxdAutoRefreshAnnotation: "true"}},
			Spec:       infrav1beta1.MaasClusterSpec{LXDConfig: &infrav1beta1.LXDConfig{AutoRefresh: &f}},
		}, want: false},
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
	target511 := defaultLXDPolicy
	target511.Target, target511.RefreshBelow = "5.0.11", "5.0.11"

	tests := []struct {
		name   string
		nodes  []corev1.Node
		policy lxdVersionPolicy
		want   bool
	}{
		{name: "node at the target lets the daemonset go", nodes: []corev1.Node{readyNode("5.0.10")}, policy: defaultLXDPolicy, want: true},
		{name: "old node keeps the daemonset", nodes: []corev1.Node{readyNode("5.0.3")}, policy: defaultLXDPolicy, want: false},
		{name: "5.0.7 keeps the daemonset until the target", nodes: []corev1.Node{readyNode("5.0.7")}, policy: defaultLXDPolicy, want: false},
		{name: "autoRefresh off ignores versions", nodes: []corev1.Node{readyNode("5.0.3")}, policy: optOut, want: true},
		{name: "newer target brings the daemonset back", nodes: []corev1.Node{readyNode("5.0.10")}, policy: target511, want: false},
		{name: "no nodes is not settled", nodes: nil, policy: defaultLXDPolicy, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdRefreshSettled(tt.nodes, tt.policy)).To(Equal(tt.want))
		})
	}
}

func TestLXDHostsBelowSecurityMinimum(t *testing.T) {
	node := func(name string, labels map[string]string) corev1.Node {
		return corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	}
	initialized := func(version string) map[string]string {
		l := map[string]string{lxdInitializedLabel: "true"}
		if version != "" {
			l[lxdVersionLabel] = version
		}
		return l
	}

	tests := []struct {
		name        string
		nodes       []corev1.Node
		wantBelow   []string
		wantUnknown []string
	}{
		{name: "patched hosts report nothing", nodes: []corev1.Node{node("a", initialized("5.0.7")), node("b", initialized("5.0.11"))}},
		{name: "old and unchecked hosts are reported separately", nodes: []corev1.Node{
			node("a", initialized("5.0.3")), node("b", initialized("")), node("c", initialized("5.0.10")),
		}, wantBelow: []string{"a"}, wantUnknown: []string{"b"}},
		{name: "a host still being set up is not reported", nodes: []corev1.Node{node("a", nil)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			below, unknown := lxdHostsBelowSecurityMinimum(tt.nodes)
			g.Expect(below).To(Equal(tt.wantBelow))
			g.Expect(unknown).To(Equal(tt.wantUnknown))
		})
	}
}

func TestLXDRefreshNodeReporting(t *testing.T) {
	failed := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a", Labels: map[string]string{lxdUpgradeStatusLabel: "failed-refresh"}}}
	blocked := corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "b",
		Labels:      map[string]string{lxdUpgradeStatusLabel: lxdBlockedStatus},
		Annotations: map[string]string{lxdUpgradeDetailAnnot: "5.0.12 has no known revision for amd64 and 5.0/stable offers 5.0.11"},
	}}
	storeBlocked := corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "d",
		Labels:      map[string]string{lxdUpgradeStatusLabel: lxdStoreBlockedStatus},
		Annotations: map[string]string{lxdUpgradeDetailAnnot: "cannot read 5.0/stable from the snap store"},
	}}
	ok := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "c", Labels: map[string]string{lxdUpgradeStatusLabel: "ok"}}}
	refreshing := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "e", Labels: map[string]string{lxdUpgradeStatusLabel: "refreshing"}}}

	tests := []struct {
		name             string
		nodes            []corev1.Node
		wantFailed       []string
		wantBlocked      []string
		wantDetail       string
		wantStoreBlocked []string
		wantStoreDetail  string
	}{
		{name: "healthy and refreshing nodes report nothing", nodes: []corev1.Node{ok, refreshing}},
		{name: "failed, target-blocked and store-blocked are reported separately", nodes: []corev1.Node{failed, blocked, storeBlocked, ok},
			wantFailed: []string{"a"}, wantBlocked: []string{"b"}, wantDetail: "5.0.12 has no known revision for amd64 and 5.0/stable offers 5.0.11",
			wantStoreBlocked: []string{"d"}, wantStoreDetail: "cannot read 5.0/stable from the snap store"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(lxdRefreshFailedNodeNames(tt.nodes)).To(Equal(tt.wantFailed))
			names, detail := lxdRefreshBlockedNodes(tt.nodes, lxdBlockedStatus)
			g.Expect(names).To(Equal(tt.wantBlocked))
			g.Expect(detail).To(Equal(tt.wantDetail))
			names, detail = lxdRefreshBlockedNodes(tt.nodes, lxdStoreBlockedStatus)
			g.Expect(names).To(Equal(tt.wantStoreBlocked))
			g.Expect(detail).To(Equal(tt.wantStoreDetail))
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
	withProviderID := func(mm *infrav1beta1.MaasMachine, id string) *infrav1beta1.MaasMachine {
		mm.Spec.ProviderID = &id
		return mm
	}
	node := func(name string, deferred bool) *corev1.Node {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}}}
		if deferred {
			n.Labels[lxdDeferRefreshLabel] = "true"
		}
		return n
	}
	nodeWithProviderID := func(name, id string) *corev1.Node {
		n := node(name, false)
		n.Spec.ProviderID = id
		return n
	}
	failMachineGet := interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*clusterv1.Machine); ok {
			return errors.New("apiserver unavailable")
		}
		return c.Get(ctx, key, obj, opts...)
	}}
	failNodePatch := interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		return errors.New("patch rejected")
	}}

	tests := []struct {
		name        string
		mgmt        []client.Object
		nodes       []client.Object
		mgmtFuncs   interceptor.Funcs
		remoteFuncs interceptor.Funcs
		wantErr     string
		wantDefer   map[string]bool
	}{
		{
			name: "defers the node by providerID when the owning Machine is already gone",
			mgmt: []client.Object{
				withProviderID(maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", true), "maas://zone1/abc123"),
			},
			nodes:     []client.Object{nodeWithProviderID("node-a", "maas://zone1/abc123"), nodeWithProviderID("node-b", "maas://zone1/def456")},
			wantDefer: map[string]bool{"node-a": true, "node-b": false},
		},
		{
			name: "returns the error and keeps labels when the owner Machine cannot be read",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", false),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", false),
			},
			nodes:     []client.Object{node("node-a", true)},
			mgmtFuncs: failMachineGet,
			wantErr:   "failed to get owner Machine of MaasMachine hcp-1-cp-xyz89",
			wantDefer: map[string]bool{"node-a": true},
		},
		{
			name: "returns node patch errors",
			mgmt: []client.Object{
				machine("hcp-1-cp-abc12", "node-a", true),
				maasMachine("hcp-1-cp-xyz89", "hcp-1-cp-abc12", false),
			},
			nodes:       []client.Object{node("node-a", false)},
			remoteFuncs: failNodePatch,
			wantErr:     "node node-a: patch rejected",
			wantDefer:   map[string]bool{"node-a": false},
		},
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
			mgmt := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.mgmt...).WithInterceptorFuncs(tt.mgmtFuncs).Build()
			remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.nodes...).WithInterceptorFuncs(tt.remoteFuncs).Build()
			r := &MaasClusterReconciler{Client: mgmt, Log: logr.Discard()}

			err := r.syncLXDDeferRefresh(context.Background(), ns, clusterName, remote)
			if tt.wantErr != "" {
				g.Expect(err).To(MatchError(ContainSubstring(tt.wantErr)))
			} else {
				g.Expect(err).ToNot(HaveOccurred())
			}

			for name, want := range tt.wantDefer {
				n := &corev1.Node{}
				g.Expect(remote.Get(context.Background(), client.ObjectKey{Name: name}, n)).To(Succeed())
				g.Expect(n.Labels[lxdDeferRefreshLabel] == "true").To(Equal(want), "node %s", name)
			}
		})
	}
}
