REGISTRY ?= ghcr.io/kavian
TAG      ?= latest
PLATFORM ?= linux/amd64

.PHONY: all build test images push agent-image controller-image run-local run-agent-local helm-lint

all: build

build:
	go build ./...

test:
	go test ./...

agent-image:
	docker buildx build --platform=$(PLATFORM) --load \
		-f agent/Dockerfile -t $(REGISTRY)/netcat-agent:$(TAG) .

controller-image:
	docker buildx build --platform=$(PLATFORM) --load \
		-f controller/Dockerfile -t $(REGISTRY)/netcat-controller:$(TAG) .

images: agent-image controller-image

push:
	docker buildx build --platform=$(PLATFORM) --push \
		-f agent/Dockerfile -t $(REGISTRY)/netcat-agent:$(TAG) .
	docker buildx build --platform=$(PLATFORM) --push \
		-f controller/Dockerfile -t $(REGISTRY)/netcat-controller:$(TAG) .

helm-lint:
	helm lint charts/netcat

# Run the controller locally against your current kubectl context.
# The UI will be at http://localhost:8080
run-local:
	KUBECONFIG_DIR=$${HOME}/.kube/netcat-configs \
	LOCAL_CLUSTER_NAME=$$(kubectl config current-context) \
	go run ./controller

run-agent-local:
	NODE_NAME=$$(hostname) go run ./agent
