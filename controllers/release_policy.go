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
	"fmt"
	"math/rand"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"

	"github.com/spectrocloud/maas-client-go/maasclient"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maasmachine "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/machine"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

const (
	// ReleaseAttemptsAnnotation records the last release error class, count,
	// first-failure time, submit time and parent host as JSON.
	ReleaseAttemptsAnnotation = "maas.lxd.io/release-attempts"
	// SkipEvacuationAnnotation is the audited replacement for removing the
	// evacuation finalizer by hand.
	SkipEvacuationAnnotation = "maas.lxd.io/skip-evacuation"
	// ReleaseStuckEvent is emitted once when the retry budget is exhausted.
	ReleaseStuckEvent = "ReleaseStuck"
)

const (
	releaseClassNotFound        = "not-found"
	releaseClassVMHostRemoval   = "vm-host-removal"
	releaseClassTimeout         = "timeout"
	releaseClassServerError     = "server-error"
	releaseClassConflict        = "conflict"
	releaseClassVMDelete        = "vm-delete"
	releaseClassParentTags      = "parent-tags"
	releaseClassGuestList       = "guest-list"
	releaseClassGuestDelete     = "guest-delete"
	releaseClassFailedReleasing = "failed-releasing"
	releaseClassSubmitted       = "submitted"
	releaseClassUnknown         = "unknown"
)

type releaseAttempts struct {
	Class         string `json:"class,omitempty"`
	Count         int    `json:"count"`
	FirstFailure  string `json:"firstFailure,omitempty"`
	Submitted     bool   `json:"submitted,omitempty"`
	SubmitTime    string `json:"submitTime,omitempty"`
	ParentHost    string `json:"parentHost,omitempty"`
	StuckNotified bool   `json:"stuckNotified,omitempty"`
}

type guestVM struct {
	SystemID string
	Name     string
}

type releaseService interface {
	ParentHostTags(ctx context.Context, systemID string, timeout time.Duration) (string, []string, error)
	GetMachineState(ctx context.Context, systemID string, timeout time.Duration) (infrav1beta1.MachineState, error)
	ReleaseMachine(ctx context.Context, systemID string, opts maasmachine.ReleaseOptions) error
	DeleteMachineRecord(ctx context.Context, systemID string, timeout time.Duration) error
	DeleteDynamicLXDVM(ctx context.Context, systemID string, timeout time.Duration) error
}

func (r *MaasMachineReconciler) releaseConfig() maint.ReleaseConfig {
	cfg := maint.DefaultReleaseConfig()
	if r.ReleaseReadTimeout > 0 {
		cfg.ReadTimeout = r.ReleaseReadTimeout
	}
	if r.ReleaseListTimeout > 0 {
		cfg.ListTimeout = r.ReleaseListTimeout
	}
	if r.ReleaseRetryBudget > 0 {
		cfg.RetryBudget = r.ReleaseRetryBudget
	}
	return cfg
}

