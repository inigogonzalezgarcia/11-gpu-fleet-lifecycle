# Runbook

## Start a rollout

1. Write a plan (see [plan.md](plan.md) and `examples/`). Start with `pauseAfterCanary: true`.
2. `bash scripts/rollout.sh start my-plan.json`
3. Watch `bash scripts/rollout.sh status`. The canary goes through Draining → Installing → Validating → Soaking → Done.
4. Check the canary yourself (workloads back on it, metrics normal), then `bash scripts/rollout.sh approve`.
5. The rollout is finished when the phase is `Complete`. `bash scripts/rollout.sh inventory` shows the version per node.

## Phase is `Halted`

`status.json` says why (`message`) and lists the failed nodes with their reason. The node annotation `lifecycle.lab/reason` and its events have the details:

```bash
kubectl describe node <node> | sed -n '/Annotations/,/Taints/p;/Events/,$p'
kubectl -n gpu-lifecycle get jobs -l lifecycle.lab/node=<node>      # failed validation Jobs are kept for 10 minutes
kubectl -n gpu-lifecycle logs job/<job-name>
```

- **RolledBack** nodes are back in service on their previous version. Nothing to do on the node.
- **Quarantined** nodes failed the rollback too. Treat them as broken hardware (repo 09's runbook), not as a driver problem.

A halted rollout does not resume. Fix the cause (or decide the version is bad) and submit a plan with a new `name`.

## Stop a rollout

- `bash scripts/rollout.sh pause`: no new nodes start; `resume` continues.
- `bash scripts/rollout.sh abort`: final. Nodes in progress finish their cycle (or roll back), so wait until `inProgress` is empty before you start anything else on those nodes.

Do not delete the controller to stop a rollout: when it comes back it continues where it was.

## The rollout does not move

| Symptom | Likely cause | Check |
|---|---|---|
| `WaitingForWindow` | Outside the maintenance window (UTC) | `window` in the plan |
| Message says nodes are "already out of service by other means" | Nodes cordoned by remediation, quarantine or someone else use up `maxUnavailable` | `kubectl get nodes`, `skipped` in status |
| A node stays in `Draining` | A PodDisruptionBudget allows no disruption | `kubectl get pdb -A`; the workload's own replicas may be unhealthy |
| A node stays in `Installing` | The agent is slow or down | `kubectl -n gpu-node-agent logs`; after `upgradeTimeout` it rolls back |
| Node listed as skipped "agent unreachable" | Agent not running on that node | `kubectl -n gpu-node-agent get pods -o wide` |
| Phase `Invalid` | The plan does not parse | `message` in status has the error |

`bash scripts/diagnose.sh` collects all of this in one go.

## Release a quarantined node

Only after the hardware has been checked:

```bash
kubectl label node <node> gpu-remediation.lab/quarantined-
kubectl uncordon <node>
```

The node then takes part in the next rollout like any other.
