POC_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
CEPH_ROOT := $(abspath $(POC_DIR)/../../..)
DOCKER ?= docker
IMAGE ?= kvrgw-build
DEPLOY_HOST ?= client
DEPLOY_DIR ?= dev/rgwkv
DIST ?= $(POC_DIR)/dist

.DEFAULT_GOAL := shell
.PHONY: shell docker-image build package package-capi deploy

docker-image:
	$(DOCKER) build -t "$(IMAGE)" - < "$(POC_DIR)/Dockerfile.build"

shell: docker-image
	$(DOCKER) run --rm -it \
		--user "$$(id -u):$$(id -g)" \
		--mount "type=bind,source=$(CEPH_ROOT),target=/workspace" \
		"$(IMAGE)"

# Compile the C++ backend and Go frontend in the build container.
build: docker-image
	$(DOCKER) run --rm \
		--user "$$(id -u):$$(id -g)" \
		--mount "type=bind,source=$(CEPH_ROOT),target=/workspace" \
		"$(IMAGE)" ./scripts/build_from_tar.sh

# 1) Compile in the build container, then bundle the runtime image tarball and
# kvrgw_perf.py/kvrgw_perf.ini into $(DIST).
package: build
	BUILDX_NO_DEFAULT_ATTESTATIONS=1 DOCKER="$(DOCKER)" "$(POC_DIR)/scripts/package_image.sh" "$(DIST)"

# Bundle libkvrgw.so, libfdb_c.so, kvrgw_c_api.h and kvrgw.yaml into
# $(DIST)/kvrgw-capi.tar.gz for cgo consumers such as k11 (build it first).
package-capi: build
	"$(POC_DIR)/scripts/package_capi.sh" "$(DIST)"

# Copy an already-built bundle to $(DEPLOY_HOST):$(DEPLOY_DIR) (relative to $HOME).
# Run `make package` first.
deploy:
	@test -f "$(DIST)/kvrgw-gw.tar.gz" || { echo "no bundle in $(DIST); run 'make package' first" >&2; exit 1; }
	ssh "$(DEPLOY_HOST)" mkdir -p "$(DEPLOY_DIR)"
	rsync -av --progress \
		"$(DIST)/kvrgw-gw.tar.gz" \
		"$(POC_DIR)/scripts/kvrgw_perf.py" \
		"$(POC_DIR)/scripts/kvrgw_perf.ini" \
		"$(DEPLOY_HOST):$(DEPLOY_DIR)/"
	@echo "On $(DEPLOY_HOST): cd $(DEPLOY_DIR) && ./kvrgw_perf.py run 8 --pin"
