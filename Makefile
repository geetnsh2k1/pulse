.PHONY: build test lint fmt install clean

BIN := bin/pulse
LDFLAGS := -X github.com/geetnsh2k1/pulse/internal/version.Version=$(shell cat VERSION 2>/dev/null || echo 0.1.0-dev)

build:
	go build -ldflags "$(LDFLAGS)" -o $(BIN) ./cmd/pulse

test:
	go test -race ./...

lint:
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

fmt:
	gofmt -w .

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/pulse

clean:
	rm -rf bin dist
