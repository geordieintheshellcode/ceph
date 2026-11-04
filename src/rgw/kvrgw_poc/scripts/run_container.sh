#!/usr/bin/env bash
# Run a self-contained single-node kv-rgw in the build container: fdbserver +
# kv-rgw-frontend (the C++ backend is loaded in-process). The gateway port is
# published to the host at http://127.0.0.1:${GW_PORT}; FDB stays container-internal.
#
# Run on the host from anywhere:   scripts/run_container.sh [--clean]
# Prerequisite: ./scripts/build_from_tar.sh has been run in the build container.
# State (FDB data, object data, logs) lives in <poc>/.run/ (gitignored).
set -euo pipefail

POC="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CEPH_ROOT="$(cd "${POC}/../../.." && pwd)"
IMAGE="${IMAGE:-kvrgw-build}"
DOCKER="${DOCKER:-docker}"
GW_PORT="${GW_PORT:-9080}"
FDB_PORT="${FDB_PORT:-4689}"

if [[ "${1:-}" != "--inner" ]]; then
  CLEAN=0
  [[ "${1:-}" == "--clean" ]] && CLEAN=1
  for f in build/kv-rgw-frontend build/libkvrgw.so; do
    [[ -f "${POC}/${f}" ]] || { echo "missing ${POC}/${f}: run build_from_tar.sh first" >&2; exit 1; }
  done
  "${DOCKER}" image inspect "${IMAGE}" >/dev/null 2>&1 \
    || "${DOCKER}" build -t "${IMAGE}" - < "${POC}/Dockerfile.build"
  TTY=(); [[ -t 0 ]] && TTY=(-it)
  exec "${DOCKER}" run --rm "${TTY[@]}" --init \
    -p "127.0.0.1:${GW_PORT}:${GW_PORT}" \
    --user "$(id -u):$(id -g)" \
    --mount "type=bind,source=${CEPH_ROOT},target=/workspace" \
    -e GW_PORT="${GW_PORT}" -e FDB_PORT="${FDB_PORT}" -e CLEAN="${CLEAN}" \
    "${IMAGE}" bash /workspace/src/rgw/kvrgw_poc/scripts/run_container.sh --inner
fi

# ---- inside the container ----
POC=/workspace/src/rgw/kvrgw_poc
RUN="${POC}/.run"
FDB_BIN="${POC}/third_party/fdb/usr"

if [[ "${CLEAN:-0}" == 1 ]]; then
  rm -rf "${RUN}"
fi
mkdir -p "${RUN}/fdb/data" "${RUN}/fdb/logs" "${RUN}/objects"

if [[ ! -x "${FDB_BIN}/sbin/fdbserver" ]]; then
  "${POC}/scripts/fetch_fdb.sh"
fi

export LD_LIBRARY_PATH="${POC}/build:${FDB_BIN}/lib64"
CLUSTER_FILE="${RUN}/fdb/fdb.cluster"
[[ -f "${CLUSTER_FILE}" ]] || echo "kvrgw:kvrgw@127.0.0.1:${FDB_PORT}" > "${CLUSTER_FILE}"

# FDB's ratekeeper throttles writes to zero when free disk space drops below
# 5% (and starts throttling below 30%). That is easily hit on a dev machine
# with a mostly-full disk, so relax the thresholds for this local POC.
# Override with FDB_EXTRA_ARGS="" to use FDB's defaults.
FDB_EXTRA_ARGS="${FDB_EXTRA_ARGS---knob_min_available_space_ratio=0.01 --knob_target_available_space_ratio=0.02}"

# shellcheck disable=SC2086
"${FDB_BIN}/sbin/fdbserver" -p "127.0.0.1:${FDB_PORT}" -C "${CLUSTER_FILE}" \
  -d "${RUN}/fdb/data" -L "${RUN}/fdb/logs" ${FDB_EXTRA_ARGS} > "${RUN}/fdb/server.out" 2>&1 &
FDB_PID=$!
trap 'kill "${FDB_PID}" 2>/dev/null || true' EXIT

# Configure only a brand-new database (marker file); never interrupt it, as a
# killed "configure new" leaves the cluster unrecoverable.
if [[ ! -f "${RUN}/fdb/.configured" ]]; then
  "${FDB_BIN}/bin/fdbcli" -C "${CLUSTER_FILE}" --exec "configure new single ssd" \
    && touch "${RUN}/fdb/.configured"
fi
"${FDB_BIN}/bin/fdbcli" -C "${CLUSTER_FILE}" --exec "status minimal" | grep -q "available" \
  || { echo "fdb not available; see ${RUN}/fdb/server.out" >&2; exit 1; }

export FDB_CLUSTER_FILE="${CLUSTER_FILE}"
export KVRGW_DATA="${RUN}/objects"
export KVRGW_ADMIN_SOCKET="${RUN}/admin.sock"
export KVRGW_IAM_DIR="${KVRGW_IAM_DIR:-${POC}/iam}"
export KVRGW_CONFIG_FILE="${KVRGW_CONFIG_FILE:-${POC}/backend/kvrgw.yaml}"

echo "Gateway: http://127.0.0.1:${GW_PORT}  (access key/secret: test/test)"
"${POC}/build/kv-rgw-frontend" -addr ":${GW_PORT}"
