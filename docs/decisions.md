# Design decisions

## 1. Why rebuild something the GPU Operator already does

In a real cluster, NVIDIA's GPU Operator manages the driver as a DaemonSet and has its own upgrade controller (cordon, drain, reinstall the driver pod, uncordon, a limited number of nodes at a time). I would use that, not this. The point of the lab is to understand the problem that controller solves and the questions around it that are fleet-specific: who goes first, how many nodes can be out, what proves a node is healthy, when to stop, how to go back. Rebuilding a small version is how I learn those trade-offs, and it lets the e2e tests exercise every failure path without a GPU.

## 2. A plan in a ConfigMap, not a CRD

A `GPURollout` custom resource with a status subresource would be the idiomatic shape. A ConfigMap keeps the lab to one binary with no CRD, no generated code and no webhooks, and still gives the same properties: declarative, versionable in Git, RBAC-controlled, readable with `kubectl`. `DisallowUnknownFields` stands in for schema validation. Moving to a CRD later changes the storage, not the logic.

## 3. State in the cluster, not in the controller

Each node's step lives in its annotations, the rollout's progress in a status ConfigMap. The controller can restart at any point (including in the middle of a drain or a validation) and continues from what the cluster says. Same choice as repo 09, and the reason is the same: an upgrade that takes hours will outlive a controller pod.

## 4. One canary, then a human

The canary catches the "this driver does not work on our hardware" class of problem with one node at risk. Halting on any canary failure, whatever the failure budget, keeps that promise simple. The approval is by rollout name so that an old approval can never let a new rollout skip its gate.

In the lab the canary is the first eligible node by name. In a real fleet it should be chosen: one node per GPU model, driver branch or rack, and preferably one with a representative workload.

## 5. maxUnavailable counts everything that is out

A node cordoned by remediation (repo 09), by someone's maintenance or by a quarantine is already capacity lost. Counting those against the rollout's budget means two well-behaved tools cannot together take out more than intended. The price is that a fleet with many broken nodes upgrades slowly or not at all, which is the right default: fix the fleet first.

## 6. Nodes in progress always finish

Pause, abort, halt and a closing window only stop new nodes from starting. A node in the middle of an install is cordoned with a half-changed driver; stopping it there is the worst state to leave it in. Finishing the cycle (or rolling back) puts it back in a known state.

## 7. Validation, then soak

The validation Job proves the node works right after the install: it runs on the node (pinned with `nodeName`, so the cordon does not stop it), takes every GPU and calls the agent's diagnostic. Some faults only appear later, so the node is then watched for `soak` before it counts as done. In the lab the soak checks the agent's XID and ECC counters; with repo 10 running it could also use Prometheus signals.

The node stays cordoned while it soaks. Soaking under real load would catch more problems, but it would also put a workload on a node that may be about to roll back. For the lab, a clean `maxUnavailable` accounting won.

## 8. Roll back automatically, quarantine if that fails too

A failed upgrade reinstalls the version the node had (`from-version` annotation) and validates again. If that works, the node returns to service on the old version and the rollout halts or continues according to the failure budget. If the rollback also fails, something other than the new driver is wrong, so the node is quarantined with repo 09's label and left for a human.

## 9. Standard library only

Same as repos 09 and 10: a small Kubernetes client over `net/http` (nodes, evictions, Jobs, events, ConfigMaps) is enough, easy to fake in tests, and keeps the image tiny. A production controller would use client-go informers and leader election.
