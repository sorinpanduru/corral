.PHONY: build test test-386 test-functional tidy

build:
	go build -o bin/corral ./cmd/corral

# Unit and integration tests. The `functional` build tag is not set, so
# tests/functional is excluded.
test:
	go vet ./...
	go test -race -count=1 ./...

# 32-bit check of the domain packages (BSON length arithmetic must not wrap).
# CI step: run next to `make test`. No -race on 386.
test-386:
	GOARCH=386 go test -count=1 ./internal/domain/...

# Binary-level tests. No Docker needed for SPEC-001 (ADR-0005).
test-functional: build
	go test -race -count=1 -tags functional ./tests/functional/...

tidy:
	go mod tidy
