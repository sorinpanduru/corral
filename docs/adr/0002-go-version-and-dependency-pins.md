# ADR-0002: Go version and dependency pins

- Status: Accepted (planner, 2026-10-24, for SPEC-001)
- Related: SPEC-001 FR-9, FR-14, FR-20, FR-23, FR-28, FR-37

## Context

SPEC-001 pins OpenTelemetry semantic conventions v1.33.0. The spec reviewer
verified that `go.opentelemetry.io/otel/semconv/v1.33.0` exists in
`go.opentelemetry.io/otel` v1.47.0. OTel Go releases its stable modules as one
version set, and its exporters pull in gRPC and the OTLP protobuf module.

## Decision

- Go: `go 1.26` in `go.mod` (assumed toolchain: Go 1.26.x). If the go.mod of
  any pinned OTel v1.47.0 module requires a newer `go` line, raise ours to
  match and record it here; do not downgrade OTel instead.
- OpenTelemetry stable set, all exactly `v1.47.0`:
  `go.opentelemetry.io/otel`, `.../otel/trace`, `.../otel/sdk`,
  `.../otel/exporters/otlp/otlptrace`,
  `.../otel/exporters/otlp/otlptrace/otlptracehttp`,
  `.../otel/exporters/otlp/otlptrace/otlptracegrpc`.
  Semantic conventions are imported from
  `go.opentelemetry.io/otel/semconv/v1.33.0` only. No other semconv version
  may be imported.
- Transitive versions chosen by OTel v1.47.0 (do not bump independently):
  `google.golang.org/grpc`, `go.opentelemetry.io/proto/otlp`,
  `github.com/go-logr/logr`, `google.golang.org/protobuf`.
  The fake OTLP collector (`internal/testkit/otlpcollector`) uses the same
  `go.opentelemetry.io/proto/otlp` collector types.
- `github.com/prometheus/client_golang`: latest stable v1.x release available
  when the dependency is first added (T7), with `collectors`, `promhttp` and
  `testutil/promlint` packages. Record the exact version in this ADR when
  added. No pre-releases.
- `golang.org/x/tools` (for `go/packages` in the constant-message check,
  `internal/testkit/constmsg`): latest stable at first use; test-only usage.
- No YAML library: the metric catalog is JSON (ADR-0004).
- No other third-party dependency may be added for SPEC-001 without a new ADR.

## Consequences

- `go mod tidy` must leave the OTel set at v1.47.0. CI or review checks
  `go list -m` output for the pinned modules.
- Upgrading OTel later requires re-verifying the semconv v1.33.0 attribute
  stability claims in FR-23.
