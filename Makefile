IMG ?= gracefulset-controller:latest

.PHONY: all
all: build

.PHONY: manifests
manifests:
	controller-gen rbac:roleName=gracefulset-controller crd paths="./..." output:crd:artifacts:config=config/crd

.PHONY: generate
generate:
	controller-gen object paths="./..."

.PHONY: build
build:
	go build -o bin/manager main.go

.PHONY: run
run: manifests generate
	go run ./main.go

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: docker-push
docker-push:
	docker push $(IMG)

.PHONY: deploy
deploy: manifests
	kubectl apply -f config/crd/
	kubectl apply -f config/rbac/
	kubectl apply -f config/manager/

.PHONY: test
test:
	go test ./... -v -coverprofile cover.out
