VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: build install test clean

build:
	go build -trimpath -ldflags "$(LDFLAGS)" -o bin/flushgopher ./cmd/flushgopher

install: build
	install -m 0755 bin/flushgopher $(HOME)/.local/bin/flushgopher

test:
	go vet ./...
	go test -race ./...

clean:
	rm -rf bin
