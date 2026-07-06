ROOT_DIR_RELATIVE := .

include $(ROOT_DIR_RELATIVE)/common.mk

VERSION ?= $(shell cat clusterctl-settings.json | jq .config.nextVersion -r)

TOOLS_DIR := hack/tools
TOOLS_DIR_DEPS := $(TOOLS_DIR)/go.sum $(TOOLS_DIR)/go.mod $(TOOLS_DIR)/Makefile
TOOLS_BIN_DIR := $(TOOLS_DIR)/bin
MANIFEST_DIR=_build/manifests
BUILD_DIR :=_build
RELEASE_DIR := _build/release
DEV_DIR := _build/dev
REPO_ROOT := $(shell git rev-parse --show-toplevel)
FIPS_ENABLE ?= ""
BUILDER_GOLANG_VERSION ?= 1.25.8
BUILD_ARGS = --build-arg CRYPTO_LIB=${FIPS_ENABLE} --build-arg BUILDER_GOLANG_VERSION=${BUILDER_GOLANG_VERSION}
ARCH ?= amd64
ALL_ARCH = amd64 arm64

RELEASE_LOC := release
ifeq ($(FIPS_ENABLE),yes)
  RELEASE_LOC := release-fips
endif

# Image URL to use all building/pushing image targets
IMAGE_NAME := cluster-api-provider-maas-controller
REGISTRY ?= "us-east1-docker.pkg.dev/spectro-images/dev/${USER}/cluster-api"
SPECTRO_VERSION ?= storage-overcommit-prevention-20260506
IMG_TAG ?= v0.6.1-spectro-${SPECTRO_VERSION}
CONTROLLER_IMG ?= ${REGISTRY}/${IMAGE_NAME}

# Set --output-base for conversion-gen if we are not within GOPATH
ifneq ($(abspath $(REPO_ROOT)),$(shell go env GOPATH)/src/github.com/spectrocloud/cluster-api-provider-maas)
	GEN_OUTPUT_BASE := --output-base=$(REPO_ROOT)
else
	export GOPATH := $(shell go env GOPATH)
endif

# Release images
# Release docker variables
RELEASE_REGISTRY := gcr.io/spectro-images-public/release/cluster-api-provider-maas
RELEASE_CONTROLLER_IMG := $(RELEASE_REGISTRY)/$(IMAGE_NAME)

# Produce CRDs that work back to Kubernetes 1.11 (no version conversion)
CRD_OPTIONS ?= "crd"

MOCKGEN := $(TOOLS_BIN_DIR)/mockgen
CONTROLLER_GEN := $(TOOLS_BIN_DIR)/controller-gen
CONVERSION_GEN := $(TOOLS_BIN_DIR)/conversion-gen
DEFAULTER_GEN := $(TOOLS_BIN_DIR)/defaulter-gen
KUSTOMIZE := $(TOOLS_BIN_DIR)/kustomize

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

all: manager

# Run tests
test: generate fmt vet manifests generate-lxd-template ## Run unit tests
	# TODO bring back
	go test ./... -coverprofile cover.out

# Build manager binary
manager: generate fmt vet generate-lxd-template ## Build manager binary
	go build -o bin/manager main.go

# Run against the configured Kubernetes cluster in ~/.kube/config
run: generate fmt vet manifests generate-lxd-template
	go run ./main.go

# Install CRDs into a cluster
install: manifests ## Install CRDs into a cluster
	kustomize build config/crd | kubectl apply -f -

# Uninstall CRDs from a cluster
uninstall: manifests ## Uninstall CRDs from a cluster
	kustomize build config/crd | kubectl delete -f -

# Deploy controller in the configured Kubernetes cluster in ~/.kube/config
deploy: manifests  ## Deploy controller in the configured Kubernetes cluster
	cd config/manager && kustomize edit set image controller=$(CONTROLLER_IMG):$(IMG_TAG)
	kustomize build config/default | kubectl apply -f -

