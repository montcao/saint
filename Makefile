# saint - terminal UI for local AI coding-agent sessions in Apple container microVMs.

BIN      := saint
PREFIX   ?= $(HOME)/.local
BINDIR   ?= $(PREFIX)/bin
# One pinned commitlint for hooks, CI and people, reading commitlint.config.mjs.
COMMITLINT := npx --yes -p @commitlint/cli@21.2.4 commitlint
# One pinned govulncheck for CI and people. It is built for this machine even
# when GOOS and GOARCH name another, which is how CI scans the darwin build
# from a Linux runner.
GOVULNCHECK_VERSION := v1.9.0
TOOLBIN := $(or $(TMPDIR),/tmp)/saint-tools
# How long `make fuzz` spends on each fuzz target.
FUZZTIME ?= 30s

.PHONY: all build install uninstall run test vet fmt fmt-check check docs-check vulncheck fuzz hooks commitlint-msg commitlint-range allow-firewall clean

all: build

build:
	go build -o $(BIN) ./cmd/saint

install: build
	@mkdir -p $(BINDIR)
	install -m 0755 $(BIN) $(BINDIR)/$(BIN)
	@echo "installed $(BINDIR)/$(BIN)"
	@case ":$$PATH:" in \
		*":$(BINDIR):"*) ;; \
		*) echo "warning: $(BINDIR) is not on PATH" ;; \
	esac

uninstall:
	rm -f $(BINDIR)/$(BIN)

run: build
	./$(BIN)

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l .); \
	if [ -n "$$out" ]; then echo "$$out"; echo "run: make fmt"; exit 1; fi

check: fmt-check vet test

# Fail on documents that point at files, targets or documents that do not exist.
docs-check:
	go run ./scripts/docs-check

# Known vulnerabilities in the code paths saint actually calls, in its
# dependencies and in the Go standard library it is built with.
vulncheck:
	@GOOS= GOARCH= GOBIN=$(TOOLBIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	$(TOOLBIN)/govulncheck ./...

# Explore every fuzz target for FUZZTIME each. `make check` already runs their
# seed inputs; this looks for new ones. A failing input is saved under
# testdata/fuzz and becomes a regression test once committed.
fuzz:
	@set -e; for pkg in $$(go list ./...); do \
		for target in $$(go test -list '^Fuzz' $$pkg | grep '^Fuzz' || true); do \
			echo "$$pkg $$target"; \
			go test $$pkg -run '^$$' -fuzz "^$$target$$" -fuzztime $(FUZZTIME); \
		done; \
	done

# Use the hooks in .githooks for this clone only: commit-msg runs commitlint,
# pre-push runs the fast checks. Undo with `git config --unset core.hooksPath`.
hooks:
	git config core.hooksPath .githooks
	@echo "hooks installed from .githooks"

# Lint one message file, or every commit in FROM..TO.
commitlint-msg:
	@test -n "$(MSG)" || { echo "usage: make commitlint-msg MSG=<file>"; exit 2; }
	$(COMMITLINT) --edit "$(MSG)"

commitlint-range:
	@test -n "$(FROM)" -a -n "$(TO)" || { echo "usage: make commitlint-range FROM=<sha> TO=<sha>"; exit 2; }
	$(COMMITLINT) --from "$(FROM)" --to "$(TO)"

# Allow the proxy to accept connections from containers. The proxy runs from
# saint's copy in the state directory, so that is the path allowed. macOS asks
# for an administrator's password in a dialog.
allow-firewall: build
	./$(BIN) firewall

clean:
	rm -f $(BIN)

# Opt-in research helper; never part of the normal build/install path.
.PHONY: experimental-adapter
experimental-adapter:
	./scripts/build-tool-disk-adapter
