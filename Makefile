# ABOUTME: Canonical checks for pr-babysitter: `make check` before every commit, `make e2e` for end-to-end runs.
# ABOUTME: Lint targets are globbed, so files that later tasks add get checked without editing this file.

ZIZMOR ?= uvx zizmor@1.30.1
YAML := $(wildcard .github/workflows/*.yml examples/*.yml action.yml)
SHELL_SCRIPTS := $(wildcard sandbox.sh e2e/*.sh)

.PHONY: check e2e

check:
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go vet ./...
	go test ./...
	$(ZIZMOR) $(YAML)
	$(if $(SHELL_SCRIPTS),shellcheck -x $(SHELL_SCRIPTS))

e2e: # scenarios to run: make e2e N="4 5 6 7"
	e2e/run.sh $(N)
