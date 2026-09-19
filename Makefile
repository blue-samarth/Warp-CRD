IMG ?= webapp-operator:latest
ENVTEST_K8S_VERSION ?= 1.37.0

CONTROLLER_TOOLS_VERSION ?= v0.22.0
ENVTEST_VERSION ?= release-0.25
KUSTOMIZE_VERSION ?= v5.7.1

LOCALBIN := $(shell pwd)/bin
CONTROLLER_GEN := $(LOCALBIN)/controller-gen
ENVTEST := $(LOCALBIN)/setup-envtest
KUSTOMIZE := $(LOCALBIN)/kustomize

GOBIN ?= $(shell go env GOPATH)/bin
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

.PHONY: help
help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

$(LOCALBIN):
	mkdir -p $(LOCALBIN)

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN)
$(CONTROLLER_GEN): $(LOCALBIN)
	test -s $(CONTROLLER_GEN) && $(CONTROLLER_GEN) --version | grep -q $(CONTROLLER_TOOLS_VERSION) || \
		GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-tools/cmd/controller-gen@$(CONTROLLER_TOOLS_VERSION)

.PHONY: envtest
envtest: $(ENVTEST)
$(ENVTEST): $(LOCALBIN)
	test -s $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: kustomize
kustomize: $(KUSTOMIZE)
$(KUSTOMIZE): $(LOCALBIN)
	test -s $(KUSTOMIZE) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/kustomize/kustomize/v5@$(KUSTOMIZE_VERSION)

.PHONY: generate
generate: controller-gen ## Generate DeepCopy methods
	$(CONTROLLER_GEN) object:headerFile=/dev/null paths="./api/..."

.PHONY: manifests
manifests: controller-gen ## Generate CRD, RBAC and webhook manifests
	$(CONTROLLER_GEN) crd rbac:roleName=manager-role webhook \
		paths="./api/..." paths="./internal/..." \
		output:crd:artifacts:config=config/crd/bases

.PHONY: verify
verify: controller-gen ## Fail if generated files are out of date
	@tmp=$$(mktemp -d); \
	cp -R api config $$tmp/; \
	$(MAKE) --no-print-directory generate manifests >/dev/null; \
	if diff -r -q -x 'zz_generated*' $$tmp/api api >/dev/null && diff -r -q $$tmp/config config >/dev/null; then \
		rm -rf $$tmp; echo "generated files up to date"; \
	else \
		echo "generated files are stale; run 'make generate manifests' and commit"; \
		diff -r -u -x 'zz_generated*' $$tmp/api api || true; \
		diff -r -u $$tmp/config config || true; \
		rm -rf $$tmp; exit 1; \
	fi

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test: generate manifests fmt vet envtest ## Run unit + envtest integration tests
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" \
		go test ./... -count=1 -coverpkg=./api/...,./internal/... -coverprofile cover.raw.out
	@grep -v zz_generated cover.raw.out > cover.out
	@go tool cover -func=cover.out | tail -1

.PHONY: build
build: generate fmt vet
	go build -o bin/manager ./cmd

.PHONY: run
run: generate manifests fmt vet ## Run against the current kubeconfig (pass flags with ARGS=)
	go run ./cmd $(ARGS)

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: install
install: manifests kustomize ## Install CRDs into the current cluster
	$(KUSTOMIZE) build config/crd | kubectl apply --server-side -f -

.PHONY: uninstall
uninstall: kustomize
	$(KUSTOMIZE) build config/crd | kubectl delete --ignore-not-found -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy the operator to the current cluster
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default | kubectl apply --server-side -f -

# WebApps go first so their finalizers run while the operator is still up, and
# the whole render goes together so the webhook configurations never outlive the
# Service they point at: with failurePolicy Fail they would reject every write.
.PHONY: undeploy
undeploy: kustomize ## Remove every WebApp, then the operator, CRDs included
	-kubectl delete webapps.webapps.example.com --all --all-namespaces --ignore-not-found --timeout=2m
	-kubectl delete webapppolicies.webapps.example.com --all --ignore-not-found --timeout=2m
	$(KUSTOMIZE) build config/default | kubectl delete --ignore-not-found -f -

.PHONY: build-installer
build-installer: manifests kustomize ## Render a single-file install manifest
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(IMG)
	$(KUSTOMIZE) build config/default > dist/install.yaml

.PHONY: clean
clean:
	rm -rf bin/manager cover.out cover.raw.out dist
