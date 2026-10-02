# The rollout plan

The plan is `plan.json` in the `rollout-plan` ConfigMap (namespace `gpu-lifecycle`). Unknown fields are rejected, so a typo is an error instead of a silently ignored setting.

| Field | Default | Meaning |
|---|---|---|
| `name` | required | Identifies the rollout. A new name starts a new rollout; approvals refer to it |
| `selector` | required | Label selector for the nodes that take part |
| `component` | `driver` | What is being upgraded (only used in messages and events in the lab) |
| `target` | required | Version to reach |
| `canary` | 1 | Nodes upgraded first, on their own. 0 disables the canary |
| `pauseAfterCanary` | false | Wait for an approval after the canary passes |
| `batchSize` | 1 | Nodes started together after the canary. A batch starts when the previous one has finished |
| `maxUnavailable` | 1 | Maximum nodes out of service at once, including nodes cordoned for other reasons |
| `failureBudget` | 0 | Failed nodes tolerated after the canary before the rollout halts |
| `soak` | `2m` | How long a validated node is watched before it is declared done |
| `upgradeTimeout` | `15m` | Maximum time for the install (and for a rollback install) |
| `validationTimeout` | `15m` | Maximum time for a validation Job |
| `window` | none (always open) | `{"start": "HH:MM", "end": "HH:MM", "days": ["Mon", …]}` in UTC. `end` earlier than `start` means it ends the next day; `days` refers to the day the window starts |

Validation rules: `name`, `selector` and `target` are required; `canary` ≥ 0 and ≤ `maxUnavailable`; `batchSize` and `maxUnavailable` ≥ 1; `failureBudget` ≥ 0; window times as `HH:MM` and days as `Mon`..`Sun`. A `batchSize` larger than `maxUnavailable` is not an error: the budget caps it.

## Controls

Other keys in the same ConfigMap:

| Key | Values | Effect |
|---|---|---|
| `control` | `run` (default), `pause`, `abort` | `pause` stops new nodes from starting; `abort` ends the rollout. In both cases nodes in progress finish their cycle |
| `approve` | the rollout name | Lets a rollout continue after its canary. Approving by name means an old approval never applies to a new rollout |

`scripts/rollout.sh` wraps these: `start`, `approve`, `pause`, `resume`, `abort`.

## Phases

| Phase | Meaning |
|---|---|
| `Canary` | The canary is being upgraded |
| `AwaitingApproval` | The canary passed; waiting for `approve` |
| `Rolling` | Batches in progress |
| `WaitingForWindow` | Nodes would start, but the maintenance window is closed |
| `Paused` | `control=pause` |
| `Halted` | The canary failed or the failure budget was exceeded. Final: fix the cause and submit a new plan |
| `Aborted` | `control=abort`. Final |
| `Complete` | Every selected node is on the target, failed within budget, or skipped |
| `Invalid` | The plan does not parse; the message says why |

Skipped nodes (quarantined, under remediation, cordoned by someone else, agent unreachable) do not block `Complete`. They appear in `status.json` under `skipped`, and the inventory label shows they are still on the old version.
