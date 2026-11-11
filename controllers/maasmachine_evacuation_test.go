package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

const testEvacuationOpID = "bada3399-cb6f-4272-b5f5-a055bb0eed81"

func testLXDClusterScope(enabled bool) *scope.ClusterScope {
	return &scope.ClusterScope{
		MaasCluster: &infrav1beta1.MaasCluster{
			Spec: infrav1beta1.MaasClusterSpec{
				LXDConfig: &infrav1beta1.LXDConfig{Enabled: ptr.To(enabled)},
			},
		},
	}
}

func testOwnerMachine(deleting bool) *clusterv1.Machine {
	machine := &clusterv1.Machine{}
	if deleting {
		now := metav1.NewTime(time.Now())
		machine.DeletionTimestamp = &now
	}
	return machine
}

func testMaasMachine(parent *string, finalizers []string, annotations map[string]string) *infrav1beta1.MaasMachine {
	return &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{
			Finalizers:  finalizers,
			Annotations: annotations,
		},
		Spec: infrav1beta1.MaasMachineSpec{Parent: parent},
	}
}

// TestShouldAddEvacuationFinalizer is the regression gate for the PCP-7993 deadlock. A host
// MaasMachine whose evacuation already completed must never get the finalizer back: HMC has
// recorded completion and will not run another evacuation, so a re-added finalizer would have
// no remover while the machine controller only requeues. A host with no completion record
// still gets the finalizer, including while the owner Machine is draining.
func TestShouldAddEvacuationFinalizer(t *testing.T) {
	vmParent := "host-system-id"

	tests := []struct {
		name         string
		maasMachine  *infrav1beta1.MaasMachine
		clusterScope *scope.ClusterScope
		want         bool
	}{
		{
			name:         "adds before evacuation completed",
			maasMachine:  testMaasMachine(nil, nil, nil),
			clusterScope: testLXDClusterScope(true),
			want:         true,
		},
		{
			name: "does not re-add after evacuation completed",
			maasMachine: testMaasMachine(nil, nil, map[string]string{
				EvacuationCompletedAnnotation: testEvacuationOpID,
			}),
			clusterScope: testLXDClusterScope(true),
			want:         false,
		},
		{
			name:         "does not add for VM machines",
			maasMachine:  testMaasMachine(&vmParent, nil, nil),
			clusterScope: testLXDClusterScope(true),
			want:         false,
		},
		{
			name:         "does not add when LXD is disabled",
			maasMachine:  testMaasMachine(nil, nil, nil),
			clusterScope: testLXDClusterScope(false),
			want:         false,
		},
		{
			name: "does not duplicate an existing finalizer",
			maasMachine: testMaasMachine(nil, []string{
				infrav1beta1.MachineFinalizer,
				HostEvacuationFinalizer,
			}, nil),
			clusterScope: testLXDClusterScope(true),
			want:         false,
		},
		{
			name:         "does not add without a cluster scope",
			maasMachine:  testMaasMachine(nil, nil, nil),
			clusterScope: nil,
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			g.Expect(shouldAddEvacuationFinalizer(tt.maasMachine, tt.clusterScope)).To(Equal(tt.want))
		})
	}
}

// TestReconcileNormalEvacuationFinalizer proves reconcileNormal honours the guard and keeps
// the safety net while the owner Machine is draining.
func TestReconcileNormalEvacuationFinalizer(t *testing.T) {
	tests := []struct {
		name            string
		annotations     map[string]string
		machineDeleting bool
		expectFinalizer bool
	}{
		{
			name:            "adds before deletion starts",
			expectFinalizer: true,
		},
		{
			name:            "adds while the owner machine is draining when evacuation is not completed",
			machineDeleting: true,
			expectFinalizer: true,
		},
		{
			name: "does not re-add after evacuation completed",
			annotations: map[string]string{
				EvacuationCompletedAnnotation: testEvacuationOpID,
			},
			expectFinalizer: false,
		},
		{
			name: "does not re-add after evacuation completed while the owner machine is draining",
			annotations: map[string]string{
				EvacuationCompletedAnnotation: testEvacuationOpID,
			},
			machineDeleting: true,
			expectFinalizer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			clusterScope := testLXDClusterScope(true)
			maasMachine := testMaasMachine(nil, []string{infrav1beta1.MachineFinalizer}, tt.annotations)
			machineScope := &scope.MachineScope{
				Logger:       logr.Discard(),
				Cluster:      &clusterv1.Cluster{},
				ClusterScope: clusterScope,
				Machine:      testOwnerMachine(tt.machineDeleting),
				MaasMachine:  maasMachine,
			}

			reconciler := &MaasMachineReconciler{}
			_, err := reconciler.reconcileNormal(context.Background(), machineScope, clusterScope)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(controllerutil.ContainsFinalizer(maasMachine, HostEvacuationFinalizer)).To(Equal(tt.expectFinalizer))
		})
	}
}

// TestReconcileMaasMachineRemovesReAddedFinalizer covers HMC's side of the contract: when HMC
// has recorded a completed evacuation, it owns removal of its finalizer even if the machine
// controller re-added it independently.
func TestReconcileMaasMachineRemovesReAddedFinalizer(t *testing.T) {
	tests := []struct {
		name            string
		finalizers      []string
		expectFinalizer bool
	}{
		{
			name: "removes a finalizer re-added after evacuation completed",
			finalizers: []string{
				infrav1beta1.MachineFinalizer,
				HostEvacuationFinalizer,
			},
			expectFinalizer: false,
		},
		{
			name:            "leaves the machine alone when the finalizer is already gone",
			finalizers:      []string{infrav1beta1.MachineFinalizer},
			expectFinalizer: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGomegaWithT(t)
			scheme := runtime.NewScheme()
			g.Expect(infrav1beta1.AddToScheme(scheme)).To(Succeed())
			g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())

			now := metav1.NewTime(time.Now())
			namespace := "cluster-test"
			owner := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name:              "host-machine",
					Namespace:         namespace,
					DeletionTimestamp: &now,
					Finalizers:        []string{"machine.cluster.x-k8s.io"},
				},
			}
			maasMachine := testMaasMachine(nil, tt.finalizers, map[string]string{
				EvacuationCompletedAnnotation: testEvacuationOpID,
			})
			maasMachine.Name = "host-maasmachine"
			maasMachine.Namespace = namespace
			maasMachine.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: clusterv1.GroupVersion.String(),
				Kind:       "Machine",
				Name:       owner.Name,
			}}

			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, maasMachine).Build()
			var stored infrav1beta1.MaasMachine
			g.Expect(k8sClient.Get(context.Background(), types.NamespacedName{
				Name:      maasMachine.Name,
				Namespace: namespace,
			}, &stored)).To(Succeed())

			reconciler := &HMCMaintenanceReconciler{Client: k8sClient, Log: logr.Discard()}
			_, err := reconciler.reconcileMaasMachine(context.Background(), &stored)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(controllerutil.ContainsFinalizer(&stored, HostEvacuationFinalizer)).To(Equal(tt.expectFinalizer))
		})
	}
}
