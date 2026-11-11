GO            ?= go
STATICCHECK   := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK   := golang.org/x/vuln/cmd/govulncheck@v1.9.0
BIN           := bin/ferrogo
EXAMPLES      := $(wildcard examples/*/main.go)

.PHONY: all build test test-short race vet staticcheck vulncheck lint examples fmt-check release-check generate generate-check clean

all: lint test build

build:
	$(GO) build -o $(BIN) ./cmd/ferrogo

# Unit + integration tests. Integration tests skip automatically when no
# Rust toolchain is installed.
test:
	$(GO) test -count=1 ./...

# Unit tests only (no Rust toolchain needed).
test-short:
	$(GO) test -short -count=1 ./...

race:
	$(GO) test -race -count=1 ./...

vet:
	$(GO) vet ./...
	GOOS=windows $(GO) vet ./...
	GOOS=linux $(GO) vet ./...

staticcheck:
	$(GO) run $(STATICCHECK) ./...

vulncheck:
	$(GO) run $(GOVULNCHECK) ./...

# Validate the release configuration and build all release binaries locally
# (no publishing). Requires goreleaser v2.
release-check:
	goreleaser check
	goreleaser release --snapshot --clean

# Regenerate inline-Rust wrappers (requires Rust + wasm32-wasip1).
generate:
	$(GO) generate ./...

# Verify generated wrappers match their Rust sources (no Rust needed).
generate-check:
	$(GO) run ./cmd/ferrogo generate -check ./examples/inline

fmt-check:
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

lint: fmt-check vet staticcheck

# Build and run every example (requires a Rust toolchain).
examples:
	@for f in $(EXAMPLES); do echo "== $$(dirname $$f)"; $(GO) run ./$$(dirname $$f) || exit 1; done

clean:
	rm -rf bin
