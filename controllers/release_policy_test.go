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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maasmachine "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/machine"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

type fakeReleaseService struct {
	parentID   string
	parentTags []string
	parentErr  error
	releaseErr error
	recordErr  error
	dynamicErr error

	releaseCalled bool
	recordCalled  bool
	dynamicCalled bool
	lastOpts      maasmachine.ReleaseOptions
}

func (f *fakeReleaseService) ParentHostTags(context.Context, string, time.Duration) (string, []string, error) {
	return f.parentID, f.parentTags, f.parentErr
}

func (f *fakeReleaseService) ReleaseMachine(_ context.Context, _ string, opts maasmachine.ReleaseOptions) error {
	f.releaseCalled = true
	f.lastOpts = opts
	return f.releaseErr
}

func (f *fakeReleaseService) DeleteMachineRecord(context.Context, string, time.Duration) error {
	f.recordCalled = true
	return f.recordErr
}

func (f *fakeReleaseService) DeleteDynamicLXDVM(context.Context, string, time.Duration) error {
	f.dynamicCalled = true
	return f.dynamicErr
}

func TestClassifyReleaseError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "not found", err: errors.New("status: 404"), want: releaseClassNotFound},
		{name: "vm host removal", err: errors.New("status: 400: VM hosts must be removed first"), want: releaseClassVMHostRemoval},
		{name: "conflict", err: errors.New("status: 409 conflict"), want: releaseClassConflict},
		{name: "deadline", err: errors.New("context deadline exceeded"), want: releaseClassTimeout},
		{name: "connect", err: errors.New("dial tcp: connect: connection refused"), want: releaseClassTimeout},
		{name: "server", err: errors.New("status: 500 UNHANDLED"), want: releaseClassServerError},
		{name: "unknown", err: errors.New("boom"), want: releaseClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyReleaseError(tt.err); got != tt.want {
				t.Fatalf("classifyReleaseError() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReleaseBackoff(t *testing.T) {
	cfg := maint.ReleaseConfig{
		InitialBackoff: 30 * time.Second,
		MaxBackoff:     5 * time.Minute,
		RetryBudget:    15 * time.Minute,
	}
	tests := []struct {
		count int
		want  time.Duration
	}{
		{count: 1, want: 30 * time.Second},
		{count: 2, want: time.Minute},
		{count: 3, want: 2 * time.Minute},
		{count: 4, want: 4 * time.Minute},
		{count: 5, want: 5 * time.Minute},
		{count: 20, want: 5 * time.Minute},
	}
	for _, tt := range tests {
		got := releaseBackoff(releaseAttempts{Count: tt.count}, cfg)
		if got != tt.want {
			t.Errorf("releaseBackoff(count=%d) = %s, want %s", tt.count, got, tt.want)
		}
	}

	expired := releaseAttempts{
		Count:        1,
		FirstFailure: time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339),
	}
	if got := releaseBackoff(expired, cfg); got != cfg.MaxBackoff {
		t.Errorf("releaseBackoff after budget = %s, want %s", got, cfg.MaxBackoff)
	}
}

func TestReleaseAttemptsRecordAndClear(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(mm).Build()
	r := &MaasMachineReconciler{Client: c, Log: logr.Discard()}
	ctx := context.Background()

	r.recordReleaseAttempt(ctx, mm, releaseClassTimeout)
	r.recordReleaseAttempt(ctx, mm, releaseClassServerError)

	got := &infrav1beta1.MaasMachine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm"}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	raw := got.Annotations[ReleaseAttemptsAnnotation]
	if raw == "" {
		t.Fatal("release-attempts annotation not set")
	}
	var state releaseAttempts
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		t.Fatalf("unmarshal attempts: %v", err)
	}
	if state.Count != 2 {
		t.Errorf("Count = %d, want 2", state.Count)
	}
	if state.Class != releaseClassServerError {
		t.Errorf("Class = %q, want %q", state.Class, releaseClassServerError)
	}
	if state.FirstFailure == "" {
		t.Error("FirstFailure is empty")
	}

	r.clearReleaseAttempts(ctx, got)
	got = &infrav1beta1.MaasMachine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm"}, got); err != nil {
		t.Fatalf("get machine after clear: %v", err)
	}
	if _, ok := got.Annotations[ReleaseAttemptsAnnotation]; ok {
		t.Error("release-attempts annotation was not cleared")
	}
}

func TestSetReleaseReadyCondition(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1beta1.MaasMachine{}).
		WithObjects(mm).
		Build()
	r := &MaasMachineReconciler{Client: c, Log: logr.Discard()}
	ctx := context.Background()

	r.setReleaseReady(ctx, mm, corev1.ConditionFalse, infrav1beta1.ReleaseFailedReason,
		clusterv1.ConditionSeverityWarning, "status: 500")

	got := &infrav1beta1.MaasMachine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "vm"}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	cond := conditions.Get(got, infrav1beta1.ReleaseReadyCondition)
	if cond == nil {
		t.Fatal("ReleaseReady condition not persisted")
	}
	if cond.Status != corev1.ConditionFalse || cond.Reason != infrav1beta1.ReleaseFailedReason {
		t.Errorf("condition = %+v", cond)
	}
	if !strings.Contains(cond.Message, "status: 500") {
		t.Errorf("Message = %q", cond.Message)
	}
}

func TestGuestNames(t *testing.T) {
	if got := guestNames(nil); got != "none" {
		t.Errorf("guestNames(nil) = %q", got)
	}
	got := guestNames([]guestVM{{SystemID: "abc"}, {SystemID: "def", Name: "vm-def"}})
	if got != "abc, vm-def" {
		t.Errorf("guestNames() = %q", got)
	}
}

