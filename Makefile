# saint - terminal UI for local AI coding-agent sessions in Apple container microVMs.

BIN      := saint
PREFIX   ?= $(HOME)/.local
BINDIR   ?= $(PREFIX)/bin
FIREWALL := /usr/libexec/ApplicationFirewall/socketfilterfw

.PHONY: all build install uninstall run test vet fmt fmt-check check allow-firewall clean

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

# Allow the installed binary to accept connections from containers.
# Needs sudo. Re-run after reinstalling to a new path.
allow-firewall:
	@test -x $(BINDIR)/$(BIN) || { echo "run: make install"; exit 1; }
	sudo $(FIREWALL) --add $(BINDIR)/$(BIN)
	sudo $(FIREWALL) --unblockapp $(BINDIR)/$(BIN)

clean:
	rm -f $(BIN)
