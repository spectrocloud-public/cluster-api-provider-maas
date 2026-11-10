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

package controllers

import (
	"context"
	"errors"
	"testing"

	. "github.com/onsi/gomega"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2/klogr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

type fakeClusterCache struct {
	clustercache.ClusterCache
	workloadClient client.Client
	err            error
}

func (f fakeClusterCache) GetClient(context.Context, client.ObjectKey) (client.Client, error) {
	return f.workloadClient, f.err
}

func (f fakeClusterCache) GetUncachedClient(context.Context, client.ObjectKey) (client.Client, error) {
	return f.workloadClient, f.err
}

func TestEraseModeForRelease(t *testing.T) {
	tests := []struct {
		name         string
		clusterScope *scope.ClusterScope
		expected     infrav1beta1.EraseMode
	}{
		{
			name:         "nil cluster scope defaults to none",
			clusterScope: nil,
			expected:     infrav1beta1.EraseModeNone,
		},
		{
			name:         "missing MaasCluster defaults to none",
			clusterScope: &scope.ClusterScope{},
			expected:     infrav1beta1.EraseModeNone,
		},
		{
			name:         "empty value defaults to full",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{}},
			expected:     infrav1beta1.EraseModeFull,
		},
		{
			name: "quick",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{
				Spec: infrav1beta1.MaasClusterSpec{EraseDisksOnRelease: infrav1beta1.EraseModeQuick},
			}},
			expected: infrav1beta1.EraseModeQuick,
		},
		{
			name: "full",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{
				Spec: infrav1beta1.MaasClusterSpec{EraseDisksOnRelease: infrav1beta1.EraseModeFull},
			}},
			expected: infrav1beta1.EraseModeFull,
		},
		{
			name: "secure",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{
				Spec: infrav1beta1.MaasClusterSpec{EraseDisksOnRelease: infrav1beta1.EraseModeSecure},
			}},
			expected: infrav1beta1.EraseModeSecure,
		},
		{
			name: "none",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{
				Spec: infrav1beta1.MaasClusterSpec{EraseDisksOnRelease: infrav1beta1.EraseModeNone},
			}},
			expected: infrav1beta1.EraseModeNone,
		},
		{
			name: "unrecognized value defaults to none",
			clusterScope: &scope.ClusterScope{MaasCluster: &infrav1beta1.MaasCluster{
				Spec: infrav1beta1.MaasClusterSpec{EraseDisksOnRelease: "unexpected"},
			}},
			expected: infrav1beta1.EraseModeNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(eraseModeForRelease(tt.clusterScope)).To(Equal(tt.expected))
		})
	}
}

func TestMachineNodeName(t *testing.T) {
	tests := []struct {
		name     string
		machine  *clusterv1.Machine
		expected string
	}{
		{
			name:     "machine without a node ref",
			machine:  &clusterv1.Machine{},
			expected: "",
		},
		{
			name: "machine with a node ref",
			machine: &clusterv1.Machine{
				Status: clusterv1.MachineStatus{
					NodeRef: clusterv1.MachineNodeReference{Name: "node-a"},
				},
			},
			expected: "node-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			machineScope := &scope.MachineScope{Machine: tt.machine}
			g.Expect(machineNodeName(machineScope)).To(Equal(tt.expected))
		})
	}

	g := NewWithT(t)
	g.Expect(machineNodeName(nil)).To(BeEmpty())
	g.Expect(machineNodeName(&scope.MachineScope{})).To(BeEmpty())
}

func TestCountAttachedVolumes(t *testing.T) {
	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(storagev1.AddToScheme(scheme)).To(Succeed())

	attachments := []client.Object{
		&storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: "a-node-a"},
			Spec:       storagev1.VolumeAttachmentSpec{NodeName: "node-a"},
		},
		&storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: "b-node-a"},
			Spec:       storagev1.VolumeAttachmentSpec{NodeName: "node-a"},
		},
		&storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: "c-node-b"},
			Spec:       storagev1.VolumeAttachmentSpec{NodeName: "node-b"},
		},
	}

	tests := []struct {
		name        string
		client      client.Client
		nodeName    string
		expected    int
		expectError bool
	}{
		{
			name:     "counts only attachments for the requested node",
			client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(attachments...).Build(),
			nodeName: "node-a",
			expected: 2,
		},
		{
			name:     "returns zero for a node without attachments",
			client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(attachments...).Build(),
			nodeName: "node-c",
			expected: 0,
		},
		{
			name:     "returns zero for an empty node name",
			client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(attachments...).Build(),
			nodeName: "",
			expected: 0,
		},
		{
			name:        "fails closed when the workload client cannot list attachments",
			client:      fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build(),
			nodeName:    "node-a",
			expected:    0,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)
			count, err := countAttachedVolumes(context.Background(), tt.client, tt.nodeName)
			if tt.expectError {
				g.Expect(err).To(HaveOccurred())
				return
			}
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(count).To(Equal(tt.expected))
		})
	}
}

