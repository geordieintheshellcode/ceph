#!/bin/bash
# Run the kv-rgw gateway container on a machine that already has an FDB cluster.
# Copy this script and kvrgw-gw.tar.gz to the machine; no repo checkout needed.
#
#   ./run_gateway.sh start|stop|logs
#
# Environment overrides:
#   TARBALL        image archive to load      (default: kvrgw-gw.tar.gz next to this script)
#   IMAGE          image name                 (default: kvrgw-gw)
#   NAME           container name             (default: kvrgw)
#   GW_PORT        gateway listen port        (default: 9080)
#   CLUSTER_FILE   host FDB cluster file      (default: /etc/foundationdb/fdb.cluster)
#   DATA_DIR       host dir for object data   (default: /var/lib/kvrgw; node-local)
#   ROOT_ACCESS_KEY / ROOT_SECRET_KEY   S3 credentials (default: test / test)
#   KVRGW_TENANT_NAME, KVRGW_GOMAXPROCS, KVRGW_DEBUG, ...   passed through if set
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TARBALL="${TARBALL:-$HERE/kvrgw-gw.tar.gz}"
IMAGE="${IMAGE:-kvrgw-gw}"
NAME="${NAME:-kvrgw}"
GW_PORT="${GW_PORT:-9080}"
CLUSTER_FILE="${CLUSTER_FILE:-/etc/foundationdb/fdb.cluster}"
DATA_DIR="${DATA_DIR:-/var/lib/kvrgw}"
DOCKER="${DOCKER:-docker}"

case "${1:-start}" in
  stop)
    "$DOCKER" rm -f "$NAME"
    ;;
  logs)
    "$DOCKER" logs -f "$NAME"
    ;;
  start)
    [[ -f "$CLUSTER_FILE" ]] || { echo "no FDB cluster file at $CLUSTER_FILE" >&2; exit 1; }
    if ! "$DOCKER" image inspect "$IMAGE" >/dev/null 2>&1; then
      gunzip -c "$TARBALL" | "$DOCKER" load
    fi
    mkdir -p "$DATA_DIR"
    "$DOCKER" rm -f "$NAME" >/dev/null 2>&1 || true

    env_args=()
    for v in ROOT_ACCESS_KEY ROOT_SECRET_KEY KVRGW_TENANT_NAME KVRGW_GOMAXPROCS \
             KVRGW_DEBUG AWS_DEFAULT_REGION KVRGW_MAX_INLINE KVRGW_MAX_KV_STORE \
             KVRGW_KV_STORE_COALESCING KVRGW_BATCH_SIZE KVRGW_BATCH_TIMEOUT_US \
             KVRGW_BATCH_THREADS KVRGW_BATCH_MAX_INFLIGHT; do
      [[ -n "${!v:-}" ]] && env_args+=(-e "$v=${!v}")
    done

    # Host networking: the client must reach the FDB coordinators and
    # fdbservers, and the gateway port is then published without NAT.
    "$DOCKER" run -d --name "$NAME" --restart unless-stopped --init \
      --network host \
      -v "$CLUSTER_FILE:/etc/foundationdb/fdb.cluster:ro" \
      -v "$DATA_DIR:/data" \
      "${env_args[@]}" \
      "$IMAGE" -addr ":$GW_PORT"
    echo "gateway on :$GW_PORT (logs: $0 logs)"
    ;;
  *)
    echo "usage: $0 start|stop|logs" >&2
    exit 2
    ;;
esac
