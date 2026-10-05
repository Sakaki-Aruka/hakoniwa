# hakoniwa is pure Go (no cgo): any 64-bit Linux target can be built from any
# host, e.g. `make dist` or `GOARCH=arm64 make build`.

VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -s -w -X main.version=$(VERSION)
PLATFORMS ?= linux/amd64 linux/arm64 linux/riscv64
GOOS      ?= linux
GOARCH    ?= $(shell go env GOARCH)

export CGO_ENABLED := 0

.PHONY: build dist test clean

build:
	GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -ldflags "$(LDFLAGS)" -o hakoniwa ./cmd/hakoniwa

dist:
	@mkdir -p dist
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "building dist/hakoniwa-$$os-$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o dist/hakoniwa-$$os-$$arch ./cmd/hakoniwa || exit 1; \
	done

test:
	go test ./...

clean:
	rm -rf hakoniwa dist