// releaseMachineWithPolicy implements D18: per-kind timeouts, one release
// submit followed by state polling, jittered retries and a stuck alert.
func (r *MaasMachineReconciler) releaseMachineWithPolicy(
	ctx context.Context,
	machineScope *scope.MachineScope,
	clusterScope *scope.ClusterScope,
	machineSvc releaseService,
	m *infrav1beta1.Machine,
) (ctrl.Result, error) {
	mm := machineScope.MaasMachine
	cfg := r.releaseConfig()
	skip := hasAnnotation(mm, SkipEvacuationAnnotation)
	state := r.loadReleaseAttempts(mm)

	// Once submitted, poll instead of releasing again.
	if state.Submitted {
		return r.pollRelease(ctx, machineSvc, machineScope, m, cfg, state)
	}

	parentID, tags, pgErr := machineSvc.ParentHostTags(ctx, m.ID, cfg.ReadTimeout)
	if pgErr != nil {
		if classifyReleaseError(pgErr) == releaseClassTimeout {
			return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
		}
		return r.retryRelease(ctx, mm, releaseClassParentTags, pgErr)
	}
	deadParent := hasTag(tags, maint.TagHostDead)

	if deadParent && r.AutoDeadHostRecovery {
		if err := machineSvc.DeleteMachineRecord(ctx, m.ID, cfg.ListTimeout); err != nil {
			if isNotFoundError(err) {
				r.releaseComplete(ctx, mm, "MAAS record already gone")
				return ctrl.Result{}, nil
			}
			return r.retryRelease(ctx, mm, releaseClassVMDelete, err)
		}
		r.releaseComplete(ctx, mm, fmt.Sprintf("deleted MAAS record for host-dead parent %s; no release or erase", parentID))
		machineScope.Info("Deleted MAAS record for VM on host-dead parent", "system-id", m.ID, "parent", parentID)
		return ctrl.Result{}, nil
	}

	// Normal release keeps erase; a host-dead parent or skip-evacuation does not.
	erase := !deadParent && !skip
	err := machineSvc.ReleaseMachine(ctx, m.ID, maasmachine.ReleaseOptions{Erase: erase, Timeout: cfg.ListTimeout})
	if err == nil {
		state = r.markReleaseSubmitted(ctx, mm, releaseClassSubmitted, parentID)
		r.setReleaseInProgress(ctx, mm, state, "release submitted; polling MAAS state")
		return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
	}

	class := classifyReleaseError(err)
	if class == releaseClassNotFound {
		r.releaseComplete(ctx, mm, "MAAS record already gone")
		return ctrl.Result{}, nil
	}

	if class == releaseClassTimeout {
		// MAAS may have accepted the release. Do not submit again; poll.
		state = r.markReleaseSubmitted(ctx, mm, releaseClassTimeout, parentID)
		r.setReleaseInProgress(ctx, mm, state, "release submit timed out; polling MAAS state")
		return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
	}

	if class == releaseClassVMHostRemoval {
		guests, gerr := listGuestVMs(ctx, clusterScope, m.ID, cfg.ListTimeout)
		if gerr != nil {
			return r.retryRelease(ctx, mm, releaseClassGuestList, gerr)
		}
		names := guestNames(guests)
		if !skip {
			r.recordReleaseAttempt(ctx, mm, class)
			r.setReleaseReady(ctx, mm, corev1.ConditionFalse, infrav1beta1.ReleaseBlockedReason,
				clusterv1.ConditionSeverityWarning,
				fmt.Sprintf("release blocked by %d guest VM(s) on host %s: %s; set %s to force",
					len(guests), m.ID, names, SkipEvacuationAnnotation))
			return ctrl.Result{RequeueAfter: cfg.MaxBackoff}, nil
		}

		if derr := deleteGuestVMs(ctx, clusterScope, guests, cfg.ListTimeout); derr != nil {
			return r.retryRelease(ctx, mm, releaseClassGuestDelete, derr)
		}
		r.bestEffortUnregisterLXDHost(clusterScope, machineScope, m)
		if rerr := machineSvc.ReleaseMachine(ctx, m.ID, maasmachine.ReleaseOptions{Erase: false, Timeout: cfg.ListTimeout}); rerr != nil {
			return r.retryRelease(ctx, mm, classifyReleaseError(rerr), rerr)
		}
		state = r.markReleaseSubmitted(ctx, mm, releaseClassSubmitted, parentID)
		r.setReleaseInProgress(ctx, mm, state, "release submitted after skip-evacuation guest cleanup")
		return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
	}

	return r.retryRelease(ctx, mm, class, err)
}

// pollRelease reads MAAS state; it never submits another release.
func (r *MaasMachineReconciler) pollRelease(
	ctx context.Context,
	machineSvc releaseService,
	machineScope *scope.MachineScope,
	m *infrav1beta1.Machine,
	cfg maint.ReleaseConfig,
	state releaseAttempts,
) (ctrl.Result, error) {
	mm := machineScope.MaasMachine
	st, err := machineSvc.GetMachineState(ctx, m.ID, cfg.ReadTimeout)
	if err != nil {
		if isNotFoundError(err) {
			r.releaseComplete(ctx, mm, "MAAS record already gone")
			return ctrl.Result{}, nil
		}
		if classifyReleaseError(err) == releaseClassTimeout {
			return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
		}
		return r.retryRelease(ctx, mm, releaseClassUnknown, err)
	}

	switch st {
	case infrav1beta1.MachineStateReady:
		r.releaseComplete(ctx, mm, "machine released")
		return ctrl.Result{}, nil
	case infrav1beta1.MachineStateReleasing, infrav1beta1.MachineStateDiskErasing:
		r.reportReleaseStuckIfNeeded(ctx, mm, &state, cfg)
		r.setReleaseInProgress(ctx, mm, state, fmt.Sprintf("release in progress (state=%s)", st))
		return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
	case infrav1beta1.MachineStateFailedReleasing:
		state.Submitted = false
		r.saveReleaseAttempts(ctx, mm, state)
		return r.retryRelease(ctx, mm, releaseClassFailedReleasing, fmt.Errorf("MAAS reported state %q", st))
	default:
		// Deployed/Allocated after submit: wait, but do not resubmit.
		if r.reportReleaseStuckIfNeeded(ctx, mm, &state, cfg) {
			return ctrl.Result{RequeueAfter: cfg.MaxBackoff}, nil
		}
		r.setReleaseInProgress(ctx, mm, state, fmt.Sprintf("waiting for MAAS release (state=%s)", st))
		return ctrl.Result{RequeueAfter: cfg.InitialBackoff}, nil
	}
}

