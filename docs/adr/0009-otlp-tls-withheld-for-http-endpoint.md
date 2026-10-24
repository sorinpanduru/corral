# ADR-0009: Withhold OTLP TLS material for http:// endpoints

Status: accepted (SPEC-001 Amendment 2, FR-41)

## Context

The OTel Go exporters read OTEL_EXPORTER_OTLP_* from the process environment
themselves. With an http:// endpoint and TLS material, otlptracehttp fails
Start (insecure endpoint with TLS config) while otlptracegrpc silently prefers
TLS credentials over Insecure. The Owner decided both protocols are plaintext
in that case, with one WARN and no config error.

## Decision

`tracing.TLSIgnoredSettings` is a pure function of EnvConfig and the environment
lookup: it returns the sorted names of the set TLS variables when the effective
traces endpoint (_TRACES_ over general, scheme case-insensitive) is http://.
Because the SDK reads the process environment directly and cannot be given a
filtered one, `newOTLP` passes explicit options that override the env-derived
TLS (http: nil TLS config plus insecure; grpc: insecure transport credentials
plus insecure), including against OTEL_EXPORTER_OTLP_INSECURE=false. The
runtime logs the WARN (component=config, constant message, `setting` names
only) before the lifecycle records. FR-27 validation in Resolve is unchanged
and runs first.

## Consequences

Values (paths) and file contents never reach logs. A future SDK upgrade that
changes option precedence is caught by the delivery tests.
