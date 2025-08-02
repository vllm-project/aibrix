# Git Commit Hash
GIT_COMMIT_HASH ?= $(shell git rev-parse HEAD)
IMAGE_TAG ?= ${GIT_COMMIT_HASH}  # Use git commit hash as default image tag

# Image URL to use all building/pushing image targets
AIBRIX_CONTAINER_REGISTRY_NAMESPACE ?= aibrix
DOCKERFILE_PATH ?= build/container
IMAGES := controller-manager gateway-plugins runtime metadata-service
AIBRIX_IMAGES := $(foreach img,$(IMAGES),$(AIBRIX_CONTAINER_REGISTRY_NAMESPACE)/$(img):nightly)


# note: this is not being used, only for tracking some commands we have not updated yet.
# TODO: Remove this line when all commands are updated to use IMAGE_TAG
IMG ?= ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/controller-manager:${IMAGE_TAG}

# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.29.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
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

GINKGO_VERSION ?= $(shell go list -m -f '{{.Version}}' github.com/onsi/ginkgo/v2)
INTEGRATION_TARGET ?= ./test/integration/webhook/...

GINKGO = $(shell pwd)/bin/ginkgo
.PHONY: ginkgo
ginkgo: ## Download ginkgo locally if necessary.
	test -s $(LOCALBIN)/ginkgo || \
	GOBIN=$(LOCALBIN) go install github.com/onsi/ginkgo/v2/ginkgo@$(GINKGO_VERSION)

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=controller-manager-role crd:maxDescLen=0,generateEmbeddedObjectMeta=true webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."
	./hack/update-codegen.sh go $(PROJECT_DIR)/bin

.PHONY: update-codegen
update-codegen:
	sh ./hack/update-codegen.sh

.PHONY: verify-codegen
verify-codegen:
	sh ./hack/verify-codegen.sh

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

.PHONY: test-code-coverage
test-code-coverage: test
	$(GO_TEST_COVERAGE) --config=./.github/.testcoverage.yml

.PHONY: test-race-condition
test-race-condition: manifests generate fmt vet envtest ## Run tests with race detection enabled.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test -race $$(go list ./... | grep -v /e2e)

.PHONY: test-integration
test-integration: manifests fmt vet envtest ginkgo ## Run integration tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
	$(GINKGO) --junit-report=junit.xml --output-dir=$(ARTIFACTS) -v $(INTEGRATION_TARGET)

# Utilize Kind or modify the e2e tests to load the image locally, enabling compatibility with other vendors.
.PHONY: test-e2e  # Run the e2e tests against a Kind k8s instance that is spun up.
test-e2e:
	./test/run-e2e-tests.sh

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter & yamllint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

# Run the licensecheck script to check for license headers
.PHONY: licensecheck
licensecheck:
	@echo ">> checking license header"
	@licRes=$$(for file in $$(find . -type f -iname '*.go' ! -path '.') ; do \
               awk 'NR<=5' $$file | grep -Eq "(Copyright|generated|GENERATED)" || echo $$file; \
       done); \
       if [ -n "$${licRes}" ]; then \
               echo "license header checking failed:"; echo "$${licRes}"; \
               exit 1; \
       fi

# Run all the linters
.PHONY: lint-all
lint-all: licensecheck lint

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/controllers/main.go

.PHONY: build-controller-manager
build-controller-manager: manifests generate fmt vet ## Build controller-manager binary without ZMQ.
	CGO_ENABLED=0 go build -tags="nozmq" -o bin/controller-manager cmd/controllers/main.go

.PHONY: build-gateway-plugins
build-gateway-plugins: manifests generate fmt vet ## Build gateway-plugins binary with ZMQ.
	CGO_ENABLED=1 \
	CGO_LDFLAGS='-Wl,-Bstatic -l:libzmq.a -l:libsodium.a -l:libnorm.a -l:libprotokit.a -l:libpgm.a -Wl,-Bdynamic -lbsd -lkrb5 -lgssapi_krb5 -lk5crypto -lpthread -lm -lstdc++' \
	go build -tags="zmq" \
		-ldflags='-extldflags "$(CGO_LDFLAGS)"' \
		-o bin/gateway-plugins cmd/plugins/main.go

.PHONY: build-metadata-service
build-metadata-service: manifests generate fmt vet ## Build metadata-service binary without ZMQ.
	CGO_ENABLED=0 go build -o bin/metadata-service cmd/metadata/main.go
.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/controllers/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/

# This is used to determine if the current branch is the main branch.
IS_MAIN_BRANCH ?= true

define build_and_tag
	$(CONTAINER_TOOL) build -t ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/$(1):${IMAGE_TAG} -f ${DOCKERFILE_PATH}/$(2) .
	if [ "${IS_MAIN_BRANCH}" = "true" ]; then $(CONTAINER_TOOL) tag ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/$(1):${IMAGE_TAG} ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/$(1):nightly; fi
endef

