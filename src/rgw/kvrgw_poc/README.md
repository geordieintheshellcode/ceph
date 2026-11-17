# kv-rgw POC

S3-compatible object storage over FoundationDB with bucket versioning.

## Features

- Full bucket versioning (Enable / Suspend / Disabled)
- Null version handling (max_uint32), delete markers, version-specific GET/HEAD/DELETE
- ListObjectVersions with pagination
- Byte-range GET, multi-delete, bucket policy
- 63/63 ceph-rgw S3 compatibility tests passing (see `ceph_rgw_tests.md`)

## Prerequisites

- FoundationDB (server + client libraries)
- CMake, GCC/Clang with C++23
- Go 1.25+
- protoc (protobuf compiler)
- nginx

## Build

```bash
bash scripts/build_from_tar.sh
```

This builds the C++ backend and Go frontend.

### Interactive container build

From the Ceph repository root:

```bash
make -C src/rgw/kvrgw_poc shell
```

Or run `make shell` from the POC directory. This builds the image (reusing
Docker's cache) before opening the shell. Use `make docker-image` to build only
the image. Set `IMAGE=...` to override the image name.

The equivalent Docker commands are:

```bash
docker build -t kvrgw-build - < src/rgw/kvrgw_poc/Dockerfile.build
docker run --rm -it \
  --user "$(id -u):$(id -g)" \
  --mount "type=bind,source=$(pwd),target=/workspace" \
  kvrgw-build
```

The container opens a Bash shell in `/workspace/src/rgw/kvrgw_poc`. Run:

```bash
./scripts/build_from_tar.sh
```

The script is named `build_from_tar.sh`, not `build_for_tar.sh`. Sources and
`build/` outputs stay in your checkout, owned by your host user. On SELinux
hosts, replace `--mount ...` with `-v "$(pwd):/workspace:Z"`.

### Packaging a runtime image for another machine

After building, `scripts/package_image.sh` bundles the gateway into a runtime
image (`Dockerfile.run`, Rocky 9, no FDB server) and writes `dist/kvrgw-gw.tar.gz`
plus `dist/run_gateway.sh`. Copy those two files to a machine that has Docker
and an existing FDB cluster, then run `./run_gateway.sh start`. It mounts the
host's `/etc/foundationdb/fdb.cluster` read-only and keeps object data in
`/var/lib/kvrgw` (node-local, so use one gateway unless that is shared). Port,
credentials and tuning knobs are environment overrides; see the script header.
The build in `build/` must be the Rocky one, matching `Dockerfile.run`.

The image is x86-64 only and includes GCC Toolset 14, Go 1.25 (with Go's
automatic toolchain selection), CMake, JSON/YAML/OpenSSL development packages,
and the FoundationDB 7.3.69 client. It does not copy source code, compile during
image creation, or start FoundationDB or the gateway. No full Ceph build is
required, but the backend uses the header-only `src/xxHash` submodule, so
initialize it on the host first:

```bash
git submodule update --init --recursive --force src/xxHash
```

The image defaults to three build jobs; override with
`BUILD_JOBS=2 ./scripts/build_from_tar.sh` if memory is limited.
Use a fresh POC `build/` directory if it already contains CMake output from a
different host path or toolchain. The compiled binaries depend on the
container's libraries and paths; this is not a portable runtime image.

### FoundationDB runtime prerequisites

To download the client and server binaries into this checkout:

```bash
./scripts/fetch_fdb.sh
```

This downloads FoundationDB 7.3.69 x86-64 RPMs, verifies their published SHA-256
checksums, and extracts them into `third_party/fdb` without root privileges.
It requires `curl`, `sha256sum`, `rpm2cpio`, and `cpio`, all included in the
build image. Set `FDB_VERSION` to select another 7.3.x release.

**The build container does not support `reload.sh` as-is.** That script uses
the lab's Docker Compose cluster, host-specific absolute bind mounts, six
mounted drives (`/mnt/fdb0` through `/mnt/fdb2` and `/mnt/fdb-log0` through
`/mnt/fdb-log2`), `sudo`, nginx, and S3 test tools. Fetching binaries does not
provide those services or mounts. In particular, `reload.sh --clean` wipes
the configured FDB data; do not use it against a cluster you need to keep.

## Run

```bash
pkill -f kv-rgw-frontend; pkill -f kv-rgw-backend; pkill nginx; sleep 3
bash scripts/reload.sh --clean 1
```

The gateway listens on `http://127.0.0.1:9080`. Use any S3 client with:
- Access key: `test`
- Secret key: `test`
- Endpoint: `http://127.0.0.1:9080`

## Tests

```bash
# Our versioning tests
bash scripts/test_version_get_put.sh
bash scripts/test_delete_bucket_versioning.sh
bash scripts/test_list_object_versions.sh

# Ceph-rgw S3 compatibility (requires s3tests repo)
cd ~/clean/ceph/src/test/rgw/s3-tests
S3TEST_CONF=/home/gbenhano/kv_poc/s3tests.conf python3.11 -m pytest \
  s3tests/functional/test_s3.py -k "<test_expression>" --tb=line -q

# Full test plan (build + unit + smoke + stress + chaos)
bash scripts/run_test_plan.sh --quick   # fast (4 phases)
bash scripts/run_test_plan.sh           # full (10 phases, ~40min)
```

## Architecture

- **C++ backend** (`backend/`): FDB transactions, object storage, versioning logic
- **Go frontend** (`frontend/`): versitygw-based S3 gateway, gRPC client to backend
- **Proto** (`proto/kvrgw.proto`): gRPC interface between frontend and backend
- **nginx**: reverse proxy + load balancer in front of the gateway

## Key Format

| Namespace | Format | Purpose |
|---|---|---|
| O: | `S<header><object_name>` | Current object version |
| V: | `S<header><object_name>\x00<vid_be32>` | Historical versions |
| B: | `B<tenant_id><bucket_name>` | Bucket metadata |
| P: | `P<header><object_name><ref_tag>` | Pending uploads |
| G: | `G<header><ref_tag><size>` | GC queue |
| D: | `D<header><ref_tag>` | KV-store data tier |
