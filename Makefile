.PHONY: build test test-functional tidy

# VERSION is stamped into `corral --version` (FR-22). It must not contain
# whitespace: the output is `corral <version>` on one line.
VERSION ?= devel

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/corral ./cmd/corral

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