define push_image
	$(CONTAINER_TOOL) push ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/$(1):${IMAGE_TAG}
	if [ "${IS_MAIN_BRANCH}" = "true" ]; then $(CONTAINER_TOOL) push ${AIBRIX_CONTAINER_REGISTRY_NAMESPACE}/$(1):nightly; fi
endef

.PHONY: docker-build-all
docker-build-all:
	make -j $(nproc) docker-build-controller-manager docker-build-gateway-plugins docker-build-runtime docker-build-metadata-service docker-build-kvcache-watcher ## Build all docker images

.PHONY: docker-build-controller-manager
docker-build-controller-manager: ## Build docker image with the manager.
	$(call build_and_tag,controller-manager,Dockerfile)

.PHONY: docker-build-gateway-plugins
docker-build-gateway-plugins: ## Build docker image with the gateway plugins.
	$(call build_and_tag,gateway-plugins,Dockerfile.gateway)

.PHONY: docker-build-runtime
docker-build-runtime: ## Build docker image with the AI Runtime.
	$(call build_and_tag,runtime,Dockerfile.runtime)

.PHONY: docker-build-metadata-service
docker-build-metadata-service: ## Build docker image with the metadata-service.
	$(call build_and_tag,metadata-service,Dockerfile.metadata)

.PHONY: docker-build-kvcache-watcher
docker-build-kvcache-watcher: ## Build docker image with the kvcache-watcher.
	$(call build_and_tag,kvcache-watcher,Dockerfile.kvcache)

.PHONY: docker-push-all
docker-push-all:
	make -j $(nproc) docker-push-controller-manager docker-push-gateway-plugins docker-push-runtime docker-push-metadata-service docker-push-kvcache-watcher ## Push all docker images

.PHONY: docker-push-controller-manager
docker-push-controller-manager: ## Push docker image with the manager.
	$(call push_image,controller-manager)

.PHONY: docker-push-gateway-plugins
docker-push-gateway-plugins: ## Push docker image with the gateway plugins.
	$(call push_image,gateway-plugins)

.PHONY: docker-push-runtime
docker-push-runtime: ## Push docker image with the AI Runtime.
	$(call push_image,runtime)

.PHONY: docker-push-metadata-service
docker-push-metadata-service: ## Push docker image with the metadata-service.
	$(call push_image,metadata-service)

.PHONY: docker-push-kvcache-watcher
docker-push-kvcache-watcher: ## Push docker image with the kvcache-watcher.
	$(call push_image,kvcache-watcher)

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx AIBRIX_CONTAINER_REGISTRY_NAMESPACE=myregistry). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via AIBRIX_CONTAINER_REGISTRY_NAMESPACE=<myregistry> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' $(DOCKERFILE_PATH)/Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name project-v3-builder
	$(CONTAINER_TOOL) buildx use project-v3-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm project-v3-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

.PHONY: docker-buildx-runtime
docker-buildx-runtime:
	$(CONTAINER_TOOL) buildx build --push --platform=${PLATFORMS} -f runtime.Dockerfile . -t ${RUNTIME_IMG}

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = true
endif

.PHONY: dev-install-in-kind
dev-install-in-kind: docker-build-all install
	@echo "Loading images into Kind cluster..."
	@for img in $(AIBRIX_IMAGES); do \
		echo "Loading $$img..."; \
		kind load docker-image $$img || { echo "Error: Failed to load $$img"; exit 1; }; \
	done

	@echo "Waiting for core components to be ready..."
	@$(KUBECTL) wait --for=condition=available --timeout=120s deployment -l app=envoy-gateway -n envoy-gateway-system || echo "Warning: Timeout waiting for Envoy Gateway"

	@echo "Applying monitoring configurations..."
	@echo "  - Applying core Prometheus configurations..."
	@$(KUBECTL) apply -k config/prometheus || { echo "Warning: Failed to apply Prometheus configurations"; }
	@echo "  - Applying additional ServiceMonitor configurations..."
	@$(KUBECTL) apply -f observability/monitor/envoy_metrics_service.yaml || echo "Warning: Failed to apply Envoy metrics service"
	@$(KUBECTL) apply -f observability/monitor/service_monitor_controller_manager.yaml || echo "Warning: Failed to apply Controller Manager ServiceMonitor"
	@$(KUBECTL) apply -f observability/monitor/service_monitor_gateway_plugin.yaml || echo "Warning: Failed to apply Gateway Plugin ServiceMonitor"
	@$(KUBECTL) apply -f observability/monitor/service_monitor_gateway.yaml || echo "Warning: Failed to apply Gateway ServiceMonitor"
	@$(KUBECTL) apply -f observability/monitor/service_monitor_vllm.yaml || echo "Warning: Failed to apply vLLM ServiceMonitor"


	@echo "Applying test configurations..."
	@$(KUBECTL) apply -k config/test || { echo "Warning: Failed to apply test configurations"; }

	@echo "Building and loading vLLM mock..."
	@cd development/app && \
	docker build -t aibrix/vllm-mock:nightly -f Dockerfile . && \
	kind load docker-image aibrix/vllm-mock:nightly && \
	$(KUBECTL) apply -k config/mock && \
	cd ../..

	@echo "AIBrix installation in Kind complete!"

