.PHONY: build check clean dev dev-stop e2e fmt fmt-check lint test

CARGO ?= $(if $(wildcard $(HOME)/.cargo/bin/cargo),$(HOME)/.cargo/bin/cargo,cargo)
WASM_TARGET ?= wasm32-wasip1

build:
	$(CARGO) build --release --target $(WASM_TARGET)

check: fmt-check lint test

fmt:
	$(CARGO) fmt --all

fmt-check:
	$(CARGO) fmt --all -- --check

# Clippy on the WASM target replaces a separate `cargo check`.
lint:
	$(CARGO) clippy --target $(WASM_TARGET) -- -D warnings
	$(CARGO) clippy --all-targets -- -D warnings

test:
	$(CARGO) test

# Headless end-to-end test of the nested pair (zellij, tmux, jq).
e2e: build
	e2e/nested.sh

# The isolated dev pair, with the real noc from ../go/bin.
dev: build
	./dev.sh start

dev-stop:
	./dev.sh stop

clean:
	$(CARGO) clean
