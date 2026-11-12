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
- The guard runs during whole-cluster deletion too. The provider mode is not used to skip it,
  because a region with `enable_disk_erasing_on_release=true` can erase even when the mode is
  `none`. If the workload API is gone during a teardown, use the skip annotation after detaching or
  unmasking the volumes.

The release request itself is controlled by `spec.eraseDisksOnRelease` on the MaasCluster:

- `full` (default): requests `erase=true` only. MAAS overwrites the whole device. This matches the
  official provider's bare erase request and MAAS's default secure or full behavior.
- `secure`: requests `erase=true` and `secure_erase=true`. MAAS uses the drive's secure erase
  feature when supported and falls back to a full wipe when it is not.
- `quick`: requests `erase=true` and `quick_erase=true`. MAAS runs `wipefs` on the partitions and
  the disk, then writes 2 MiB of zeros at the start and end. Faster than a full wipe.
- `none`: sends no erase request. Use it for SAN-backed clusters.

The field applies to every release, including node replacement. The attachment guard still applies,
so a release that will erase is held while volumes are attached.

On a region with `enable_disk_erasing_on_release=true`, MAAS applies the region's quick and secure
settings to every release and overrides the request, so the mode is not authoritative. The
attachment guard remains the safety mechanism.

To opt a SAN-backed cluster out of the provider erase:

```
kubectl -n <namespace> patch maascluster <name> --type=merge \
  -p '{"spec":{"eraseDisksOnRelease":"none"}}'
```

Until Palette exposes the field directly, set it with the `maasCluster` passthrough override.

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

## Configuration

| Object | Field | Values | Purpose |
|---|---|---|---|
| MaasCluster | `spec.eraseDisksOnRelease` | `full` (default), `secure`, `quick`, `none` | MAAS erase requested on release. Use `none` for SAN-backed clusters. |

## Follow-up

Automatic fencing (MAAS power-off plus the out-of-service taint) is not implemented here. It needs a
`PowerManagerOff` method in `maas-client-go`, which currently exposes only `PowerManagerOn`. Until
that lands, a held release on a NotReady or Unknown node requires the operator steps above.
