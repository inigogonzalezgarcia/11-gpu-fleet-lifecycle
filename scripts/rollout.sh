#!/usr/bin/env bash
# Operate rollouts through the rollout-plan ConfigMap.
#   scripts/rollout.sh start PLAN.json   submit a plan (a new name starts a new rollout)
#   scripts/rollout.sh status            rollout status and per-node state
#   scripts/rollout.sh approve           let a rollout continue after its canary
#   scripts/rollout.sh pause|resume      stop/allow starting new nodes (nodes in progress finish)
#   scripts/rollout.sh abort             stop the rollout for good
#   scripts/rollout.sh inventory         versions installed on each node
set -euo pipefail
NS=${NS:-gpu-lifecycle}
plan_name() { kubectl -n "$NS" get configmap rollout-plan -o jsonpath='{.data.plan\.json}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["name"])'; }
control() { kubectl -n "$NS" patch configmap rollout-plan --type merge -p "{\"data\":{\"control\":\"$1\"}}" >/dev/null; echo "control=$1"; }
case "${1:-}" in
  start)
    file=${2:?usage: rollout.sh start PLAN.json}
    kubectl -n "$NS" create configmap rollout-plan --from-file=plan.json="$file" --from-literal=control=run \
      --dry-run=client -o yaml | kubectl apply -f - >/dev/null
    echo "submitted $(plan_name)"
    ;;
  status)
    kubectl -n "$NS" get configmap rollout-status -o jsonpath='{.data.status\.json}' 2>/dev/null || echo "no status yet"
    echo
    kubectl get nodes -l nvidia.com/gpu.present=true -o custom-columns='NODE:.metadata.name,VERSION:.metadata.labels.lifecycle\.lab/version,ROLLOUT:.metadata.annotations.lifecycle\.lab/rollout,STATE:.metadata.annotations.lifecycle\.lab/state,UNSCHEDULABLE:.spec.unschedulable'
    ;;
  approve)
    name=$(plan_name)
    kubectl -n "$NS" patch configmap rollout-plan --type merge -p "{\"data\":{\"approve\":\"$name\"}}" >/dev/null
    echo "approved $name"
    ;;
  pause) control pause ;;
  resume) control run ;;
  abort) control abort ;;
  inventory)
    kubectl get nodes -l nvidia.com/gpu.present=true -L lifecycle.lab/version -L gpu-remediation.lab/quarantined
    ;;
  *) sed -n '2,9p' "$0"; exit 2 ;;
esac
