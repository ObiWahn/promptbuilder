SHELL := bash
.SHELLFLAGS := -euo pipefail -c

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

# --- gitleaks: secret scanning (from the gitleaks skill) ---
# check-secrets scans all commits (gitleaks git) and the worktree including
# uncommitted changes (gitleaks dir). Both scans always run; the target fails
# if either finds something. Secret values are redacted (percent, 0..100) to
# keep them out of the log; `make check-secrets GITLEAKS_REDACT=0` shows them.
# The log lives outside the repo, so the worktree scan never sees it.
# If $(GITLEAKS_PRIVATE) exists it is used as config (untracked, for personal
# patterns and allowlists); otherwise gitleaks' built-in rules apply.
# Install: GOBIN=~/.local/bin go install github.com/zricethezav/gitleaks/v8@latest
#
# git-hooks points git at the versioned .githooks/ dir, whose pre-commit hook
# runs check-secrets. This lives in .git/config (not versioned), so a fresh
# clone needs this target run once.
GITLEAKS_REPO    ?= $(abspath $(dir $(firstword $(MAKEFILE_LIST))))
GITLEAKS_LOG     ?= $(or $(XDG_CACHE_HOME),$(HOME)/.cache)/check-secrets/$(notdir $(GITLEAKS_REPO)).log
GITLEAKS_REDACT  ?= 100
GITLEAKS_PRIVATE ?= $(HOME)/.config/gitleaks/private.toml
GITLEAKS_FLAGS   := --redact=$(GITLEAKS_REDACT) --verbose --no-banner --no-color $(if $(wildcard $(GITLEAKS_PRIVATE)),--config $(GITLEAKS_PRIVATE))

.PHONY: check-secrets check-secrets-unredacted git-hooks

check-secrets: | $(dir $(GITLEAKS_LOG))
	command -v gitleaks >/dev/null || { echo "gitleaks not found, install: GOBIN=~/.local/bin go install github.com/zricethezav/gitleaks/v8@latest" >&2; exit 1; }
	{ rc=0; \
	  gitleaks git $(GITLEAKS_FLAGS) $(GITLEAKS_REPO) || rc=1; \
	  gitleaks dir $(GITLEAKS_FLAGS) $(GITLEAKS_REPO) || rc=1; \
	  exit $$rc; } 2>&1 | tee $(GITLEAKS_LOG)

# Same scans as check-secrets, but showing the actual secret values, so you
# can tell a real leak from a false positive. Deliberately no `| tee`: the
# values go to the terminal only and are never written to a log file.
check-secrets-unredacted:
	command -v gitleaks >/dev/null || { echo "gitleaks not found, install: GOBIN=~/.local/bin go install github.com/zricethezav/gitleaks/v8@latest" >&2; exit 1; }
	rc=0; \
	gitleaks git $(subst --redact=$(GITLEAKS_REDACT),--redact=0,$(GITLEAKS_FLAGS)) $(GITLEAKS_REPO) || rc=1; \
	gitleaks dir $(subst --redact=$(GITLEAKS_REDACT),--redact=0,$(GITLEAKS_FLAGS)) $(GITLEAKS_REPO) || rc=1; \
	exit $$rc

git-hooks:
	git -C $(GITLEAKS_REPO) config core.hooksPath .githooks

# Directory targets end in '/' so they never clash with phony targets of the same name.
%/:
	mkdir -p $@
