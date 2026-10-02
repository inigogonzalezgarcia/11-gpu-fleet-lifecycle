#!/usr/bin/env bash
# Helpers shared by the lab scripts.
# shellcheck source=scripts/versions.env
source "$(dirname "${BASH_SOURCE[0]}")/versions.env"

step() { printf '\n==> %s\n' "$*"; }
pass() { printf '  PASS  %s\n' "$*"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL  %s\n' "$*"; FAILED=$((FAILED + 1)); }
PASSED=0
FAILED=0

gpu_nodes() { kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort; }

# status_field FIELD -> value from status.json (lists joined with commas)
status_field() {
  kubectl -n "$NS" get configmap rollout-status -o jsonpath='{.data.status\.json}' 2>/dev/null | python3 -c '
import json, sys
try:
    v = json.load(sys.stdin).get(sys.argv[1], "")
except Exception:
    v = ""
print(",".join(v) if isinstance(v, list) else v)' "$1"
}

phase() { status_field phase; }
rollout() { status_field rollout; }
node_state() { kubectl get node "$1" -o jsonpath='{.metadata.annotations.lifecycle\.lab/state}'; }
node_version() { kubectl get node "$1" -o jsonpath='{.metadata.labels.lifecycle\.lab/version}'; }
node_reason() { kubectl get node "$1" -o jsonpath='{.metadata.annotations.lifecycle\.lab/reason}'; }
schedulable() { [ "$(kubectl get node "$1" -o jsonpath='{.spec.unschedulable}')" != "true" ]; }
cordoned_count() { kubectl get nodes -l nvidia.com/gpu.present=true -o jsonpath='{range .items[*]}{.spec.unschedulable}{"\n"}{end}' | grep -c true || true; }

# wait_phase ROLLOUT PHASE TIMEOUT -> waits until that rollout reports that phase
wait_phase() {
  local name=$1 want=$2 timeout=${3:-300} i
  for ((i = 0; i < timeout; i += 3)); do
    [ "$(rollout)" = "$name" ] && [ "$(phase)" = "$want" ] && return 0
    sleep 3
  done
  echo "  timed out waiting for $name to be $want (is $(rollout) $(phase): $(status_field message))"
  return 1
}

controller_metrics() {
  kubectl get --raw "/api/v1/namespaces/$NS/services/gpu-lifecycle:metrics/proxy/metrics"
}
