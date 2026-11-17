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
# Environment: PERF_TENANT_PREFIX (default perf), STAGGER (seconds between launches, default 0), RUN_ID, LOGDIR, ENV_FILE, plus any
# run_gateway.sh override (CLUSTER_FILE, DOCKER, ...) or PERF_* value (overrides .env).
#   PIN=1            pin each instance to its own share of the cores (docker --cpuset-cpus,
#                    the container equivalent of taskset); N > cores shares cores round-robin
#   CPU_LIST=0-15    cores to spread over (default: all online cores, e.g. "0-7,16-23")
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
GW="$HERE/run_gateway.sh"
N="${1:-3}"
[[ "$N" =~ ^[1-9][0-9]*$ ]] || { echo "usage: $0 [N]" >&2; exit 2; }

RUN="${RUN_ID:-$(date +%Y%m%d%H%M%S)}"
PREFIX="${PERF_TENANT_PREFIX:-perf}"
STAGGER="${STAGGER:-0}"
LOGDIR="${LOGDIR:-$PWD/perf-logs/$RUN}"
DOCKER="${DOCKER:-docker}"
mkdir -p "$LOGDIR"

# Load the image once rather than once per instance.
"$GW" load >/dev/null
export SKIP_LOAD=1

pids=()
names=()

# Expand "0-3,8" into one CPU per array element.
cpus=()
if [[ "${PIN:-0}" == 1 ]]; then
  IFS=',' read -ra parts <<< "${CPU_LIST:-$(cat /sys/devices/system/cpu/online)}"
  for p in "${parts[@]}"; do
    if [[ "$p" == *-* ]]; then
      for ((c = ${p%-*}; c <= ${p#*-}; c++)); do cpus+=("$c"); done
    else
      cpus+=("$p")
    fi
  done
fi

# CPUs for instance $1: an even contiguous slice, or one shared core when N > cores.
cpuset_for() {
  local i=$1 M=${#cpus[@]} lo hi out=()
  if ((N <= M)); then
    lo=$((i * M / N)); hi=$(((i + 1) * M / N))
  else
    lo=$((i % M)); hi=$((lo + 1))
  fi
  for ((c = lo; c < hi; c++)); do out+=("${cpus[c]}"); done
  (IFS=,; echo "${out[*]}")
}
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
  cpuset=""
  ((${#cpus[@]})) && cpuset="$(cpuset_for "$i")"
  KVRGW_INSTANCE_ID="$i" KVRGW_TENANT_NAME="$tenant" PERF_CONTAINER_NAME="$name" PERF_CPUSET="$cpuset" \
    "$GW" perf-run > "$LOGDIR/instance-$i.log" 2>&1 &
  pids+=($!)
  echo "  instance $i: tenant $tenant${cpuset:+, cpus $cpuset}"
  # Optional spacing between launches (default 0: all start together).
  if ((i < N - 1 && STAGGER > 0)); then sleep "$STAGGER"; fi
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
