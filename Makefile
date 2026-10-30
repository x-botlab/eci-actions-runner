BINARY  := eci-runner
PKG     := github.com/x-botlab/eci-actions-runner
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X $(PKG)/internal/cli.Version=$(VERSION)

.PHONY: build run test vet fmt fmt-check tidy check docker clean

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

clean:
	rm -rf bin
