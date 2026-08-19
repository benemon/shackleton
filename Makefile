VERSION ?= $(shell git describe --tags --always --dirty)
CONTAINER_TOOL ?= podman
LDFLAGS := -s -w -X main.version=$(VERSION)

ui:
	cd ui && npm ci && npm run build

build:
	CGO_ENABLED=0 go build $(if $(GOTAGS),-tags $(GOTAGS)) -trimpath -ldflags "$(LDFLAGS)" -o bin/shackleton ./cmd/shackleton

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(if $(GOTAGS),-tags $(GOTAGS)) -trimpath -ldflags "$(LDFLAGS)" -o bin/shackleton-linux-amd64 ./cmd/shackleton

test:
	@fmt=$$(gofmt -l .); if [ -n "$$fmt" ]; then echo "gofmt: $$fmt"; exit 1; fi
	go vet ./...
	go test ./... -count=1

image:
	$(CONTAINER_TOOL) build -t shackleton:$(VERSION) --build-arg APP_VERSION=$(VERSION) .

clean:
	rm -rf bin

.PHONY: ui build linux test image clean
