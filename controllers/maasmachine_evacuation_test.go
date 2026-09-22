package controllers

import (
	"context"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/klogr"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

// evacuation finalizer test fixtures. Host machines on LXD-enabled (HCP) clusters
// carry HostEvacuationFinalizer; HMC (--cluster-role=hcp) is its only remover, so
// the finalizer must only be added — and must be dropped — when HMC is deployed
// in the same process (PCP-7660).

func evacuationTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = infrav1beta1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = k8sscheme.AddToScheme(scheme)
	return scheme
}

// evacuationFinalizer=true pre-sets HostEvacuationFinalizer for the delete-path
// tests; the normal-reconcile tests must start without it so the add-guard is
// what puts it there (or doesn't).
func evacuationTestMachine(name string, deleting bool, parent *string, evacuationFinalizer bool) *infrav1beta1.MaasMachine {
	finalizers := []string{infrav1beta1.MachineFinalizer}
	if evacuationFinalizer {
		finalizers = append(finalizers, HostEvacuationFinalizer)
	}
	m := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  "test-ns",
			Finalizers: finalizers,
		},
		Spec: infrav1beta1.MaasMachineSpec{
			Parent:   parent,
			SystemID: ptr.To("abc123"),
		},
	}
	if deleting {
		now := metav1.Now()
		m.DeletionTimestamp = &now
	}
	return m
}

func lxdEnabledCluster(enabled bool) *infrav1beta1.MaasCluster {
	return &infrav1beta1.MaasCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test-ns"},
		Spec: infrav1beta1.MaasClusterSpec{
			LXDConfig: &infrav1beta1.LXDConfig{Enabled: &enabled},
		},
	}
}

func newEvacuationTestScopes(t *testing.T, maasCluster *infrav1beta1.MaasCluster, maasMachine *infrav1beta1.MaasMachine) (*scope.ClusterScope, *scope.MachineScope, client.Client) {
	t.Helper()
	scheme := evacuationTestScheme(t)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(maasCluster, maasMachine).Build()

	cluster := &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "test-cluster", Namespace: "test-ns"}}
	cs, err := scope.NewClusterScope(scope.ClusterScopeParams{
		Client:      fakeClient,
		Logger:      klogr.New(),
		Cluster:     cluster,
		MaasCluster: maasCluster,
	})
	if err != nil {
		t.Fatalf("NewClusterScope: %v", err)
	}

	ms, err := scope.NewMachineScope(scope.MachineScopeParams{
		Logger:       klogr.New(),
		Client:       fakeClient,
		Cluster:      cluster,
		ClusterScope: cs,
		Machine:      &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Namespace: "test-ns"}},
		MaasMachine:  maasMachine,
	})
	if err != nil {
		t.Fatalf("NewMachineScope: %v", err)
	}
	return cs, ms, fakeClient
}

func TestReconcileNormalEvacuationFinalizer(t *testing.T) {
	vmParent := "host-abc123"

	tests := []struct {
		name          string
		hmcEnabled    bool
		parent        *string
		lxdEnabled    bool
		wantFinalizer bool
	}{
		{
			name:          "HMC deployed: adds evacuation finalizer to host machine",
			hmcEnabled:    true,
			lxdEnabled:    true,
			wantFinalizer: true,
		},
		{
			name:          "HMC missing: must not add an ownerless evacuation finalizer (PCP-7660)",
			hmcEnabled:    false,
			lxdEnabled:    true,
			wantFinalizer: false,
		},
		{
			name:          "HMC deployed but machine is a VM: no evacuation finalizer",
			hmcEnabled:    true,
			parent:        &vmParent,
			lxdEnabled:    true,
			wantFinalizer: false,
		},
		{
			name:          "HMC deployed but LXD disabled: no evacuation finalizer",
			hmcEnabled:    true,
			lxdEnabled:    false,
			wantFinalizer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			machine := evacuationTestMachine("test-maasmachine", false, tt.parent, false)
			_, ms, _ := newEvacuationTestScopes(t, lxdEnabledCluster(tt.lxdEnabled), machine)

			r := &MaasMachineReconciler{
				Log:        klogr.New(),
				HMCEnabled: tt.hmcEnabled,
			}
			_, err := r.reconcileNormal(context.Background(), ms, ms.ClusterScope)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(controllerutil.ContainsFinalizer(ms.MaasMachine, HostEvacuationFinalizer)).To(Equal(tt.wantFinalizer))
		})
	}
}

func TestReconcileDeleteDropsOrphanedEvacuationFinalizer(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("MAAS_ENDPOINT", "http://localhost:5240/MAAS")
	t.Setenv("MAAS_API_KEY", "test:key:secret")

	machine := evacuationTestMachine("test-maasmachine", true, nil, true)
	_, ms, fakeClient := newEvacuationTestScopes(t, lxdEnabledCluster(true), machine)

	r := &MaasMachineReconciler{
		Log:        klogr.New(),
		Recorder:   record.NewFakeRecorder(8),
		HMCEnabled: false, // HMC not deployed in this process
	}
	_, err := r.reconcileDelete(context.Background(), ms, ms.ClusterScope)
	g.Expect(err).ToNot(HaveOccurred())
	// reconcileDelete only drops the finalizer in the scope's patch set; the
	// deferred Close() in Reconcile is what persists it in production.
	g.Expect(ms.Close()).To(Succeed())

	updated := &infrav1beta1.MaasMachine{}
	g.Expect(fakeClient.Get(context.Background(), client.ObjectKeyFromObject(machine), updated)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(updated, HostEvacuationFinalizer)).To(BeFalse(),
		"deletion must not stay blocked on a finalizer whose owner controller is not deployed")
	g.Expect(controllerutil.ContainsFinalizer(updated, infrav1beta1.MachineFinalizer)).To(BeTrue(),
		"the regular machine finalizer must be untouched so the normal release path still runs")
}

func TestReconcileDeleteKeepsFinalizerWhenHMCEnabled(t *testing.T) {
	g := NewGomegaWithT(t)
	t.Setenv("MAAS_ENDPOINT", "http://localhost:5240/MAAS")
	t.Setenv("MAAS_API_KEY", "test:key:secret")

	machine := evacuationTestMachine("test-maasmachine", true, nil, true)
	_, ms, fakeClient := newEvacuationTestScopes(t, lxdEnabledCluster(true), machine)

	r := &MaasMachineReconciler{
		Log:        klogr.New(),
		Recorder:   record.NewFakeRecorder(8),
		HMCEnabled: true,
	}
	result, err := r.reconcileDelete(context.Background(), ms, ms.ClusterScope)
	g.Expect(err).ToNot(HaveOccurred())
	g.Expect(result.RequeueAfter).To(Equal(10 * time.Second))

	g.Expect(ms.Close()).To(Succeed())
	updated := &infrav1beta1.MaasMachine{}
	g.Expect(fakeClient.Get(context.Background(), client.ObjectKeyFromObject(machine), updated)).To(Succeed())
	g.Expect(controllerutil.ContainsFinalizer(updated, HostEvacuationFinalizer)).To(BeTrue(),
		"when HMC is deployed the finalizer must stay until evacuation completes")
}
