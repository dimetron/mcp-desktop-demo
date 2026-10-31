SHELL := bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

# Local harness (see e2e.sh).
RUNTIME ?= runc

# Published images: $(IMAGE):<variant> and $(IMAGE):<variant>-<short sha>.
REGISTRY  ?= ghcr.io/dimetron
IMAGE     ?= $(REGISTRY)/mcp-desktop
VARIANT   ?= cua
VARIANTS  ?= x11 cdm cua
PLATFORMS ?= linux/amd64,linux/arm64
GIT_SHA   ?= $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo dev)
IMAGE_TAG ?= $(VARIANT)
# Resolved from the registry when not given; SandboxTemplate requires a digest.
DIGEST    ?= $(shell docker buildx imagetools inspect $(IMAGE):$(IMAGE_TAG) --format '{{.Manifest.Digest}}' 2>/dev/null)

# kagent deployment.
NAMESPACE         ?= kagent
WORKER_POOL       ?= kagent-default
SNAPSHOT_LOCATION ?= gs://ate-snapshots/$(NAMESPACE)/mcp-desktop/
KUBECTL           ?= kubectl
KUSTOMIZE         ?= $(KUBECTL) kustomize
OVERLAY           := out/deploy
MANIFEST          := out/deploy.yaml
LOCAL_PORT        ?= 18080

##@ General

.PHONY: help
help: ## Show this help.
	@awk 'BEGIN{FS=":.*##"} /^##@/{printf "\n%s\n", substr($$0,5)} /^[a-zA-Z0-9_.-]+:.*##/{printf "  %-16s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

##@ Develop

.PHONY: test
test: ## go vet and race tests for desktop-gw.
	cd desktop-gw && go vet ./... && go test -race ./...

.PHONY: lint
lint: ## shellcheck scripts, actionlint workflows, validate rendered manifests.
	shellcheck e2e.sh k8s-e2e.sh start-desktop.sh
	@if command -v actionlint >/dev/null; then actionlint; else echo "actionlint not installed, skipping"; fi
	$(MAKE) --no-print-directory manifests DIGEST=sha256:$$(printf '0%.0s' {1..64})
	@if command -v kubeconform >/dev/null; then kubeconform -strict -summary -ignore-missing-schemas $(MANIFEST); else echo "kubeconform not installed, skipping"; fi

##@ Local end-to-end (Docker)

.PHONY: build
build: ## Build the x11, base, cdm and cua images locally.
	RUNTIME=$(RUNTIME) ./e2e.sh build

.PHONY: e2e
e2e: ## Build, start, test and report (RUNTIME=runc|gvisor).
	RUNTIME=$(RUNTIME) ./e2e.sh all

.PHONY: e2e-gvisor
e2e-gvisor: ## Same as e2e, with runsc inside a Docker-in-Docker host.
	$(MAKE) --no-print-directory e2e RUNTIME=gvisor

.PHONY: up
up: ## Start the cdm, cua and mtls sandboxes.
	RUNTIME=$(RUNTIME) ./e2e.sh up

.PHONY: e2e-test
e2e-test: ## Run the checks against running sandboxes.
	RUNTIME=$(RUNTIME) ./e2e.sh test

.PHONY: stats
stats: ## Print image sizes and memory use.
	RUNTIME=$(RUNTIME) ./e2e.sh stats

.PHONY: info
info: ## Print viewer URLs and the token.
	./e2e.sh info

.PHONY: logs
logs: ## Tail sandbox logs (VARIANT=cdm|cua|mtls).
	RUNTIME=$(RUNTIME) ./e2e.sh logs $(VARIANT)

.PHONY: down
down: ## Remove all demo containers.
	./e2e.sh down

.PHONY: clean
clean: down kind-down ## Remove containers, the kind cluster and out/.
	rm -rf out

##@ Kubernetes end-to-end (kind)

.PHONY: kind-e2e
kind-e2e: ## kind + kagent CRDs: admit deploy/, roll out, test MCP via the Service.
	./k8s-e2e.sh all

.PHONY: kind-down
kind-down: ## Delete the kind cluster.
	./k8s-e2e.sh down