func TestHoldReleaseForAttachedVolumes(t *testing.T) {
	deletionTime := metav1.Now()

	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(infrav1beta1.AddToScheme(scheme)).To(Succeed())
	g.Expect(clusterv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(storagev1.AddToScheme(scheme)).To(Succeed())

	attachment := func(name, nodeName string) *storagev1.VolumeAttachment {
		return &storagev1.VolumeAttachment{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec:       storagev1.VolumeAttachmentSpec{NodeName: nodeName},
		}
	}
	attachedClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(attachment("va-a", "node-a")).Build()
	emptyClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	tests := []struct {
		name                 string
		cluster              *clusterv1.Cluster
		nodeName             string
		force                bool
		workloadClient       client.Client
		trackerErr           error
		eraseRequested       bool
		preexistingCondition bool
		expectedHeld         bool
		expectedReason       string
	}{
		{
			name:           "not held when the machine has no node ref",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			workloadClient: attachedClient,
			expectedHeld:   false,
		},
		{
			name:           "not held during cluster deletion",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", DeletionTimestamp: &deletionTime}},
			nodeName:       "node-a",
			workloadClient: attachedClient,
			expectedHeld:   false,
		},
		{
			name:           "held during cluster deletion when the release requests an erase",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", DeletionTimestamp: &deletionTime}},
			nodeName:       "node-a",
			workloadClient: attachedClient,
			eraseRequested: true,
			expectedHeld:   true,
			expectedReason: infrav1beta1.WaitingForVolumeDetachReason,
		},
		{
			name:           "not held during cluster deletion with erase and the skip annotation",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c", DeletionTimestamp: &deletionTime}},
			nodeName:       "node-a",
			force:          true,
			workloadClient: attachedClient,
			eraseRequested: true,
			expectedHeld:   false,
		},
		{
			name:           "not held when the skip guard annotation is set",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			nodeName:       "node-a",
			force:          true,
			workloadClient: attachedClient,
			expectedHeld:   false,
		},
		{
			name:           "held while an attachment exists",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			nodeName:       "node-a",
			workloadClient: attachedClient,
			expectedHeld:   true,
			expectedReason: infrav1beta1.WaitingForVolumeDetachReason,
		},
		{
			name:           "not held when no attachment exists",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			nodeName:       "node-a",
			workloadClient: emptyClient,
			expectedHeld:   false,
		},
		{
			name:                 "clears a pending condition when no attachment exists",
			cluster:              &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			nodeName:             "node-a",
			workloadClient:       emptyClient,
			preexistingCondition: true,
			expectedHeld:         false,
		},
		{
			name:           "held when the workload client is unavailable",
			cluster:        &clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "c"}},
			nodeName:       "node-a",
			trackerErr:     errors.New("cluster cache miss"),
			expectedHeld:   true,
			expectedReason: infrav1beta1.VolumeDetachCheckFailedReason,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			// GetWorkloadClusterClient goes through the tracker, which is unexported.
			clusterScope, err := scope.NewClusterScope(scope.ClusterScopeParams{
				Client:      fake.NewClientBuilder().WithScheme(scheme).Build(),
				Logger:      klogr.New(),
				Cluster:     tt.cluster,
				MaasCluster: &infrav1beta1.MaasCluster{ObjectMeta: metav1.ObjectMeta{Name: "mc"}},
				Tracker:     fakeClusterCache{workloadClient: tt.workloadClient, err: tt.trackerErr},
			})
			g.Expect(err).ToNot(HaveOccurred())

			maasMachine := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{Name: "mm"}}
			if tt.force {
				maasMachine.Annotations = map[string]string{SkipVolumeDetachGuardAnnotation: "true"}
			}
			if tt.preexistingCondition {
				conditions.MarkFalse(maasMachine, infrav1beta1.VolumeDetachPendingCondition, infrav1beta1.WaitingForVolumeDetachReason, clusterv1.ConditionSeverityInfo, "previous reconcile")
			}
			machine := &clusterv1.Machine{}
			if tt.nodeName != "" {
				machine.Status.NodeRef = clusterv1.MachineNodeReference{Name: tt.nodeName}
			}
			machineScope := &scope.MachineScope{
				Logger:      klogr.New(),
				Machine:     machine,
				MaasMachine: maasMachine,
			}

			reconciler := &MaasMachineReconciler{Recorder: record.NewFakeRecorder(10)}
			held, res := reconciler.holdReleaseForAttachedVolumes(context.Background(), machineScope, clusterScope, tt.eraseRequested)
			g.Expect(held).To(Equal(tt.expectedHeld))
			if tt.expectedHeld {
				g.Expect(res.RequeueAfter).To(Equal(volumeDetachRequeueInterval))
			}
			condition := conditions.Get(maasMachine, infrav1beta1.VolumeDetachPendingCondition)
			if tt.expectedReason == "" {
				g.Expect(condition).To(BeNil())
			} else {
				g.Expect(condition).ToNot(BeNil())
				g.Expect(condition.Reason).To(Equal(tt.expectedReason))
			}
		})
	}
}
