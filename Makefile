.PHONY: build test test-functional tidy

# VERSION is stamped into `corral --version` (FR-22). It must not contain
# whitespace: the output is `corral <version>` on one line.
VERSION ?= devel

# VERSION is checked and used in one recipe line, read through the shell
# environment (never pasted into the command by make), so even `make -i`
# cannot continue past a rejected value. It is checked, as a whole string, so that a
# multi-line or quoted value cannot reach the shell. It is exported so the
# recipe shell sees it unexpanded (recipe environments receive exported
# variables in every GNU make version). GOFLAGS is cleared so an environment
# flag such as -tags=corraltest cannot leak into a release build.
export VERSION

build:
	@case "$$VERSION" in ''|*[!A-Za-z0-9._+-]*) echo 'VERSION must match ^[A-Za-z0-9._+-]+$$' >&2; exit 1;; esac; \
	GOFLAGS= go build -ldflags "-X main.version=$$VERSION" -o bin/corral ./cmd/corral

# Unit and integration tests. The `functional` build tag is not set, so
# tests/functional is excluded.
test:
	go vet ./...
	go test -race -count=1 ./...

# Binary-level tests. No Docker needed for SPEC-001 (ADR-0005).
test-functional: build
	go test -race -count=1 -tags functional ./tests/functional/...

tidy:
	go mod tidy
