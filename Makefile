# Root dispatcher: every target delegates to go/ or zellij/.
#
#   make check zellij-check     all unit tests and linters
#   make build zellij           go/bin/noc and the plugin (.wasm)
#   make dev                    run this checkout as an isolated dev pair
#   make zellij-e2e             headless end-to-end test of the nested pair
#   make dist VERSION=v1.2.3    release artifacts in dist/
.PHONY: build test check fmt clean zellij zellij-check zellij-fmt zellij-test \
	zellij-clean zellij-e2e zellij-install-config dev dev-installed dev-stop dist

VERSION ?= dev
DIST := $(CURDIR)/dist

build:
	$(MAKE) -C go build VERSION=$(VERSION)
test:
	$(MAKE) -C go test
check:
	$(MAKE) -C go check
fmt:
	$(MAKE) -C go fmt
clean:
	$(MAKE) -C go clean
	$(MAKE) -C zellij clean
	rm -rf $(DIST)

zellij:
	$(MAKE) -C zellij build
zellij-check:
	$(MAKE) -C zellij check
zellij-fmt:
	$(MAKE) -C zellij fmt
zellij-test:
	$(MAKE) -C zellij test
zellij-clean:
	$(MAKE) -C zellij clean
zellij-e2e:
	$(MAKE) -C zellij e2e

# The dev pair: this checkout's binary and plugin under their own session
# names, Zellij config directory and noc state. See docs/DEVELOPMENT.md.
# The dev pair: this checkout's binary and plugin, isolated from daily use.
#   dev             with this checkout's zellij/config/ files
#   dev-installed   with your installed Zellij configuration
dev: build zellij
	zellij/dev.sh start
dev-installed: build zellij
	zellij/dev.sh --installed start
dev-stop:
	zellij/dev.sh stop

# Put zellij/config/ into your Zellij configuration directory.
zellij-install-config:
	zellij/install-config.sh

# Everything a release carries: noc for each platform, the plugin, checksums.
dist:
	@case '$(VERSION)' in \
	v[0-9]*) for f in zellij/config/layouts/noc-host.kdl zellij/config/layouts/noc-inner.kdl; do \
		if grep 'zellij-noc\.wasm' $$f | grep -q -v -- 'releases/download/$(VERSION)/zellij-noc.wasm' || \
			! grep -q -- 'releases/download/$(VERSION)/zellij-noc.wasm' $$f; then \
		echo "$$f does not load the plugin from $(VERSION); update the plugin URL there (and in the docs) first" >&2; exit 1; fi; done; \
		grep -q -- '^version = "$(VERSION:v%=%)"$$' zellij/Cargo.toml || { \
		echo "zellij/Cargo.toml is not version $(VERSION:v%=%); the plugin compares releases against it" >&2; exit 1; } ;; \
	esac
	rm -rf $(DIST) && mkdir -p $(DIST)
	$(MAKE) -C go dist VERSION=$(VERSION) DIST=$(DIST)
	$(MAKE) -C zellij build
	cp zellij/target/wasm32-wasip1/release/zellij-noc.wasm $(DIST)/zellij-noc.wasm
	cd $(DIST) && (shasum -a 256 noc-* zellij-noc.wasm 2>/dev/null || sha256sum noc-* zellij-noc.wasm) > checksums.txt
	@ls -l $(DIST)
