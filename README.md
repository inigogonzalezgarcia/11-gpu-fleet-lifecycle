# GPU Fleet Lifecycle

A small Kubernetes controller that rolls a new GPU driver across a fleet of nodes the careful way: one canary first, a human approval, then batches, with every node drained, upgraded, validated and watched before the next one starts. A node that fails is rolled back to its previous version; a bad canary stops the rollout before it reaches anything else. Tested end to end on every push against a kind cluster with simulated GPU nodes.

**Plan → canary → approval → batches → drain / install / validate / soak → done or rolled back**

![ci](https://github.com/inigogonzalezgarcia/11-gpu-fleet-lifecycle/actions/workflows/ci.yml/badge.svg)

> A learning-in-public lab about "day 2" operations for GPU clusters. I don't have production GPU fleet experience; this project is how I am learning the problem. There is no real GPU and no real driver install in CI: each node runs a small agent that pretends to install versions, and some versions are deliberately broken to exercise the failure paths. In a real cluster the NVIDIA GPU Operator has its own driver upgrade controller; this lab rebuilds a simplified version of the idea to understand it ([why](docs/decisions.md#1-why-rebuild-something-the-gpu-operator-already-does)).

Third in a series: [09 – node remediation](https://github.com/inigogonzalezgarcia/09-gpu-node-remediation) fixes broken nodes, [10 – fleet observability](https://github.com/inigogonzalezgarcia/10-gpu-fleet-observability) watches the fleet, and this one changes it safely.

```
Pending ─▶ Draining ─▶ Installing ─▶ Validating ─▶ Soaking ─▶ Done
                           │              │            │
                           └───── any failure ─────────┘
                                          ▼
                    RollingBack ─▶ RollbackValidating ─▶ RolledBack (old version, back in service)
                                                    └──▶ Quarantined (cordoned, needs a human)
```

A rollout is a JSON plan in a ConfigMap:

```json
{
  "name": "driver-r580",
  "selector": "nvidia.com/gpu.present=true",
  "target": "r580-lab",
  "canary": 1,
  "pauseAfterCanary": true,
  "batchSize": 2,
  "maxUnavailable": 2,
  "failureBudget": 0,
  "soak": "20s",
  "window": {"start": "22:00", "end": "06:00", "days": ["Mon", "Tue", "Wed", "Thu"]}
}
```

| Piece | What it does |
|---|---|
| `internal/plan` | Parses and validates the plan (unknown fields are errors), maintenance windows that cross midnight |
| `internal/rollout` | The controller. Rollout progress in a status ConfigMap, each node's step in its annotations: no state in memory, so a restart resumes where it left off |
| `internal/agent` | Client for the node agent: which version is installed, install a version, GPU health (XID, ECC) |
| `internal/k8s` | Kubernetes API calls with the standard library: nodes, evictions, Jobs, events, ConfigMaps |
| `cmd/node-agent` | Lab stand-in for the driver installer on each node, with versions that fail validation or raise XID 79 after install |
| `cmd/gpulifecycle` | The controller binary, with Prometheus metrics |

## Safety controls

- **Canary first:** the first node(s) are upgraded alone. If the canary fails, the rollout halts and no other node is touched.
- **Human gate:** with `pauseAfterCanary`, the rollout waits until someone approves that rollout by name.
- **Disruption budget:** never more than `maxUnavailable` nodes out of service, counting nodes cordoned for other reasons (repair, quarantine, someone else's maintenance).
- **Evictions, not deletions:** every pod goes through the Eviction API, so the workloads' PodDisruptionBudgets decide how fast a node drains.
- **Prove it, then watch it:** after the install, a validation Job pinned to the node takes every GPU; then the node is watched for a soak period before it goes back into service. A hardware XID or an uncorrectable ECC error during the soak fails the upgrade.
- **Automatic rollback:** a failed node reinstalls the version it had and is validated again. If that also fails, it is quarantined for a human (with the same label repo 09 uses).
- **Failure budget:** after the canary, the rollout halts once more nodes have failed than the plan allows.
- **Maintenance window, pause and abort:** new nodes only start inside the window and while the rollout is running. Nodes already in progress always finish their cycle, so nothing is left half-upgraded.
- **Hands off what isn't ours:** quarantined nodes, nodes under remediation and nodes cordoned by someone else are skipped.

## Run it

Needs Docker, [kind](https://kind.sigs.k8s.io/) and kubectl.

```bash
bash scripts/up.sh      # kind cluster with 4 "GPU" workers, node agent, controller, workload + PDB
bash scripts/test.sh    # the scenarios below
bash scripts/down.sh
```

Run a rollout yourself:

```bash
bash scripts/rollout.sh start examples/r580.json
bash scripts/rollout.sh status      # phase, message and each node's step
bash scripts/rollout.sh approve     # after the canary
bash scripts/rollout.sh pause       # or resume / abort
bash scripts/rollout.sh inventory   # driver version per node
```

## What the tests prove

`scripts/test.sh` runs in GitHub Actions on every push:

| # | Scenario | Expected |
|---|---|---|
| 1 | Maintenance window closed | `WaitingForWindow`, no node touched, inventory labels on every node |
| 2 | Driver that fails validation | Canary rolled back to the old version and back in service; rollout `Halted`; other nodes untouched; failed validation Job kept for diagnosis |
| 3 | Good driver, canary + approval, batches of 2 | Waits for approval after the canary; never more than 2 nodes cordoned; all 4 nodes upgraded, schedulable, workload 4/4 |
| 4 | Driver that raises XID 79 during the soak | Canary passed validation but fails the soak, rolled back, rollout `Halted`, reason names the XID |
| 5 | Controller metrics | Halts, rollbacks and completed nodes counted |

Plus unit tests for the plan parser and windows, the controller (with a fake cluster and a fake agent: canary, batches, approval, windows, pause/abort, PDB waits, failure budget, rollback and quarantine), the API client and the agent.

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md): the loop, the state it keeps and where, failure handling
- [docs/plan.md](docs/plan.md): every plan field, phases and controls
- [docs/decisions.md](docs/decisions.md): design decisions and trade-offs
- [docs/runbook.md](docs/runbook.md): running a rollout, what to do when one halts, troubleshooting

## Roadmap

- Use the fleet metrics from repo 10 (Prometheus) as an extra soak signal: utilisation drop, power, NVLink errors.
- Pick canaries deliberately (one per hardware generation or rack) instead of the first node by name.
- A real installer: the GPU Operator's driver DaemonSet with a pinned version per node pool.
- Leader election so the controller can run with more than one replica.

## Customisation and contact

Want to talk about GPU fleet operations, safe rollouts on Kubernetes or a lab like this for your team? Get in touch:

- Email: [inigogonzalezgarcia@yahoo.es](mailto:inigogonzalezgarcia@yahoo.es)
- LinkedIn: [linkedin.com/in/igonzalez93](https://www.linkedin.com/in/igonzalez93)

## License

MIT
