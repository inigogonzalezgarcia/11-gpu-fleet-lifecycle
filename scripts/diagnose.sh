#!/usr/bin/env bash
# What to look at when the lab misbehaves. Never fails.
cd "$(dirname "$0")/.." || exit 1
set -x
bash scripts/rollout.sh status
kubectl get pods -A -o wide
kubectl get pdb -A
kubectl -n gpu-lifecycle get jobs,pods
kubectl -n gpu-lifecycle logs deploy/gpu-lifecycle --tail=200
kubectl -n gpu-node-agent logs ds/node-agent --tail=50 --all-containers --prefix
kubectl get events -n default --sort-by=.lastTimestamp | tail -60
true
