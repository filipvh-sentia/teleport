# VERSION defines the project version for the bundle.
# Update this value when you upgrade the version of your project.
# To re-generate a bundle for another specific version without changing the standard setup, you can:
# - use the VERSION as arg of the bundle target (e.g make bundle VERSION=0.0.2)
# - use environment variables to overwrite this value (e.g export VERSION=0.0.2)
VERSION ?= 0.0.1

# Image URL to use all building/pushing image targets
IMG ?= teleport-operator:latest

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.25

# Setting SHELL to bash allows bash commands to be executed by recipes.
# This is a requirement for 'setup-envtest.sh' in the test target.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

# include BUILDBOX_VERSION, BUILDBOX and BUILDBOX_variant variables
include ../../build.assets/images.mk

# Configure which compiler and buildbox to use
OS ?= $(shell go env GOOS)
ARCH ?= $(shell go env GOARCH)
ifeq ("$(OS)","linux")
ifeq ("$(ARCH)","amd64")
COMPILER ?= x86_64-linux-gnu-gcc
PLATFORM_BUILDBOX ?= $(BUILDBOX)
else ifeq ("$(ARCH)","386")
COMPILER ?= x86_64-linux-gnu-gcc
PLATFORM_BUILDBOX ?= $(BUILDBOX)
else ifeq ("$(ARCH)","arm")
COMPILER ?= arm-linux-gnueabihf-gcc
PLATFORM_BUILDBOX ?= $(BUILDBOX_ARM)
else ifeq ("$(ARCH)","arm64")
COMPILER ?= aarch64-linux-gnu-gcc
PLATFORM_BUILDBOX ?= $(BUILDBOX_ARM)
endif
endif

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: crdgen
crdgen: ## Generate CRDs
	make -C crdgen

.PHONY: manifests
manifests: crdgen controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
# Generate CRDs for apis/resources (custom generation through protoc plugin).

# The wrappers.proto file needed for this generator exist only inside the go mod cache,
# so we retrieve the file path for the cached proto files with go mod tools.
	$(eval PROTOBUF_MOD_PATH := $(shell go mod download --json github.com/gogo/protobuf | awk -F: '/"Dir"/ { print $$2 }' | tr -d ' ",'))
	$(eval CUSTOM_IMPORTS_TMP_DIR := $(shell mktemp -d))
	mkdir -p $(CUSTOM_IMPORTS_TMP_DIR)/github.com/gravitational/teleport/api/proto/teleport/legacy/types/wrappers
	cp ../../api/proto/teleport/legacy/types/wrappers/wrappers.proto $(CUSTOM_IMPORTS_TMP_DIR)/github.com/gravitational/teleport/api/proto/teleport/legacy/types/wrappers
	protoc \
		-I=../../api/proto \
		-I=$(PROTOBUF_MOD_PATH) \
		-I=$(CUSTOM_IMPORTS_TMP_DIR) \
		--plugin=./crdgen/protoc-gen-crd \
		--crd_out=:./config/crd/bases \
		../../api/proto/teleport/legacy/types/types.proto
	rm -rf $(CUSTOM_IMPORTS_TMP_DIR)

# Some comments have `{{xyz}}` which triggers the golang templating engine when installing the chart.
# We don't want that, so we escape them.
	$(foreach file, $(wildcard $(CURDIR)/config/crd/bases/*), \
		$(shell sed 's/`{{/{{ `{{/g;s/}}`/}}` }}/g' $(file) > $(CURDIR)/../../examples/chart/teleport-cluster/charts/teleport-operator/templates/$(notdir $(file))))

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest crdgen-test ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -coverprofile cover.out

.PHONY: crdgen-test
crdgen-test: ## Run crdgen tests.
	make -C crdgen test

##@ Build

.PHONY: build
build: generate fmt vet ## Build manager binary.
	go build -o bin/manager main.go namespace.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./main.go ./namespace.go

.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	docker buildx build --platform="$(OS)/$(ARCH)" --build-arg BUILDBOX=$(PLATFORM_BUILDBOX) \
		--build-arg COMPILER_NAME=$(COMPILER) -t ${IMG} --load ../.. -f ./Dockerfile

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	docker push ${IMG}

##@ Build Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
ENVTEST ?= $(LOCALBIN)/setup-envtest

## Tool Versions
KUSTOMIZE_VERSION ?= v3.8.7
CONTROLLER_TOOLS_VERSION ?= v0.9.0

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST) ## Download envtest-setup locally if necessary.
$(ENVTEST): $(LOCALBIN)
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
