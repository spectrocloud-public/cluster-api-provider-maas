# Volume detach guard and erase behavior

CAPMAAS must not hand a MAAS host back to MAAS while the workload cluster still reports volumes
attached to its node. A release can run MAAS `wipe-disks`, and that script erases every writable
block device the host can see, including SAN LUNs that are still mapped. The first 2 MiB and the
filesystem signatures are lost, and VMs on those disks fail at their next boot.

## What the controller does

`reconcileDelete` calls `holdReleaseForAttachedVolumes` before releasing the MAAS machine.

- It resolves the node name from the Machine `status.nodeRef`. If the machine never registered a
  node, there is nothing to check and the release continues.
- It lists `storage.k8s.io/v1` `VolumeAttachment` objects through the live workload client
  (`ClusterCache.GetUncachedClient`). A cached list can lag its watch or return an empty result
  while the workload API is unreachable.
- If one or more attachments reference the node, the release is held. The controller sets a
  `VolumeDetachPending` condition on the MaasMachine, emits a `VolumeDetachPending` event, and
  requeues every 30 seconds.
- If the workload client cannot be reached, the release is held and the condition reason is
  `VolumeDetachCheckFailed`. The guard fails closed.
- When no attachment remains, the condition is removed and the release continues.
- During a whole-cluster deletion, the guard is skipped only when the release will not erase. If
  the cluster deletion opts into an erase, the guard stays active.

The release request itself is also gated:

- An individual machine deletion never sends `erase=true`.
- A whole-cluster deletion sends `erase=true` only when the MaasCluster carries
  `infrastructure.cluster.x-k8s.io/erase-disks-on-cluster-delete: "true"`.

Erase is therefore opt-in for whole-cluster deletions. This is a deliberate change from the
previous erase-on-every-release behavior. Operators that relied on erase at cluster deletion must
set the annotation, and should do so only when no LUNs are mapped to the hosts being released.

The flag only controls the provider request. A MAAS region with
`enable_disk_erasing_on_release=true` adds `wipe-disks` to every release on its own. The attachment
guard, not the flag, is the safety mechanism for mapped LUNs.

## Operator runbook for a held release

1. Find the machine and the condition:

   ```
   kubectl -n <namespace> get maasmachine <name> -o yaml
   ```

   Look for `VolumeDetachPending`, and for the `VolumeDetachPending` event.

2. Confirm the attachments in the workload cluster:

   ```
   kubectl --kubeconfig <workload-kubeconfig> get volumeattachments \
     -o custom-columns=NAME:.metadata.name,NODE:.spec.nodeName,ATTACHED:.status.attached \
     | grep <node>
   ```

3. If the node is reachable, allow the CSI driver to finish the detach. If the node is NotReady or
   Unknown, it will not clear the attachments on its own. Fence the host first, then taint the
   node:

   ```
   # After the host is powered off or confirmed down:
   kubectl --kubeconfig <workload-kubeconfig> taint node <node> \
     node.kubernetes.io/out-of-service=nodeshutdown:NoExecute
   ```

   Pod GC and the attach/detach controller then force-delete the stuck pods and force-detach the
   volumes, which calls CSI `ControllerUnpublish` and unmaps the LUNs on the array.

4. If the array still reports the LUNs mapped to the host, remove the host from the masking view or
   host group before releasing.

5. Only after the attachments are gone, or the LUNs are unmasked at the array, set the bypass on
   the MaasMachine:

   ```
   kubectl -n <namespace> annotate maasmachine <name> \
     infrastructure.cluster.x-k8s.io/skip-volume-detach-guard=true --overwrite
   ```

   The release proceeds on the next reconcile. The annotation is removed with the MaasMachine.

6. Remove the out-of-service taint after the replacement node is healthy.

Do not use the bypass while volumes are still attached unless the LUNs have been unmasked at the
array. The bypass exists for the case where the workload API is gone or the CSI driver cannot
complete the detach.

## Annotations

| Object | Annotation | Purpose |
|---|---|---|
| MaasMachine | `infrastructure.cluster.x-k8s.io/skip-volume-detach-guard: "true"` | Release without the attachment check. Use only after the volumes are detached or unmasked. |
| MaasCluster | `infrastructure.cluster.x-k8s.io/erase-disks-on-cluster-delete: "true"` | Request a MAAS disk erase during a whole-cluster deletion. Individual machine deletions never erase. |

## Follow-up

Automatic fencing (MAAS power-off plus the out-of-service taint) is not implemented here. It needs a
`PowerManagerOff` method in `maas-client-go`, which currently exposes only `PowerManagerOn`. Until
that lands, a held release on a NotReady or Unknown node requires the operator steps above.
