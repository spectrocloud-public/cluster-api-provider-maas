# cluster-api-provider-maas — Learnings

Captured patterns that future sessions should not have to re-learn.
Managed by `/capture-learnings` (spectro-guardrails) and the PCP agent pod.

## Recurring Pitfalls

- **Never add a finalizer whose only remover is registered under a different condition** — `MaasMachineReconciler.HMCEnabled` in `controllers/maasmachine_controller.go`. An ownerless finalizer deadlocks every deletion of that object; gate add/keep/honor on the remover's own registration flag and self-heal orphans on the delete path. (spectrocloud-public/cluster-api-provider-maas#405)

