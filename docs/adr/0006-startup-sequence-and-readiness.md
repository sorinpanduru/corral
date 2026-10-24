# ADR-0006: Startup sequence, startup-complete and readiness gating

- Status: Accepted (planner, 2026-10-24). The readiness response details
  were decided by Orchestrator under Owner's standing instruction,
  2026-10-24, Owner may override (former question Q6, decision D6 in
  `plans/SPEC-001.md`).
- Amended by ADR-0013 (SPEC-003): steps 0 to 4 are replaced by the
  ADR-0013 sequence (mode selection, configuration load including `OTEL_*`
  validation, failure and success output paths). Steps 5 to 9, readiness
  and shutdown are unchanged.
- Amended by ADR-0021 (SPEC-002; SPEC-001 Amendment 3, item 10): binding
  the MongoDB wire listener is now the last startup step, after
  `tracing.Start`. The upstream monitor and pool start before it, without
  waiting. Shutdown runs the SPEC-002 FR-47 drain before the admin and
  tracing shutdown.
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

0. Parse and validate logical settings. Errors are logged with
   `config.LogError` via `logging.BootstrapLogger` (JSON on stdout,
   `component=config`, FR-27). Stdlib only: no grpc, otel or other
   third-party code runs in this step, so nothing can write to stderr.
1. Build the configured slog handler and the logging factory from the
   settings.
2. `logbridge.Install(factory, level, now)`: otel error handler and logger,
   grpclog, `slog.Default()` and std `log` bridges (FR-28), directly on the
   configured handler and level. This runs before any grpc, otel, tracing or
   admin-listener code, because grpc requires `grpclog.SetLoggerV2` before
   any other grpc call and the otel/grpc defaults write to stderr until it
   runs. No later handler or level switch is needed.
3. Resolve whether tracing is enabled and validate all honoured/validated
   `OTEL_*` variables (only if enabled; pure env parsing). Errors are logged
   with `config.LogError` via `Factory.Logger(logging.Config)` (FR-27).

Steps 0–3 decided by Orchestrator under Owner's standing instruction,
2026-10-24.

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

Shutdown (SIGINT/SIGTERM or context cancel): mark the admin listener
stopping, so `/readyz` returns 503 `shutting_down`; immediately emit the
`lifecycle` *shutting down* record (so `/readyz` reports `shutting_down`
from the moment that record exists); shut down the admin server; shut down tracing with a
5-second bound (FR-34); exit 0. Any startup failure exits with a non-zero
code after its error record is written.

Readiness (D6):

- All `/readyz` responses use `Content-Type: text/plain; charset=utf-8`.
  200 has body `ok\n`; 503 has body `<reason>\n`.
- Until step 9, `/readyz` returns 503 `starting`. Registered readiness
  checks are not consulted before then.
- After step 9, it delegates to the registered check if any, else returns
  200.
- If a registered check reports a reason outside the defined set, the
  response is 503 `unknown`; the reported reason is not echoed.
- From the moment the lifecycle *shutting down* record is emitted, `/readyz`
  returns 503 `shutting_down` regardless of any registered check.
- The defined reason set therefore includes `starting`, `shutting_down` and
  `unknown`; the spec writer is adding `unknown` and `shutting_down` to the
  spec.

## Consequences

- AC-31 is testable in-process by holding the `BeforeLastStep` hook.
- The startup-complete record and the 200 transition happen at the same
  point, so tests can assert their ordering.
