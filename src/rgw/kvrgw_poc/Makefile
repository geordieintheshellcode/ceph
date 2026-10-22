POC_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
CEPH_ROOT := $(abspath $(POC_DIR)/../../..)
DOCKER ?= docker
IMAGE ?= kvrgw-build

.DEFAULT_GOAL := shell
.PHONY: shell docker-image

docker-image:
	$(DOCKER) build -t "$(IMAGE)" - < "$(POC_DIR)/Dockerfile.build"

shell: docker-image
	$(DOCKER) run --rm -it \
		--user "$$(id -u):$$(id -g)" \
		--mount "type=bind,source=$(CEPH_ROOT),target=/workspace" \
		"$(IMAGE)"
