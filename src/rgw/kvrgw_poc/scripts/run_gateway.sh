#!/bin/bash
# Run the kv-rgw gateway container on a machine that already has an FDB cluster.
# Copy this script and kvrgw-gw.tar.gz to the machine; no repo checkout needed.
#
#   ./run_gateway.sh start|stop|logs|load|perf|perf-run
#
# Environment overrides:
#   TARBALL        image archive to load      (default: kvrgw-gw.tar.gz next to this script)
#   IMAGE          image name                 (default: kvrgw-gw)
#   NAME           container name             (default: kvrgw)
#   GW_PORT        gateway listen port        (default: 9080)
#   CLUSTER_FILE   host FDB cluster file      (default: /etc/foundationdb/fdb.cluster)
#   DATA_DIR       host dir for object data   (default: /var/lib/kvrgw; node-local)
#   ROOT_ACCESS_KEY / ROOT_SECRET_KEY   S3 credentials (default: test / test)
#   DOCKER_RUN_ARGS   extra `docker run` args (default: --security-opt seccomp=unconfined,
#                     needed for glibc 2.34+ images on Docker < 20.10.10; set to "" to disable)
#   ENV_FILE       defaults file (default: .env next to this script; KEY=VALUE lines,
#                  real environment variables take precedence)
#   KVRGW_TENANT_NAME, KVRGW_GOMAXPROCS, KVRGW_DEBUG, ...   passed through if set
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# Defaults from ENV_FILE (KEY=VALUE lines, parsed not sourced). Anything already
# set in the environment wins, including an explicitly empty value.
ENV_FILE="${ENV_FILE:-$HERE/.env}"
if [[ -f "$ENV_FILE" ]]; then
  while IFS= read -r line || [[ -n "$line" ]]; do
    line="${line%$'\r'}"
    [[ "$line" =~ ^[[:space:]]*([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]] || continue
    key="${BASH_REMATCH[1]}"
    val="${BASH_REMATCH[2]}"
    [[ -z "${!key+x}" ]] && export "$key=$val"
  done < "$ENV_FILE"
fi

TARBALL="${TARBALL:-$HERE/kvrgw-gw.tar.gz}"
IMAGE="${IMAGE:-kvrgw-gw}"
NAME="${NAME:-kvrgw}"
GW_PORT="${GW_PORT:-9080}"
CLUSTER_FILE="${CLUSTER_FILE:-/etc/foundationdb/fdb.cluster}"
DATA_DIR="${DATA_DIR:-/var/lib/kvrgw}"
DOCKER="${DOCKER:-docker}"
# Unset uses the default; an explicitly empty value disables it.
DOCKER_RUN_ARGS="${DOCKER_RUN_ARGS---security-opt seccomp=unconfined}"

# Variables forwarded into the container when non-empty.
PASS_VARS=(ROOT_ACCESS_KEY ROOT_SECRET_KEY KVRGW_TENANT_NAME KVRGW_GOMAXPROCS KVRGW_DEBUG
           AWS_DEFAULT_REGION KVRGW_MAX_INLINE KVRGW_MAX_KV_STORE KVRGW_KV_STORE_COALESCING
           KVRGW_BATCH_SIZE KVRGW_BATCH_TIMEOUT_US KVRGW_BATCH_THREADS KVRGW_BATCH_MAX_INFLIGHT
           KVRGW_INSTANCE_ID KVRGW_PREFIX_LEN KVRGW_SUFFIX_LEN KVRGW_KEY_PREFIX KVRGW_KEY_SUFFIX
           KVRGW_RANDOM_PREFIX_PER_KEY KVRGW_TAG_COUNT KVRGW_TAG_NAME_BASE KVRGW_TAG_DATA_SIZE
           KVRGW_METADATA_COUNT KVRGW_METADATA_DATA_SIZE KVRGW_METRICS_FILE KVRGW_METRICS_INTERVAL_MS)

# Always (re)load the tarball when present so a redeploy replaces a stale image
# of the same name; unchanged layers are reused, so this is cheap.
load_image() {
  [[ -n "${SKIP_LOAD:-}" ]] && return 0
  if [[ -f "$TARBALL" ]]; then
    gunzip -c "$TARBALL" | "$DOCKER" load
  else
    "$DOCKER" image inspect "$IMAGE" >/dev/null 2>&1 \
      || { echo "no $TARBALL and no local image $IMAGE" >&2; exit 1; }
  fi
}

# Run the in-process perf driver in a throwaway container. Commands come from
# stdin; a TTY is allocated only when stdin is one.
perf_container() {
  [[ -f "$CLUSTER_FILE" ]] || { echo "no FDB cluster file at $CLUSTER_FILE" >&2; exit 1; }
  load_image
  local env_args=() tty_args=(-i) v
  [[ -t 0 ]] && tty_args=(-it)
  for v in "${PASS_VARS[@]}"; do
    [[ -n "${!v:-}" ]] && env_args+=(-e "$v=${!v}")
  done
  "$DOCKER" run --rm "${tty_args[@]}" --init ${DOCKER_RUN_ARGS:-} --network host \
    ${PERF_CONTAINER_NAME:+--name "$PERF_CONTAINER_NAME"} \
    ${PERF_CPUSET:+--cpuset-cpus "$PERF_CPUSET"} \
    -v "$CLUSTER_FILE:/etc/foundationdb/fdb.cluster:ro" \
    ${PERF_METRICS_DIR:+-v "$PERF_METRICS_DIR:/metrics"} \
    "${env_args[@]}" \
    --entrypoint /opt/kvrgw/kv-rgw-backend "$IMAGE" --perf /tmp/kvrgw-perf.sock /tmp/perf-data
}

# The perf driver command sequence, from PERF_* settings (see .env).
perf_commands() {
  echo "create-buckets buckets=${PERF_BUCKETS:-4} mode=${PERF_MODE:-versioned}"
  if [[ -n "${KVRGW_BATCH_SIZE:-}" && -n "${KVRGW_BATCH_TIMEOUT_US:-}" && -n "${KVRGW_BATCH_THREADS:-}" ]]; then
    echo "set-batch size=$KVRGW_BATCH_SIZE timeout=$KVRGW_BATCH_TIMEOUT_US threads=$KVRGW_BATCH_THREADS"
  fi
  echo "set-sim-disk-write-us ${PERF_SIM_DISK_WRITE_US:-0}"
  echo "set-sim-disk-read-us ${PERF_SIM_DISK_READ_US:-0}"
  echo "put c=${PERF_C:-128} tiers=${PERF_TIERS:-8192} duration=${PERF_DURATION:-180} burst=${PERF_BURST:-5}"
  if [[ "${PERF_CLEANUP:-no}" == "yes" ]]; then
    echo "delete c=${PERF_C:-128}"
    echo "delete-buckets"
  fi
  echo "quit"
}

case "${1:-start}" in
  stop)
    "$DOCKER" rm -f "$NAME"
    ;;
  logs)
    "$DOCKER" logs -f "$NAME"
    ;;
  start)
    [[ -f "$CLUSTER_FILE" ]] || { echo "no FDB cluster file at $CLUSTER_FILE" >&2; exit 1; }
    load_image
    mkdir -p "$DATA_DIR"
    "$DOCKER" rm -f "$NAME" >/dev/null 2>&1 || true

    env_args=()
    for v in "${PASS_VARS[@]}"; do
      [[ -n "${!v:-}" ]] && env_args+=(-e "$v=${!v}")
    done

    # Host networking: the client must reach the FDB coordinators and
    # fdbservers, and the gateway port is then published without NAT.
    "$DOCKER" run -d --name "$NAME" --restart unless-stopped --init \
      ${DOCKER_RUN_ARGS:-} \
      --network host \
      -v "$CLUSTER_FILE:/etc/foundationdb/fdb.cluster:ro" \
      -v "$DATA_DIR:/data" \
      "${env_args[@]}" \
      "$IMAGE" -addr ":$GW_PORT"
    echo "gateway on :$GW_PORT (logs: $0 logs)"
    ;;
  load)
    load_image
    ;;
  perf)
    # Interactive in-process perf driver (no S3/HTTP; writes real data into FDB
    # under KVRGW_TENANT_NAME). Try: create-buckets buckets=4 mode=none / put c=128 tiers=4000 duration=60
    perf_container
    ;;
  perf-run)
    # Non-interactive: run the PERF_* workload from .env once (see scripts/perf_multi.sh).
    perf_commands | perf_container
    ;;
  *)
    echo "usage: $0 start|stop|logs|load|perf|perf-run" >&2
    exit 2
    ;;
esac
