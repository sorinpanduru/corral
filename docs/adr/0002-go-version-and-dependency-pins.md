# ADR-0002: Go version and dependency pins

- Status: Accepted (planner, 2026-10-24, for SPEC-001)
- Related: SPEC-001 FR-9, FR-14, FR-20, FR-23, FR-28, FR-37

## Context

SPEC-001 pins OpenTelemetry semantic conventions v1.33.0. The spec reviewer
verified that `go.opentelemetry.io/otel/semconv/v1.33.0` exists in
`go.opentelemetry.io/otel` v1.47.0. OTel Go releases its stable modules as one
version set, and its exporters pull in gRPC and the OTLP protobuf module.

## Decision

- Go: `go 1.26.0` in `go.mod` (toolchain: Go 1.26.x). OTel v1.47.0 requires
  `go 1.26.0`, so the line was raised from `go 1.26` to match (decided by
  Orchestrator under Owner's standing instruction, 2026-10-24). If a pinned
  module later requires a newer `go` line, raise ours to match and record it
  here; do not downgrade OTel instead.
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
- Security exception to the rule above (decided by Orchestrator under Owner's
  standing instruction, 2026-10-24): `golang.org/x/net` is bumped to
  `v0.61.0` for GO-2026-6617, GO-2026-6612, GO-2026-6611 and GO-2026-6603.
  `golang.org/x/sys` `v0.49.0` and `golang.org/x/text` `v0.43.0` followed as
  required by that bump. This deviates from "versions chosen by OTel" for
  security reasons only. The bump is re-checked (and dropped if OTel's own
  selection reaches or exceeds it) when OTel is upgraded.
- `github.com/prometheus/client_golang` `v1.25.0` (added by T7), with
  `collectors`, `promhttp` and `testutil/promlint` packages. No
  pre-releases. It brings `github.com/prometheus/client_model` `v0.6.3` and
  the transitive `github.com/prometheus/common` `v0.72.0`,
  `github.com/prometheus/procfs` `v0.22.0`, `github.com/beorn7/perks`,
  `github.com/munnerz/goautoneg`, and `github.com/kylelemons/godebug`
  (test-only), at the versions client_golang v1.25.0 selects (recorded by
  Orchestrator, 2026-10-24).
- `golang.org/x/tools` `v0.52.0` (added by T4, for `go/packages` in the
  constant-message check, `internal/testkit/constmsg`); test-only usage. No
  non-test package outside `internal/testkit` may import it (enforced by
  T21).
- No YAML library: the metric catalog is JSON (ADR-0004).
- No other third-party dependency may be added for SPEC-001 without a new ADR.
- Test writers may add `go.mod`/`go.sum` requirements when a self-test needs
  them (decided by Orchestrator under Owner's standing instruction,
  2026-10-24), despite ADR-0001 giving `go.mod` to implementers. Any module
  not already listed here is recorded in this ADR by the planner afterwards.

- SPEC-002 additions (planner, 2026-10-24):
  - production (approved by the Owner, 2026-10-24, P2), as pinned in
    `go.mod`:

    | Module | Version | Requirement |
    |---|---|---|
    | `github.com/xdg-go/scram` | v1.2.0 | direct; used behind the ADR-0019 adapter |
    | `github.com/xdg-go/stringprep` | v1.0.4 | indirect, via scram |
    | `github.com/xdg-go/pbkdf2` | v1.0.0 | indirect, via scram |
    | `golang.org/x/sys` | v0.49.0 | direct (`TCP_USER_TIMEOUT`); unchanged version |

    `golang.org/x/text` stays at the already-pinned v0.43.0.
  - test-only: `go.mongodb.org/mongo-driver/v2` v2.9.2. It is allowed only
    in `_test.go` files, `internal/testkit/...`, `tests/...` and
    `twins/...` (ADR-0022). The production import lint enforces this.
  - Functional fixture images and packages (ADR-0022; P1 access granted by
    the Owner). The source of truth is the code named below; update both
    together, with a full functional run.

    | Item | Pin | Defined in |
    |---|---|---|
    | `mongo:7.0` | `docker.io/library/mongo@sha256:c53852c00c23ca9f23fc39690350ffa9a80e3642fe8f96879726d8da0abe80a7` | `internal/testkit/mongofx/images.go` |
    | `mongo:8.0` | `docker.io/library/mongo@sha256:4a4ab5d4834207389e0d7270fac988e21ffedccecba77991a552f755df229907` | `internal/testkit/mongofx/images.go` |
    | PHP 8.3 FPM base (Debian bookworm) | `docker.io/library/php@sha256:6b4641a875dd3e0f7551b2751e1f5909084856d18166af7fbb522ac230feb702` | `internal/testkit/phpfpm/phpfpm.go` (`BaseImage`) |
    | PECL `mongodb` (`ext-mongodb`) | 2.5.5 | `internal/testkit/phpfpm/phpfpm.go` (`PeclMongoDBVersion`) |

    The mongo digests are the multi-platform index digests as of
    2026-10-24.

## Consequences

- `go mod tidy` must leave the OTel set at v1.47.0. CI or review checks
  `go list -m` output for the pinned modules.
- Upgrading OTel later requires re-verifying the semconv v1.33.0 attribute
  stability claims in FR-23.