$(MANIFEST_DIR):
	mkdir -p $(MANIFEST_DIR)

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

$(OVERRIDES_DIR):
	@mkdir -p $(OVERRIDES_DIR)

.PHONY: dev-version-check
dev-version-check:
ifndef VERSION
	$(error VERSION must be set)
endif

.PHONY: release-version-check
release-version-check:
ifeq ($(VERSION), 0.0.0)
	$(error VERSION must be >0.0.0 for release)
endif

.PHONY: release-manifests
release-manifests: test
	$(MAKE) generate-lxd-template STAGE=release VERSION=$(VERSION)
	$(MAKE) manifests STAGE=release MANIFEST_DIR=$(RELEASE_DIR) PULL_POLICY=IfNotPresent IMAGE=$(RELEASE_CONTROLLER_IMG):$(VERSION) VERSION=$(VERSION)
	cp metadata.yaml $(RELEASE_DIR)/metadata.yaml
	$(MAKE) templates OUTPUT_DIR=$(RELEASE_DIR)

.PHONY: release-overrides
release-overrides:
	$(MAKE) manifests STAGE=release MANIFEST_DIR=$(OVERRIDES_DIR) PULL_POLICY=IfNotPresent IMAGE=$(RELEASE_CONTROLLER_IMG):$(VERSION)

.PHONY: dev-manifests
dev-manifests:
	$(MAKE) manifests STAGE=dev MANIFEST_DIR=$(DEV_DIR) PULL_POLICY=Always IMAGE=$(CONTROLLER_IMG):$(IMG_TAG)
	cp metadata.yaml $(DEV_DIR)/metadata.yaml
	$(MAKE) templates OUTPUT_DIR=$(DEV_DIR)

# Generate manifests e.g. CRD, RBAC etc.
manifests: $(CONTROLLER_GEN) $(MANIFEST_DIR) $(KUSTOMIZE) $(BUILD_DIR) ## Generate manifests e.g. CRD, RBAC etc.
	rm -rf $(BUILD_DIR)/config
	cp -R config $(BUILD_DIR)/config
	sed -i'' -e 's@imagePullPolicy: .*@imagePullPolicy: '"$(PULL_POLICY)"'@' $(BUILD_DIR)/config/default/manager_pull_policy.yaml
	sed -i'' -e 's@image: .*@image: '"$(IMAGE)"'@' $(BUILD_DIR)/config/default/manager_image_patch.yaml
	"$(KUSTOMIZE)" build $(BUILD_DIR)/config/default > $(MANIFEST_DIR)/infrastructure-components.yaml

# Run go fmt against code
fmt: ## Run go fmt against code
	go fmt ./...

# Run go vet against code
vet:  ## Run go vet against code
	go vet ./...

# Generate code
generate: $(CONTROLLER_GEN) $(CONVERSION_GEN)
	$(MAKE) generate-go
	$(MAKE) generate-manifests

generate-go:
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

	$(CONVERSION_GEN) \
		--extra-peer-dirs=github.com/spectrocloud/cluster-api-provider-maas/api/v1beta1 \
		--output-file=zz_generated.conversion \
		--go-header-file=./hack/boilerplate.go.txt \
		./api/v1beta1

generate-manifests:  ## Generate manifests
	$(CONTROLLER_GEN) $(CRD_OPTIONS) rbac:roleName=manager-role webhook paths="./..." output:crd:artifacts:config=config/crd/bases


# Build the docker image
.PHONY: docker-build
docker-build: generate-lxd-template ## Build CAPMAAS controller image (ensures lxd-initializer template is processed first with correct image tag)
	@# Template is already processed by generate-lxd-template with correct image tag via kustomize
	docker buildx build --load --platform linux/$(ARCH) ${BUILD_ARGS} --build-arg ARCH=$(ARCH)  --build-arg  LDFLAGS="$(LDFLAGS)" --build-arg CRYPTO_LIB=${FIPS_ENABLE} . -t $(CONTROLLER_IMG)-$(ARCH):$(IMG_TAG)

