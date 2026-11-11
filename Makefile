GO ?= go
PYTHON ?= python3
DIST ?= dist
GOEXE := $(shell $(GO) env GOEXE)
HOST_BIN := $(DIST)/praector$(GOEXE)
LDFLAGS ?= -s -w

.PHONY: build build-all build-linux build-windows build-darwin install test test-go test-reference parity check smoke smoke-state sync-assets check-assets clean

# Build the native executable for the current host OS/architecture.
build: check-assets
	@mkdir -p $(DIST)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(HOST_BIN) ./cmd/praector
	@echo "Built $(HOST_BIN)"

# Cross-build the release matrix. These targets do not require CGO/toolchains.
build-all: check-assets build-linux build-windows build-darwin

build-linux:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-linux-amd64 ./cmd/praector
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-linux-arm64 ./cmd/praector

build-windows:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-windows-amd64.exe ./cmd/praector
	CGO_ENABLED=0 GOOS=windows GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-windows-arm64.exe ./cmd/praector

build-darwin:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-darwin-amd64 ./cmd/praector
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(DIST)/praector-darwin-arm64 ./cmd/praector

# Go is the primary runtime. The Python implementation remains in-tree as a
# reference implementation until native parity has been exercised in production.
test: test-go test-reference

test-go: check-assets
	$(GO) test ./...

test-reference:
	$(PYTHON) -m compileall -q src
	$(PYTHON) -m pytest -q

# Pure compatibility checks that intentionally keep the Go and Python contracts
# synchronized while both implementations are present.
parity: build
	$(PYTHON) scripts/parity_check.py --go $(HOST_BIN)

check: check-assets
	@test -z "$$($(GO)fmt -l cmd internal)" || { echo "gofmt required:"; $(GO)fmt -l cmd internal; exit 1; }
	$(GO) vet ./...
	$(GO) test ./...
	$(PYTHON) -m compileall -q src
	$(PYTHON) -m pytest -q

smoke: build
	$(HOST_BIN) --version
	$(HOST_BIN) --help >/dev/null
	$(HOST_BIN) collection pr --help >/dev/null
	$(HOST_BIN) task request --help >/dev/null
	$(HOST_BIN) profile list >/dev/null
	$(HOST_BIN) profile show implement >/dev/null
	@echo "Native smoke checks passed"

# Optional binary-to-store smoke. With a real Dolt on PATH this exercises a fresh
# Git repository and isolated PRAECTOR_HOME end to end. In build environments without
# Dolt, the script falls back to a deliberately slower SQLite-backed Dolt shim.
smoke-state: build
	$(PYTHON) scripts/native_smoke.py --praector $(HOST_BIN)

# While the Python reference remains in-tree, its assets are the reference copy.
# The Go binary embeds synchronized copies so it has no runtime Python/package-data dependency.
sync-assets:
	@mkdir -p internal/assets/prompts
	cp src/praector/schema.sql internal/assets/schema.sql
	cp src/praector/prompts/*.md internal/assets/prompts/

check-assets:
	@cmp -s src/praector/schema.sql internal/assets/schema.sql || { echo "internal/assets/schema.sql is out of sync; run 'make sync-assets'"; exit 1; }
	@for f in src/praector/prompts/*.md; do b=$$(basename $$f); cmp -s $$f internal/assets/prompts/$$b || { echo "internal/assets/prompts/$$b is out of sync; run 'make sync-assets'"; exit 1; }; done

install: build
	@mkdir -p $(HOME)/.local/bin
	cp $(HOST_BIN) $(HOME)/.local/bin/praector$(GOEXE)
	@echo "Installed $(HOME)/.local/bin/praector$(GOEXE)"

clean:
	rm -rf $(DIST)
