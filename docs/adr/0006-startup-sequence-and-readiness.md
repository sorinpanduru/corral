# ADR-0006: Startup sequence, startup-complete and readiness gating

- Status: Accepted (planner, 2026-10-24), with the readiness response details
  Proposed pending Owner answer Q6 in `plans/SPEC-001.md`.
- Related: SPEC-001 FR-7 (lifecycle), FR-18, FR-27, FR-28, FR-34, FR-35,
  AC-3, AC-21, AC-30, AC-31

## Context

FR-18 defines *startup completed* as the moment every startup step has
finished; until the wire listener exists the last step is "binding the admin
listener and finishing the tracing bootstrap". `/readyz` must return 503
`starting` while startup is held before its last step (AC-31), so the admin
listener must already be serving at that point. Invalid configuration,
including `OTEL_*` values when tracing is enabled, must stop startup (FR-27).

## Decision

Startup order in `internal/app/runtime`:

1. Parse and validate logical settings. Errors go to a bootstrap JSON logger
   on stdout with `component=config` (FR-27).
2. Resolve whether tracing is enabled and validate all honoured/validated
   `OTEL_*` variables (only if enabled).
3. Build the configured slog handler; `slog.SetDefault`; install the
   otel, grpclog and default-logger bridges (FR-28). grpclog is installed
   before any gRPC code runs.
4. Emit the `lifecycle` *starting* record.
5. Lint the embedded metric catalog; build the private Prometheus registry
   and register catalog metrics (including
   `corral_tracing_spans_dropped_total`).
6. Bind the admin listener (failure: FR-35) and start serving. `/healthz`
   is live; `/readyz` answers 503 `starting`.
7. Test hook `BeforeLastStep` (no-op in production).
8. Last step: start the tracing provider (exporter and processor if
   enabled). When the wire listener exists, binding it becomes the last step
   and this ADR is updated.
9. Mark startup complete in the admin listener, then emit the `lifecycle`
   *startup-complete* record.

Shutdown (SIGINT/SIGTERM or context cancel): emit `lifecycle` *shutting
down*; mark not ready; shut down the admin server; shut down tracing with a
5-second bound (FR-34); exit 0. Any startup failure exits with a non-zero
code after its error record is written.

Readiness (proposed, Q6): `/readyz` returns 503 `starting` until step 9
regardless of any registered readiness check, then delegates to the
registered check if any, else returns 200. Response bodies are
`text/plain; charset=utf-8`: `ok\n` for 200 and `<reason>\n` for 503.

## Consequences

- AC-31 is testable in-process by holding the `BeforeLastStep` hook.
- The startup-complete record and the 200 transition happen at the same
  point, so tests can assert their ordering.
