REGISTRY ?= kaviankarimzadeh
TAG      ?= v0.0.11
PLATFORM ?= linux/amd64

.DEFAULT_GOAL := help

.PHONY: help build test agent-image controller-image images push \
        helm-lint helm-template run-local run-agent-local clean

help: ## Show this help
	@printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"
	@awk -F':.*?## ' '/^[a-zA-Z0-9_-]+:.*## / { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 }' \
	      $(MAKEFILE_LIST)
	@printf "\nVariables:\n  REGISTRY=%s\n  TAG=%s\n  PLATFORM=%s\n\n" "$(REGISTRY)" "$(TAG)" "$(PLATFORM)"

build: ## Compile the Go binaries locally
	go build ./...

test: ## Run Go unit tests
	go test ./...

agent-image: ## Build the agent container image (linux/amd64)
	docker buildx build --platform=$(PLATFORM) --load \
		-f agent/Dockerfile -t $(REGISTRY)/netcat-agent:$(TAG) .

controller-image: ## Build the controller container image (linux/amd64)
	docker buildx build --platform=$(PLATFORM) --load \
		-f controller/Dockerfile -t $(REGISTRY)/netcat-controller:$(TAG) .

images: agent-image controller-image ## Build both container images

push: ## Build and push both container images
	docker buildx build --platform=$(PLATFORM) --push \
		-f agent/Dockerfile -t $(REGISTRY)/netcat-agent:$(TAG) .
	docker buildx build --platform=$(PLATFORM) --push \
		-f controller/Dockerfile -t $(REGISTRY)/netcat-controller:$(TAG) .

helm-lint: ## helm lint the chart
	helm lint charts/netcat

helm-template: ## Render chart with default values to stdout
	helm template netcat charts/netcat

run-local: ## Run the controller locally against your current kube context
	KUBECONFIG_DIR=$${HOME}/.kube/netcat-configs \
	LOCAL_CLUSTER_NAME=$$(kubectl config current-context) \
	go run ./controller

run-agent-local: ## Run an agent locally (probes from this machine)
	NODE_NAME=$$(hostname) go run ./agent

clean: ## Remove build artifacts
	rm -rf ./bin ./out
	go clean ./...