func newReleaseTestReconciler(t *testing.T, mm *infrav1beta1.MaasMachine, autoDead bool) (*MaasMachineReconciler, *infrav1beta1.MaasMachine) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := infrav1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&infrav1beta1.MaasMachine{}).
		WithObjects(mm).
		Build()
	return &MaasMachineReconciler{Client: c, Log: logr.Discard(), AutoDeadHostRecovery: autoDead}, mm
}

func newReleaseMachineScope(mm *infrav1beta1.MaasMachine) *scope.MachineScope {
	return &scope.MachineScope{
		Logger:      logr.Discard(),
		MaasMachine: mm,
		Machine:     &clusterv1.Machine{},
	}
}

func TestReleaseMachineWithPolicyDeadParentDeletesRecord(t *testing.T) {
	mm := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"}}
	r, _ := newReleaseTestReconciler(t, mm, true)
	svc := &fakeReleaseService{parentID: "host-1", parentTags: []string{maint.TagHostDead}}

	res, err := r.releaseMachineWithPolicy(context.Background(), newReleaseMachineScope(mm), nil, svc, &infrav1beta1.Machine{ID: "vm-1"})
	if err != nil || !res.IsZero() {
		t.Fatalf("result = %+v, err = %v; want no requeue", res, err)
	}
	if !svc.recordCalled {
		t.Fatal("expected MAAS record delete")
	}
	if svc.releaseCalled {
		t.Fatal("release must not be called for a host-dead parent")
	}
	got := &infrav1beta1.MaasMachine{}
	if err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "vm"}, got); err != nil {
		t.Fatalf("get machine: %v", err)
	}
	cond := conditions.Get(got, infrav1beta1.ReleaseReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionTrue || cond.Reason != infrav1beta1.ReleaseCompleteReason {
		t.Fatalf("condition = %+v", cond)
	}
}

func TestReleaseMachineWithPolicyNotFoundIsReleased(t *testing.T) {
	mm := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"}}
	r, _ := newReleaseTestReconciler(t, mm, false)
	svc := &fakeReleaseService{releaseErr: errors.New("status: 404")}

	res, err := r.releaseMachineWithPolicy(context.Background(), newReleaseMachineScope(mm), nil, svc, &infrav1beta1.Machine{ID: "vm-1"})
	if err != nil || !res.IsZero() {
		t.Fatalf("result = %+v, err = %v; want no requeue", res, err)
	}
	got := &infrav1beta1.MaasMachine{}
	_ = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "vm"}, got)
	cond := conditions.Get(got, infrav1beta1.ReleaseReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionTrue {
		t.Fatalf("condition = %+v", cond)
	}
}

func TestReleaseMachineWithPolicyServerErrorRetries(t *testing.T) {
	mm := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"}}
	r, _ := newReleaseTestReconciler(t, mm, false)
	svc := &fakeReleaseService{releaseErr: errors.New("status: 500 UNHANDLED")}

	res, err := r.releaseMachineWithPolicy(context.Background(), newReleaseMachineScope(mm), nil, svc, &infrav1beta1.Machine{ID: "vm-1"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("RequeueAfter = %s, want 30s", res.RequeueAfter)
	}
	got := &infrav1beta1.MaasMachine{}
	_ = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "vm"}, got)
	cond := conditions.Get(got, infrav1beta1.ReleaseReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionFalse || cond.Reason != infrav1beta1.ReleaseFailedReason {
		t.Fatalf("condition = %+v", cond)
	}
	if got.Annotations[ReleaseAttemptsAnnotation] == "" {
		t.Fatal("release-attempts annotation not set")
	}
}

func TestReleaseMachineEraseSelection(t *testing.T) {
	mm := &infrav1beta1.MaasMachine{ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"}}
	r, _ := newReleaseTestReconciler(t, mm, false)

	normal := &fakeReleaseService{}
	if _, err := r.releaseMachineWithPolicy(context.Background(), newReleaseMachineScope(mm), nil, normal, &infrav1beta1.Machine{ID: "vm-1"}); err != nil {
		t.Fatalf("normal release: %v", err)
	}
	if !normal.lastOpts.Erase {
		t.Error("normal release should erase")
	}

	dead := &fakeReleaseService{parentTags: []string{maint.TagHostDead}}
	if _, err := r.releaseMachineWithPolicy(context.Background(), newReleaseMachineScope(mm), nil, dead, &infrav1beta1.Machine{ID: "vm-1"}); err != nil {
		t.Fatalf("dead-parent release: %v", err)
	}
	if dead.lastOpts.Erase {
		t.Error("host-dead release must not erase when the flag is off")
	}
}

func TestMaybeDeleteDynamicLXDVMFailureKeepsFinalizer(t *testing.T) {
	mm := &infrav1beta1.MaasMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "vm", Namespace: "default"},
		Spec: infrav1beta1.MaasMachineSpec{
			LXD: &infrav1beta1.MachineLXDConfig{Enabled: ptr.To(true)},
		},
	}
	r, _ := newReleaseTestReconciler(t, mm, false)
	svc := &fakeReleaseService{dynamicErr: errors.New("status: 500")}

	res, err := r.maybeDeleteDynamicLXDVM(context.Background(), svc, newReleaseMachineScope(mm), "vm-1")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.RequeueAfter <= 0 {
		t.Fatal("expected a requeue after VM delete failure")
	}
	got := &infrav1beta1.MaasMachine{}
	_ = r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "vm"}, got)
	cond := conditions.Get(got, infrav1beta1.ReleaseReadyCondition)
	if cond == nil || cond.Status != corev1.ConditionFalse || cond.Reason != infrav1beta1.VMDeleteFailedReason {
		t.Fatalf("condition = %+v", cond)
	}
}