func (r *MaasMachineReconciler) retryRelease(
	ctx context.Context,
	mm *infrav1beta1.MaasMachine,
	class string,
	cause error,
) (ctrl.Result, error) {
	state := r.recordReleaseAttempt(ctx, mm, class)
	cfg := r.releaseConfig()
	d := releaseBackoff(state, cfg)
	if r.reportReleaseStuckIfNeeded(ctx, mm, &state, cfg) {
		d = cfg.MaxBackoff
	} else {
		r.setReleaseReady(ctx, mm, corev1.ConditionFalse, infrav1beta1.ReleaseFailedReason,
			clusterv1.ConditionSeverityWarning,
			fmt.Sprintf("release failed (%s, attempt %d, first failure %s): %v",
				class, state.Count, state.FirstFailure, cause))
	}
	return ctrl.Result{RequeueAfter: d}, nil
}

func (r *MaasMachineReconciler) reportReleaseStuckIfNeeded(
	ctx context.Context,
	mm *infrav1beta1.MaasMachine,
	state *releaseAttempts,
	cfg maint.ReleaseConfig,
) bool {
	if !releaseOverBudget(*state, cfg) {
		return false
	}
	if !state.StuckNotified {
		state.StuckNotified = true
		r.saveReleaseAttempts(ctx, mm, *state)
		r.setReleaseReady(ctx, mm, corev1.ConditionFalse, infrav1beta1.ReleaseFailedReason,
			clusterv1.ConditionSeverityError,
			fmt.Sprintf("release stuck after %s (class=%s, attempts=%d, first failure %s, submit %s, parent %s)",
				cfg.RetryBudget, state.Class, state.Count, state.FirstFailure, state.SubmitTime, state.ParentHost))
		if r.Recorder != nil {
			r.Recorder.Eventf(mm, corev1.EventTypeWarning, ReleaseStuckEvent,
				"Release stuck after %s (class=%s, attempts=%d)", cfg.RetryBudget, state.Class, state.Count)
		}
	}
	return true
}

func releaseOverBudget(state releaseAttempts, cfg maint.ReleaseConfig) bool {
	ref := state.SubmitTime
	if ref == "" {
		ref = state.FirstFailure
	}
	if ref == "" || cfg.RetryBudget <= 0 {
		return false
	}
	t, err := time.Parse(time.RFC3339, ref)
	if err != nil {
		return false
	}
	return time.Since(t) >= cfg.RetryBudget
}

func (r *MaasMachineReconciler) releaseComplete(ctx context.Context, mm *infrav1beta1.MaasMachine, message string) {
	r.clearReleaseAttempts(ctx, mm)
	r.setReleaseReady(ctx, mm, corev1.ConditionTrue, infrav1beta1.ReleaseCompleteReason,
		clusterv1.ConditionSeverityNone, message)
}

func (r *MaasMachineReconciler) setReleaseInProgress(ctx context.Context, mm *infrav1beta1.MaasMachine, state releaseAttempts, message string) {
	r.setReleaseReady(ctx, mm, corev1.ConditionFalse, infrav1beta1.ReleaseInProgressReason,
		clusterv1.ConditionSeverityInfo,
		fmt.Sprintf("%s (class=%s, submit %s, parent %s)", message, state.Class, state.SubmitTime, state.ParentHost))
}

