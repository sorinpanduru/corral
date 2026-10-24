# ADR-0001: Repository layout, DDD layering and file ownership

- Status: Accepted (planner, 2026-10-24, for SPEC-001)
- Related: CLAUDE.md (Architecture, Factory layout), SPEC-001 FR-1, FR-2, AC-1

## Context

corral uses Domain-Driven Design. SPEC-001 FR-1 fixes one hard boundary:
packages under `github.com/sorinpanduru/corral/internal/domain/` may import
only the standard library (minus `log/slog`), other domain packages, and an
empty allowlist. The factory splits work between implementers (production
code) and test writers (tests, under `tests/` per CLAUDE.md). Shared test
harnesses (log-check handler, marker scanner, fake OTLP collector, import and
constant-message checkers) are code under test in their own right; they have
self-tests.

## Decision

Layout (Go module root = repo root):

| Path | Layer / purpose | May import |
|---|---|---|
| `cmd/corral/` | binary entry point; only calls `internal/app/runtime` | anything under `internal/` except `internal/testkit` |
| `internal/domain/...` | domain model and ports (readiness port, later sessions, pooling, routing, wire model, observer ports) | FR-1 only |
| `internal/app/...` | application layer: composition root, startup and shutdown lifecycle | domain, infra |
| `internal/infra/...` | adapters: config parsing, telemetry (logging, metrics, tracing, log bridges, error classification, sanitisers), admin HTTP listener; later the wire listener and upstream pool I/O | domain, other infra |
| `internal/testkit/...` | non-`_test.go` helper packages used only by tests (harnesses, fakes, static checkers) | anything; never imported by production packages |
| `tests/lint/` | repo-wide static checks run as Go tests (AC-1, AC-5 static part) | testkit, infra |
| `tests/integration/...` | in-process multi-package integration tests (tracing bootstrap, marker scans) | anything |
| `tests/functional/...` | binary-level tests, build tag `functional` | anything |
| `docs/` | operator documentation and ADRs | n/a |

Dependency direction: `cmd` -> `app` -> `infra` -> `domain`. `domain` never
imports `app`, `infra` or `testkit`.

Observer ports (FR-2) are declared in the domain package that owns the
concept. Their adapters live under `internal/infra/telemetry/...`.
Classification of domain errors in infra is done by duck-typed interfaces (for
example a server-reply error exposes its code and code name through methods),
so infra does not need concrete domain error types before they exist.

Ownership:

- Implementers own all non-test `.go` files under `cmd/`, `internal/`
  (including `internal/testkit/`), plus `Makefile`, `go.mod`, `go.sum`,
  `LICENSE`, `docs/operations/`, and any embedded data files (for example the
  metric catalog).
- Test writers own every `*_test.go` file anywhere, every `testdata/`
  directory, and everything under `tests/`.
- Unit tests are co-located `_test.go` files. They should use external test
  packages (`package foo_test`) so they can import `internal/testkit/...`
  without import cycles.

Build tags: `tests/functional/...` uses the `functional` build tag and is run
only by `make test-functional`. Everything else, including
`tests/integration/...` and `tests/lint/...`, runs under `make test`.

## Consequences

- AC-1 needs at least one package under `internal/domain/`, otherwise
  `go list ./internal/domain/...` matches nothing. SPEC-001 introduces
  `internal/domain/health` (readiness port) for this reason and because the
  readiness port is a domain concept implemented by the future pool.
- Production code must not import `internal/testkit`. Reviewers check this;
  a later lint can enforce it.
- The `tests/` ownership rule in CLAUDE.md is preserved: harness code that
  needs an independent self-test sits in `internal/testkit/` (implementer) and
  its self-tests sit in `_test.go` files or `tests/` (test writer).
