# ADR-0007: Tracing bootstrap structure and bounded-queue span processor

- Status: Accepted (planner, 2026-10-24, for SPEC-001)
- Related: SPEC-001 FR-8, FR-19, FR-20, FR-21, FR-27, FR-30, FR-33, FR-34,
  FR-37

## Context

Tracing is off by default and, when off, must create no spans, start no
exporter and open no connection. When on, corral must validate a defined set
of `OTEL_*` variables (fatal if invalid), choose the OTLP protocol, filter
resource attributes, use its own bounded-queue processor that counts drops,
and flush for at most 5 seconds on shutdown.

## Decision

- Package `internal/infra/telemetry/tracing` is split into:
  - `env.go`-level validation: a pure function over an env lookup function
    that returns either a validated configuration or a config error naming
    the variable. It is called in startup step 2 (ADR-0006), before anything
    binds.
  - sampler and resource construction (pure, unit-testable).
  - `Start`, which builds the provider in startup step 8.
- Disabled tracing returns the OTel no-op `TracerProvider` and reports
  `Enabled() == false`. Nothing else is constructed. The log trace-correlation
  handler is not installed when disabled (FR-8).
- `OTEL_TRACES_EXPORTER=none` builds an SDK provider with sampler and
  resource but no span processor or exporter; `Enabled()` is true. The log
  trace-correlation handler is installed, so logs carry `trace_id`/`span_id`
  whenever tracing is enabled, including with `none` (decision D9 in
  `plans/SPEC-001.md`, decided by Orchestrator under Owner's standing
  instruction, 2026-10-24, Owner may override).
- Exporters are `otlptracehttp` or `otlptracegrpc` v1.47.0. They read the
  standard `OTEL_EXPORTER_OTLP_*` variables themselves (pass-through
  semantics). corral passes no option that overrides those variables except
  where needed to apply the protocol choice. corral's own validation runs
  first so the SDK never sees a value corral considers invalid.
- The OTel SDK `BatchSpanProcessor` is not used. Sub-package
  `internal/infra/telemetry/tracing/spanqueue` implements
  `sdktrace.SpanProcessor` with a fixed-capacity channel (`OTEL_BSP_MAX_QUEUE_SIZE`),
  non-blocking `OnEnd` (drop and increment
  `corral_tracing_spans_dropped_total` when full), one export goroutine that
  exports when `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` spans are queued or
  `OTEL_BSP_SCHEDULE_DELAY` elapses, a per-call `OTEL_BSP_EXPORT_TIMEOUT`,
  `ForceFlush`, and `Shutdown` honouring the caller's context deadline.
  Only sampled spans are enqueued.
- `Tracing.Shutdown` always applies a 5-second deadline (FR-34), shorter if
  the caller's context is shorter.
- For tests only, `Start` accepts an exporter override so AC-36 can inject a
  blocking exporter. The override is ignored when tracing is disabled.
- Exporter and SDK errors reach corral only through the otel error handler /
  logger bridges (FR-28, FR-33), never as raw text.
- Bootstrap requirements for `Start` (decided by Orchestrator under Owner's
  standing instruction, 2026-10-24):
  - (a) The OTLP exporter is wrapped as
    `spanqueue.New(tracing.FilterResource(otlpExp, res), ...)`, with `res`
    the resource `NewResource` returned without error. A bootstrap-level
    AC-27 test uses real `OTEL_RESOURCE_ATTRIBUTES` with banned keys.
  - (b) The OTLP exporters read `os.Getenv` directly, not corral's
    `LookupEnv`, so `Resolve` must validate the same real process
    environment.
  - (c) Export errors go to `otel.Handle`, so `logbridge.Install` must have
    run first (ADR-0006 step 2).
  - (d) `Shutdown` passes a context with a 5-second deadline (FR-34).
  - (e) Collector rejections are wrapped in an `errclass.ProtocolError`
    type, so they classify as `protocol`/`other`.
  - (f) No processor or exporter path, including the test-only exporter
    override, may bypass `FilterResource`.
- `NewResource` tolerates `resource.ErrPartialResource`; other detector
  errors become a constant sentinel. The resource denylist checks raw and
  percent-decoded keys.
- spanqueue: injectable ticker (`WithTicker`), `ForceFlush` exports the
  snapshot queued at call time, a second `Shutdown` returns its own
  `ctx.Err()`, and `Config` clamping is defence in depth behind T10's
  validation.

## Consequences

- FR-19's per-message rule ("check enabled before calling `Tracer.Start`")
  is supported by `Enabled()`; Part B AC-22 will test call sites when they
  exist.
- Changing OTel versions requires re-checking that the exporters' env
  handling still matches FR-20.
