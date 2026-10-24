# Plan: SPEC-001 Observability conventions (Part A)

- Spec: `specs/SPEC-001-observability-conventions.md` (approved 2026-10-24, accepted on Part A ACs)
- Planner date: 2026-10-24
- Scope: everything needed for the Part A ACs (AC-1, 3, 4, 5, 6, 7, 8, 11, 12, 13, 14, 15, 16, 21, 24, 25, 26, 27, 28, 29, 30, 31, 33, 35, 36, 37, 38), the repository bootstrap, and the FR-36 operator documentation (verified by review).
- Out of scope: Part B ACs (AC-2, 9, 10, 17, 18, 19, 22, 23, 34, 39). The FR-40 collection extractor (only exercised by Part B AC-39) is not built here; the first spec that records collection names builds it. FR-22/FR-32 span shapes are not built (no proxy path yet). `Enabled()` from T15 is the hook later specs use for FR-19/AC-22.
- Go version: `go 1.26.0` in go.mod, because OTel v1.47.0 requires it (ADR-0002; decided by Orchestrator under Owner's standing instruction, 2026-10-24).
- Docker: no Part A task needs Docker. All collectors are in-process fakes (ADR-0005). **No twin is needed.**

## ADRs written for this plan

- `docs/adr/0001-repository-layout-and-layering.md`: layout, layering, file ownership, build tags.
- `docs/adr/0002-go-version-and-dependency-pins.md`: `go 1.26.0`, OTel v1.47.0 set, semconv v1.33.0, x/net security bump, other deps, test-writer go.mod rule.
- `docs/adr/0003-interim-settings-loading.md`: (Accepted, D1) flags named like logical settings.
- `docs/adr/0004-metric-catalog.md`: embedded JSON catalog, lint, catalog-backed factory; error labels `error_class` / `error_detail` (Accepted, D4).
- `docs/adr/0005-telemetry-test-infrastructure.md`: in-process fake OTLP collector, marker scanner, fake clock, no Docker, runtime-generated certificates.
- `docs/adr/0006-startup-sequence-and-readiness.md`: startup order, startup-complete, readiness gating and `/readyz` responses (Accepted, D6).
- `docs/adr/0007-tracing-bootstrap-and-span-processor.md`: tracing package split, disabled/none behaviour (log correlation on with `none`, D9), custom bounded-queue processor.
- `docs/adr/0008-early-slog-neutralisation.md`: `earlyslog` neutralises `slog.Default()` and std `log` during package init; `logbridge.Install` hands off and replays counts (T22).
- `docs/adr/0009-otlp-tls-withheld-for-http-endpoint.md`: for an `http://` traces endpoint the exporters get explicit plaintext options; one FR-41 warn record (T23).

## Dependency pins (ADR-0002)

| Module | Version |
|---|---|
| `go.opentelemetry.io/otel`, `/trace`, `/sdk` | v1.47.0 |
| `go.opentelemetry.io/otel/exporters/otlp/otlptrace`, `/otlptracehttp`, `/otlptracegrpc` | v1.47.0 |
| `go.opentelemetry.io/otel/semconv/v1.33.0` | package inside otel v1.47.0; only semconv version allowed |
| `google.golang.org/grpc`, `go.opentelemetry.io/proto/otlp`, `github.com/go-logr/logr`, `google.golang.org/protobuf` | as selected by OTel v1.47.0 (no independent bump) |
| `golang.org/x/net` | v0.61.0 (security bump for GO-2026-6617/6612/6611/6603; deviates from OTel's selection) |
| `golang.org/x/sys`, `golang.org/x/text` | v0.49.0, v0.43.0 (followed the x/net bump) |
| `github.com/prometheus/client_golang` | v1.25.0 (T7) |
| `github.com/prometheus/client_model`, `/common`, `/procfs` | v0.6.3, v0.72.0, v0.22.0 (via client_golang) |
| `github.com/beorn7/perks`, `github.com/munnerz/goautoneg`, `github.com/kylelemons/godebug` (test-only) | as selected by client_golang v1.25.0 |
| `golang.org/x/tools` | v0.52.0 (T4); `internal/testkit` and test-only use (T21 enforces) |

Test writers may add `go.mod`/`go.sum` requirements when a self-test needs them; the planner records any new module in ADR-0002 (decided by Orchestrator under Owner's standing instruction, 2026-10-24).

## Rules for every task

- Paths: implementers own non-test `.go` files and data files listed under "Implementer owns". Test writers own `*_test.go`, `testdata/` and `tests/**` listed under "Test writer owns" (ADR-0001). Neither side edits the other's files.
- From T4 onward, every test suite that emits or captures logs wires in `internal/testkit/logcheck` through the logging factory (AC-5). T4's test writer retrofits T3's suites.
- Synthetic data only. Markers are random synthetic strings generated per test.
- Unit tests prefer external test packages (`package x_test`) to avoid cycles with `internal/testkit`.
- Tests that set `OTEL_*` use `t.Setenv` and do not run in parallel.
- No production package imports `internal/testkit/...` or `golang.org/x/tools/...` (lint test from T21).
- Residual risk (FR-24): `logcheck` and `constmsg` check keys, components and message constness, not the values of allowed keys. FR-24 therefore relies on the source sanitisers (T5 `errclass`, T6 `sanitize`) and on the marker scans (T14, T19). Every new call site that emits `error`, `stack`, or db/address keys must take the value from an `errclass`/`sanitize` helper, never from raw input or `err.Error()`. Reviewers check this.
- Control characters: the sanitisers pass control characters through by design (T5/T6). Such values may only be rendered by slog's escaping handlers (JSON/text from `logging.NewHandler`); never concatenate them into strings, messages, headers or response bodies (applies especially to T8 bridges and T17 responses).
- `make test` runs with `-race`.
- Test fixtures must not commit `*.pem`, `*.key` or `*.p12` files (they are gitignored in `/.gitignore`). Tests that need certificates or keys generate them at runtime (for example `crypto/x509` + `crypto/ecdsa` into `t.TempDir()`); only synthetic, non-secret material is generated.
- `make test-functional` is not a gate until T18 creates `tests/functional`. Before then, reviewers and the Orchestrator gate on `make build` and `make test` only.

## Package map (introduced by this plan)

| Package | Task |
|---|---|
| `cmd/corral` | T1 (stub), T18 |
| `internal/domain` (doc only), `internal/domain/health` | T1, T2 |
| `internal/app/runtime` | T18 |
| `internal/infra/config` | T9 |
| `internal/infra/telemetry/attr` | T3 |
| `internal/infra/telemetry/logging` | T3 |
| `internal/infra/telemetry/errclass` | T5 |
| `internal/infra/telemetry/sanitize` | T6 |
| `internal/infra/telemetry/metrics` (+ `catalog.json`) | T7 |
| `internal/infra/telemetry/ratelimit`, `internal/infra/telemetry/logbridge` | T8 |
| `internal/infra/telemetry/earlyslog` | T22 |
| `internal/infra/telemetry/tracing` | T10, T11, T15, T16 |
| `internal/infra/telemetry/tracing/spanqueue` | T12 |
| `internal/infra/adminhttp` | T17 |
| `internal/testkit/importcheck`, `constmsg`, `logcheck`, `fakeclock`, `otlpcollector`, `markerscan` | T2 (+T21), T4, T8, T13, T14 |

---

## Tasks

### T1: Repository bootstrap

- Goal: buildable empty module and factory tooling.
  - `go.mod`: `module github.com/sorinpanduru/corral`, `go 1.26.0` (raised from `go 1.26` per ADR-0002). No requires yet (later tasks add pinned deps).
  - `Makefile` targets: `build` (builds `./cmd/corral` into `bin/corral`), `test` (`go vet ./...` then `go test -race -count=1 ./...`; the `functional` build tag is not set so functional tests are excluded), `test-functional` (`go test -race -count=1 -tags functional ./tests/functional/...`; depends on `build`; does not require Docker for SPEC-001, see ADR-0005; not a gate until T18 creates `tests/functional`). Optional `tidy` target.
  - `LICENSE`: full Apache License 2.0 text, unmodified.
  - Skeleton: `cmd/corral/main.go` (stub `main` that exits 0), `internal/domain/doc.go` (package doc stating the FR-1 rule), `.gitignore` (`bin/`, coverage files, `*.pem`, `*.key`, `*.p12`; already present, keep these entries).
  - `make build` and `make test` succeed on the empty tree.
- FRs/ACs: none directly (prerequisite for all).
- Implementer owns: `go.mod`, `go.sum`, `Makefile`, `LICENSE`, `.gitignore`, `cmd/corral/main.go`, `internal/domain/doc.go`.
- Test writer owns: nothing.
- Interfaces defined: Make targets `build`, `test`, `test-functional`; build tag `functional`.
- Docker: no.
- Depends on: none.

### T2: Domain readiness port and domain import-boundary check

- Goal: the first domain package and the FR-1 import check.
  - `internal/domain/health`: readiness port and fixed reason enum (domain types only, stdlib only).
  - `internal/testkit/importcheck`: runs `go list -deps -json <pattern>` in a given directory, selects packages whose import path is under a given domain prefix, and checks each one's `Imports` (non-test files only; `TestImports`/`XTestImports` ignored) against: stdlib (packages with `Standard: true`) except `log/slog`, other packages under the prefix, and an explicit allowlist (the FR-1 domain import allowlist, a package-level variable, currently empty). Returns all violations.
- FRs/ACs: FR-1, FR-18 (port shape); AC-1.
- Implementer owns: `internal/domain/health/*.go` (non-test), `internal/testkit/importcheck/*.go` (non-test).
- Test writer owns: `tests/lint/domain_imports_test.go` (runs the check on `./internal/domain/...` from the repo root), `tests/lint/testdata/**` (a violating fixture laid out so the check sees it as a domain package, e.g. a nested module or explicit pattern; must import `log/slog` and a non-stdlib package), self-test of importcheck.
- Interfaces defined:
  - `health.Reason` (string type); `health.ReasonStarting = "starting"`, `health.ReasonShuttingDown = "shutting_down"`, `health.ReasonUnknown = "unknown"` (D6; the spec writer is adding `unknown` and `shutting_down` to the spec's reason set); `health.Reasons() []Reason` (the full defined set).
  - `health.Status{Ready bool; Reason Reason}`.
  - `health.Readiness` interface: `Readiness(ctx context.Context) Status`.
  - `importcheck.Check(dir, pattern, domainPrefix string, allow []string) ([]Violation, error)`; `importcheck.Scan(dir, pattern, domainPrefix string) ([]string, error)` (returns the matched package import paths under the prefix); `importcheck.DomainAllowlist` (empty).
  - `Check` and `Scan` reject a `pattern` with a leading `-` (no flag injection into `go list`) and return an error when the pattern matches no package under the prefix (an empty match never passes silently). They run `go list` with a hermetic environment (T21 adds `GOENV=off` and `GOPACKAGESDRIVER=off`; T21 also adds `importcheck.Forbidden`). Decided by Orchestrator under Owner's standing instruction, 2026-10-24.
- Docker: no.
- Depends on: T1.

### T3: Logging core (components, key allowlist, handler, factory, trace correlation)

- Goal: the only way to obtain a logger, with FR-5/6/7/8/39 behaviour.
  - `internal/infra/telemetry/attr`: string constants for every allowed log key and every FR-23 span attribute key, and for `error`, `corral.error.detail`, `setting`, `stack`, `trace_id`, `span_id`, `suppressed_count`. Used by logging, errclass, tracing and logcheck so the allowlist exists once.
  - `logging`: component enum and validation; key allowlist predicate (fixed keys, FR-23 keys, and the numeric `_seconds`/`_bytes`/`_count` snake_case rule); `ParseFormat`/`ParseLevel` (both case-insensitive; `ParseLevel` rejects offsets like `debug-4`, `info+2`); `NewHandler` writing synchronously to a given writer (FR-38) in JSON or slog text; `WithTraceCorrelation` wrapper adding `trace_id`/`span_id` (lowercase hex 32/16) when the record's context has a valid span context, sampled or not (installed whenever tracing is enabled, including `OTEL_TRACES_EXPORTER=none`, D9); `Factory` that returns loggers with a bound `component` and rejects unknown components; `BootstrapLogger` (JSON to a writer, `component=config`) for errors before configuration (FR-27).
  - No `slog` groups are used anywhere in this package's output.
- FRs/ACs: FR-4, FR-5, FR-6, FR-7, FR-8 (handler part), FR-38, FR-39; AC-3 (unit part), AC-4, AC-5 (factory part), AC-6 (unit part of the correlation wrapper).
- Implementer owns: `internal/infra/telemetry/attr/*.go`, `internal/infra/telemetry/logging/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/logging/*_test.go`, `internal/infra/telemetry/attr/*_test.go`.
- Interfaces defined:
  - `logging.Component` with constants `Config, Lifecycle, AdminHTTP, Tracing, OTel, GRPC, ThirdParty` (`ThirdParty = "thirdparty"`, D5; the spec writer is amending FR-7/FR-28); `logging.Components() []Component`; `logging.ValidComponent(Component) bool`.
  - `logging.AllowedKey(key string) bool`; `logging.NumericKey(key string) bool`.
  - `logging.Format` (`FormatJSON`, `FormatText`); `logging.ParseFormat(string) (Format, error)`; `logging.ParseLevel(string) (slog.Level, error)`. Both parsers are case-insensitive (decided by Orchestrator under Owner's standing instruction, 2026-10-24).
  - Sentinel errors (match with `errors.Is`): `logging.ErrInvalidFormat`, `logging.ErrInvalidLevel`, `logging.ErrUnknownComponent` (returned by `(*Factory).Logger` for an unknown component).
  - `logging.NewHandler(w io.Writer, f Format, level slog.Leveler) slog.Handler`.
  - `logging.WithTraceCorrelation(h slog.Handler) slog.Handler`.
  - `logging.NewFactory(h slog.Handler) *Factory`; `(*Factory).Logger(c Component) (*slog.Logger, error)`; `(*Factory).Handler() slog.Handler`.
  - `logging.BootstrapLogger(w io.Writer) *slog.Logger`.
- Docker: no.
- Depends on: T1. Adds dependency `go.opentelemetry.io/otel/trace` v1.47.0.

### T4: Log-check test handler and constant-message static check

- Goal: the AC-5 enforcement tools every later suite uses.
  - `internal/testkit/logcheck`: a `slog.Handler` bound to a `testing.TB` that fails the test on a record with no `component`, a component outside `logging.Components()`, a key that `logging.AllowedKey` rejects, any group (including `WithGroup` and `slog.Group` values), a numeric-suffix key with a non-numeric value, a caller-set `msg`, `level` or `time` attribute key, or a duplicate `component` (for example `With("component", ...)` on an already bound logger). It also records records (level, msg, attrs) for assertions and can tee to a writer (to also produce real JSON/text output).
  - `internal/testkit/constmsg`: loads packages with `golang.org/x/tools/go/packages`, finds every call to the `log/slog` package-level functions and `*slog.Logger` methods that take a message (`Debug/Info/Warn/Error`, their `Context` variants, `Log`, `LogAttrs`) and reports each call whose message argument is not a compile-time constant (`types.TypeAndValue.Value == nil`).
    - Loading: `-tags=functional` (so functional tests are covered), environment `GOPACKAGESDRIVER=off` and `CGO_ENABLED=0`.
    - Also flagged: those slog functions/methods used as values (not called directly), method expressions (for example `(*slog.Logger).Info`), `slog.NewRecord` with a non-constant message, and multi-value spreads into a message-taking call (`l.Info(f())`).
    - Accepted limitations (not flagged): calls through user-defined interfaces, and `slog.NewLogLogger(...).Printf` and similar `*log.Logger` output. Revisit `NewLogLogger` at T8 (the `thirdparty` bridge and admin HTTP `ErrorLog`), where the bridges replace the message with a constant anyway.
    - Decided by Orchestrator under Owner's standing instruction, 2026-10-24.
    - Escape hatch (decided by Orchestrator under Owner's standing instruction, 2026-10-24): a `//constmsg:allow <reason>` comment (exact keyword, non-empty reason) suppresses findings on its own line or the next line only. It exists for test code that simulates third-party `slog.Default()` callers (T8). Every use is reviewed; production use needs a reviewer-accepted reason.
- FRs/ACs: FR-7, FR-39; AC-5 (test-handler and static-check parts).
- Implementer owns: `internal/testkit/logcheck/*.go`, `internal/testkit/constmsg/*.go` (non-test).
- Test writer owns: self-tests `internal/testkit/logcheck/*_test.go`, `internal/testkit/constmsg/*_test.go` with `testdata/` fixtures (one constant and one non-constant message); `tests/lint/constmsg_test.go` running the check over `./...` (excluding testdata); retrofit T3 suites to use logcheck.
- Interfaces defined: `logcheck.New(tb testing.TB, opts ...Option) *Handler`; `(*Handler).Records() []Record`; option `Tee(io.Writer, logging.Format)`. `constmsg.Check(dir string, patterns ...string) ([]Finding, error)`.
- Docker: no.
- Depends on: T3. Adds `golang.org/x/tools` v0.52.0.

### T5: Error classification and sanitisation

- Goal: FR-29 class and detail enums, the classifier, and the only helpers that put errors into logs and spans.
  - Class enum: `timeout, network, protocol, server, config, internal`. Detail enum: the 14 FR-29 values.
  - Classifier walks the error chain (`errors.Is`/`errors.As`): `context.DeadlineExceeded`, `context.Canceled`, `os.ErrDeadlineExceeded`/`net.Error.Timeout()`, `syscall.Errno` values (ECONNREFUSED, ECONNRESET, ECONNABORTED, ETIMEDOUT, EPIPE, EHOSTUNREACH, ENETUNREACH, EADDRINUSE), `io.EOF`/`io.ErrUnexpectedEOF`, `*net.DNSError`, TLS handshake errors (`tls.RecordHeaderError`, `*tls.CertificateVerificationError`, `x509` errors, alert errors), server-reply errors, config errors, recovered panics. Class mapping per D3.
  - Duck-typed contracts so no concrete domain/config types are needed: a server-reply error exposes `ServerCode() int32` and `ServerCodeName() string`; a config error exposes `ConfigSetting() string`; a protocol error exposes `ProtocolError()` (marker method, class `protocol`).
  - Review decisions (decided by Orchestrator under Owner's standing instruction, 2026-10-24):
    - Precedence when a chain matches several kinds: panic > config > server > protocol > errno (then the remaining D3 rules).
    - A raw `EADDRINUSE` (not wrapped in a config error) is `network`/`EADDRINUSE`. The admin bind failure is `config`/`EADDRINUSE` only because T17 wraps it in `config.Error` and config outranks errno.
    - `LogAttrs(nil)` returns nil; `RecordSpan(span, nil)` is a no-op.
    - `ServerCodeName()` is emitted only if it matches `^[A-Za-z0-9_]{1,64}$`; otherwise `corral.db.response.code_name` is omitted (the numeric code is still emitted).
    - Stacks kept by `FromPanic` are capped at 32 KiB, cut at a line boundary.
    - The chain walk (`Unwrap() error` and `Unwrap() []error`) is bounded (fixed maximum depth/node count) so cyclic or very deep chains terminate.
    - T15 must wrap OTLP collector rejections in a type implementing `ProtocolError()` so they classify as `protocol`/`other` (D3).
  - `FromPanic` converts a recovered value into an `internal` error that keeps an optional stack and discards the value (its `Error()` returns a constant).
  - Log helper returns `error=<class>`, `corral.error.detail=<detail>`, and for server errors `db.response.status_code` (string) and `corral.db.response.code_name`. Never `err.Error()`.
  - Span helper sets status Error with empty description, `error.type`, `corral.error.detail`, and server attributes. Never calls `RecordError`, never adds events.
- FRs/ACs: FR-26, FR-29; AC-35.
- Implementer owns: `internal/infra/telemetry/errclass/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/errclass/*_test.go` (real `net.Dial` to a closed loopback port for ECONNREFUSED, reset/broken-pipe via loopback sockets, `tls` handshake against a plaintext listener, DNS failure using an `.invalid` name with a stub resolver, EADDRINUSE via double listen; reset/EPIPE loopback tests are race-tolerant (flakiness fixed after review); fake server-reply error type carrying a marker in its message; exporter-style `*url.Error` with a userinfo marker; panic with marker; spans recorded with `sdk/trace/tracetest`).
- Interfaces defined:
  - `errclass.Class`, `errclass.Detail`, `errclass.Classes() []Class`, `errclass.Details() []Detail`.
  - `errclass.Classify(err error) (Class, Detail)`.
  - `errclass.ServerError`, `errclass.ConfigError` and `errclass.ProtocolError{ ProtocolError() }` interfaces as above.
  - `errclass.FromPanic(recovered any, stack []byte) error`; `errclass.Stack(err error) ([]byte, bool)`.
  - `errclass.LogAttrs(err error) []slog.Attr`.
  - `errclass.RecordSpan(span trace.Span, err error)`.
- Docker: no.
- Depends on: T3 (attr keys). Adds `go.opentelemetry.io/otel` (codes, attribute) v1.47.0, `go.opentelemetry.io/otel/sdk` v1.47.0 (tests).
- Note: class table fixed by D3 (no longer gated).

### T6: Command-name and client-metadata sanitisers

- Goal: FR-12 allowlist/alias handling and FR-25 UTF-8 replacement and truncation.
  - Central command-name allowlist (artifact owned by SPEC-001) and alias table (`isMaster`, `ismaster` -> `hello`), case-sensitive matching.
  - `MetricLabel(name)` returns the allowlisted name (after alias) or `other`.
  - `CommandForTelemetry(name)` returns the name as received (no alias), invalid UTF-8 replaced with U+FFFD, truncated to <= 64 bytes on a rune boundary.
  - `AppName` (<= 128 bytes) and `DriverName`/`DriverVersion` (<= 64 bytes each): replace invalid UTF-8, then truncate on a rune boundary.
- FRs/ACs: FR-12, FR-25; AC-37.
- Implementer owns: `internal/infra/telemetry/sanitize/*.go` (non-test). The allowlist lives in one file, `internal/infra/telemetry/sanitize/commands.go`.
- Test writer owns: `internal/infra/telemetry/sanitize/*_test.go`.
- Interfaces defined: `sanitize.MetricLabel(string) string`; `sanitize.CommandForTelemetry(string) string`; `sanitize.AppName(string) string`; `sanitize.DriverName(string) string`; `sanitize.DriverVersion(string) string`; `sanitize.CommandAllowlist() []string`; `sanitize.OtherCommand = "other"`.
- Docker: no.
- Depends on: T1.
- Note: the initial allowlist is exactly `find` and `hello` (D2). Later feature specs extend it by amending SPEC-001.

### T7: Metric catalog, lint and catalog-backed metrics factory

- Goal: FR-31 catalog with the FR-37 counter; lint; constructors that refuse unknown names; registry with Go and process collectors (ADR-0004).
  - `catalog.json` with exactly one entry: `corral_tracing_spans_dropped_total`, counter, no labels, help "Spans dropped because the trace export queue was full."
  - `Lint` checks: `corral_` prefix; snake_case name; non-base unit suffix ban (`_ms`, `_milliseconds`, `_us`, `_microseconds`, `_ns`, `_nanoseconds`, `_minutes`, `_hours`, `_kb`, `_mb`, `_gb`, `_kib`, `_mib`, `_gib`, `_kilobytes`, `_megabytes`, `_percent` and similar); `_total` iff counter; non-empty help; label names only from `metrics.AllowedLabelNames()` (currently exactly `command`, `error_class`, `error_detail`; feature specs extend it by amending SPEC-001); label values match `^[a-z0-9_]+$`, except `error_detail`, whose values must be in `errclass.Details()`; forbidden label names kept as a second line of defence (address, ip, host, port, `*_id`/`id`, lsid, session, cursor, request, connection, app_name/appname, driver*, user*/username, database/db/namespace, collection, code/code_name/codename, trace*/span*); every label has a non-empty value set; error-class label values within the FR-29 class enum; error-detail label values within the FR-29 detail enum; series cap (cartesian product <= 100, `le`/`quantile` excluded and not declarable as labels).
  - Factory constructors by name; refuse unknown names and type mismatches; vectors refuse undeclared label values. Exemplars are never attached (FR-11).
  - Review decisions (decided by Orchestrator under Owner's standing instruction, 2026-10-24):
    - The factory caches the collectors it created (a second request for the same name returns the cached handle) and refuses to proceed when the registerer already holds a foreign collector with the same name (`AlreadyRegisteredError` from a collector it did not create is an error, never adopted).
    - Returned handles are narrow corral wrappers exposing only the needed methods (for example `Inc`/`Add`, `Set`, `Observe`, `WithLabelValues` on vectors); they do not expose `prometheus.ExemplarAdder`/`ExemplarObserver` or the underlying collector.
    - The catalog is deep-copied: `LoadCatalog`/`ParseCatalog` results and anything the factory keeps or hands out cannot be mutated by callers to alter the factory's view.
  - `NewRegistry()` returns a private `prometheus.Registry` with `collectors.NewGoCollector()` and `collectors.NewProcessCollector(...)` registered.
- FRs/ACs: FR-9 (registry part), FR-10, FR-11, FR-13 (lint), FR-14, FR-31, FR-37 (catalog entry); AC-8 (all unit parts).
- Implementer owns: `internal/infra/telemetry/metrics/*.go` (non-test), `internal/infra/telemetry/metrics/catalog.json`.
- Test writer owns: `internal/infra/telemetry/metrics/*_test.go`, `internal/infra/telemetry/metrics/testdata/**` (bad catalogs, one per rule); test that the factory refuses an unlisted name; test that every gathered `corral_` family from a registry populated through the factory is in the catalog; `testutil/promlint` over `corral_` families only.
- Interfaces defined:
  - `metrics.Catalog`, `metrics.MetricDef`, `metrics.LabelDef`; `metrics.LoadCatalog() (*Catalog, error)` (embedded); `metrics.ParseCatalog([]byte) (*Catalog, error)`; `metrics.Lint(*Catalog) []Problem`.
  - `metrics.NewRegistry() *prometheus.Registry`.
  - `metrics.NewFactory(reg prometheus.Registerer, c *Catalog) (*Factory, error)`; `(*Factory).Counter(name) (metrics.Counter, error)`, `CounterVec`, `Gauge`, `GaugeVec`, `Histogram`, `HistogramVec`, each returning a narrow corral wrapper type (`metrics.Counter`, `metrics.CounterVec`, ...; no exemplar interfaces).
  - `metrics.AllowedLabelNames() []string` (returns a copy; `command`, `error_class`, `error_detail`).
  - `metrics.RegisterAll(f *Factory) error` registers every catalog metric so it appears at `/metrics` with zero value (AC-7 needs the counter visible).
  - `metrics.NameTracingSpansDropped = "corral_tracing_spans_dropped_total"`.
- Docker: no.
- Depends on: T5 (enums). Adds `github.com/prometheus/client_golang` v1.25.0 (ADR-0002).
- Note: the error label names are `error_class` and `error_detail` (D4, ADR-0004). The lint enum checks apply to labels with exactly these names.

### T8: Rate limiter and third-party log bridges

- Goal: FR-28 routing of otel, grpclog, admin HTTP server and default-logger output to slog with fixed messages, level mapping and FR-33 limiting.
  - `ratelimit`: window limiter (30 s) with injectable clock; first event in a window is emitted with `suppressed_count` equal to events dropped since the last emitted one (0 for the first). Level filtering happens before the limiter so records below the configured level do not consume the slot (D7).
  - Limiter scope (D7, spec Amendment 1 FR-28/AC-26): one limiter per source and level, each allowing 1 record per 30 s with `suppressed_count`. Sources: otel error handler, otel logger, grpclog, admin HTTP error log, std `log` bridge, `slog.Default()` bridge. No limiter is shared between sources or levels; in particular the std `log` bridge and the `slog.Default()` bridge have separate limiters.
  - `logbridge`:
    - otel: `otel.SetErrorHandler` -> warn, fixed message, `component=otel`, `errclass.LogAttrs(err)`, own limiter; `otel.SetLogger` with a `logr.LogSink` -> `Info` at any verbosity at debug, `Error` at warn (D7), fixed message per level, no key/values forwarded, one limiter per level.
    - grpclog: `grpclog.LoggerV2` with Info->debug, Warning->warn, Error->error, Fatal->error then flush and `os.Exit(1)` (fatal not rate-limited); `V(l)` reports according to the configured level; per-level limiter; fixed message per level; `component=grpc`.
    - admin HTTP: `*log.Logger` built with `slog.NewLogLogger` over a handler that replaces the message with a fixed constant, sets `component=admin_http`, level warn, and is rate-limited.
    - third-party bridges (D5, spec Amendment 1): two separate bridges, both emitting `component=thirdparty` with a fixed exported message constant (one constant per bridge is acceptable); the original message text and attributes are never forwarded.
      - `slog.Default()` bridge: `slog.SetDefault` installs a bridge handler over the configured handler. Records keep their caller's level.
      - std `log` bridge: records from Go's `log` package arrive at info. Because `slog.SetDefault` redirects `log` output into the default handler (indistinguishable from `slog.Default()` calls), `Install` sets the std `log` output to its own bridge writer after calling `slog.SetDefault`, so the two sources stay distinct.
      - Both follow the configured level (level filtering, then the bridge's per-level limiter). They are not subject to the warn-and-above rule that applies to `grpc`/`otel`/`admin_http`.
      - corral's own code never logs through `slog.Default()` or `log`; it always uses the factory.
  - All fixed messages are exported constants.
  - Control characters (Rules for every task): bridged values (error class/detail attrs) are only rendered through slog's escaping handlers; never concatenated.
  - Test code simulating third-party `slog.Default()` callers with non-constant messages marks those calls with `//constmsg:allow <reason>` (T4 escape hatch, reviewed per use).
  - Revisit the T4 `constmsg` limitation for `slog.NewLogLogger(...).Printf` here: the admin HTTP `ErrorLog` and std `log` bridge must replace the message with a constant regardless of what is printed.
- FRs/ACs: FR-28, FR-33; AC-26, AC-29.
- Implementer owns: `internal/infra/telemetry/ratelimit/*.go`, `internal/infra/telemetry/logbridge/*.go`, `internal/testkit/fakeclock/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/ratelimit/*_test.go`, `internal/infra/telemetry/logbridge/*_test.go` (fresh limiter per case, fake clock, marker-seeded third-party text, stderr captured via fd redirection to prove nothing is written there; the fatal path tested in a subprocess; std `log` and `slog.Default()` bridges limited independently, so a burst on one does not suppress the other; `thirdparty` records at debug/info emitted when the configured level allows them).
- Interfaces defined:
  - `ratelimit.New(window time.Duration, now func() time.Time) *Limiter`; `(*Limiter).Allow() (emit bool, suppressed int)`.
  - `logbridge.Install(f *logging.Factory, level slog.Leveler, now func() time.Time)` (installs otel handler, otel logger, grpclog, `slog.Default()` bridge, then std `log` bridge, in that order); individual constructors `NewOTelErrorHandler`, `NewOTelLogSink`, `NewGRPCLogger`, `NewHTTPErrorLog(f, now) *log.Logger`, `NewSlogDefaultHandler(f, level, now) slog.Handler`, `NewStdLogWriter(f, level, now) io.Writer`.
  - Startup ordering requirement (decided by Orchestrator under Owner's standing instruction, 2026-10-24; ADR-0006 steps 0–3): `Install` is called once with the configured factory and level, right after settings parsing (stdlib only) and factory construction, and before any grpc, otel, tracing or admin-listener code runs (grpc requires `grpclog.SetLoggerV2` before any other grpc call; otel/grpc defaults write to stderr until `Install` runs). No bootstrap-to-configured handler switching or level swap is needed.
  - `fakeclock.New(start time.Time) *Clock`; `(*Clock).Now()`, `Advance(d)`.
- Docker: no.
- Depends on: T3, T5. Adds `google.golang.org/grpc` (grpclog) and `github.com/go-logr/logr` at the versions OTel v1.47.0 selects.
- Note: no longer gated (D5, D7).

### T9: Settings parsing, validation and interim loader

- Goal: logical settings, defaults, FR-27 error contract.
  - Parse a map of logical setting name -> raw string into typed `Settings` with defaults (`json`, `info`, `127.0.0.1:9187`, `false`, `1.0`).
  - Validation: `log.format`, `log.level` via `logging.Parse*`; `admin.listen` must be `host:port` with non-empty host and a valid port 0..65535 (`:9187` rejected); the host must be an IP literal or a valid DNS hostname whose last label is not numeric or hex-like (rejects `0`, `0x0`, `127.1`, whitespace, underscores; ADR-0003 amendment); `tracing.enabled` bool (`strconv.ParseBool` semantics or a stricter set); `tracing.sample_ratio` finite float in [0.0, 1.0]; unknown setting names in the `Parse` map rejected with `setting=unknown` (`config.SettingUnknown`), the name never echoed.
  - `config.Error` carries the setting name and wrapped cause; its `Error()` text contains the setting name and a constant reason only (never the value); implements `ConfigSetting()` for errclass (class `config`).
  - `LogError(logger, err)` writes the FR-27 record: `setting`, `errclass.LogAttrs`, constant message. It adds no `component` itself; callers must pass a logger from `Factory.Logger(logging.Config)` or `logging.BootstrapLogger` so the record carries `component=config` (decided by Orchestrator under Owner's standing instruction, 2026-10-24).
  - Interim loader (ADR-0003, D1 and its amendment): command-line flags named exactly like the logical settings, for example `-log.level=debug`; `--log.level=debug` is also accepted. `--` is not an end-of-flags marker (it is a malformed flag). Errors:
    - unknown flag names and malformed flags (`-`, `--`, `-=x`, empty name, control characters) -> `config.Error` with `setting=unknown` (`config.SettingUnknown`); the given text is never echoed;
    - positional arguments -> `setting=argument` (`config.SettingArgument`), never echoed;
    - a missing value or a repeated known flag -> `config.Error` naming the known setting, never the value.
- FRs/ACs: FR-5, FR-6, FR-16, FR-19 (`tracing.enabled`), FR-21 (`tracing.sample_ratio`), FR-27; AC-21 (unit part 1: each setting invalid, `log.level=debug-4`, `admin.listen=:9187`, `log.level=DEBUG` accepted, marker no-echo including in unknown/malformed flag names and positional args), AC-11 (default value).
- Implementer owns: `internal/infra/config/*.go` (non-test).
- Test writer owns: `internal/infra/config/*_test.go`.
- Interfaces defined:
  - `config.Settings{LogFormat logging.Format; LogLevel slog.Level; AdminListen string; TracingEnabled bool; SampleRatio float64}`; `config.Defaults() Settings`.
  - `config.Setting*` name constants (`"log.format"`, ...), plus `config.SettingUnknown = "unknown"` and `config.SettingArgument = "argument"`.
  - `config.Parse(raw map[string]string) (Settings, error)`.
  - `config.Error{Setting string}` with `Unwrap`, `ConfigSetting()`; `config.NewError(setting string, cause error) error`.
  - `config.LogError(l *slog.Logger, err error)` (caller supplies the `component=config` logger).
  - `config.FromArgs(args []string) (map[string]string, error)` (interim).
- Docker: no.
- Depends on: T3, T5.

### T10: Tracing environment resolution and OTEL_* validation

- Goal: decide whether tracing is on, and validate every honoured or validated `OTEL_*` variable when it is.
  - `OTEL_SDK_DISABLED`: `true` (case-insensitive) disables; anything else is false, never an error.
  - When enabled, validate: `OTEL_TRACES_EXPORTER` (`otlp` default, `none`); protocol (`OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` over `OTEL_EXPORTER_OTLP_PROTOCOL`; `http/protobuf` default, `grpc`); endpoint (both variants); headers (both variants, W3C baggage-style `key=value` list, percent-decoded); timeout (both variants); certificate / client certificate / client key (readable, PEM-parsable; contents never logged or kept beyond parse); compression (`gzip`/`none`); insecure (`true`/`false`, case-insensitive); `OTEL_TRACES_SAMPLER` (six values); `OTEL_TRACES_SAMPLER_ARG` (number in [0,1] only when the effective sampler uses a ratio, ignored otherwise); `OTEL_BSP_*` (FR-37 mapping, defaults 2048 / 5000 ms / 30000 ms / 512, batch <= queue); `OTEL_RESOURCE_ATTRIBUTES` format; `OTEL_SERVICE_NAME`. Rules not spelled out in the spec follow D8.
  - When disabled, nothing except `OTEL_SDK_DISABLED` is read.
  - Every error is a `config.Error` whose setting is the variable name.
  - D8 refinements after T10 review (decided by Orchestrator under Owner's standing instruction, 2026-10-24):
    - `*_HEADERS`: keys must be RFC 7230 tokens and are not percent-decoded; values are percent-decoded.
    - Client certificate and client key are paired per prefix (generic `OTEL_EXPORTER_OTLP_*` vs `OTEL_EXPORTER_OTLP_TRACES_*`) and must load together with `tls.X509KeyPair`.
    - Values are `strings.TrimSpace`d before the empty (= unset) check.
    - An unset `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` defaults to `min(512, queue size)`.
    - `OTEL_BSP_MAX_QUEUE_SIZE` and `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` must be ≤ `1<<20`.
    - Exporter, protocol and sampler values are case-insensitive; compression is matched exactly.
    - PEM files must be regular files of at most 4 MiB, opened with `O_NONBLOCK` (so a FIFO or device cannot block startup).
    - Private keys in PKCS#8, PKCS#1 or SEC1 form are accepted; encrypted keys are rejected.
- FRs/ACs: FR-19, FR-20, FR-21 (validation), FR-27, FR-37 (variable mapping); AC-21 (unit part 2, all eight listed values rejected when on and accepted/ignored when off, marker no-echo), AC-14 (unit: `OTEL_SDK_DISABLED=yes` is false and not an error), AC-16 (ratio range rejection).
- Implementer owns: `internal/infra/telemetry/tracing/env.go` and other non-test files it creates for validation in `internal/infra/telemetry/tracing/` (prefix `env*`).
- Test writer owns: `internal/infra/telemetry/tracing/env*_test.go`, `internal/infra/telemetry/tracing/testdata/**` (non-PEM and other non-certificate fixtures only). Certificate and key fixtures are synthetic self-signed material generated at test time into `t.TempDir()`; no `*.pem`, `*.key` or `*.p12` file is committed.
- Interfaces defined:
  - `tracing.LookupEnv func(key string) (string, bool)`.
  - `tracing.EnvConfig{Exporter ExporterKind; Protocol Protocol; Sampler SamplerKind (or unset); SamplerArg *float64; Queue QueueSettings; ...}`.
  - `tracing.QueueSettings{MaxQueueSize, MaxExportBatchSize int; ScheduleDelay, ExportTimeout time.Duration}` (T15 maps it to `spanqueue.Config`, so T10 does not depend on T12).
  - `tracing.Resolve(s config.Settings, env LookupEnv) (enabled bool, cfg EnvConfig, err error)`.
- Docker: no.
- Depends on: T9.

### T11: Sampler and resource construction

- Goal: FR-21 sampler selection and FR-30 resource allowlist.
  - Default: `ParentBased(TraceIDRatioBased(ratio))` with ratio from `OTEL_TRACES_SAMPLER_ARG` if set, else `tracing.sample_ratio`. `OTEL_TRACES_SAMPLER` replaces it with the named sampler; ratio-based variants use the effective ratio.
  - Resource: `service.name` default `corral` (overridden by `OTEL_SERVICE_NAME`), `telemetry.sdk.*`, `host.name`, and `OTEL_RESOURCE_ATTRIBUTES` contents; no other detectors; then remove `process.command_args`, `process.command_line`, `process.owner` whatever their source. Do not merge with `resource.Default()`.
  - Review decisions (decided by Orchestrator under Owner's standing instruction, 2026-10-24):
    - `NewResource` tolerates `resource.ErrPartialResource` (uses the partial resource); any other detector error becomes a constant sentinel error (no detector text).
    - The denylist checks both the raw key and its percent-decoded form.
    - `filter.go` defines `tracing.FilterResource(exp, res)`: an exporter wrapper that ensures exported spans carry no denylisted resource attributes (using the filtered `res` from `NewResource`), as the final guard before export (used by T15).
- FRs/ACs: FR-21, FR-30; AC-16, AC-25, AC-27 (unit part), AC-24 (resource part, unit).
- Implementer owns: `internal/infra/telemetry/tracing/sampler.go`, `internal/infra/telemetry/tracing/resource.go`, `internal/infra/telemetry/tracing/filter.go`.
- Test writer owns: `internal/infra/telemetry/tracing/sampler_test.go`, `internal/infra/telemetry/tracing/resource_test.go`, `internal/infra/telemetry/tracing/filter_test.go`.
- Interfaces defined: `tracing.NewSampler(cfg EnvConfig, ratio float64) sdktrace.Sampler`; `tracing.NewResource(ctx context.Context, env LookupEnv) (*resource.Resource, error)`; `tracing.FilterResource(exp sdktrace.SpanExporter, res *resource.Resource) sdktrace.SpanExporter`.
- Docker: no.
- Depends on: T10. Uses `go.opentelemetry.io/otel/sdk` and `semconv/v1.33.0`.

### T12: Bounded-queue span processor

- Goal: FR-37 processor (ADR-0007).
  - Implements `sdktrace.SpanProcessor`. `OnStart` no-op. `OnEnd` enqueues sampled spans with a non-blocking send; on full queue drops and increments the dropped counter. One worker exports when batch size is reached or the schedule delay elapses with spans queued; each export call bounded by the export timeout. `ForceFlush(ctx)` drains and exports. `Shutdown(ctx)` stops intake, drains until the context deadline, then drops the rest (not counted as queue-full drops), and shuts the exporter down.
  - Export errors are passed to `otel.Handle` (routed by T8), never logged directly.
  - Clock injectable for the schedule delay: option `WithTicker(NewTicker)` (decided by Orchestrator under Owner's standing instruction, 2026-10-24, as are the next items).
  - `ForceFlush` exports only the snapshot of spans queued at call time (spans ended afterwards are not waited for).
  - A second `Shutdown` returns its own `ctx.Err()`.
  - `Config` clamping and fallbacks (non-positive or oversized values, batch > queue) are defence in depth; T10 is the validator.
- FRs/ACs: FR-34 (processor side), FR-37; AC-36 (unit: blocking exporter, start/end non-blocking within a bound, dropped counter equals ended minus exported).
- Implementer owns: `internal/infra/telemetry/tracing/spanqueue/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/tracing/spanqueue/*_test.go`.
- Interfaces defined: `spanqueue.Config{MaxQueueSize, MaxExportBatchSize int; ScheduleDelay, ExportTimeout time.Duration}`; `spanqueue.DefaultConfig()`; `spanqueue.WithTicker(NewTicker) Option` (injectable ticker factory for the schedule delay); `spanqueue.New(exp sdktrace.SpanExporter, cfg Config, dropped DropCounter, opts ...Option) *Processor`, where spanqueue defines its own minimal `type DropCounter interface{ Add(float64) }` (satisfied by T7's `*metrics.Counter`; easy to fake). spanqueue does not import `metrics` (decided by Orchestrator under Owner's standing instruction, 2026-10-24).
- Docker: no.
- Depends on: none at the code level (own `DropCounter` interface); T7 supplies the real counter at wiring time (T15). Scheduling unchanged.

### T13: In-process fake OTLP collector

- Goal: the test collector used by all tracing integration tests (ADR-0005). No twin.
  - OTLP/HTTP `POST /v1/traces` (protobuf; gzip request bodies decoded and flagged) and OTLP/gRPC `TraceService/Export` (gzip compressor registered and flagged), each on its own ephemeral `127.0.0.1` listener.
  - Modes: accept, reject (configurable HTTP status / gRPC code), unresponsive (blocks until `Close`).
  - Records: decoded `ResourceSpans` (span names, attributes, events, status, resource attributes), per-request transport headers / metadata, content encoding, accepted TCP connection count (via a counting `net.Listener`).
  - Helper to reserve an unreachable loopback endpoint (bind, read port, close).
  - Review decisions (decided by Orchestrator under Owner's standing instruction, 2026-10-24):
    - `const MaxBodyBytes = 4 << 20` (4 MiB). OTLP/HTTP: caps both the raw request body (`http.MaxBytesReader`) and the decompressed gzip stream; an oversized body gets 413 and no spans are recorded. OTLP/gRPC: relies on grpc-go's default 4 MiB receive limit, which applies after decompression.
    - Attribute rendering in recorded spans/resources: bytes values as padded standard base64; arrays and kvlists as compact JSON with sorted keys. T14 builds its `spans` scan text from this rendering, so marker encodings must account for it.
    - `ConnCount()` is read while the collector is open, before `Close()`.
- FRs/ACs: supports AC-3 (integration), AC-14, AC-15, AC-24, AC-27, AC-28, AC-33, AC-38.
- Implementer owns: `internal/testkit/otlpcollector/*.go` (non-test).
- Test writer owns: `internal/testkit/otlpcollector/*_test.go` (self-test using the real OTel exporters).
- Interfaces defined: `otlpcollector.Start(tb testing.TB, opts ...Option) *Collector`; `(*Collector).HTTPEndpoint() string`, `GRPCEndpoint() string`, `Spans() []RecordedSpan`, `Resources() []map[string]string`, `Requests() []RequestInfo{Protocol, Headers, ContentEncoding}`, `ConnCount() int`, `SetMode(Mode)`, `Close()`; `otlpcollector.UnreachableEndpoint(tb) string`.
- Docker: no.
- Depends on: T1. Adds `go.opentelemetry.io/proto/otlp` and `google.golang.org/grpc` at OTel v1.47.0's versions.

### T14: Marker-scan harness (AC-33)

- Goal: reusable leak scanner for AC-38 now and AC-18 in feature specs.
  - Sources: `stdout`, `stderr`, `metrics`, `readyz`, `spans` (collector payload text built from span names, attribute keys and values, event names and attributes, status descriptions, resource attributes; transport headers excluded). Attribute values use T13's rendering (bytes as padded standard base64, arrays/kvlists as sorted compact JSON); the raw-marker pattern must still match inside JSON-rendered arrays, and a marker carried in a bytes attribute is found via its base64 patterns.
  - Capture helpers: in-process stdout/stderr fd redirection; child-process pipes; HTTP body fetch for `/metrics` and `/readyz`; collector payload extraction from T13.
  - Encodings per marker: raw; hex lower and upper; base64 standard and URL-safe, each with 0, 1 and 2 prefix bytes, dropping unstable edge characters.
  - Per-marker source selection (all or subset).
- FRs/ACs: AC-33 (harness and its self-test).
- Implementer owns: `internal/testkit/markerscan/*.go` (non-test).
- Test writer owns: `internal/testkit/markerscan/*_test.go` (self-test planting each encoding at byte offsets 0..2 in each source and expecting detection; negative control with no marker).
- Interfaces defined: `markerscan.New() *Scanner`; `(*Scanner).AddMarker(m string, sources ...Source)`; `(*Scanner).Add(src Source, data []byte)`; `(*Scanner).AddCollector(c *otlpcollector.Collector)`; `(*Scanner).Findings() []Finding`; `markerscan.CaptureStdio(tb) (stop func() (stdout, stderr []byte))`; `markerscan.Patterns(marker string) []Pattern`.
- Docker: no.
- Depends on: T13.

### T15: Tracing bootstrap core

- Goal: `Start` builds the provider from validated config (ADR-0007) and wires log correlation.
  - Disabled: no-op provider, `Enabled()==false`, no exporter, no goroutines, no sockets.
  - `none`: SDK provider with sampler and resource, no processor.
  - `otlp`: `otlptracehttp` or `otlptracegrpc` per protocol, process environment pass-through, wrapped in `tracing.FilterResource` and then the T12 processor with the T7 counter; sampler and resource from T11; sets the global provider and `propagation` is not configured (no incoming context).
  - Returns whether the logging layer must use `logging.WithTraceCorrelation` (whenever tracing is enabled, including `OTEL_TRACES_EXPORTER=none`; D9).
  - Test-only exporter override option.
  - OTLP collector rejections (HTTP non-2xx, gRPC non-transport status) are wrapped in a corral type implementing `errclass.ProtocolError` before they reach `otel.Handle`/logs, so they classify as `protocol`/`other` (D3, T5 review decision).
  - Requirements (decided by Orchestrator under Owner's standing instruction, 2026-10-24; also in ADR-0007):
    - (a) The OTLP exporter is wrapped as `spanqueue.New(tracing.FilterResource(otlpExp, res), ...)`, where `res` is the resource returned by `NewResource` without error. Add a bootstrap-level AC-27 integration test that sets real `OTEL_RESOURCE_ATTRIBUTES` with banned keys and asserts they never reach the collector.
    - (b) The OTLP exporters read `os.Getenv` directly, not `LookupEnv`, so `Resolve` validation must run on the same real process environment (in production `Options.Env` is `os.LookupEnv`; tests use `t.Setenv`, not a fake lookup, when exporters are involved).
    - (c) Export errors go to `otel.Handle`, so `logbridge.Install` must have run first (ADR-0006 step 2).
    - (d) `Shutdown` passes a context with a 5 s deadline (shorter if the caller's is shorter) to the provider/processor (FR-34).
    - (e) Collector rejections are wrapped in an `errclass.ProtocolError` type (above).
    - (f) No processor or exporter path may bypass `FilterResource`, including the test-only exporter override.
- FRs/ACs: FR-8, FR-19, FR-20, FR-21, FR-30, FR-37 wiring; AC-3 (integration: unreachable collector, all captured stdout lines JSON, no stderr), AC-6 (integration: sampled and unsampled spans give `trace_id`/`span_id`, also with `OTEL_TRACES_EXPORTER=none` per D9; disabled gives neither), AC-14 (integration: three setups, provider records no spans except `none`, zero TCP connections), AC-15 (HTTP default, grpc, TRACES_ precedence, `otlp` same as unset, endpoint honoured, gzip), AC-24 (header received, resource values, `service.name=corral` default), AC-27 (integration, through the real bootstrap with banned keys in `OTEL_RESOURCE_ATTRIBUTES`).
- Implementer owns: `internal/infra/telemetry/tracing/tracing.go` and `internal/infra/telemetry/tracing/start*.go`.
- Test writer owns: `tests/integration/tracing/*_test.go` (bootstrap + logging factory with logcheck tee + T13 collector + `markerscan.CaptureStdio`).
- Interfaces defined: `tracing.Options{Settings config.Settings; Env LookupEnv; Config EnvConfig; Enabled bool; Metrics *metrics.Factory; Logger *slog.Logger; Exporter sdktrace.SpanExporter /* tests only */}`; `tracing.Start(ctx, Options) (*Tracing, error)`; `(*Tracing).Enabled() bool`; `TracerProvider() trace.TracerProvider`; `ForceFlush(ctx) error`; `Shutdown(ctx) error`.
- Docker: no.
- Depends on: T8, T10, T11, T12, T13 (and T4 for tests). Adds the OTLP exporter modules v1.47.0.
- Note: `Start` assumes `logbridge.Install` has already run (ADR-0006 step 2, before any grpc/otel/tracing code; T8/T18); it never installs or replaces the otel error handler, otel logger or grpclog itself. Integration tests call `Install` before `Start`. Wires T7's counter into spanqueue's `DropCounter`.

### T16: Tracing shutdown bound and backpressure integration

- Goal: FR-34 5-second bounded shutdown and FR-37 behaviour through the real bootstrap.
  - `Shutdown` applies `min(caller deadline, 5s)`; with an unresponsive collector it returns within 5 s plus processor/exporter teardown; spans ended just before shutdown reach a responsive collector.
  - Drop counting through the bootstrap with the exporter override and small `OTEL_BSP_MAX_QUEUE_SIZE`.
- FRs/ACs: FR-34, FR-37; AC-28, AC-36 (integration).
- Implementer owns: `internal/infra/telemetry/tracing/shutdown.go` (and fixes in T15/T12 files only via those tasks' owners; if a change in `spanqueue` is needed, it is raised against T12).
- Test writer owns: `tests/integration/tracing/shutdown_test.go`, `tests/integration/tracing/backpressure_test.go`.
- Interfaces defined: none new (`(*Tracing).Shutdown` semantics fixed).
- Docker: no.
- Depends on: T15.

### T17: Admin HTTP listener

- Goal: FR-15..FR-18 and FR-35 on a dedicated mux.
  - `http.ServeMux` (never `DefaultServeMux`) with exact `GET` patterns for `/metrics`, `/healthz`, `/readyz` (HEAD served by GET patterns); other methods 405 with `Allow: GET, HEAD`; any other path including `/metrics/`, `/debug/pprof/`, `/debug/vars` 404; query strings ignored.
  - `/metrics`: `promhttp.HandlerFor` the private registry, default content negotiation, no exemplars. `promhttp` must keep OpenMetrics exemplars off (for example by leaving `HandlerOpts.EnableOpenMetrics` false); a test asserts no exemplar appears for an OpenMetrics `Accept` header; confirmed again after the T10–T12 reviews. The admin listener only gathers; it never registers metrics (nothing outside the metrics factory and `metrics.NewRegistry` registers collectors) (decided by Orchestrator under Owner's standing instruction, 2026-10-24).
  - `/healthz`: 200, `ok\n`, `text/plain; charset=utf-8`.
  - `/readyz` (ADR-0006, D6): all responses `Content-Type: text/plain; charset=utf-8`. 200 body `ok\n`; 503 body `<reason>\n`. 503 `starting` until `MarkStarted`, and the registered `health.Readiness` is not consulted before then. After `MarkStarted`, delegates to the registered `health.Readiness` (if any) or returns 200. A reason outside `health.Reasons()` is returned as 503 `unknown` (the original reason is never echoed). From the `MarkStopping` call onward (graceful shutdown), 503 `shutting_down` regardless of the registered check. `MarkStopping` takes effect synchronously before it returns, so any `/readyz` request served after it returns gets `shutting_down`.
  - `Listen` binds `admin.listen`; on failure returns `config.NewError("admin.listen", cause)` (detail e.g. `EADDRINUSE`), never the address in any text. `http.Server.ErrorLog` from `logbridge.NewHTTPErrorLog`. Sensible server timeouts (read header timeout) to avoid slowloris on an unauthenticated listener.
  - Control characters (Rules for every task): response bodies contain only fixed strings (`ok`, reasons from `health.Reasons()`); nothing from requests or sanitiser output is concatenated into responses or headers.
- FRs/ACs: FR-9, FR-14 (exposure), FR-15, FR-16 (bind), FR-17, FR-18, FR-28 (ErrorLog wiring), FR-35; AC-7 (handler-level), AC-11 (unit: default bind `127.0.0.1:9187`, configured address, `0.0.0.0:<port>`), AC-12, AC-13, AC-30 (unit: in-use address gives `config.Error` with `admin.listen`, no address text).
- Implementer owns: `internal/infra/adminhttp/*.go` (non-test).
- Test writer owns: `internal/infra/adminhttp/*_test.go`.
- Interfaces defined: `adminhttp.Options{Addr string; Gatherer prometheus.Gatherer; Readiness health.Readiness; ErrorLog *log.Logger}`; `adminhttp.New(Options) *Server`; `(*Server).Listen() error`; `Serve() error`; `Addr() net.Addr`; `MarkStarted()`; `MarkStopping()`; `Handler() http.Handler`; `Shutdown(ctx) error`.
- Docker: no.
- Depends on: T2, T7, T8, T9.

### T18: Runtime composition root, lifecycle, binary and functional suite

- Goal: wire everything in the ADR-0006 order; produce the `corral` binary; binary-level functional tests.
  - Startup steps 0–3 (ADR-0006): 0 parse settings, logging errors via `logging.BootstrapLogger` (stdlib only, no third-party code); 1 build the logging factory from the settings; 2 `logbridge.Install(factory, level, now)` before any grpc, otel, tracing or admin-listener code; 3 resolve tracing and validate `OTEL_*`, logging errors via `Factory.Logger(logging.Config)`.
  - `runtime.Run` implements startup steps 0..9, signal-driven graceful shutdown, exit codes (0 on graceful shutdown, non-zero on any startup failure), lifecycle records (`starting`, `startup complete`, `shutting down`, constant messages). On shutdown, calls `MarkStopping` on the admin server immediately before emitting the lifecycle `shutting down` record, so `/readyz` reports 503 `shutting_down` from the moment that record is emitted (D6, spec Amendment 1); only then shuts the admin server down. Recovered panics in runtime goroutines are logged via `errclass.FromPanic` (class `internal`, optional `stack`).
  - `cmd/corral/main.go` calls `runtime.Run` with `os.Args[1:]`, `os.LookupEnv`, `os.Stdout`, signal context.
  - Metrics are registered only through the metrics factory (`metrics.RegisterAll` and factory constructors) on the registry from `metrics.NewRegistry`; the runtime never calls `Register`/`MustRegister` directly (decided by Orchestrator under Owner's standing instruction, 2026-10-24).
  - Config errors are logged with `config.LogError` using `logging.BootstrapLogger(stdout)` before the configured handler exists, and `Factory.Logger(logging.Config)` afterwards; never a logger without `component=config` (`LogError` adds no component).
  - Functional AC-21 cases include an unknown flag, a malformed flag and a positional argument (expect `setting=unknown` / `setting=argument`, no echo of the given text), and `--name=value` accepted (ADR-0003 amendment).
  - Hooks for in-process tests: `BeforeLastStep` (AC-31) and `Started` (exposes admin address and tracer provider for AC-38 test spans).
- FRs/ACs: FR-4, FR-7 (lifecycle), FR-18 (startup-complete), FR-27 (fatal + pre-logger JSON), FR-28 (install order), FR-34 (shutdown call), FR-35; AC-31 (in-process), AC-3 (functional: default settings + `tracing.enabled=true` + unreachable collector; every stdout line JSON; lifecycle records; empty stderr), AC-7 (functional on the binary), AC-8 (live registry of the running process has no `corral_` family outside the catalog, checked in-process via `Started` hook), AC-11 (functional default bind and configured bind), AC-12 (functional), AC-21 (functional: invalid setting; tracing on + invalid `OTEL_*`; non-zero exit; JSON error on stdout with `component=config` and `setting`), AC-30 (functional).
- Implementer owns: `internal/app/runtime/*.go` (non-test), `cmd/corral/main.go`.
- Test writer owns: `internal/app/runtime/*_test.go`, `tests/functional/**` (build tag `functional`; `TestMain` builds the binary with `go build` into a temp dir; stdout/stderr captured separately; graceful shutdown via SIGTERM).
- Interfaces defined: `runtime.Hooks{BeforeLastStep func(ctx context.Context); Started func(Handles)}`; `runtime.Handles{AdminAddr net.Addr; TracerProvider trace.TracerProvider; Gatherer prometheus.Gatherer}`; `runtime.Run(ctx context.Context, args []string, env tracing.LookupEnv, stdout io.Writer, hooks Hooks) int`.
- Docker: no.
- Depends on: T15, T16, T17 (and transitively all earlier tasks). Functional tests pass settings as interim flags (D1, ADR-0003). This task creates `tests/functional`; from its completion `make test-functional` becomes a gate.
- Note for test writer: the default-bind functional case uses the real port 9187 on loopback; skip with a clear message if it is already in use on the host rather than fail spuriously.

### T19: AC-38 marker leak verification

- Goal: integration test proving no marker leaks in the AC-38 setups. Test-only task; any defect found is routed back to the owning task's implementer (T5, T8, T15, T17 or T18).
  - Tracing on, `log.level=debug`, in-process `runtime.Run` with the `Started` hook; test spans created through the exposed provider; `markerscan` over stdout, stderr, `/metrics`, `/readyz` (fetched both during the `BeforeLastStep` hold and after start) and collector payloads.
  - Setups: `OTEL_EXPORTER_OTLP_HEADERS=authorization=<marker>` with a rejecting collector; same header with an unreachable collector; endpoint URL with a userinfo marker over `http/protobuf` and over `grpc`.
- FRs/ACs: FR-24, FR-28, FR-33; AC-38.
- Implementer owns: none.
- Test writer owns: `tests/integration/leaks/ac38_test.go`.
- Interfaces defined: none.
- Docker: no.
- Depends on: T14, T18.

### T20: Operator documentation

- Goal: FR-36 (verified by review) and FR-38 documentation.
  - States: the admin listener has no authentication or TLS and a non-loopback bind exposes metrics and health to anyone who can reach it; Kubernetes deployments must set `admin.listen` to a non-loopback address (explicit `0.0.0.0`, `[::]` or pod IP); a blocked stdout can stall the proxy (FR-38, synchronous logging).
  - Also documents logical settings and defaults, supported `OTEL_*` variables and which are fatal when invalid, the endpoints, and `corral_tracing_spans_dropped_total`. No example contains credentials; header and certificate examples use placeholders such as `<YOUR_API_KEY>`.
- FRs/ACs: FR-36, FR-38 (review items).
- Implementer owns: `docs/operations/observability.md`, link from a top-level `README.md` if one is created.
- Test writer owns: none.
- Interfaces defined: none.
- Docker: no.
- Depends on: T9, T17 (facts stable). Documents the interim flag syntax (D1 and the ADR-0003 amendment: `-name=value` or `--name=value`; `--` is not an end-of-flags marker; unknown or malformed flags report `setting=unknown`, positional arguments `setting=argument`; `admin.listen` host rules) as temporary until the configuration spec, and the `/readyz` reasons (`starting`, `shutting_down`, `unknown`, plus any other defined reasons).

### T21: Production import lint and hermetic importcheck environment

Added after the T2–T6/T13 reviews (decided by Orchestrator under Owner's standing instruction, 2026-10-24).

- Goal:
  - (a) A lint test proving that no non-test package in the module, outside `internal/testkit/...`, imports `github.com/sorinpanduru/corral/internal/testkit/...` or `golang.org/x/tools/...`. Reuses `importcheck` (same `go list -deps -json` machinery; non-test `Imports` only).
  - (a2) A dependency-closure check (`importcheck.ForbiddenDeps`) proving the shipped binary's dependencies on the release platforms contain no test infrastructure (see Interfaces).
  - (b) `importcheck`'s `go list` environment adds `GOENV=off` and `GOPACKAGESDRIVER=off` to the existing hermetic environment.
  - The new check keeps T2's guards: leading `-` in a pattern rejected, empty match is an error.
- FRs/ACs: supports AC-1 (same import-boundary tooling); enforces the ADR-0001/ADR-0002 rule that production code does not import test infrastructure (FR-24 hygiene, review item).
- Implementer owns: `internal/testkit/importcheck/*.go` (non-test).
- Test writer owns: `tests/lint/testkit_imports_test.go` (runs the check over `./...` from the repo root; a violating fixture under `tests/lint/testdata/**` proves it fails); updates to importcheck self-tests for the env change.
- Interfaces defined: `importcheck.Forbidden(dir, pattern string, exclude []string, forbidden []string) ([]Violation, error)`: reports every non-test import, by a matched package whose import path is not under any `exclude` prefix, of a path under any `forbidden` prefix. `Check` and `Scan` are unchanged apart from the environment.
  - `importcheck.ForbiddenDeps(...)` (decided by Orchestrator under Owner's standing instruction, 2026-10-24): computes the `go list -deps` closure of `./cmd/...` for each of `linux/amd64`, `darwin/arm64` and `windows/amd64` (`GOOS`/`GOARCH` set per run) and reports any dependency under `github.com/sorinpanduru/corral/internal/testkit`, `golang.org/x/tools`, or the `testing` package. Run from `tests/lint/testkit_imports_test.go`.
  - Accepted limitation: custom build tags are not checked. If release builds ever use build tags, add them to `ForbiddenDeps`' runs.
- Docker: no.
- Depends on: T4 (needs `golang.org/x/tools` present so the rule is meaningful; `constmsg` is the expected only importer). Independent of T5–T20; can run in any wave after T4.

### T22: Early slog neutralisation (done)

Verification fix after T19/T18 functional runs (decided by Orchestrator under Owner's standing instruction, 2026-10-24). Status: **done** (branch `factory/SPEC-001/T22-thirdparty-stderr`).

- Goal: no third-party `slog`/`log` output reaches stderr during package init, before `logbridge.Install` runs (ADR-0008). Trigger: `go.opentelemetry.io/auto/sdk` calls `slog.Warn` at init for invalid `OTEL_*_LIMIT` values and echoes the raw value.
  - `earlyslog` (stdlib imports only) installs at init a counting-only default slog handler and counts std `log` writes; nothing is retained but per-level counts.
  - `cmd/corral` blank-imports `earlyslog`; `logbridge` imports it. `logbridge.Install` calls `earlyslog.Handoff` and replays the counts through the `thirdparty` bridges (fixed message, per-source/per-level limiter, `suppressed_count`).
  - Functional guard on init order (`tests/functional/initorder_test.go`) with a reviewed allowlist of `log`-only importers that init before `earlyslog`.
- FRs/ACs: FR-24, FR-28 (Amendment 1 `thirdparty`); AC-3 (empty stderr), AC-26.
- Implementer owned: `internal/infra/telemetry/earlyslog/*.go` (non-test), `internal/infra/telemetry/logbridge/install.go`, `cmd/corral/main.go`.
- Test writer owned: `internal/infra/telemetry/earlyslog/*_test.go`, `internal/infra/telemetry/logbridge/replay_early_test.go`, `tests/functional/thirdparty_stderr_test.go`, `tests/functional/initorder_test.go`.
- Interfaces defined: `earlyslog.Counts{Debug, Info, Warn, Error, StdLog uint64}`; `earlyslog.Handoff(h slog.Handler) Counts`; `earlyslog.Drain() Counts`.
- Docker: no.
- Depends on: T8, T18.

### T23: FR-41 plaintext for `http://` OTLP endpoints (done)

Implements spec Amendment 2 (FR-41, AC-40). Status: **done** (branch `factory/SPEC-001/T23-otlp-ca-http`, 00af1e0).

- Goal: with an `http://` effective traces endpoint, both protocols export in plaintext, TLS material is not used, and exactly one FR-41 warn record names the set TLS variables (ADR-0009).
  - `tracing.TLSIgnoredSettings` returns the sorted names of the set TLS variables when the effective traces endpoint (`_TRACES_` over general, scheme case-insensitive) is `http://`.
  - `newOTLP` passes explicit plaintext options whenever the effective endpoint is `http://` (http: nil TLS config plus insecure; grpc: insecure transport credentials plus insecure), overriding env-derived TLS, including `OTEL_EXPORTER_OTLP_INSECURE=false`.
  - The runtime emits the warn record (`component=config`, constant message, `setting` = names only) before the lifecycle records. FR-27 validation in `Resolve` runs first and is unchanged.
- FRs/ACs: FR-7 (`setting` names only), FR-20, FR-24, FR-27, FR-41; AC-40.
- Implementer owned: `internal/infra/telemetry/tracing/plaintext.go`, `internal/infra/telemetry/tracing/start*.go`, `internal/app/runtime/runtime.go`.
- Test writer owned: `internal/infra/telemetry/tracing/plaintext*_test.go`, `tests/functional/otlp_tls_http_test.go`.
- Interfaces defined: `tracing.TLSIgnoredSettings(cfg EnvConfig, env LookupEnv) []string`.
- Docker: no.
- Depends on: T10, T15, T18, T22.

### T24: Hardening follow-ups

Scoped only to behaviour the approved spec already requires (Amendments 1 and 2 included) and to test and documentation quality. No new behaviour. Sub-items are independent and can be delivered as separate implementer/test-writer rounds; each one is done when its acceptance holds and `make build`, `make test` and `make test-functional` pass.

#### T24.1: ADR-0008 wording (docs)

- Goal: correct ADR-0008's claim that `earlyslog` "runs first". What is true and what the guard enforces: `earlyslog` initialises before every linked third-party package that uses `log/slog` (and, except for the reviewed allowlist, `log`) at init. Standard-library packages and `log`-only packages may init earlier; the Residual risk paragraph stays and is aligned with the T24.2 guard.
- FRs/ACs: FR-28 (documentation of the mechanism); no AC.
- Owner: planner (ADR text). No code.
- Files: `docs/adr/0008-early-slog-neutralisation.md`.
- Interfaces: none.
- Acceptance: the ADR no longer says `earlyslog` runs first. It states the property in terms of third-party packages that use `slog`/`log` at init, names the T24.2 guard as the enforcement, and keeps the residual-risk statement.
- Depends on: none (can run first in T24).

#### T24.2: Init-order guard on transitive dependents

- Goal: `tests/functional/initorder_test.go` currently selects only direct importers of `log/slog` and `log` (`Imports`). A package that reaches `slog`/`log` through an intermediate package is not checked. Strengthen it:
  - Select every non-standard package in `go list -deps ./cmd/corral` whose transitive closure (`Deps`, from `-json=ImportPath,Standard,Deps`) contains `log/slog` or `log`.
  - Determine the real init order from the built binary with `GODEBUG=inittrace=1` (order of `init <importpath>` lines on stderr). Packages that do no init work do not appear and cannot log at init, so they need no check. Do not re-derive the order from the Go spec's sort rule as the only source.
  - Fail if any selected package inits before `github.com/sorinpanduru/corral/internal/infra/telemetry/earlyslog` and is not on the reviewed allowlist.
  - Stale-allowlist check: each allowlist entry must still (a) be a transitive dependent of `log` or `log/slog` and (b) actually init before `earlyslog` in the trace. If either is false, the test fails and asks for the entry to be removed.
  - The test uses the hermetic `go list` environment (`GOENV=off`, `GOPACKAGESDRIVER=off`, as in T21). It runs against the binary that `TestMain` builds, with no tracing configured.
- FRs/ACs: FR-28; AC-3 (empty stderr), supports ADR-0008.
- Test writer owns: `tests/functional/initorder_test.go`.
- Implementer owns: nothing, unless the strengthened guard finds an unallowlisted package. That finding goes to the Orchestrator, because extending the allowlist or changing `earlyslog` needs a review decision.
- Interfaces: none new (test-local helpers only).
- Acceptance: the guard selects transitive dependents. A self-check in the test shows that at least one known indirect dependent of `log/slog` (one with no direct `log/slog` import) is in the selected set. Every allowlist entry is shown both as a dependent and as initialising before `earlyslog`. The suite passes on the current tree.
- Depends on: T22.

#### T24.3: grpclog output never reaches stderr (FR-28)

- Goal: `google.golang.org/grpc/grpclog` initialises before `earlyslog` and, at init, builds its default `LoggerV2` with its own `log.New(os.Stderr, ...)` loggers. Its severity and verbosity come from `GRPC_GO_LOG_SEVERITY_LEVEL` and `GRPC_GO_LOG_VERBOSITY_LEVEL`. Prove that no grpc log output reaches stderr before or after `logbridge.Install`.
  - Functional cases (built binary, separate stdout/stderr capture), each with `GRPC_GO_LOG_SEVERITY_LEVEL` in {`info`, `warning`, `error`} and `GRPC_GO_LOG_VERBOSITY_LEVEL` in {unset, `99`}:
    - tracing on, `OTEL_EXPORTER_OTLP_PROTOCOL=grpc`, unreachable collector (`otlpcollector.UnreachableEndpoint`), `log.level=debug`;
    - tracing on, grpc, collector in reject mode;
    - tracing off (only init-time behaviour is exercised).
  - Each case asserts empty stderr, every stdout line is JSON, and any grpc record has `component=grpc` and a fixed message.
  - Each case also plants a marker (for example in the endpoint path or userinfo, as in AC-38) and runs a `markerscan` pass over stdout and stderr.
  - Fix only if a test shows a leak. Any fix stays within T8/T22-owned files (`internal/infra/telemetry/logbridge/*.go`, `internal/infra/telemetry/earlyslog/*.go`) and is routed through the Orchestrator.
- FRs/ACs: FR-24, FR-28; AC-3 (empty stderr), AC-26, AC-29.
- Test writer owns: `tests/functional/grpclog_stderr_test.go`.
- Implementer owns (only on a failing test): `internal/infra/telemetry/logbridge/*.go`, `internal/infra/telemetry/earlyslog/*.go` (non-test).
- Interfaces: none new.
- Acceptance: all cases pass with empty stderr and no marker findings. If a case fails, the fix lands and the same cases pass.
- Depends on: T8, T13, T14, T18, T22.

#### T24.4: otel logger sink never emits key/value arguments (FR-28, FR-24, FR-29)

- Goal: the OTel SDK's envconfig calls `global.Error(err, "read tls client cert", "file", <path>)` (and similar for the CA certificate and client key) if a cert file becomes unreadable after `Resolve` validated it. Verify that the `logr` sink installed by `logbridge` (`otelLogSink`) never emits the key/value arguments or the message, and that the error carries only the FR-29 class and detail (the `*os.PathError` text contains the path).
  - Unit: call the sink's `Info`/`Error` (and `WithValues`/`WithName` chains) with a marker-bearing message, marker-bearing kv values, and an `*os.PathError` whose path contains the marker. Assert the record has `component=otel`, the fixed per-level message, the error class and detail only, and no marker in any attribute (logcheck tee and `markerscan`).
  - Integration: valid synthetic PEM files (generated at runtime) under marker paths, `OTEL_EXPORTER_OTLP_CERTIFICATE` / `_CLIENT_CERTIFICATE` / `_CLIENT_KEY` set, an `https://` endpoint so FR-41 does not apply. `Resolve` succeeds; the files are then made unreadable (removed or `chmod 000`) before `tracing.Start`. Check both protocols. Assert no marker in stdout or stderr, and no stderr output. Whether the exporter then fails or exports is not asserted. Skip the `chmod` variant when running as root.
- FRs/ACs: FR-7 (`setting` names only), FR-24, FR-28 (no key/value arguments forwarded), FR-29.
- Test writer owns: `internal/infra/telemetry/logbridge/otel_kv_test.go`, `tests/integration/tracing/envconfig_unreadable_test.go`.
- Implementer owns (only on a failing test): `internal/infra/telemetry/logbridge/otel.go`.
- Interfaces: none new.
- Acceptance: both tests pass with no marker findings and empty stderr.
- Depends on: T8, T14, T15.

#### T24.5: otlpcollector records span links and instrumentation scope

- Goal: test-quality. The fake collector currently drops span links and the instrumentation scope, so tests cannot assert on them. FR-24 says forbidden data must not be emitted "in any signal", and OTLP span payloads carry links and scope. The collector should therefore record them so that FR-24/FR-23 tests can make assertions on the full payload.
  - Record, per span: its links (each link's trace ID and span ID as lowercase hex, plus its attributes) and its instrumentation scope (name, version, attributes). Use the existing T13 rendering for attribute values: bytes as padded standard base64, arrays and kvlists as compact JSON with sorted keys.
  - This applies to both OTLP/HTTP and OTLP/gRPC.
  - `markerscan`'s `spans` source is **not** changed in T24. AC-33 lists the scanned span fields (names, attributes, events, status descriptions, resource attributes). Scanning links and scope as well is a superset; whether to do it is listed under "Proposed for Owner" (P5).
- FRs/ACs: test infrastructure for FR-23 and FR-24. No FR or AC names link or scope attributes (see the spec-support note in P5).
- Implementer owns: `internal/testkit/otlpcollector/*.go` (non-test).
- Test writer owns: `internal/testkit/otlpcollector/*_test.go` (self-test using the real OTel exporters: one span with a link carrying attributes and a tracer with scope name, version and attributes, over both protocols).
- Interfaces defined (additive; existing fields unchanged):
  - `otlpcollector.RecordedLink{TraceID, SpanID string; Attributes map[string]string}`;
  - `otlpcollector.RecordedScope{Name, Version string; Attributes map[string]string}`;
  - new fields `RecordedSpan.Links []RecordedLink` and `RecordedSpan.Scope RecordedScope`.
- Acceptance: the self-test asserts the link and scope values over both protocols. Existing T13/T14/T15/T19/T23 suites pass unchanged.
- Depends on: T13.

#### T24.6: gRPC `codes.Unavailable` classification: dropped from T24

- FR-29 defines only the class enum (`timeout`, `network`, `protocol`, `server`, `config`, `internal`). It does not say which errors belong to which class; in particular it does not say that connection failures are `network`.
- The mapping in use is D3, a plan decision, not spec text. D3 covers errno and DNS/TLS/EOF failures (as `network`) and OTLP collector rejections (gRPC non-transport status as `protocol`/`other`). It does not cover a gRPC status that reports a transport failure.
- Today `tracing.ClassifyExportError` passes `Unavailable`/`DeadlineExceeded`/`Canceled` status errors through unchanged. Because a gRPC status error does not unwrap to the underlying `net`/errno error, they classify as `internal`/`other`.
- Changing this needs an Owner decision: see P4. Not planned.

---

## Ordering and parallelism

- Wave 1: T1.
- Wave 2: T2, T3, T6, T13 (parallel).
- Wave 3: T4, T5, T14 (T4 and T5 need T3; T14 needs T13).
- Wave 4: T7, T8, T9 (need T5; T8 and T9 also need T3); T21 (needs only T4; may also run in any later wave).
- Wave 5: T10, T12, T17 (T10 needs T9; T12 needs T7; T17 needs T2, T7, T8, T9).
- Wave 6: T11 (needs T10).
- Wave 7: T15 (needs T8, T10, T11, T12, T13).
- Wave 8: T16, T20.
- Wave 9: T18.
- Wave 10: T19.
- Wave 11: T22, then T23 (both done; verification fixes after Wave 10).
- Wave 12: T24. T24.1 first (docs only), then T24.2 to T24.5 in parallel. T24.6 is not planned (P4).

## Coverage table (Part A)

| AC | Task(s) |
|---|---|
| AC-1 | T2, T21 (hermetic env, production import lint) |
| AC-3 | T3 (unit), T15 (integration), T18 (functional), T22 (init-time stderr), T24.2/T24.3 (guards) |
| AC-4 | T3 |
| AC-5 | T3 (factory rejects unknown components), T4 (test handler, constant-message check; applied by all later suites) |
| AC-6 | T3 (correlation wrapper unit), T15 (integration) |
| AC-7 | T17 (handler level), T18 (functional) |
| AC-8 | T7 (catalog lint, constructor refusal, promlint), T18 (live registry of running process) |
| AC-11 | T9 (default), T17 (unit bind), T18 (functional) |
| AC-12 | T17, T18 |
| AC-13 | T17 |
| AC-14 | T10 (unit `OTEL_SDK_DISABLED=yes`), T15 (integration) |
| AC-15 | T15 |
| AC-16 | T10 (range validation), T11 |
| AC-21 | T9 (settings), T10 (`OTEL_*`), T18 (functional) |
| AC-24 | T11 (resource), T15 (integration with headers) |
| AC-25 | T11 |
| AC-26 | T8, T22 (early replay), T24.3 |
| AC-27 | T11 (unit), T15 (integration) |
| AC-28 | T16 |
| AC-29 | T8 |
| AC-30 | T17 (unit), T18 (functional) |
| AC-31 | T18 |
| AC-33 | T14 |
| AC-35 | T5 |
| AC-36 | T12 (unit), T16 (integration) |
| AC-37 | T6 |
| AC-38 | T19 (uses T13, T14) |
| AC-40 | T23 |

Review-only items: FR-6 read-once (T9/T18), FR-20 pass-through vars (T15), FR-24 no TLS/key/uid paths (all; T24.4 adds a test for TLS file paths in OTel SDK logs), FR-29 no `err.Error()`/panic values at call sites (all), FR-36 (T20), FR-38 (T3, T20).

## Decisions (former open questions Q1-Q9)

All decisions below were decided by Orchestrator under Owner's standing instruction, 2026-10-24, Owner may override. Dn records the answer to former question Qn. If the Owner overrides one, the tasks and ADRs cited in it are re-planned.

No open questions remain from Q1–Q9. Items awaiting the Owner are listed under "Proposed for Owner" below.

- **D1 (Q1; T9, T18, T20; ADR-0003 Accepted).** Interim loader: command-line flags named exactly like the logical settings (for example `-log.level=debug`), per ADR-0003, to be replaced by the configuration spec. Amended after T9 review (ADR-0003 Amendment 1): `--name=value` accepted, `--` not an end-of-flags marker, unknown/malformed flags and unknown `Parse` keys report `setting=unknown`, positional args `setting=argument`, stricter `admin.listen` host rules.
- **D2 (Q2; T6).** The initial central command-name allowlist is exactly `find` and `hello`.
- **D3 (Q3; T5, T17).** FR-29 error class mapping, as proposed by the planner: `deadline_exceeded` -> `timeout`; OS/net timeouts (`ETIMEDOUT`, `net.Error.Timeout()`) -> `timeout`; `canceled` -> `internal`; `ECONNREFUSED`/`ECONNRESET`/`ECONNABORTED`/`EPIPE`/`EHOSTUNREACH`/`ENETUNREACH`/`eof`/`dns_resolution`/`tls_handshake` -> `network`; MongoDB server reply -> `server`/`other`; wire-protocol parse errors -> `protocol`/`other`; any error wrapped as a config error (including the admin bind failure, detail `EADDRINUSE`) -> `config`; recovered panic -> `internal`/`other`; OTLP collector rejections (HTTP non-2xx, gRPC non-transport status) -> `protocol`/`other`; anything else -> `internal`/`other`. In particular: `canceled` -> `internal`; collector rejections -> `protocol`/`other`; admin bind failure -> `config` with detail `EADDRINUSE`. Refined after T5 review (decided by Orchestrator under Owner's standing instruction, 2026-10-24): precedence panic > config > server > protocol > errno; a raw `EADDRINUSE` is `network`/`EADDRINUSE`; protocol errors (including T15's collector-rejection wrapper) are recognised via the `ProtocolError()` marker method.
- **D4 (Q4; T7; ADR-0004 Accepted).** Metric label names for the FR-29 error class and detail are `error_class` and `error_detail`. The spec writer is amending AC-8's wording (which referred to a `corral.error.detail` label) to match.
- **D5 (Q5; T3, T4, T8).** A new component `thirdparty` is added to the fixed set. Records from Go's standard `log` package and third-party `slog.Default()` calls are emitted with `component=thirdparty` and a fixed message; their original text is never forwarded. Per spec Amendment 1, these are two bridges with separate limiters; std `log` records arrive at info, `slog.Default()` records keep their caller's level, and both follow the configured level (not the warn-and-above rule for `grpc`/`otel`/`admin_http`). The spec writer is amending FR-7/FR-28. T3 adds `logging.ThirdParty`; T4's logcheck accepts it through `logging.Components()`.
- **D6 (Q6; T2, T17, T18, T20; ADR-0006 Accepted).** `/readyz`: (a) 200 body `ok\n` with `Content-Type: text/plain; charset=utf-8`; (b) 503 body `<reason>\n`, same Content-Type; (c) 503 `starting` until startup completes, registered checks not consulted before then; (d) a reason outside the defined set returns 503 `unknown`; (e) during graceful shutdown, 503 `shutting_down`, from the moment the lifecycle shutting-down record is emitted (spec Amendment 1). The spec writer is adding `unknown` and `shutting_down` to the reason set.
- **D7 (Q7; T8).** One rate limiter per source and level, with level filtering before limiting (a record below the configured level never consumes a slot). The otel logger's logr `Error` calls map to warn.
- **D8 (Q8; T10).** Validation rules for honoured variables that the spec does not spell out, as proposed by the planner: `OTEL_EXPORTER_OTLP_*ENDPOINT` must be an absolute `http`/`https` URL with a host; `*_TIMEOUT` a non-negative integer in milliseconds; `*_HEADERS` a comma-separated list of `key=value` with non-empty keys and valid percent-encoding; `OTEL_RESOURCE_ATTRIBUTES` the same list format; `OTEL_SERVICE_NAME` any non-empty string (empty counts as unset); `OTEL_BSP_MAX_QUEUE_SIZE` and `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` positive integers; `OTEL_BSP_SCHEDULE_DELAY` and `OTEL_BSP_EXPORT_TIMEOUT` positive integers in milliseconds. Refined after T10 review (see T10 "D8 refinements"; decided by Orchestrator under Owner's standing instruction, 2026-10-24): header keys are RFC 7230 tokens and not percent-decoded; per-prefix client cert/key pairing via `tls.X509KeyPair`; TrimSpace before the empty check; unset batch is `min(512, queue)`; queue and batch ≤ `1<<20`; exporter/protocol/sampler case-insensitive, compression exact; PEM files regular, ≤ 4 MiB, opened `O_NONBLOCK`; PKCS#8/PKCS#1/SEC1 keys accepted, encrypted keys rejected.
- **D9 (Q9; T3, T15; ADR-0007).** `trace_id`/`span_id` are present in logs whenever tracing is enabled, including with `OTEL_TRACES_EXPORTER=none`.

## Proposed for Owner (spec amendments, not decisions)

These came out of the T10–T15 reviews. They are not decided and not planned into any task; they need the Owner's approval and a spec amendment before any implementation.

- **P1 (FR-27/FR-28 area; would affect T10, T15, T18).** Emit a warning at startup when OTLP authentication headers would be sent over plaintext, i.e. `*_HEADERS` is set and the effective endpoint is `http://` or `*_INSECURE=true`. The warning names only the variable names, never header keys or values.
- **P2 (FR-30; would affect T11).** Widen FR-30's resource-attribute ban list to also remove `process.executable.path`, `process.environment_variable.*`, `process.*user.name` (for example `process.user.name`, `process.real_user.name`, `process.saved_user.name`), `container.command*`, `user.*`, `enduser.*`, `host.ip` and `host.mac`.
- **P3 (FR-15/FR-17 area, new behaviour; would affect T17, T20).** Add a `Host`-header allowlist on the admin listener's `/metrics` endpoint (and possibly `/healthz` and `/readyz`) to mitigate DNS-rebinding reads from a browser on the same host as a loopback-bound listener. Open points: the allowed values (the bind address, `localhost`, operator-configured names), the response for a disallowed host (for example 421 or 403 with a fixed body), and whether it is configurable. This is new behaviour and needs a spec amendment; it is not in T24.
- **P4 (FR-29 class definitions; would affect T5, T15).** Should a gRPC `codes.Unavailable` export error (transport-level failure to reach the collector) be class `network`, and with which detail (`other`, or a mapped errno when one is available)? FR-29 defines the class enum but no class semantics, and D3 does not cover gRPC transport statuses. Today it is `internal`/`other`. The same question applies to `codes.DeadlineExceeded` (`timeout`/`deadline_exceeded`?) and `codes.Canceled`. This was dropped from T24 pending the Owner.
- **P5 (AC-33 scope; would affect T14).** Should AC-33's `spans` scan source also include span link attributes and instrumentation-scope name, version and attributes? FR-24's "in any signal" arguably covers them, but AC-33 and the data-minimisation heading list only span names, attributes, events, status, and resource attributes. T24.5 makes the data available in the collector; extending the scanner waits for this decision.
