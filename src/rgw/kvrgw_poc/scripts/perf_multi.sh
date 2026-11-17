#!/bin/bash
# Run N independent perf-driver instances in parallel against the same FDB cluster.
#
#   ./perf_multi.sh [N]          (default N=3)
#
# Every instance reads the same config (.env next to this script, see run_gateway.sh;
# PERF_* = workload, KVRGW_* = batch/tier settings) and gets
#   - its own tenant  <prefix>-<run>-<i>   => its own perf-bucket-* set (buckets are
#                                            named per tenant, so none are shared)
#   - its own KVRGW_INSTANCE_ID <i>        => distinct object names
# Output per instance goes to $LOGDIR/instance-<i>.log; a summary is printed at the end.
#
# Environment: PERF_TENANT_PREFIX (default perf), STAGGER (seconds between launches, default 2), RUN_ID, LOGDIR, ENV_FILE, plus any
# run_gateway.sh override (CLUSTER_FILE, DOCKER, ...) or PERF_* value (overrides .env).
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GW="$HERE/run_gateway.sh"
N="${1:-3}"
[[ "$N" =~ ^[1-9][0-9]*$ ]] || { echo "usage: $0 [N]" >&2; exit 2; }

RUN="${RUN_ID:-$(date +%Y%m%d%H%M%S)}"
PREFIX="${PERF_TENANT_PREFIX:-perf}"
STAGGER="${STAGGER:-2}"
LOGDIR="${LOGDIR:-$PWD/perf-logs/$RUN}"
DOCKER="${DOCKER:-docker}"
mkdir -p "$LOGDIR"

# Load the image once rather than once per instance.
"$GW" load >/dev/null
export SKIP_LOAD=1

pids=()
names=()
cleanup() {
  trap - INT TERM
  echo "interrupted; stopping instances" >&2
  for n in "${names[@]}"; do "$DOCKER" rm -f "$n" >/dev/null 2>&1 || true; done
  kill "${pids[@]}" 2>/dev/null || true
  exit 130
}
trap cleanup INT TERM

echo "run $RUN: $N instance(s), logs in $LOGDIR"
for ((i = 0; i < N; i++)); do
  name="kvrgw-perf-$RUN-$i"
  tenant="$PREFIX-$RUN-$i"
  names+=("$name")
  KVRGW_INSTANCE_ID="$i" KVRGW_TENANT_NAME="$tenant" PERF_CONTAINER_NAME="$name" \
    "$GW" perf-run > "$LOGDIR/instance-$i.log" 2>&1 &
  pids+=($!)
  echo "  instance $i: tenant $tenant"
  # Startup allocates ids from shared FDB counters with only 3 retries; staggering avoids conflicts.
  if ((i < N - 1)); then sleep "$STAGGER"; fi
done

rcs=()
for pid in "${pids[@]}"; do
  rc=0
  wait "$pid" || rc=$?
  rcs+=("$rc")
done

printf '\n%-9s %-4s %12s %8s %8s %10s\n' instance rc ops errors retries IOPS
total_ops=0
total_iops=0
total_err=0
for ((i = 0; i < N; i++)); do
  log="$LOGDIR/instance-$i.log"
  line="$(awk '/^=== PUT/ {p=1; next} p && /ops=/ {print; exit}' "$log")"
  if [[ "$line" =~ ops=([0-9]+)\ errors=([0-9]+)\ retries=([0-9]+)\ IOPS=([0-9]+) ]]; then
    ops="${BASH_REMATCH[1]}"; err="${BASH_REMATCH[2]}"
    ret="${BASH_REMATCH[3]}"; iops="${BASH_REMATCH[4]}"
    total_ops=$((total_ops + ops)); total_err=$((total_err + err)); total_iops=$((total_iops + iops))
    printf '%-9s %-4s %12s %8s %8s %10s\n' "$i" "${rcs[$i]}" "$ops" "$err" "$ret" "$iops"
  else
    printf '%-9s %-4s %s\n' "$i" "${rcs[$i]}" "no PUT result (see $log)"
  fi
done
printf '%-9s %-4s %12s %8s %8s %10s\n' total "" "$total_ops" "$total_err" "" "$total_iops"
echo "logs: $LOGDIR"