.PHONY: dev-uninstall-from-kind
dev-uninstall-from-kind:
	@echo "Uninstalling AIBrix from Kind cluster..."

	@echo "Removing test configurations..."
	@$(KUBECTL) delete -k config/test --ignore-not-found=true || echo "Warning: Issue removing test configurations"

	@echo "Removing vLLM mock..."
	@cd development/app && \
	$(KUBECTL) delete -k config/mock --ignore-not-found=true || echo "Warning: Issue removing vLLM mock" && \
	cd ../..

	@echo "Removing monitoring configurations..."
	@echo "  - Removing additional ServiceMonitor configurations..."
	@$(KUBECTL) delete -f observability/monitor/envoy_metrics_service.yaml --ignore-not-found=true || echo "Warning: Issue removing Envoy metrics service"
	@$(KUBECTL) delete -f observability/monitor/service_monitor_controller_manager.yaml --ignore-not-found=true || echo "Warning: Issue removing Controller Manager ServiceMonitor"
	@$(KUBECTL) delete -f observability/monitor/service_monitor_gateway_plugin.yaml --ignore-not-found=true || echo "Warning: Issue removing Gateway Plugin ServiceMonitor"
	@$(KUBECTL) delete -f observability/monitor/service_monitor_gateway.yaml --ignore-not-found=true || echo "Warning: Issue removing Gateway ServiceMonitor"
	@$(KUBECTL) delete -f observability/monitor/service_monitor_vllm.yaml --ignore-not-found=true || echo "Warning: Issue removing vLLM ServiceMonitor"

	@echo "  - Removing core Prometheus configurations..."
	@$(KUBECTL) delete -k config/prometheus --ignore-not-found=true || echo "Warning: Issue removing Prometheus configurations"

	@echo "Removing base configurations with uninstall target..."
	@$(MAKE) uninstall ignore-not-found=true

	@echo "AIBrix uninstallation from Kind complete!"

.PHONY: dev-port-forward
dev-port-forward:
	@KUBECTL=$(KUBECTL) ./scripts/port-forward.sh

.PHONY: dev-stop-port-forward
dev-stop-port-forward:
	@echo "Stopping all port forwarding sessions..."
	@-kill $$(cat .envoy-pf.pid 2>/dev/null) 2>/dev/null && rm .envoy-pf.pid 2>/dev/null || true
	@-kill $$(cat .redis-pf.pid 2>/dev/null) 2>/dev/null && rm .redis-pf.pid 2>/dev/null || true
	@-kill $$(cat .prometheus-pf.pid 2>/dev/null) 2>/dev/null && rm .prometheus-pf.pid 2>/dev/null || true
	@-kill $$(cat .grafana-pf.pid 2>/dev/null) 2>/dev/null && rm .grafana-pf.pid 2>/dev/null || true
	@echo "All port forwarding sessions stopped"

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
    ## helm creates objects without aibrix prefix, hence deploying gateway components outside of kustomization
	$(KUBECTL) apply -k config/dependency --server-side

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -k config/dependency

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy-release
deploy-release: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/overlays/release | $(KUBECTL) apply -f -

.PHONY: install-vke
install-vke: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
    ## helm creates objects without aibrix prefix, hence deploying gateway components outside of kustomization
	$(KUBECTL) apply -k config/overlays/vke/dependency --server-side

.PHONY: uninstall-vke
uninstall-vke: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -k config/overlays/vke/dependency

.PHONY: deploy-vke
deploy-vke: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/overlays/vke/default | $(KUBECTL) apply -f -

.PHONY: undeploy-vke
undeploy-vke: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/overlays/vke/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy-vke-ipv4
deploy-vke-ipv4: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/overlays/vke-ipv4 | $(KUBECTL) apply -f -

.PHONY: undeploy-vke-ipv4
undeploy-vke-ipv4: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/overlays/vke-ipv4 | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KUSTOMIZE ?= $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GO_TEST_COVERAGE = $(LOCALBIN)/go-test-coverage-$(GO_TEST_COVERAGE_VERSION)

## Tool Versions
KUSTOMIZE_VERSION ?= v5.3.0
CONTROLLER_TOOLS_VERSION ?= v0.16.1
ENVTEST_VERSION ?= release-0.17
GOLANGCI_LINT_VERSION ?= v1.57.2
GO_TEST_COVERAGE_VERSION ?= v2.14.3

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,${GOLANGCI_LINT_VERSION})

.PHONY: go-test-coverage
go-test-coverage: $(GO_TEST_COVERAGE) ## Download go-test-coverage locally if necessary.
$(GO_TEST_COVERAGE): $(LOCALBIN)
	$(call go-install-tool,$(GO_TEST_COVERAGE),github.com/vladopajic/go-test-coverage/v2,${GO_TEST_COVERAGE_VERSION})

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef
