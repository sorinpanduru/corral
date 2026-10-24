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
`go.opentelemetry.io/...` and earlyslog has no non-stdlib dependencies, so it
runs first. cmd/corral blank-imports it and logbridge imports it, so it is
always linked. `logbridge.Install` drains the counters and replays at most one
record per level through the thirdparty default-slog handler (fixed message,
rate limited). Raw values are never retained or output.

## Consequences

Correctness relies on init ordering; this is pinned by the T22 functional
test (the binary must have empty stderr). A future dependency whose import
path sorts before `github.com/sorinpanduru` and logs at init would not be
covered. Rejected: removing auto/sdk (unavoidable transitive dependency).
