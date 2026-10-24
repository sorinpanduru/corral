# ADR-0005: Telemetry test infrastructure (fake OTLP collector, marker scanner, clocks)

- Status: Accepted (planner, 2026-10-24, for SPEC-001)
- Related: SPEC-001 AC-5, AC-14, AC-15, AC-24, AC-26, AC-27, AC-28, AC-29,
  AC-33, AC-36, AC-38

## Context

Part A tracing criteria are in-process tests of the tracing bootstrap that
export to "an in-process test collector". Several criteria need collector
behaviours that a real OpenTelemetry Collector cannot easily provide on
demand: counting accepted TCP connections, rejecting requests, never
responding, recording transport headers and content encoding.

## Decision

- No digital twin and no external collector container is used for SPEC-001.
  `internal/testkit/otlpcollector` is an in-process fake collector that
  serves OTLP/HTTP (protobuf, `POST /v1/traces`, gzip aware) and OTLP/gRPC
  (`TraceService/Export`, gzip aware) on ephemeral loopback ports. Modes:
  accept, reject (HTTP 4xx/5xx, gRPC non-OK status), unresponsive (hold the
  request until the test closes the collector). It records decoded span
  payloads, resource attributes, transport headers / metadata, content
  encoding, and the number of accepted TCP connections.
- An "unreachable collector" in tests is a loopback address whose port was
  reserved and then closed by the test, not a remote host.
- `internal/testkit/markerscan` implements the AC-33 harness. It captures
  in-process stdout and stderr (file-descriptor redirection), child-process
  stdout and stderr, HTTP response bodies (`/metrics`, `/readyz`) and
  collector span payloads, and scans them for markers in the encodings AC-33
  lists. Collector transport headers are excluded.
- `internal/testkit/logcheck` is the AC-5 test slog handler. Every test suite
  that emits logs wires it in through the logging factory.
- Time-dependent production code (rate limiter, span processor schedule)
  takes an injectable clock (`func() time.Time` plus, where timers are
  needed, a small clock interface). Tests use a fake clock from
  `internal/testkit/fakeclock`. `testing/synctest` may be used by test
  writers as an alternative but is not required by production code.
- No Part A test needs Docker. `make test-functional` builds the binary and
  runs `tests/functional/...` against it locally; Docker-backed `mongod`
  fixtures arrive with the first proxying spec. `make test-functional` is not
  a gate until SPEC-001 task T18 creates `tests/functional`.
- Certificates and keys used by tests (TLS collectors, `OTEL_*` certificate
  validation) are synthetic and generated at test runtime into a temporary
  directory. No `*.pem`, `*.key` or `*.p12` file is committed; these
  patterns are gitignored.

## Consequences

- If a later spec needs collector behaviour beyond these modes (for example
  OTLP partial-success responses), extend the fake first and reconsider a twin
  only if the protocol surface grows substantially.
- Tests that set `OTEL_*` variables use `t.Setenv`, so they cannot call
  `t.Parallel()`. The OTLP exporters read the process environment directly.
