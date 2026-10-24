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
  resource but no span processor or exporter; `Enabled()` is true.
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

## Consequences

- FR-19's per-message rule ("check enabled before calling `Tracer.Start`")
  is supported by `Enabled()`; Part B AC-22 will test call sites when they
  exist.
- Changing OTel versions requires re-checking that the exporters' env
  handling still matches FR-20.
