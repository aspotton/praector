GO ?= go
DIST ?= dist
GOEXE := $(shell $(GO) env GOEXE)
HOST_BIN := $(DIST)/praector$(GOEXE)
LDFLAGS ?= -s -w

.PHONY: build build-all build-linux build-windows build-darwin install test test-go check smoke clean

# Build the native executable for the current host OS/architecture.
build:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags="$(LDFLAGS)" -o $(HOST_BIN) ./cmd/praector
	@echo "Built $(HOST_BIN)"

# Cross-build the release matrix. These targets do not require CGO/toolchains.
build-all: build-linux build-windows build-darwin

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

test: test-go

test-go:
	$(GO) test ./...

check:
	@test -z "$$($(GO)fmt -l cmd internal)" || { echo "gofmt required:"; $(GO)fmt -l cmd internal; exit 1; }
	$(GO) vet ./...
	$(GO) test ./...

smoke: build
	$(HOST_BIN) --version
	$(HOST_BIN) --help >/dev/null
	$(HOST_BIN) collection pr --help >/dev/null
	$(HOST_BIN) task request --help >/dev/null
	$(HOST_BIN) profile list >/dev/null
	$(HOST_BIN) profile show implement >/dev/null
	@echo "Native smoke checks passed"

install: build
	@mkdir -p $(HOME)/.local/bin
	cp $(HOST_BIN) $(HOME)/.local/bin/praector$(GOEXE)
	@echo "Installed $(HOME)/.local/bin/praector$(GOEXE)"

clean:
	rm -rf $(DIST)
