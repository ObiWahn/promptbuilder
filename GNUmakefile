SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

BINDIR      ?= /usr/local/bin
BUILD_DIR   := .build
TEST_TIMEOUT := 30s
LDFLAGS     := -s -w

.PHONY: all build install fmt test restart-daemon clean

all: build

$(BUILD_DIR):
	mkdir -p $(BUILD_DIR)

build: $(BUILD_DIR)
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o promptbuilder . 2>&1 | tee $(BUILD_DIR)/build.log

# Built as you, installed root-owned: the binary runs on every prompt
# render, so nothing running as you may be able to replace it.
install: build
	sudo install -o root -g root -m 0755 promptbuilder $(BINDIR)/promptbuilder 2>&1 | tee $(BUILD_DIR)/install.log

fmt: $(BUILD_DIR)
	gofmt -l -w . && go vet ./... 2>&1 | tee $(BUILD_DIR)/fmt.log

test: $(BUILD_DIR)
	timeout $(TEST_TIMEOUT) go test ./... 2>&1 | tee $(BUILD_DIR)/test.log

# Rebuilding doesn't affect an already-running daemon - it keeps serving
# with whatever code it was started with until it idles out on its own.
# Run this after `install` if you want a rebuilt daemon to take effect now
# rather than whenever it next self-evicts everything and exits.
restart-daemon:
	pid=$$(for p in /proc/[0-9]*/cmdline; do \
		{ mapfile -d '' -t argv < "$$p"; } 2>/dev/null || continue; \
		if [[ "$$(basename "$${argv[0]:-}" 2>/dev/null)" == "promptbuilder" && "$${argv[1]:-}" == "--daemon" ]]; then \
			basename "$$(dirname "$$p")"; \
		fi; \
	done); \
	if [[ -n "$$pid" ]]; then kill $$pid && echo "killed daemon pid $$pid - it will respawn on the next prompt render"; \
	else echo "no daemon currently running"; fi

clean:
	rm -rf $(BUILD_DIR) promptbuilder