// maybeDeleteDynamicLXDVM deletes the MAAS record for a dynamic LXD VM and
// keeps the finalizer when that delete fails.
func (r *MaasMachineReconciler) maybeDeleteDynamicLXDVM(
	ctx context.Context,
	machineSvc releaseService,
	machineScope *scope.MachineScope,
	systemID string,
) (ctrl.Result, error) {
	if !machineScope.GetDynamicLXD() {
		return ctrl.Result{}, nil
	}
	cfg := r.releaseConfig()
	machineScope.Info("Deleting LXD VM after release", "system-id", systemID)
	if err := machineSvc.DeleteDynamicLXDVM(ctx, systemID, cfg.ListTimeout); err != nil {
		if isNotFoundError(err) {
			return ctrl.Result{}, nil
		}
		state := r.recordReleaseAttempt(ctx, machineScope.MaasMachine, releaseClassVMDelete)
		r.setReleaseReady(ctx, machineScope.MaasMachine, corev1.ConditionFalse, infrav1beta1.VMDeleteFailedReason,
			clusterv1.ConditionSeverityWarning,
			fmt.Sprintf("VM record delete failed (attempt %d): %v", state.Count, err))
		return ctrl.Result{RequeueAfter: releaseBackoff(state, cfg)}, nil
	}
	r.clearReleaseAttempts(ctx, machineScope.MaasMachine)
	r.setReleaseReady(ctx, machineScope.MaasMachine, corev1.ConditionTrue, infrav1beta1.ReleaseCompleteReason,
		clusterv1.ConditionSeverityNone, "MAAS record deleted")
	machineScope.Info("Successfully deleted LXD VM after release", "system-id", systemID)
	return ctrl.Result{}, nil
}

func (r *MaasMachineReconciler) loadReleaseAttempts(mm *infrav1beta1.MaasMachine) releaseAttempts {
	state := releaseAttempts{}
	if mm.Annotations != nil {
		if raw := mm.Annotations[ReleaseAttemptsAnnotation]; raw != "" {
			_ = json.Unmarshal([]byte(raw), &state)
		}
	}
	return state
}

func (r *MaasMachineReconciler) saveReleaseAttempts(ctx context.Context, mm *infrav1beta1.MaasMachine, state releaseAttempts) {
	raw, err := json.Marshal(state)
	if err != nil {
		r.Log.Error(err, "failed to marshal release attempts")
		return
	}
	patch := client.MergeFrom(mm.DeepCopy())
	if mm.Annotations == nil {
		mm.Annotations = map[string]string{}
	}
	mm.Annotations[ReleaseAttemptsAnnotation] = string(raw)
	if err := r.Patch(ctx, mm, patch); err != nil {
		r.Log.Error(err, "failed to save release attempts", "annotations", ReleaseAttemptsAnnotation)
	}
}

func (r *MaasMachineReconciler) markReleaseSubmitted(ctx context.Context, mm *infrav1beta1.MaasMachine, class, parentID string) releaseAttempts {
	state := r.loadReleaseAttempts(mm)
	if state.FirstFailure == "" && class != releaseClassSubmitted {
		state.FirstFailure = time.Now().UTC().Format(time.RFC3339)
	}
	state.Class = class
	state.Count++
	state.Submitted = true
	state.SubmitTime = time.Now().UTC().Format(time.RFC3339)
	state.ParentHost = parentID
	r.saveReleaseAttempts(ctx, mm, state)
	return state
}

func (r *MaasMachineReconciler) recordReleaseAttempt(
	ctx context.Context,
	mm *infrav1beta1.MaasMachine,
	class string,
) releaseAttempts {
	state := r.loadReleaseAttempts(mm)
	if state.FirstFailure == "" {
		state.FirstFailure = time.Now().UTC().Format(time.RFC3339)
	}
	state.Class = class
	state.Count++
	r.saveReleaseAttempts(ctx, mm, state)
	return state
}

func (r *MaasMachineReconciler) clearReleaseAttempts(ctx context.Context, mm *infrav1beta1.MaasMachine) {
	if mm.Annotations == nil || mm.Annotations[ReleaseAttemptsAnnotation] == "" {
		return
	}
	patch := client.MergeFrom(mm.DeepCopy())
	delete(mm.Annotations, ReleaseAttemptsAnnotation)
	if err := r.Patch(ctx, mm, patch); err != nil {
		r.Log.Error(err, "failed to clear release attempts", "annotations", ReleaseAttemptsAnnotation)
	}
}

