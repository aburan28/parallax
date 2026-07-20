# parallax — build, generate, test, and local-env targets.
# See docs/DESIGN.md §18 for the repository layout this Makefile drives.

SHELL := /usr/bin/env bash
.DEFAULT_GOAL := help

MODULE      := github.com/aburan28/parallax
LOCALBIN    := $(shell pwd)/hack/bin
GOBIN       := $(LOCALBIN)
export PATH := $(LOCALBIN):$(PATH)

# Pinned tool versions (kept in lockstep with go.mod / kapture).
CONTROLLER_GEN_VERSION ?= v0.17.3
PROTOC_GEN_GO_VERSION  ?= v1.36.11
PROTOC_GEN_GRPC_VERSION?= v1.5.1
BUF_VERSION            ?= v1.47.2

CONTROLLER_GEN := $(LOCALBIN)/controller-gen
BUF            := $(LOCALBIN)/buf

# Binaries built by `make build`.
BINS := parallax parallax-operator plugin-installer
PLUGIN_DIRS := $(wildcard cmd/plugins/*)
PLUGINS := $(notdir $(PLUGIN_DIRS))

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.0.0-dev")
VCS_REF ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
LDFLAGS := -X $(MODULE)/internal/version.Version=$(VERSION) -X $(MODULE)/internal/version.VCSRef=$(VCS_REF)

##@ General

.PHONY: help
help: ## Print this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: tools
tools: $(LOCALBIN) ## Install pinned code-generation tools into hack/bin.
	GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_GEN_VERSION)
	GOBIN=$(LOCALBIN) go install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)
	GOBIN=$(LOCALBIN) go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GRPC_VERSION)
	GOBIN=$(LOCALBIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: proto
proto: tools ## Regenerate gRPC/protobuf plugin ABI from proto/.
	$(BUF) generate

.PHONY: generate
generate: tools proto ## Run controller-gen (deepcopy) + proto codegen.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./api/..."

.PHONY: manifests
manifests: tools ## Generate CRD YAML into config/crd and charts.
	$(CONTROLLER_GEN) crd rbac:roleName=parallax-manager paths="./api/..." \
		output:crd:artifacts:config=config/crd
	cp -f config/crd/*.yaml charts/parallax/crds/ 2>/dev/null || true

.PHONY: fmt vet
fmt: ## go fmt.
	go fmt ./...
vet: ## go vet.
	go vet ./...

.PHONY: tidy
tidy: ## go mod tidy.
	go mod tidy

##@ Build

.PHONY: build
build: $(addprefix build-,$(BINS)) build-plugins ## Build all binaries into bin/.

build-%:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$* ./cmd/$*

.PHONY: build-plugins
build-plugins: ## Build first-party plugin binaries as parallax-<kind>-<name>.
	@for d in $(PLUGIN_DIRS); do \
		name=$$(basename $$d); \
		echo "  building parallax-$$name"; \
		CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/parallax-$$name ./$$d || exit 1; \
	done

##@ Test

.PHONY: test
test: ## Unit tests.
	go test ./... -count=1

.PHONY: test-integration
test-integration: ## envtest integration suite (requires setup-envtest assets).
	go test ./test/integration/... -count=1

.PHONY: conformance
conformance: build-plugins ## Run the plugin conformance suites against first-party plugins.
	go test ./test/conformance/... -count=1

##@ Local environment

.PHONY: env-up
env-up: ## Bring up the kind dev cluster (kapture + Envoy Gateway + MinIO + prom-lite).
	./hack/env-up.sh

.PHONY: env-down
env-down: ## Tear down the kind dev cluster.
	./hack/env-down.sh

.PHONY: local
local: build-plugins ## Run a Study end-to-end in --local mode (SQLite + kind).
	go run ./cmd/parallax apply --local $(STUDY)
