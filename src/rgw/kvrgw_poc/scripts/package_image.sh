#!/bin/bash
# Package the built gateway into a runtime image and a copy-able bundle:
#
#   scripts/package_image.sh [outdir]      (default: ./dist)
#
# Run scripts/build_from_tar.sh (e.g. via `make shell`) first. Produces
#   <outdir>/kvrgw-gw.tar.gz   docker image (docker save | gzip)
#   <outdir>/run_gateway.sh    start script to run on the target machine
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
cp -r "$POC/iam" "$STAGE/iam"

"$DOCKER" build -t "$IMAGE" -f "$POC/Dockerfile.run" "$STAGE"

mkdir -p "$OUT"
"$DOCKER" save "$IMAGE" | gzip > "$OUT/kvrgw-gw.tar.gz"
cp "$POC/scripts/run_gateway.sh" "$OUT/run_gateway.sh"
echo "Bundle in $OUT:"
ls -lh "$OUT"
