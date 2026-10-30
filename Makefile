BINARY  := eci-runner
PKG     := github.com/x-botlab/eci-actions-runner
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

# Runner image. RUNNER_VERSION must track the upstream release: a JIT runner
# cannot disable its own auto-update, so a stale image updates itself on start.
REGISTRY       ?= registry.cn-hangzhou.aliyuncs.com/xbotlab
IMAGE          ?= $(REGISTRY)/actions-runner
RUNNER_VERSION ?= 2.338.2
TAG            ?= $(RUNNER_VERSION)
NODE_VERSION   ?= 24.22.2
GO_VERSION     ?= 1.27.3
PLATFORM       ?= linux/amd64
APT_MIRROR     ?=

.PHONY: build run test vet fmt fmt-check tidy check docker clean \
        image-build image-push image-versions

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/eci-runner

run:
	go run ./cmd/eci-runner server --config pools.yaml

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

fmt-check:
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

tidy:
	go mod tidy

check: fmt-check vet test

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(BINARY):$(VERSION) .

# ACR rejects the attestation manifest BuildKit adds by default, so both are off.
image-build:
	docker build --platform $(PLATFORM) -f image/Dockerfile \
	  --build-arg RUNNER_VERSION=$(RUNNER_VERSION) \
	  --build-arg NODE_VERSION=$(NODE_VERSION) \
	  --build-arg GO_VERSION=$(GO_VERSION) \
	  --build-arg APT_MIRROR=$(APT_MIRROR) \
	  --provenance=false --sbom=false \
	  -t $(IMAGE):$(TAG) image/

image-push: image-build
	docker tag $(IMAGE):$(TAG) $(IMAGE):latest
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest

image-versions:
	@printf 'pinned runner: %s\n' "$(RUNNER_VERSION)"
	@printf 'upstream      : %s\n' "$$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')"

clean:
	rm -rf bin
