#!/bin/bash
# Package the built gateway into a runtime image and a copy-able bundle:
#
#   scripts/package_image.sh [outdir]      (default: ./dist)
#
# Run scripts/build_from_tar.sh (e.g. via `make shell`) first. Produces
#   <outdir>/kvrgw-gw.tar.gz   docker image (docker save | gzip)
#   <outdir>/kvrgw_perf.py     perf runner to run on the target machine
#   <outdir>/kvrgw_perf.ini    its config
# Copy both to the node; nothing else (no repo checkout) is needed there.
set -euo pipefail

POC="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$(realpath -m "${1:-$POC/dist}")"
IMAGE="${IMAGE:-kvrgw-gw}"
DOCKER="${DOCKER:-docker}"
FDB_LIB="${FDB_LIB:-$POC/third_party/fdb/usr/lib64/libfdb_c.so}"

for f in "$POC/build/kv-rgw-frontend" "$POC/build/kv-rgw-backend" "$POC/build/libkvrgw.so" "$FDB_LIB"; do
  [[ -f "$f" ]] || { echo "missing $f (run build_from_tar.sh / fetch_fdb.sh first)" >&2; exit 1; }
done

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
cp "$POC/build/kv-rgw-frontend" "$POC/build/kv-rgw-backend" "$POC/build/libkvrgw.so" "$STAGE/"
cp "$FDB_LIB" "$STAGE/libfdb_c.so"
cp "$POC/backend/kvrgw.yaml" "$STAGE/"

# Optional perf harness: included only if it was built (-DKVRGW_BUILD_PERF_HARNESS=ON).
mkdir "$STAGE/perf-bin"
WITH_PERF=0
FDBCLI="${FDBCLI:-$POC/third_party/fdb/usr/bin/fdbcli}"
if [[ -f "$POC/build/kvrgw-perf" && -f "$POC/build/perf/kvrgw-perf-worker" ]]; then
  [[ -f "$FDBCLI" ]] || { echo "missing $FDBCLI (needed by the perf controller)" >&2; exit 1; }
  cp "$POC/build/kvrgw-perf" "$POC/build/perf/kvrgw-perf-worker" "$FDBCLI" "$STAGE/perf-bin/"
  WITH_PERF=1
  echo "including perf harness in the image"
fi
cp -r "$POC/iam" "$STAGE/iam"

"$DOCKER" build --build-arg WITH_PERF="$WITH_PERF" -t "$IMAGE" -f "$POC/Dockerfile.run" "$STAGE"

mkdir -p "$OUT"
"$DOCKER" save "$IMAGE" | gzip > "$OUT/kvrgw-gw.tar.gz"
cp "$POC/scripts/kvrgw_perf.py" "$POC/scripts/kvrgw_perf.ini" "$OUT/"
echo "Bundle in $OUT:"
ls -lh "$OUT"
