#!/usr/bin/env bash
# End-to-end scenarios against the lab built by scripts/up.sh. Exits non-zero if any check fails.
#   1. Maintenance window closed: nothing starts.
#   2. Bad driver: the canary fails validation, is rolled back, the rollout halts.
#   3. Good driver: canary, approval, batches of 2, never more than 2 nodes out, all nodes upgraded.
#   4. Driver that raises XID 79 during the soak: the canary is rolled back, the rollout halts.
#   5. Controller metrics.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
# shellcheck source=scripts/lib.sh
source scripts/lib.sh
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

mapfile -t NODES < <(gpu_nodes)
CANARY=${NODES[0]}
OTHERS=("${NODES[@]:1}")
echo "GPU nodes: ${NODES[*]} (canary: ${CANARY})"

untouched() { # untouched NODE VERSION -> node still schedulable, on VERSION, not part of a rollout step
  local n=$1 v=$2
  schedulable "$n" && [ "$(node_version "$n")" = "$v" ] && [ -z "$(node_state "$n")" ]
}

step "1. Maintenance window closed"
python3 - "$WORK/closed.json" <<'PY'
import datetime, json, sys
h = (datetime.datetime.now(datetime.timezone.utc).hour + 3) % 24
plan = json.load(open("examples/r580.json"))
plan.update(name="driver-r580-closed", window={"start": f"{h:02d}:00", "end": f"{(h + 1) % 24:02d}:00"})
json.dump(plan, open(sys.argv[1], "w"))
PY
bash scripts/rollout.sh start "$WORK/closed.json"
if wait_phase driver-r580-closed WaitingForWindow 60; then pass "phase is WaitingForWindow"; else fail "phase is WaitingForWindow"; fi
sleep 10
ok=1
for n in "${NODES[@]}"; do untouched "$n" r570-lab || ok=0; done
if [ $ok = 1 ]; then pass "no node touched while the window is closed"; else fail "no node touched while the window is closed"; fi
if [ "$(kubectl get nodes -l lifecycle.lab/version=r570-lab --no-headers | wc -l)" = "${#NODES[@]}" ]; then
  pass "inventory label lifecycle.lab/version=r570-lab on every GPU node"
else fail "inventory label lifecycle.lab/version=r570-lab on every GPU node"; fi

step "2. Bad driver: canary fails validation"
bash scripts/rollout.sh start examples/r580-bad.json
if wait_phase driver-r580-bad Halted 240; then pass "rollout halted"; else fail "rollout halted"; fi
if [ "$(node_state "$CANARY")" = RolledBack ]; then pass "canary ${CANARY} is RolledBack"; else fail "canary state is $(node_state "$CANARY")"; fi
if [ "$(node_version "$CANARY")" = r570-lab ] && schedulable "$CANARY"; then
  pass "canary back on r570-lab and schedulable"
else fail "canary back on r570-lab and schedulable"; fi
ok=1
for n in "${OTHERS[@]}"; do untouched "$n" r570-lab || ok=0; done
if [ $ok = 1 ]; then pass "no other node touched"; else fail "no other node touched"; fi
if [ "$(kubectl -n "$NS" get jobs -l "lifecycle.lab/node=${CANARY}" -o jsonpath='{range .items[*]}{.status.failed}{"\n"}{end}' | grep -c '^1$')" -ge 1 ]; then
  pass "failed validation Job kept for diagnosis"
else fail "failed validation Job kept for diagnosis"; fi

step "3. Good driver: canary, approval, batches"
bash scripts/rollout.sh start examples/r580.json
if wait_phase driver-r580 AwaitingApproval 240; then pass "canary passed, waiting for approval"; else fail "canary passed, waiting for approval"; fi
if [ "$(node_version "$CANARY")" = r580-lab ] && schedulable "$CANARY"; then pass "canary on r580-lab and back in service"; else fail "canary on r580-lab and back in service"; fi
sleep 6
ok=1
for n in "${OTHERS[@]}"; do untouched "$n" r570-lab || ok=0; done
if [ $ok = 1 ]; then pass "nothing else starts before approval"; else fail "nothing else starts before approval"; fi
bash scripts/rollout.sh approve
max=0
complete=0
for ((i = 0; i < 400; i += 2)); do
  c=$(cordoned_count)
  [ "$c" -gt "$max" ] && max=$c
  if [ "$(rollout)" = driver-r580 ] && [ "$(phase)" = Complete ]; then complete=1; break; fi
  sleep 2
done
if [ $complete = 1 ]; then pass "rollout Complete"; else fail "rollout Complete (is $(phase): $(status_field message))"; fi
if [ "$max" -le 2 ]; then pass "at most 2 nodes cordoned at once (saw ${max})"; else fail "at most 2 nodes cordoned at once (saw ${max})"; fi
if [ "$max" -eq 2 ]; then pass "batches of 2 ran in parallel"; else fail "batches of 2 ran in parallel (saw ${max})"; fi
ok=1
for n in "${NODES[@]}"; do
  [ "$(node_version "$n")" = r580-lab ] && schedulable "$n" && [ "$(node_state "$n")" = Done ] || ok=0
done
if [ $ok = 1 ]; then pass "all GPU nodes on r580-lab, Done and schedulable"; else fail "all GPU nodes on r580-lab, Done and schedulable"; fi
if kubectl -n ml rollout status deploy/trainer --timeout=120s >/dev/null; then pass "trainer 4/4 ready after the rollout"; else fail "trainer 4/4 ready after the rollout"; fi

step "4. Driver that raises XID 79 during the soak"
bash scripts/rollout.sh start examples/r580-xid.json
if wait_phase driver-r580-xid Halted 240; then pass "rollout halted"; else fail "rollout halted"; fi
if [ "$(node_state "$CANARY")" = RolledBack ]; then pass "canary is RolledBack"; else fail "canary state is $(node_state "$CANARY")"; fi
if node_reason "$CANARY" | grep -q "XID 79"; then pass "reason mentions XID 79 ($(node_reason "$CANARY"))"; else fail "reason mentions XID 79 ($(node_reason "$CANARY"))"; fi
if [ "$(node_version "$CANARY")" = r580-lab ] && schedulable "$CANARY"; then pass "canary back on r580-lab and schedulable"; else fail "canary back on r580-lab and schedulable"; fi
ok=1
for n in "${OTHERS[@]}"; do [ "$(node_version "$n")" = r580-lab ] && schedulable "$n" || ok=0; done
if [ $ok = 1 ]; then pass "no other node touched"; else fail "no other node touched"; fi

step "5. Controller metrics"
controller_metrics >"$WORK/metrics" || true
metric() { awk -v k="gpu_lifecycle_actions_total{action=\"$1\"}" '$1 == k {print $2}' "$WORK/metrics"; }
for want in halt:2 rollback:2 rolled_back:2 done:4; do
  a=${want%%:*} n=${want##*:} got=$(metric "${want%%:*}")
  if [ "${got:-0}" -ge "$n" ]; then pass "actions_total{action=\"$a\"} = ${got} (>= $n)"; else fail "actions_total{action=\"$a\"} = ${got:-missing} (want >= $n)"; fi
done
if grep -q 'gpu_lifecycle_rollout_phase{rollout="driver-r580-xid",target="r580-lab-xid",phase="Halted"} 1' "$WORK/metrics"; then
  pass "rollout_phase reports Halted"
else fail "rollout_phase reports Halted"; fi

printf '\n%d passed, %d failed\n' "$PASSED" "$FAILED"
[ "$FAILED" -eq 0 ]
