# Architecture

## The loop

Every few seconds (`-interval`) the controller runs one reconcile pass:

1. Read the `rollout-plan` ConfigMap (`plan.json`, `control`, `approve`). No ConfigMap, nothing to do.
2. Parse the plan. An invalid plan sets phase `Invalid` with the error and changes nothing.
3. List the nodes that match `selector`. For each one:
   - ask its agent which version it runs and keep the `lifecycle.lab/version` label up to date (the inventory);
   - if the node is in a step of **this** rollout, move it forward one step;
   - classify it: in progress, done, failed, skipped (quarantined, under remediation, cordoned by someone else, agent unreachable) or pending. A node already on the target version counts as done.
4. Decide the phase: halted (canary failed, or more failures than `failureBudget`), aborted, complete, or which nodes may start now.
5. Start those nodes, unless the rollout is paused or outside its maintenance window.
6. Write `status.json` to the `rollout-status` ConfigMap.

Nodes in progress are moved forward in step 3, before any phase decision, so a halt, an abort or a closed window never leaves a node half-upgraded.

## Where state lives

| What | Where | Why |
|---|---|---|
| The plan and the operator's controls | `rollout-plan` ConfigMap | `kubectl apply` / `patch`, reviewable in Git, RBAC-controlled |
| Rollout progress | `rollout-status` ConfigMap, `status.json` | Readable without access to the controller; survives restarts |
| Each node's step | Node annotations `lifecycle.lab/{rollout,state,from-version,since,validation-job,reason}` | `kubectl describe node` explains the node; a restart resumes each node where it was |
| Installed version | Node label `lifecycle.lab/version` | `kubectl get nodes -L lifecycle.lab/version` is the fleet inventory, and selectors can use it |
| History | Kubernetes events on each node (`UpgradeStarted`, `UpgradeFailed`, `UpgradeRolledBack`…) | The usual place people look |

The controller keeps nothing in memory except metrics counters.

## One node, step by step

| State | The node is | Moves on when | On failure or timeout |
|---|---|---|---|
| Draining | cordoned, pods being evicted (PDBs respected) | no evictable pod left; then asks the agent to install | keeps waiting (a PDB can block for a long time) |
| Installing | cordoned | the agent reports the target version | `upgradeTimeout` → RollingBack |
| Validating | cordoned, validation Job running on it | the Job succeeds | Job fails or `validationTimeout` → RollingBack |
| Soaking | cordoned, health watched | `soak` elapsed with no hardware XID or ECC error | XID / ECC → RollingBack |
| Done | schedulable, on the target | – | – |
| RollingBack | cordoned, agent reinstalling `from-version` | the agent reports `from-version` | `upgradeTimeout` → Quarantined |
| RollbackValidating | cordoned, validation Job running | the Job succeeds → RolledBack (schedulable, old version) | → Quarantined |
| Quarantined | cordoned, label `gpu-remediation.lab/quarantined=true` | a human releases it | – |

## Interaction with node remediation (repo 09)

Both controllers can run in the same cluster. They agree through two markers: the quarantine label `gpu-remediation.lab/quarantined` and the remediation annotation `gpu-remediation.lab/state`. The rollout skips nodes that carry either, and counts them against `maxUnavailable` when they are cordoned, so the two tools together never take more nodes out than the plan allows. Quarantine from a failed rollback uses the same label, so repo 09's release procedure applies.

## Metrics

`:8080/metrics`:

- `gpu_lifecycle_rollout_phase{rollout,target,phase}`: 1 for the current phase
- `gpu_lifecycle_nodes{status}`: in_progress, done, pending, failed, skipped
- `gpu_lifecycle_actions_total{action}`: start, evict, install, done, rollback, rolled_back, quarantine, halt
- `gpu_lifecycle_reconcile_total`, `gpu_lifecycle_reconcile_errors_total`

A useful alert: `gpu_lifecycle_rollout_phase{phase="Halted"} == 1` for more than a few minutes.

## Failure handling

- **Controller restart:** state is in the cluster; the next pass continues every node from its annotation. The validation Job keeps running meanwhile.
- **Agent unreachable:** a node not yet started is skipped; a node in progress stays in its step (the pass reports an error) until the agent answers or a timeout fires.
- **Agent restarted mid-install:** if it no longer reports the install, the controller asks again.
- **Validation Job deleted by someone:** treated as a failed validation.
- **Plan edited mid-rollout:** a different `name` starts a new rollout; nodes of the old one are no longer advanced by this controller, so finish or abort a rollout before replacing it.
