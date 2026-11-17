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
#   LIVE=1           (default) print live aggregate throughput while the run is going; rates
#                    come from the drivers' metrics files ($LOGDIR/metrics-<i>.csv: cumulative
#                    counters sampled on a shared wall clock). LIVE=0 turns the display off.
#   LIVE_INTERVAL=2  seconds between live lines; the series is also written to $LOGDIR/live.csv
#   KVRGW_METRICS_INTERVAL_MS=1000  driver sampling period; 0 disables metrics and the live view
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
LIVE="${LIVE:-1}"
LIVE_INTERVAL="${LIVE_INTERVAL:-2}"
METRICS_MS="${KVRGW_METRICS_INTERVAL_MS:-1000}"
[[ "$LIVE_INTERVAL" =~ ^[1-9][0-9]*$ ]] || { echo "LIVE_INTERVAL must be a positive integer" >&2; exit 2; }
[[ "$METRICS_MS" =~ ^[0-9]+$ ]] || { echo "KVRGW_METRICS_INTERVAL_MS must be a non-negative integer" >&2; exit 2; }
METRICS=1
((METRICS_MS == 0)) && METRICS=0
mkdir -p "$LOGDIR"

# Load the image once rather than once per instance.
"$GW" load >/dev/null
export SKIP_LOAD=1

pids=()
names=()
sampler_pid=""

# Live view: per instance, the rate between the last two samples (distinct timestamps) of
# its metrics file; a decrease means the driver reset its counters between commands, so
# that instance is skipped for the tick. Prints: total reporting min max errors.
live_sample() {
  local i now_ms
  now_ms=$(date +%s%3N)
  for ((i = 0; i < N; i++)); do
    [[ -s "$LOGDIR/metrics-$i.csv" ]] || { echo "-"; continue; }
    tail -n 64 "$LOGDIR/metrics-$i.csv" | awk -F, -v now="$now_ms" -v stale="$((LIVE_INTERVAL * 3000 + METRICS_MS))" '
      $1 ~ /^[0-9]+$/ {
        if ($1 != t) { pt = t; for (k in po) delete po[k]; for (k in o) po[k] = o[k]; pe = e; t = $1; delete o; e = 0 }
        o[$3] = $4; e += $5
      }
      END {
        if (pt == "" || t == "" || now - t > stale) { print "-"; exit }
        so = 0; po_s = 0
        for (k in o) so += o[k]
        for (k in po) po_s += po[k]
        d = so - po_s; dt = t - pt
        if (d < 0 || dt <= 0) { print "R", e; exit }
        printf "%.1f %d\n", d * 1000 / dt, e
      }'
  done | awk -v n="$N" '
    $1 == "R" { err += $2; next }
    $1 == "-" { next }
    { r++; tot += $1; err += $2; if (r == 1 || $1 < mn) mn = $1; if (r == 1 || $1 > mx) mx = $1 }
    END { printf "%.0f %d %.0f %.0f %d\n", tot, r, mn, mx, err }'
}

live_loop() {
  local start=$SECONDS tot rep mn mx err el
  while sleep "$LIVE_INTERVAL"; do
    el=$((SECONDS - start))
    read -r tot rep mn mx err <<< "$(live_sample)"
    if ((rep == 0)); then
      printf '[%02d:%02d] starting... (%d/%d reporting)\n' $((el / 60)) $((el % 60)) 0 "$N"
      continue
    fi
    printf '[%02d:%02d] total %s ops/s | %d/%d instances | per-instance min %s max %s | errors %s\n' \
      $((el / 60)) $((el % 60)) "$tot" "$rep" "$N" "$mn" "$mx" "$err"
    echo "$el,$tot,$rep,$mn,$mx,$err" >> "$LOGDIR/live.csv"
  done
}

stop_sampler() {
  if [[ -n "$sampler_pid" ]]; then
    kill "$sampler_pid" 2>/dev/null || true
    wait "$sampler_pid" 2>/dev/null || true
    sampler_pid=""
  fi
}

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
  stop_sampler
  for n in "${names[@]}"; do "$DOCKER" rm -f "$n" >/dev/null 2>&1 || true; done
  kill "${pids[@]}" 2>/dev/null || true
  exit 130
}
trap cleanup INT TERM
trap stop_sampler EXIT

echo "run $RUN: $N instance(s), logs in $LOGDIR"
for ((i = 0; i < N; i++)); do
  name="kvrgw-perf-$RUN-$i"
  tenant="$PREFIX-$RUN-$i"
  names+=("$name")
  cpuset=""
  ((${#cpus[@]})) && cpuset="$(cpuset_for "$i")"
  metrics_env=()
  if ((METRICS)); then
    : > "$LOGDIR/metrics-$i.csv"
    metrics_env=(PERF_METRICS_DIR="$LOGDIR" KVRGW_METRICS_FILE="/metrics/metrics-$i.csv")
  fi
  env "${metrics_env[@]}" KVRGW_INSTANCE_ID="$i" KVRGW_TENANT_NAME="$tenant" PERF_CONTAINER_NAME="$name" PERF_CPUSET="$cpuset" \
    "$GW" perf-run > "$LOGDIR/instance-$i.log" 2>&1 &
  pids+=($!)
  echo "  instance $i: tenant $tenant${cpuset:+, cpus $cpuset}"
  # Optional spacing between launches (default 0: all start together).
  if ((i < N - 1 && STAGGER > 0)); then sleep "$STAGGER"; fi
done

if [[ "$LIVE" == 1 ]] && ((METRICS)); then
  echo "elapsed,total_ops_s,reporting,min_ops_s,max_ops_s,errors" > "$LOGDIR/live.csv"
  live_loop &
  sampler_pid=$!
fi

rcs=()
for pid in "${pids[@]}"; do
  rc=0
  wait "$pid" || rc=$?
  rcs+=("$rc")
done
stop_sampler

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
if [[ -s "$LOGDIR/live.csv" ]]; then
  awk -F, 'NR > 1 { n++; s += $2; if ($2 > p) p = $2 }
    END { if (n) printf "live total ops/s: peak %.0f, average %.0f (%d samples)\n", p, s / n, n }' "$LOGDIR/live.csv"
fi
echo "logs: $LOGDIR"
