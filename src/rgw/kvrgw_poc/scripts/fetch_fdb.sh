#!/usr/bin/env bash

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DEST="${ROOT}/third_party/fdb"
VER="${FDB_VERSION:-7.3.77}"

if [[ "$(uname -m)" != x86_64 ]]; then
  echo "ERROR: fetch_fdb.sh currently supports x86_64 only" >&2
  exit 1
fi
if [[ ! "${VER}" =~ ^7\.3\.[0-9]+$ ]]; then
  echo "ERROR: FDB_VERSION must be a 7.3.x release (got ${VER})" >&2
  exit 1
fi
for cmd in curl sha256sum rpm2cpio cpio; do
  if ! command -v "${cmd}" >/dev/null 2>&1; then
    echo "ERROR: ${cmd} not found; install curl, coreutils, rpm2cpio and cpio (rpm-build on Rocky Linux)" >&2
    exit 1
  fi
done

mkdir -p "${ROOT}/third_party"
WORK="$(mktemp -d "${ROOT}/third_party/.fdb-fetch.XXXXXX")"
trap 'rm -rf -- "${WORK}"' EXIT
cd "${WORK}"
mkdir extracted

for package in clients server; do
  rpm="foundationdb-${package}-${VER}-1.el7.x86_64.rpm"
  url="https://github.com/apple/foundationdb/releases/download/${VER}/${rpm}"
  echo "Downloading ${rpm}..."
  curl -fSL --retry 3 "${url}" -o "${rpm}"
  curl -fsSL --retry 3 "${url}.sha256" -o "${rpm}.sha256"
  sha256sum -c "${rpm}.sha256"
  rpm2cpio "${rpm}" > package.cpio
  (cd extracted && cpio -idmu < ../package.cpio)
done

test -f extracted/usr/include/foundationdb/fdb_c.h
test -f extracted/usr/lib64/libfdb_c.so
test -x extracted/usr/bin/fdbcli
test -x extracted/usr/sbin/fdbserver
mkdir -p "${DEST}"
cp -a extracted/usr "${DEST}/"
echo "FoundationDB ${VER} client and server installed in ${DEST}"
