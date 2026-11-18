#!/bin/bash
# Bundle what an external cgo consumer (k11) needs to link the in-process C API:
#
#   scripts/package_capi.sh [outdir]      (default: ./dist)
#
# Run scripts/build_from_tar.sh (e.g. via `make build`) first. Produces
#   <outdir>/kvrgw-capi.tar.gz containing
#     include/kvrgw_c_api.h, lib/libkvrgw.so, lib/libfdb_c.so, kvrgw.yaml
set -euo pipefail

POC="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$(realpath -m "${1:-$POC/dist}")"
FDB_LIB="${FDB_LIB:-$POC/third_party/fdb/usr/lib64/libfdb_c.so}"

for f in "$POC/build/libkvrgw.so" "$POC/backend/include/kvrgw_c_api.h" "$POC/backend/kvrgw.yaml" "$FDB_LIB"; do
  [[ -f "$f" ]] || { echo "missing $f (run build_from_tar.sh / fetch_fdb.sh first)" >&2; exit 1; }
done

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT
mkdir -p "$STAGE/include" "$STAGE/lib"
cp "$POC/backend/include/kvrgw_c_api.h" "$STAGE/include/"
cp "$POC/build/libkvrgw.so" "$STAGE/lib/"
cp "$FDB_LIB" "$STAGE/lib/libfdb_c.so"
cp "$POC/backend/kvrgw.yaml" "$STAGE/"

mkdir -p "$OUT"
tar -C "$STAGE" -czf "$OUT/kvrgw-capi.tar.gz" .
echo "C API bundle: $OUT/kvrgw-capi.tar.gz"