func (r *MaasMachineReconciler) setReleaseReady(
	ctx context.Context,
	mm *infrav1beta1.MaasMachine,
	status corev1.ConditionStatus,
	reason string,
	severity clusterv1.ConditionSeverity,
	message string,
) {
	patch := client.MergeFrom(mm.DeepCopy())
	conditions.Set(mm, &clusterv1.Condition{
		Type:     infrav1beta1.ReleaseReadyCondition,
		Status:   status,
		Reason:   reason,
		Severity: severity,
		Message:  message,
	})
	if err := r.Status().Patch(ctx, mm, patch); err != nil {
		r.Log.Error(err, "failed to set ReleaseReady condition", "reason", reason)
	}
}

func classifyReleaseError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case isNotFoundError(err):
		return releaseClassNotFound
	case isVMHostRemovalRequiredError(err):
		return releaseClassVMHostRemoval
	case strings.Contains(msg, "status: 409") || strings.Contains(msg, "409 conflict"):
		return releaseClassConflict
	case strings.Contains(msg, "timeout"),
		strings.Contains(msg, "timed out"),
		strings.Contains(msg, "deadline exceeded"),
		strings.Contains(msg, "connect"):
		return releaseClassTimeout
	case strings.Contains(msg, "status: 5"),
		strings.Contains(msg, " 500"),
		strings.Contains(msg, " 502"),
		strings.Contains(msg, " 503"),
		strings.Contains(msg, " 504"):
		return releaseClassServerError
	default:
		return releaseClassUnknown
	}
}

func isNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "status: 404") || strings.Contains(msg, "no machine matches")
}

func releaseBackoff(state releaseAttempts, cfg maint.ReleaseConfig) time.Duration {
	initial := cfg.InitialBackoff
	if initial <= 0 {
		initial = time.Minute
	}
	max := cfg.MaxBackoff
	if max <= 0 {
		max = 10 * time.Minute
	}

	backoff := initial
	for i := 1; i < state.Count && backoff < max; i++ {
		backoff *= 2
	}
	if backoff > max {
		backoff = max
	}
	return withJitter(backoff, cfg.Jitter)
}

func withJitter(d time.Duration, pct float64) time.Duration {
	if d <= 0 || pct <= 0 {
		return d
	}
	delta := float64(d) * pct
	return d + time.Duration((rand.Float64()*2-1)*delta)
}

func hasTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func hasAnnotation(mm *infrav1beta1.MaasMachine, key string) bool {
	return mm != nil && mm.Annotations != nil && mm.Annotations[key] != ""
}

func guestNames(guests []guestVM) string {
	if len(guests) == 0 {
		return "none"
	}
	names := make([]string, 0, len(guests))
	for _, g := range guests {
		name := g.Name
		if name == "" {
			name = g.SystemID
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func listGuestVMs(
	ctx context.Context,
	clusterScope *scope.ClusterScope,
	hostSystemID string,
	timeout time.Duration,
) ([]guestVM, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	api := clusterScope.GetMaasClientIdentity()
	client := maasclient.NewAuthenticatedClientSet(api.URL, api.Token)
	hosts, err := client.VMHosts().List(cctx, nil)
	if err != nil {
		return nil, err
	}
	for _, host := range hosts {
		if host.HostSystemID() != hostSystemID {
			continue
		}
		machines, err := host.Machines().List(cctx)
		if err != nil {
			return nil, err
		}
		guests := make([]guestVM, 0, len(machines))
		for _, m := range machines {
			id := m.SystemID()
			if id == "" {
				continue
			}
			detail, derr := client.Machines().Machine(id).Get(cctx)
			if derr != nil {
				if isNotFoundError(derr) {
					continue
				}
				return nil, derr
			}
			guests = append(guests, guestVM{SystemID: detail.SystemID(), Name: detail.Hostname()})
		}
		return guests, nil
	}
	return nil, fmt.Errorf("vm-host for system-id %s not found", hostSystemID)
}

func deleteGuestVMs(ctx context.Context, clusterScope *scope.ClusterScope, guests []guestVM, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	api := clusterScope.GetMaasClientIdentity()
	client := maasclient.NewAuthenticatedClientSet(api.URL, api.Token)
	var failures []string
	for _, guest := range guests {
		if err := client.Machines().Machine(guest.SystemID).Delete(cctx); err != nil && !isNotFoundError(err) {
			failures = append(failures, fmt.Sprintf("%s: %v", guest.SystemID, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("failed to delete guest VMs: %s", strings.Join(failures, "; "))
	}
	return nil
}