##@ Publish

.PHONY: push
push: ## Build and push multi-arch $(IMAGE):$(VARIANT) (needs docker login).
	docker buildx build --platform $(PLATFORMS) --target $(VARIANT) \
	  -t $(IMAGE):$(VARIANT) -t $(IMAGE):$(VARIANT)-$(GIT_SHA) --push .

.PHONY: push-all
push-all: ## push for every variant in VARIANTS.
	for v in $(VARIANTS); do $(MAKE) --no-print-directory push VARIANT=$$v; done

##@ Deploy to kagent

define OVERLAY_KUSTOMIZATION
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: $(NAMESPACE)
resources:
  - ../../deploy
configurations:
  - kustomizeconfig.yaml
images:
  - name: ghcr.io/dimetron/mcp-desktop
    newName: $(IMAGE)
    digest: $(DIGEST)
patches:
  - target: {kind: RemoteMCPServer, name: mcp-desktop}
    patch: |-
      - op: replace
        path: /spec/url
        value: http://mcp-desktop.$(NAMESPACE).svc.cluster.local:8080/mcp
  - target: {kind: SandboxTemplate, name: mcp-desktop}
    patch: |-
      - op: replace
        path: /spec/substrate/workerPoolRef/name
        value: $(WORKER_POOL)
      - op: replace
        path: /spec/substrate/snapshotPolicy/location
        value: $(SNAPSHOT_LOCATION)
endef
export OVERLAY_KUSTOMIZATION

.PHONY: manifests
manifests: ## Render out/deploy.yaml pinned to $(IMAGE)@$(DIGEST).
	@[[ "$(DIGEST)" =~ ^sha256:[a-f0-9]{64}$$ ]] || { echo "DIGEST must be sha256:<64 hex>; push $(IMAGE):$(IMAGE_TAG) first or set DIGEST" >&2; exit 1; }
	@mkdir -p $(OVERLAY)
	@cp deploy/kustomizeconfig.yaml $(OVERLAY)/
	@printf '%s\n' "$$OVERLAY_KUSTOMIZATION" >$(OVERLAY)/kustomization.yaml
	$(KUSTOMIZE) $(OVERLAY) >$(MANIFEST)
	@echo "wrote $(MANIFEST) ($(IMAGE)@$(DIGEST))"

.PHONY: deploy-secret
deploy-secret: ## Create the mcp-desktop-auth Secret once (GW_TOKEN or random).
	@if $(KUBECTL) -n $(NAMESPACE) get secret mcp-desktop-auth >/dev/null 2>&1; then \
	  echo "secret $(NAMESPACE)/mcp-desktop-auth exists"; \
	else \
	  t=$${GW_TOKEN:-$$(openssl rand -hex 24)}; \
	  $(KUBECTL) -n $(NAMESPACE) create secret generic mcp-desktop-auth \
	    --from-literal=token="$$t" --from-literal=authorization="Bearer $$t"; \
	fi

.PHONY: deploy
deploy: manifests deploy-secret ## Apply the MCP server, RemoteMCPServer and SandboxTemplate.
	$(KUBECTL) apply -f $(MANIFEST)
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/mcp-desktop --timeout=5m
	$(KUBECTL) -n $(NAMESPACE) get remotemcpserver/mcp-desktop sandboxtemplate/mcp-desktop

.PHONY: undeploy
undeploy: ## Delete the rendered resources (keeps the Secret).
	$(KUBECTL) delete -f $(MANIFEST) --ignore-not-found

.PHONY: port-forward
port-forward: ## Forward the MCP server to 127.0.0.1:$(LOCAL_PORT) and print the viewer URL.
	@t=$$($(KUBECTL) -n $(NAMESPACE) get secret mcp-desktop-auth -o jsonpath='{.data.token}' | base64 -d); \
	  echo "viewer: http://127.0.0.1:$(LOCAL_PORT)/#token=$$t"; \
	  echo "mcp:    http://127.0.0.1:$(LOCAL_PORT)/mcp"
	$(KUBECTL) -n $(NAMESPACE) port-forward svc/mcp-desktop $(LOCAL_PORT):8080