# Push the docker image
.PHONY: docker-push
docker-push: ## Push the docker image to gcr
	docker push  $(CONTROLLER_IMG)-$(ARCH):$(IMG_TAG)

## --------------------------------------
## Docker — All ARCH
## --------------------------------------
.PHONY: docker-build-all ## Build all the architecture docker images
docker-build-all: $(addprefix docker-build-,$(ALL_ARCH))

docker-build-%:
	$(MAKE) ARCH=$* docker-build

.PHONY: docker-push-all ## Push all the architecture docker images
docker-push-all: $(addprefix docker-push-,$(ALL_ARCH))
	$(MAKE) docker-push-manifest

docker-push-%:
	$(MAKE) ARCH=$* docker-push

.PHONY: docker-push-manifest
docker-push-manifest: ## Push the fat manifest docker image.
	## Minimum docker version 18.06.0 is required for creating and pushing manifest images.
	docker manifest create --amend $(CONTROLLER_IMG):$(IMG_TAG) $(shell echo $(ALL_ARCH) | sed -e "s~[^ ]*~$(CONTROLLER_IMG)\-&:$(IMG_TAG)~g")
	@for arch in $(ALL_ARCH); do docker manifest annotate --arch $${arch} ${CONTROLLER_IMG}:${IMG_TAG} ${CONTROLLER_IMG}-$${arch}:${IMG_TAG}; done
	docker manifest push --insecure --purge $(CONTROLLER_IMG):$(IMG_TAG)

docker-rmi: ## Remove the docker image locally
	docker rmi $(CONTROLLER_IMG):$(IMG_TAG)

mock: $(MOCKGEN)
	go generate ./...

clean-release:
	rm -rf $(RELEASE_DIR)

release: release-manifests
	@# Ensure template is processed with release image before building controller
	$(MAKE) generate-lxd-template STAGE=release VERSION=$(VERSION)
	$(MAKE) docker-build STAGE=release VERSION=$(VERSION)
	$(MAKE) docker-push STAGE=release VERSION=$(VERSION)
	@echo "Building and pushing LXD initializer image for release..."
	$(MAKE) lxd-initializer-docker-build STAGE=release VERSION=$(VERSION)
	$(MAKE) lxd-initializer-docker-push STAGE=release VERSION=$(VERSION)

.PHONY: templates
templates: ## Generate release templates
	cp templates/cluster-template*.yaml $(OUTPUT_DIR)/

version: ## Prints version of current make
	@echo $(VERSION)

# --------------------------------------------------------------------
# LXD-initializer image (privileged DaemonSet)
# --------------------------------------------------------------------
INIT_IMAGE_NAME ?= "lxd-initializer"
# INIT_IMG_TAG    ?= $(IMG_TAG)          # reuse the same tag as controller
# INIT_DRI_IMG    ?= us-east1-docker.pkg.dev/spectro-images/dev/cluster-api/$(INIT_IMAGE_NAME)
INIT_DRI_IMG     ?= $(REGISTRY)/$(INIT_IMAGE_NAME)
INIT_IMG_TAG     ?= $(TAG)
# Release image for LXD initializer (without tag)
INIT_RELEASE_IMG ?= us-east1-docker.pkg.dev/spectro-images/dev/cluster-api/$(INIT_IMAGE_NAME)

.PHONY: lxd-initializer-docker-build
lxd-initializer-docker-build: generate-lxd-template ## Build LXD initializer image (ensures template is processed first)
	@# Determine image to build: use INIT_DRI_IMG if explicitly set, otherwise use INIT_RELEASE_IMG for release
	@if [ -n "$(VERSION)" ] && [ "$(STAGE)" = "release" ]; then \
		BUILD_IMG="$(INIT_RELEASE_IMG):$(VERSION)"; \
	else \
		BUILD_IMG="$(INIT_DRI_IMG):$(INIT_IMG_TAG)"; \
	fi; \
	echo "Building LXD initializer image: $$BUILD_IMG"; \
	docker buildx build --load --platform linux/$(ARCH) \
	    -f lxd-initializer/Dockerfile \
	    ${BUILD_ARGS} \
	    lxd-initializer -t $$BUILD_IMG

