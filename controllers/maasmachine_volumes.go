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
	"strings"
	"time"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	conditions "sigs.k8s.io/cluster-api/util/conditions/deprecated/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1beta1 "github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1"
	"github.com/spectrocloud/cluster-api-provider-maas/pkg/maas/scope"
)

const (
	// SkipVolumeDetachGuardAnnotation allows an operator to release a machine while the
	// workload cluster still reports volumes attached to its node. Set it only after the
	// volumes are detached, or unmasked at the storage array, for the affected host.
	SkipVolumeDetachGuardAnnotation = "infrastructure.cluster.x-k8s.io/skip-volume-detach-guard"

	// EraseDisksOnClusterDeleteAnnotation opts a whole-cluster deletion into a MAAS disk
	// erase. Individual machine deletions never request an erase, even when it is set.
	EraseDisksOnClusterDeleteAnnotation = "infrastructure.cluster.x-k8s.io/erase-disks-on-cluster-delete"

	// volumeDetachRequeueInterval is how often a held release is retried.
	volumeDetachRequeueInterval = 30 * time.Second
)

func boolAnnotation(annotations map[string]string, key string) bool {
	return strings.EqualFold(strings.TrimSpace(annotations[key]), "true")
}

// shouldEraseDisksOnRelease reports whether this release may request a MAAS disk erase.
// Erase is limited to a whole-cluster deletion with an explicit opt-in. MAAS treats
// erase=true with neither quick_erase nor secure_erase as a full-device overwrite, so an
// ordinary machine deletion must never ask for it.
func shouldEraseDisksOnRelease(clusterScope *scope.ClusterScope) bool {
	if clusterScope == nil || clusterScope.Cluster == nil || clusterScope.Cluster.DeletionTimestamp.IsZero() {
		return false
	}
	if clusterScope.MaasCluster == nil {
		return false
	}
	return boolAnnotation(clusterScope.MaasCluster.Annotations, EraseDisksOnClusterDeleteAnnotation)
}

// machineNodeName returns the workload cluster node name for the machine, or an empty
// string when the machine has not registered a node.
func machineNodeName(machineScope *scope.MachineScope) string {
	if machineScope == nil || machineScope.Machine == nil || !machineScope.Machine.Status.NodeRef.IsDefined() {
		return ""
	}
	return machineScope.Machine.Status.NodeRef.Name
}

// countAttachedVolumes counts VolumeAttachments in the workload cluster whose node name
// matches. Unbound attachments are counted too: the incident volumes had no PVC claim ref,
// so a PVC-based check would have passed.
func countAttachedVolumes(ctx context.Context, workloadClient client.Client, nodeName string) (int, error) {
	if nodeName == "" {
		return 0, nil
	}
	attachments := &storagev1.VolumeAttachmentList{}
	if err := workloadClient.List(ctx, attachments); err != nil {
		return 0, errors.Wrap(err, "failed to list VolumeAttachments in the workload cluster")
	}
	count := 0
	for i := range attachments.Items {
		if attachments.Items[i].Spec.NodeName == nodeName {
			count++
		}
	}
	return count, nil
}

// holdReleaseForAttachedVolumes reports whether the release must wait. It is held while the
// workload cluster reports any volume attached to the machine's node, unless the
// skip-volume-detach-guard annotation is set. The guard fails closed when the node is known
// but the workload cluster cannot be queried, because a pending detach is still possible.
func (r *MaasMachineReconciler) holdReleaseForAttachedVolumes(ctx context.Context, machineScope *scope.MachineScope, clusterScope *scope.ClusterScope) (bool, ctrl.Result) {
	nodeName := machineNodeName(machineScope)
	if nodeName == "" {
		return false, ctrl.Result{}
	}
	// A whole-cluster deletion is an explicit teardown. The guard is for node
	// replacement, and holding here can deadlock the teardown once the workload API
	// server is gone. Cluster deletion relies on the no-erase default instead.
	if clusterScope.Cluster != nil && !clusterScope.Cluster.DeletionTimestamp.IsZero() {
		machineScope.Info("Skipping the volume detach guard: cluster is being deleted", "node", nodeName)
		return false, ctrl.Result{}
	}
	maasMachine := machineScope.MaasMachine

	if boolAnnotation(maasMachine.Annotations, SkipVolumeDetachGuardAnnotation) {
		machineScope.Info("Releasing without the volume detach guard: skip annotation set", "node", nodeName)
		r.Recorder.Eventf(maasMachine, corev1.EventTypeWarning, "VolumeDetachGuardSkipped",
			"Releasing while volumes may still be attached to node %q because %s is set", nodeName, SkipVolumeDetachGuardAnnotation)
		conditions.Delete(maasMachine, infrav1beta1.VolumeDetachPendingCondition)
		return false, ctrl.Result{}
	}

	// Use the live client. A cached list can be stale while its watch lags, and it can
	// return an empty result when the workload API is unreachable, which would permit the
	// release instead of failing closed.
	workloadClient, err := clusterScope.GetWorkloadClusterUncachedClient(ctx)
	if err != nil {
		message := fmt.Sprintf("workload cluster client unavailable, cannot confirm volumes are detached from node %q: %v", nodeName, err)
		conditions.MarkFalse(maasMachine, infrav1beta1.VolumeDetachPendingCondition, infrav1beta1.VolumeDetachCheckFailedReason, clusterv1.ConditionSeverityWarning, "%s", message)
		r.Recorder.Eventf(maasMachine, corev1.EventTypeWarning, "VolumeDetachPending", "%s", message)
		machineScope.Info("Holding release: workload cluster client unavailable", "node", nodeName, "error", err.Error())
		return true, ctrl.Result{RequeueAfter: volumeDetachRequeueInterval}
	}

	count, err := countAttachedVolumes(ctx, workloadClient, nodeName)
	if err != nil {
		message := fmt.Sprintf("cannot list volume attachments for node %q: %v", nodeName, err)
		conditions.MarkFalse(maasMachine, infrav1beta1.VolumeDetachPendingCondition, infrav1beta1.VolumeDetachCheckFailedReason, clusterv1.ConditionSeverityWarning, "%s", message)
		r.Recorder.Eventf(maasMachine, corev1.EventTypeWarning, "VolumeDetachPending", "%s", message)
		machineScope.Info("Holding release: volume attachment check failed", "node", nodeName, "error", err.Error())
		return true, ctrl.Result{RequeueAfter: volumeDetachRequeueInterval}
	}

	if count == 0 {
		conditions.Delete(maasMachine, infrav1beta1.VolumeDetachPendingCondition)
		return false, ctrl.Result{}
	}

	message := fmt.Sprintf("%d volume attachment(s) still present for node %q; release is held until they clear", count, nodeName)
	conditions.MarkFalse(maasMachine, infrav1beta1.VolumeDetachPendingCondition, infrav1beta1.WaitingForVolumeDetachReason, clusterv1.ConditionSeverityInfo, "%s", message)
	r.Recorder.Eventf(maasMachine, corev1.EventTypeNormal, "VolumeDetachPending", "%s", message)
	machineScope.Info("Holding release: volumes still attached", "node", nodeName, "attachments", count)
	return true, ctrl.Result{RequeueAfter: volumeDetachRequeueInterval}
}
