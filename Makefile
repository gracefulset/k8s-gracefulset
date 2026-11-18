IMG ?= gracefulset-controller:latest
# controller-gen v0.15.0 matches k8s.io/api v0.30 / Go 1.22.
CONTROLLER_GEN ?= controller-gen
SETUP_ENVTEST ?= setup-envtest
ENVTEST_K8S_VERSION ?= 1.30.x
CHART := charts/gracefulset-controller

.PHONY: all
all: build

# Generate the CRD straight into the Helm chart from the Go types.
.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd:generateEmbeddedObjectMeta=true paths="./api/..." output:crd:artifacts:config=$(CHART)/crds

.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths="./api/..."

.PHONY: build
build:
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate
	go run ./main.go

.PHONY: tools
tools:
	go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.15.0
	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.19

# Unit tests plus the envtest suite against a real kube-apiserver.
.PHONY: test
test:
	KUBEBUILDER_ASSETS="$$($(SETUP_ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" go test ./... -race -coverprofile cover.out

.PHONY: lint
lint:
	go vet ./...
	helm lint $(CHART)

# Fails if generated code or the CRD is out of date with the Go types.
.PHONY: verify
verify: generate manifests
	git diff --exit-code -- api $(CHART)/crds

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

.PHONY: deploy
deploy:
	helm upgrade --install gracefulset $(CHART) -n gracefulset-system --create-namespace
	# Helm does not upgrade CRDs in crds/, so apply them explicitly.
	kubectl apply --server-side -f $(CHART)/crds/
