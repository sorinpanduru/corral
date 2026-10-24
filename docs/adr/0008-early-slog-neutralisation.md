# ADR-0008: Neutralise slog.Default during package init

Status: accepted (SPEC-001 FR-24, FR-28, Amendment 1 thirdparty)

## Context

go.opentelemetry.io/auto/sdk (indirect, linked through otel -> otel/internal/global,
so it cannot be removed) evaluates `var maxSpan = newSpanLimits()` at package
init. For invalid OTEL_*_LIMIT values it calls slog.Warn echoing the variable
name and raw value. That runs before main and before logbridge.Install, so the
stdlib default handler writes it to stderr.

## Decision

Package `internal/infra/telemetry/earlyslog` (stdlib imports only) sets, in its
init, a slog default handler that keeps only a per-level counter and drops
message and attributes. Go initialises packages in dependency order, ties
broken by import path; `github.com/sorinpanduru/...` sorts before
`go.opentelemetry.io/...` and earlyslog has no non-stdlib dependencies.
earlyslog does not run first overall: standard-library packages, and packages
that import only `log` (not `log/slog`), may initialise earlier. The property
is narrower: earlyslog initialises before every linked third-party package that
uses `log/slog` or `log` at init, apart from the reviewed allowlist. The
init-order guard (tests/functional/initorder_test.go) enforces this property.
cmd/corral blank-imports earlyslog and logbridge imports it, so it is always
linked. `logbridge.Install` calls `earlyslog.Handoff`, which returns the counts and makes
later records of handlers captured during init forward to the bridge handler.
Every early slog record is then replayed through the rate-limited,
fixed-message thirdparty default-slog handler (first per level emitted, the rest
counted in suppressed_count). Std `log` writes made during init are counted
separately and replayed through the std-log writer (own message and limiter).
Install has a tiny window between `slog.SetDefault(dh)` and `log.SetOutput(w)`
in which std log writes carry the slog-default source label; nothing leaks. A
`*log.Logger` that captured `log.Writer()` at init stays counted, not replayed.
Raw values are never retained or output.

## Consequences

Correctness relies on init ordering; this is pinned by the T22 functional
test (the binary must have empty stderr) and by the init-order guard, which
checks the property stated above against the linked dependency set. A future dependency whose import
path sorts before `github.com/sorinpanduru` and logs at init would not be
covered. Rejected: removing auto/sdk (unavoidable transitive dependency).

Residual risk: a package that imports only `log` (not `log/slog`) can be
initialised before `log/slog` and therefore before earlyslog, regardless of
import path (for example otel/internal/errorhandler). If such a package writes
to the log package during init, that output goes to stderr unneutralised.
Today none of the linked dependencies do so for the covered inputs.
