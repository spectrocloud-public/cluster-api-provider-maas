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
	"fmt"
	"sort"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	maint "github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/maintenance"
)

// Event reasons introduced or reused by S1 (PCP-7843).
const (
	HostEvacuationBlockedEvent   = "EvacuationBlocked"
	WLCReplacementPendingEvent   = "WLCReplacementPending"
	HostEvacuationCompletedEvent = "HostEvacuationCompleted"
)

// vmClass is the classification used in the HostEvacuationReady message.
type vmClass string

const (
	vmClassControlPlane vmClass = "wlc-cp"
	vmClassUnmanaged    vmClass = "unmanaged"
)

// hostEvacuationSnapshot is a deterministic view of the VMs on a host. It is
// used for the condition message and for deciding whether an event/log line is
// a state change or a repeat.
type hostEvacuationSnapshot struct {
	Total     int
	Counts    map[vmClass]int
	Names     []string
	Signature string
}

// buildHostEvacuationSnapshot classifies the host's VMs and builds a stable
// signature from their system IDs and classes.
func buildHostEvacuationSnapshot(vms []maint.Machine) hostEvacuationSnapshot {
	sorted := append([]maint.Machine(nil), vms...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].SystemID != sorted[j].SystemID {
			return sorted[i].SystemID < sorted[j].SystemID
		}
		return maintenanceVMName(sorted[i]) < maintenanceVMName(sorted[j])
	})

	snap := hostEvacuationSnapshot{
		Total:  len(sorted),
		Counts: map[vmClass]int{},
	}
	sigParts := make([]string, 0, len(sorted))
	for _, vm := range sorted {
		class := classifyMaintenanceVM(vm)
		snap.Counts[class]++
		if len(snap.Names) < 5 {
			snap.Names = append(snap.Names, fmt.Sprintf("%s (%s)", maintenanceVMName(vm), class))
		}
		sigParts = append(sigParts, fmt.Sprintf("%s:%s", vm.SystemID, class))
	}
	snap.Signature = strings.Join(sigParts, ",")
	return snap
}

// classifyMaintenanceVM identifies the owner class from MAAS tags. Worker VM
// tags are a future improvement (F3); until they exist, non-CP VMs, including
// LXD worker VMs, are reported as unmanaged.
func classifyMaintenanceVM(vm maint.Machine) vmClass {
	for _, tag := range vm.Tags {
		if tag == maint.TagVMControlPlane {
			return vmClassControlPlane
		}
	}
	return vmClassUnmanaged
}

func maintenanceVMName(vm maint.Machine) string {
	switch {
	case vm.FQDN != "":
		return vm.FQDN
	case vm.Hostname != "":
		return vm.Hostname
	case vm.SystemID != "":
		return vm.SystemID
	default:
		return "unknown"
	}
}

// buildHostEvacuationMessage is the condition message: opID, remaining count,
// per-class counts and the first five VM names.
func buildHostEvacuationMessage(opID string, snap hostEvacuationSnapshot) string {
	if opID == "" {
		opID = "<none>"
	}
	if snap.Total == 0 {
		return fmt.Sprintf("opID=%s; host is empty; waiting for replacement acknowledgement", opID)
	}
	msg := fmt.Sprintf(
		"opID=%s; %d VM(s) remaining (wlc-cp=%d, unmanaged=%d)",
		opID, snap.Total, snap.Counts[vmClassControlPlane], snap.Counts[vmClassUnmanaged],
	)
	if len(snap.Names) > 0 {
		msg += ": " + strings.Join(snap.Names, ", ")
	}
	return msg
}

// setHostEvacuationCondition sets HostEvacuationReady and persists only the
// status subresource.
func setHostEvacuationCondition(
	ctx context.Context,
	c client.Client,
	maasMachine *infrav1beta1.MaasMachine,
	status corev1.ConditionStatus,
	reason string,
	severity clusterv1.ConditionSeverity,
	message string,
) error {
	if maasMachine == nil {
		return nil
	}
	patch := client.MergeFrom(maasMachine.DeepCopy())
	conditions.Set(maasMachine, &clusterv1.Condition{
		Type:     infrav1beta1.HostEvacuationReadyCondition,
		Status:   status,
		Severity: severity,
		Reason:   reason,
		Message:  message,
	})
	return c.Status().Patch(ctx, maasMachine, patch)
}

// evacuationStateTracker remembers the last state signature per object so HMC
// and the MaasMachine controller log/emit an event only when something changes.
// The zero value is usable.
type evacuationStateTracker struct {
	states sync.Map
}

func (t *evacuationStateTracker) changed(key, signature string) bool {
	if current, ok := t.states.Load(key); ok {
		if s, _ := current.(string); s == signature {
			return false
		}
	}
	t.states.Store(key, signature)
	return true
}

func (t *evacuationStateTracker) forget(key string) {
	t.states.Delete(key)
}

func evacuationStateKey(maasMachine *infrav1beta1.MaasMachine) string {
	return maasMachine.Namespace + "/" + maasMachine.Name
}