.PHONY: lxd-initializer-docker-push
lxd-initializer-docker-push: lxd-initializer-docker-build ## Push LXD initializer image (builds first if needed)
	@# Determine image to push: use INIT_DRI_IMG if explicitly set, otherwise use INIT_RELEASE_IMG for release
	@if [ -n "$(VERSION)" ] && [ "$(STAGE)" = "release" ]; then \
		PUSH_IMG="$(INIT_RELEASE_IMG):$(VERSION)"; \
	else \
		PUSH_IMG="$(INIT_DRI_IMG):$(INIT_IMG_TAG)"; \
	fi; \
	echo "Pushing LXD initializer image: $$PUSH_IMG"; \
	docker push $$PUSH_IMG

# File target for processed template - ensures it exists before Go compilation
# This file target ensures the processed template exists, making it a proper dependency
controllers/templates/lxd_initializer_ds.yaml.processed: controllers/templates/lxd_initializer_ds.yaml
	@$(MAKE) process-lxd-initializer-template

process-lxd-initializer-template: ## Process LXD initializer template with image substitution using envsubst
	@# Check if envsubst is available
	@command -v envsubst >/dev/null 2>&1 || { echo "ERROR: envsubst not found. Please install gettext package."; exit 1; }
	@# Determine which image to use (dev or release) and check if already processed
	@if [ -n "$(VERSION)" ] && [ "$(STAGE)" = "release" ]; then \
		INIT_IMG="$(INIT_RELEASE_IMG):$(VERSION)"; \
	else \
		INIT_IMG="$(INIT_DRI_IMG):$(INIT_IMG_TAG)"; \
	fi; \
	if [ -f controllers/templates/lxd_initializer_ds.yaml.processed ]; then \
		if grep -q "$$INIT_IMG" controllers/templates/lxd_initializer_ds.yaml.processed; then \
			echo "Template already processed with image: $$INIT_IMG (skipping)"; \
			exit 0; \
		fi; \
	fi; \
	echo "Processing LXD initializer template with image: $$INIT_IMG"; \
	LXD_INITIALIZER_IMAGE=$$INIT_IMG envsubst '$$LXD_INITIALIZER_IMAGE' \
		< controllers/templates/lxd_initializer_ds.yaml > controllers/templates/lxd_initializer_ds.yaml.processed
	@# Verify the image was substituted
	@if [ -n "$(VERSION)" ] && [ "$(STAGE)" = "release" ]; then \
		VERIFY_IMG="$(INIT_RELEASE_IMG):$(VERSION)"; \
	else \
		VERIFY_IMG="$(INIT_DRI_IMG):$(INIT_IMG_TAG)"; \
	fi; \
	if ! grep -q "$$VERIFY_IMG" controllers/templates/lxd_initializer_ds.yaml.processed; then \
		if grep -q "\$${LXD_INITIALIZER_IMAGE}" controllers/templates/lxd_initializer_ds.yaml.processed; then \
			echo "ERROR: Image substitution failed! Still contains placeholder '\$${LXD_INITIALIZER_IMAGE}'"; \
			echo "Expected image: $$VERIFY_IMG"; \
			exit 1; \
		fi; \
	fi

.PHONY: generate-lxd-template
generate-lxd-template: controllers/templates/lxd_initializer_ds.yaml.processed ## Generate processed LXD initializer template for embedding
	@# Ensure processed file exists (required for go:embed)
	@if [ ! -f controllers/templates/lxd_initializer_ds.yaml.processed ]; then \
		echo "ERROR: Processed template not found"; \
		exit 1; \
	fi
	@echo "LXD initializer template ready for embedding"
