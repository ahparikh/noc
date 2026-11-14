GO ?= go
VERSION ?= dev
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
DIST ?= $(CURDIR)/dist
PLATFORMS ?= darwin/arm64 darwin/amd64 linux/amd64 linux/arm64

PKG := noc.local/noc/internal/buildinfo
LDFLAGS := -s -w -X $(PKG).Version=$(VERSION) -X $(PKG).Commit=$(COMMIT) -X $(PKG).Date=$(DATE)

.PHONY: build dist test check fmt clean

build:
	mkdir -p bin && $(GO) build -ldflags '$(LDFLAGS)' -o bin/noc ./cmd/noc

# One static binary per platform, named noc-<os>-<arch>.
dist:
	@mkdir -p $(DIST)
	@set -e; for platform in $(PLATFORMS); do \
		os=$${platform%/*}; arch=$${platform#*/}; \
		echo "building noc-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags '$(LDFLAGS)' \
			-o $(DIST)/noc-$$os-$$arch ./cmd/noc; \
	done

test:
	$(GO) test ./...

check: test
	$(GO) vet ./...
	@unformatted="$$(gofmt -l .)"; if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; fi

fmt:
	$(GO) fmt ./...

clean:
	$(GO) clean
	rm -rf bin dist
