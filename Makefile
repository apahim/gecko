.PHONY: lint lint-fix lint-fmt
lint lint-fix lint-fmt:
	@rc=0; for module in orlop platform-api controllers; do \
		$(MAKE) -C $$module $@ || rc=1; \
	done; exit $$rc

.PHONY: test
test:
	$(MAKE) -C orlop test
	$(MAKE) -C platform-api test
	$(MAKE) -C controllers test

# Cross-compile on the host, then package without executing target-architecture
# tools. In particular, Apple Silicon does not need to emulate an amd64 compiler.
CONTAINER_ENGINE ?= podman
LOCAL_ARCH ?= amd64
LOCAL_IMAGE_REPOSITORY ?= localhost/gecko
LOCAL_SOURCE_SUFFIX = $(shell if test -n "$$(git status --porcelain --untracked-files=normal 2>/dev/null)"; then printf '%s' '-dirty'; fi)
LOCAL_IMAGE_TAG ?= local-$(shell git rev-parse --short=12 HEAD 2>/dev/null || printf unknown)-$(LOCAL_ARCH)$(LOCAL_SOURCE_SUFFIX)
LOCAL_IMAGE_REVISION = $(shell git rev-parse HEAD 2>/dev/null || printf unknown)$(LOCAL_SOURCE_SUFFIX)
LOCAL_BUILD_ROOT := $(CURDIR)/bin/local/linux-$(LOCAL_ARCH)

.PHONY: local-build-check local-binaries local-binary-api local-binary-controllers
local-build-check:
	@case "$(LOCAL_ARCH)" in amd64|arm64) ;; *) echo "LOCAL_ARCH must be amd64 or arm64" >&2; exit 1 ;; esac

local-binaries: local-binary-api local-binary-controllers

local-binary-api: local-build-check
	mkdir -p "$(LOCAL_BUILD_ROOT)/api/app"
	cd platform-api && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$(LOCAL_ARCH) go build -mod=readonly -trimpath -o "$(LOCAL_BUILD_ROOT)/api/app/gecko" ./cmd/platform-api-server
	chmod 0555 "$(LOCAL_BUILD_ROOT)/api/app/gecko"

local-binary-controllers: local-build-check
	mkdir -p "$(LOCAL_BUILD_ROOT)/controllers/app"
	cd controllers && GOWORK=off CGO_ENABLED=0 GOOS=linux GOARCH=$(LOCAL_ARCH) go build -mod=readonly -trimpath -o "$(LOCAL_BUILD_ROOT)/controllers/app/gecko" .
	chmod 0555 "$(LOCAL_BUILD_ROOT)/controllers/app/gecko"

.PHONY: local-images local-image-api local-image-controllers
local-images: local-image-api local-image-controllers

local-image-api: local-binary-api
	$(CONTAINER_ENGINE) build --platform linux/$(LOCAL_ARCH) --file "$(CURDIR)/deploy/Containerfile.local" --build-arg BINARY=platform-api-server --label org.opencontainers.image.revision=$(LOCAL_IMAGE_REVISION) --tag $(LOCAL_IMAGE_REPOSITORY):$(LOCAL_IMAGE_TAG)-api "$(LOCAL_BUILD_ROOT)/api"

local-image-controllers: local-binary-controllers
	$(CONTAINER_ENGINE) build --platform linux/$(LOCAL_ARCH) --file "$(CURDIR)/deploy/Containerfile.local" --build-arg BINARY=gecko-controllers --label org.opencontainers.image.revision=$(LOCAL_IMAGE_REVISION) --tag $(LOCAL_IMAGE_REPOSITORY):$(LOCAL_IMAGE_TAG)-controllers "$(LOCAL_BUILD_ROOT)/controllers"
