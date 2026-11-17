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
	"strings"
	"testing"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
)

func vm(systemID string, tags ...string) maint.Machine {
	return maint.Machine{
		SystemID: systemID,
		Hostname: systemID,
		Tags:     tags,
	}
}

func TestClassifyMaintenanceVM(t *testing.T) {
	tests := []struct {
		name string
		vm   maint.Machine
		want vmClass
	}{
		{name: "control plane", vm: vm("vm-1", maint.TagVMControlPlane, "maas-lxd-wlc-abc"), want: vmClassControlPlane},
		{name: "worker tag is unmanaged until F3", vm: vm("vm-2", "maas-lxd-wlc-worker", "maas-lxd-wlc-abc"), want: vmClassUnmanaged},
		{name: "unmanaged", vm: vm("vm-3", "some-other-tag"), want: vmClassUnmanaged},
		{name: "no tags", vm: vm("vm-4"), want: vmClassUnmanaged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyMaintenanceVM(tt.vm); got != tt.want {
				t.Fatalf("classifyMaintenanceVM() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildHostEvacuationSnapshot(t *testing.T) {
	vms := []maint.Machine{
		vm("vm-6"),
		vm("vm-2", maint.TagVMControlPlane),
		vm("vm-4"),
		vm("vm-1", maint.TagVMControlPlane),
		vm("vm-5", "maas-lxd-wlc-worker"),
		vm("vm-3"),
	}

	snap := buildHostEvacuationSnapshot(vms)
	if snap.Total != 6 {
		t.Fatalf("Total = %d, want 6", snap.Total)
	}
	if got := snap.Counts[vmClassControlPlane]; got != 2 {
		t.Errorf("control-plane count = %d, want 2", got)
	}
	if got := snap.Counts[vmClassUnmanaged]; got != 4 {
		t.Errorf("unmanaged count = %d, want 4", got)
	}
	if len(snap.Names) != 5 {
		t.Errorf("first-five names = %d, want 5", len(snap.Names))
	}
	if !strings.Contains(snap.Names[0], "vm-1 (wlc-cp)") {
		t.Errorf("first name = %q, want vm-1 (wlc-cp)", snap.Names[0])
	}

	// A different input order must produce the same signature.
	reordered := []maint.Machine{vms[5], vms[3], vms[0], vms[4], vms[1], vms[2]}
	if got := buildHostEvacuationSnapshot(reordered).Signature; got != snap.Signature {
		t.Errorf("signature changed with input order: %q vs %q", got, snap.Signature)
	}
}

func TestBuildHostEvacuationMessage(t *testing.T) {
	empty := buildHostEvacuationSnapshot(nil)
	if got := buildHostEvacuationMessage("op-1", empty); !strings.Contains(got, "host is empty") {
		t.Errorf("empty-host message = %q", got)
	}

	snap := buildHostEvacuationSnapshot([]maint.Machine{
		vm("vm-1", maint.TagVMControlPlane),
		vm("vm-2", "maas-lxd-wlc-worker"),
		vm("vm-3"),
	})
	got := buildHostEvacuationMessage("op-1", snap)
	for _, want := range []string{"opID=op-1", "3 VM(s) remaining", "wlc-cp=1", "unmanaged=2", "vm-1 (wlc-cp)"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q does not contain %q", got, want)
		}
	}
	if strings.Contains(got, "wlc-worker") {
		t.Errorf("message %q must not classify a worker before F3", got)
	}
}

func TestEvacuationStateTracker(t *testing.T) {
	var tracker evacuationStateTracker
	key := "default/host"

	if !tracker.changed(key, "a") {
		t.Fatal("first state should be a change")
	}
	if tracker.changed(key, "a") {
		t.Fatal("identical state should not be a change")
	}
	if !tracker.changed(key, "b") {
		t.Fatal("different state should be a change")
	}
	tracker.forget(key)
	if !tracker.changed(key, "b") {
		t.Fatal("state after forget should be a change")
	}
}

func TestReportEvacuationSnapshotDeduplicatesEvents(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "default"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1beta1.MaasMachine{}).
		WithObjects(mm).
		Build()
	events := record.NewFakeRecorder(10)
	r := &HMCMaintenanceReconciler{Client: c, Recorder: events}
	log := logr.Discard()

	first := []maint.Machine{vm("vm-1", maint.TagVMControlPlane)}
	r.reportEvacuationSnapshot(context.Background(), mm, log, "host-1", "op-1", first)
	select {
	case event := <-events.Events:
		if !strings.Contains(event, HostEvacuationBlockedEvent) || !strings.Contains(event, "1 VM(s) remaining") {
			t.Fatalf("unexpected event: %q", event)
		}
	default:
		t.Fatal("expected an EvacuationBlocked event")
	}

	// Same VM set and reason: no new event.
	r.reportEvacuationSnapshot(context.Background(), mm, log, "host-1", "op-1", first)
	select {
	case event := <-events.Events:
		t.Fatalf("unexpected duplicate event: %q", event)
	default:
	}

	// Changed VM set: one new event.
	second := []maint.Machine{vm("vm-1", maint.TagVMControlPlane), vm("vm-2")}
	r.reportEvacuationSnapshot(context.Background(), mm, log, "host-1", "op-1", second)
	select {
	case event := <-events.Events:
		if !strings.Contains(event, "2 VM(s) remaining") {
			t.Fatalf("unexpected changed-state event: %q", event)
		}
	default:
		t.Fatal("expected an event for the changed VM set")
	}
}

func TestSetHostEvacuationConditionPersistsStatus(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "host", Namespace: "default"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1beta1.MaasMachine{}).
		WithObjects(mm).
		Build()

	err := setHostEvacuationCondition(context.Background(), c, mm, corev1.ConditionFalse,
		infrav1beta1.HostEvacuationVMsRemainingReason, clusterv1.ConditionSeverityWarning,
		"opID=op-1; 2 VM(s) remaining")
	if err != nil {
		t.Fatalf("set condition: %v", err)
	}

	got := &infrav1beta1.MaasMachine{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "host"}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	cond := conditions.Get(got, infrav1beta1.HostEvacuationReadyCondition)
	if cond == nil {
		t.Fatal("HostEvacuationReady condition not persisted")
	}
	if cond.Status != corev1.ConditionFalse {
		t.Errorf("Status = %q, want False", cond.Status)
	}
	if cond.Reason != infrav1beta1.HostEvacuationVMsRemainingReason {
		t.Errorf("Reason = %q, want %q", cond.Reason, infrav1beta1.HostEvacuationVMsRemainingReason)
	}
	if !strings.Contains(cond.Message, "2 VM(s) remaining") {
		t.Errorf("Message = %q", cond.Message)
	}

	if err := setHostEvacuationCondition(context.Background(), c, got, corev1.ConditionTrue,
		infrav1beta1.HostEvacuationCompleteReason, clusterv1.ConditionSeverityNone, "complete"); err != nil {
		t.Fatalf("set true condition: %v", err)
	}
	got = &infrav1beta1.MaasMachine{}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "host"}, got); err != nil {
		t.Fatalf("get machine after true: %v", err)
	}
	cond = conditions.Get(got, infrav1beta1.HostEvacuationReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionTrue || cond.Reason != infrav1beta1.HostEvacuationCompleteReason {
		t.Fatalf("true condition = %+v", cond)
	}
}
