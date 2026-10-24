# SPEC-001: Observability conventions (logging, metrics, tracing)

Status: approved (Owner, 2026-10-24); Amendment 1 approved (Owner, 2026-10-24); Amendment 2 approved (Owner, 2026-10-24)
Owner: Sorin Panduru
Last updated: 2026-10-24 (Amendment 2)

## Goal

Set the logging, metrics, tracing and health conventions that every corral
feature must follow. Operators get consistent, low-cardinality, privacy-safe
telemetry, and the domain layer stays free of observability infrastructure.

## Non-goals

- Defining the specific metrics, log events or spans of individual features
  (pooling, sessions, routing). Those specs add them under these conventions.
- Defining what "ready" means. That belongs to the future pooling spec. This
  spec defines only the readiness endpoint contract and its behaviour when no
  readiness check is registered.
- Defining how configuration is loaded (flags, env vars, file). That belongs to
  a future configuration spec. This spec names logical settings only.
- A latency-overhead target for observability. That moves to a later
  performance spec. The zero-allocation rule (FR-19, AC-22) stays in this
  spec.
- Authentication, authorization or TLS on the admin HTTP listener in v1 (see
  the security note under Non-functional requirements).
- Any wire-dump, message-body or query logging feature, at any log level,
  including debug.
- Changing the log level or format at runtime. Changes need a restart.
- OTel metrics or OTel logs. Metrics use Prometheus only, and logs are emitted
  only through `log/slog`.
- Accepting incoming trace context from clients. The MongoDB wire protocol has
  no standard way to carry it.
- Profiling or debug endpoints (`/debug/pprof/`, `/debug/vars`) on the admin
  listener.
- Asynchronous or non-blocking log output in v1 (see FR-38).
- Forwarding the free text of third-party log messages (see FR-28).
- Prometheus exemplars in v1 (see FR-11).

## Users and context

- **Operators / SREs** run corral in front of MongoDB for PHP-FPM fleets. They
  scrape `/metrics` with Prometheus, probe `/healthz` and `/readyz` from
  orchestrators such as Kubernetes, ship stdout logs to a log pipeline, and can
  turn on tracing to an OTLP collector when diagnosing latency.
- **Contributors / factory agents** building later features need one set of
  rules for adding logs, metrics and spans without breaking the domain boundary
  or the data-minimisation rules.
- **Clients** of corral are application servers (PHP-FPM hosts), not end users.

## Functional requirements

Requirement numbers are stable across revisions. New requirements are added
at the end of the list.

### Architecture boundary

- **FR-1:** A *domain package* is any Go package whose import path is under
  `github.com/sorinpanduru/corral/internal/domain/`. A domain package may
  import only:
  - (a) Go standard-library packages, except `log/slog`;
  - (b) other domain packages;
  - (c) the packages in the domain import allowlist kept in this spec. The
    allowlist is currently empty, and adding to it requires amending this spec.

  The rule applies to the non-test Go files of domain packages. `_test.go`
  files are exempt.
- **FR-2:** The domain reports observable occurrences only through observer
  ports (interfaces) that it defines, in domain terms (for example "pool
  checkout waited", "upstream connection lost"). Infrastructure adapters
  implement these ports and turn the calls into log records, metric updates
  and spans. Every observer-port method takes `context.Context` as its first
  parameter, so that adapters can create child spans and correlate logs with
  the active trace.
- **FR-3:** Every observer port has a no-op implementation. Domain behaviour
  must be the same whether a no-op, a test fake or the real adapter is wired
  in.

### Logging

- **FR-4:** All log output goes through `log/slog` and is written to stdout.
- **FR-5:** Log format is set by the `log.format` setting: `json` (default) or
  `text`.
- **FR-6:** Log level is set by the `log.level` setting: `debug`, `info`
  (default), `warn` or `error`.
  - Matching is case-insensitive, so `DEBUG` and `Info` are accepted.
  - Level offsets such as `debug-4` or `info+2` are rejected.
  - Records below the configured level are not emitted.
  - Format and level are read once at startup.
- **FR-7:** Every log record carries a `component` attribute. Its value comes
  from a fixed set:
  - this spec defines `config`, `lifecycle`, `admin_http`, `tracing`, `otel`,
    `grpc` and `thirdparty`;
  - `lifecycle` is used for the starting, startup-complete and shutting-down
    records;
  - `thirdparty` is used for records routed from Go's standard `log` package
    and from third-party `slog.Default()` calls (FR-28);
  - each feature spec declares the components it adds.

  Loggers are obtained only through a factory that requires a component from
  this set. Records that report an error carry an `error` attribute produced
  as described in FR-29. Records about a configuration setting carry a
  `setting` attribute holding the setting or variable *name* (or, for the
  FR-41 record, the list of names FR-41 defines), never its value.
  Log attribute keys and messages follow FR-39.
- **FR-8:** When tracing is enabled and a record is emitted with a valid span
  context in its `context.Context`, the record carries `trace_id` and
  `span_id`. This applies whether the span is sampled or not. Formats are
  lowercase hex: 32 characters for `trace_id`, 16 for `span_id`. When tracing
  is disabled, these attributes are absent.

### Metrics

- **FR-9:** Metrics are built with `github.com/prometheus/client_golang` and
  exposed at `GET /metrics` on the admin listener (FR-15). The response is in
  Prometheus text exposition format by default; the standard `promhttp`
  content negotiation (e.g. OpenMetrics) is allowed.
- **FR-10:** Naming. Every corral metric name:
  - starts with `corral_` and is snake_case;
  - has a base-unit suffix (`_seconds`, `_bytes`) only when the quantity has a
    unit, and never a non-base suffix (`_ms`, `_milliseconds`, `_kb`, `_mb`,
    and similar);
  - ends in `_total` if it is a counter, and only then;
  - has non-empty help text.

  Label names are snake_case.
- **FR-11:** Labels. Every label has a fixed, enumerated set of values
  declared in the metric catalog (FR-31).
  - Errors are labelled only by error class, using the FR-29 class enum
    values, under the label name `error_class`.
  - These must never be used as metric labels:
    - client network address or port;
    - upstream host or port;
    - connection, session (lsid), request or cursor IDs;
    - client `appName`, driver name or driver version;
    - usernames;
    - database names;
    - collection names;
    - MongoDB error `code` or `codeName`;
    - trace or span IDs;
    - any value in FR-24.
  - Exemplars are not used in v1. No corral metric attaches exemplars.
  - The error detail value (FR-29) is not used as a metric label unless the
    metric's catalog entry (FR-31) declares that label, under the label name
    `error_detail`, with its bounded set of values. The span and log
    attribute key stays `corral.error.detail` (FR-29); `error_class` and
    `error_detail` are metric label names only.
- **FR-12:** Command names. This spec owns one central command-name allowlist.
  Feature specs extend it by amending this spec, so the series cap (FR-13) is
  checked in one place.
  - Matching against the allowlist is case-sensitive.
  - An alias table maps `isMaster` and `ismaster` to `hello`. The alias is
    applied only for allowlist matching.
  - **Metric labels:** the matched allowlist name. Every command that doesn't
    match is the literal value `other`.
  - **Logs and spans:** the command name as received, whether or not it is
    allowlisted, and without alias mapping. Invalid UTF-8 is replaced with
    U+FFFD, and the result is truncated to at most 64 bytes without splitting a
    UTF-8 character.
- **FR-13:** No metric family may have more than 100 series. A *series* is a
  distinct combination of label values, not counting the `le` (histogram) and
  `quantile` (summary) labels.
- **FR-14:** The standard Go runtime (`go_*`) and process (`process_*`)
  collectors are registered and exposed at `/metrics`.

### Admin HTTP listener (metrics and health)

- **FR-15:** corral runs one admin HTTP listener, separate from the MongoDB
  wire-protocol listener. It serves `/metrics`, `/healthz` and `/readyz` from
  a dedicated `http.ServeMux`, never `http.DefaultServeMux`.
  - Only `GET` and `HEAD` are accepted. Any other method gets 405 with an
    `Allow: GET, HEAD` header.
  - Paths match exactly. A trailing slash (`/metrics/`) or any other path,
    including `/debug/pprof/` and `/debug/vars`, returns 404.
  - Query strings are ignored for routing and have no effect on the response.
- **FR-16:** The admin listener's bind address is set by the `admin.listen`
  setting (host:port), default `127.0.0.1:9187`. An empty host (e.g. `:9187`)
  is rejected at startup with a config error (FR-27). Binding to all
  interfaces requires an explicit `0.0.0.0` or `[::]`.
- **FR-17:** `GET /healthz` (liveness) returns 200 while the process is running
  and its admin listener is serving. The body is `ok\n` and the
  `Content-Type` is `text/plain; charset=utf-8`.
- **FR-18:** `GET /readyz` (readiness) returns 200 when corral reports ready
  and 503 when it does not.
  - **Response format:** `Content-Type` is `text/plain; charset=utf-8`. The
    200 body is `ok\n`. A 503 body is `<reason>\n`, where `<reason>` is a
    short, fixed reason string from the defined set, with no FR-24 data and no
    client addresses.
  - **Reason set:** this spec defines `starting`, `shutting_down` and
    `unknown`. Feature specs add readiness reasons by amending this spec, the
    same way as FR-12 and FR-40.
  - Readiness is supplied to the admin listener through a readiness port; its
    meaning is defined by the pooling spec.
  - **Before startup has completed:** `/readyz` returns 503 with reason
    `starting`, without consulting any registered readiness check.
  - **From the start of graceful shutdown:** `/readyz` returns 503 with reason
    `shutting_down`, without consulting any registered readiness check. The
    start of graceful shutdown is the moment corral begins shutdown, when it
    emits the `lifecycle` shutting-down record.
  - **After startup has completed and before graceful shutdown starts:**
    - when no readiness check is registered, `/readyz` returns 200;
    - when a registered readiness check reports ready, `/readyz` returns 200;
    - when it reports not ready with a reason from the defined set, `/readyz`
      returns 503 with that reason;
    - when it reports a reason outside the defined set, `/readyz` returns 503
      with reason `unknown`.
  - *Startup completed* is the moment after every startup step has finished.
    The last step is binding the MongoDB wire listener. Until the wire
    listener exists in the codebase, the last step is binding the admin
    listener and finishing the tracing bootstrap.
  - The `lifecycle` startup-complete record is emitted at that moment.

### Tracing

- **FR-19:** Tracing is set by the `tracing.enabled` setting and is off by
  default.
  - `OTEL_SDK_DISABLED` set to `true` (case-insensitive) turns tracing off
    whatever `tracing.enabled` says. Any other value of `OTEL_SDK_DISABLED`,
    or leaving it unset, counts as false and is never an error.
  - When tracing is off, corral creates no spans, starts no exporter and opens
    no connection to any trace collector.
  - Code on the per-message path checks whether tracing is enabled before
    calling `Tracer.Start`, and does not call it when tracing is disabled.
- **FR-20:** When tracing is on, spans are exported over OTLP.
  - `OTEL_TRACES_EXPORTER` accepts only `otlp` (the default when unset) and
    `none`. With `none`, no exporter or span processor is started. Any other
    value, including `console`, is a config error when tracing is enabled
    (FR-27).
  - corral reads `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL`, falling back to
    `OTEL_EXPORTER_OTLP_PROTOCOL`, to choose the protocol: `http/protobuf`
    (default) or `grpc`.
  - corral honours these variables:
    - endpoint (`OTEL_EXPORTER_OTLP_ENDPOINT` and the `_TRACES_` variant);
    - headers (`OTEL_EXPORTER_OTLP_HEADERS` and the `_TRACES_` variant);
    - timeout (`OTEL_EXPORTER_OTLP_TIMEOUT` and the `_TRACES_` variant);
    - `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`;
    - the batch processor variables `OTEL_BSP_MAX_QUEUE_SIZE`,
      `OTEL_BSP_SCHEDULE_DELAY`, `OTEL_BSP_EXPORT_TIMEOUT` and
      `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`, mapped as in FR-37.
  - Pass-through variables: `OTEL_EXPORTER_OTLP_CERTIFICATE`,
    `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_KEY`,
    `OTEL_EXPORTER_OTLP_COMPRESSION`, `OTEL_EXPORTER_OTLP_INSECURE`, and their
    `_TRACES_` variants.
    - These go to the Go OTLP exporters with standard SDK semantics, except
      that when the effective traces endpoint uses the `http://` scheme, the
      exporter's effective transport uses no TLS: corral overrides any TLS
      configuration the SDK derives from the certificate and key variables
      and from `OTEL_EXPORTER_OTLP_INSECURE` /
      `OTEL_EXPORTER_OTLP_TRACES_INSECURE` (FR-41).
    - To satisfy FR-27, corral first validates them when tracing is on:
      certificate and key paths must be readable and parse as PEM,
      compression must be `gzip` or `none`, and insecure must be `true` or
      `false` (case-insensitive). This validation applies whatever the
      endpoint scheme (FR-41).
    - File contents are never logged (FR-24).
  - Other `OTEL_*` variables are not validated by corral, and the SDK may still
    apply its own defaults. corral does not set explicit span limits. The OTel
    Go SDK's own handling of `OTEL_ATTRIBUTE_*`, `OTEL_SPAN_*` and `OTEL_GO_X_*`
    applies. Whatever those variables cause, FR-23, FR-24 and FR-30 still
    hold: corral sets only allowlisted attributes, and resource keys outside
    FR-30 are removed. These variables are not subject to the fatal-on-invalid
    rule in FR-27.
- **FR-21:** Sampling.
  - The default sampler is parent-based trace-ID-ratio. Its ratio comes from
    `tracing.sample_ratio`, in the range 0.0 to 1.0, default 1.0.
  - `OTEL_TRACES_SAMPLER`, when set, replaces the default sampler. Accepted
    values: `always_on`, `always_off`, `traceidratio`,
    `parentbased_always_on`, `parentbased_always_off`,
    `parentbased_traceidratio`. Any other value is a config error when tracing
    is on.
  - `OTEL_TRACES_SAMPLER_ARG`, when set, replaces `tracing.sample_ratio` as the
    ratio. It must be a number from 0.0 to 1.0 when the effective sampler uses
    a ratio, and is ignored otherwise. When it is unset, `tracing.sample_ratio`
    is the ratio.
  - Child spans inherit their parent's sampling decision.
- **FR-22:** Span model. Each client request/response exchange that corral
  proxies produces one root span, except the exchanges FR-32 excludes. For an
  ordinary request/reply exchange, that root span has these child spans:
  - a checkout span covering the time spent waiting to check out an upstream
    connection from the pool (only when a checkout happens);
  - a round-trip span covering the upstream round-trip to MongoDB.

  No spans cover the lifetime of a client connection, a session or an upstream
  connection. Other exchange types follow FR-32.
- **FR-23:** Span names, kinds and attributes follow OpenTelemetry semantic
  conventions v1.33.0 (database spans).
  - *Verified 2026-10-24 by the reviewer against the OTel Go v1.47.0 source:*
    `db.system.name`, `db.namespace`, `db.collection.name`,
    `db.operation.name` and `db.response.status_code` (a string) are Stable in
    this version.
  - Attributes are an exact allowlist; no attribute outside it may be set.

  | Span | Kind | Name |
  |---|---|---|
  | root | `SERVER` | `{db.operation.name} {db.collection.name}`, or `{db.operation.name}` when there is no collection |
  | round-trip | `CLIENT` | same format as root |
  | checkout | `INTERNAL` | `pool.checkout` |

  Required attributes:

  | Attribute | Value | Required on |
  |---|---|---|
  | `db.system.name` | `mongodb` | root, round-trip |
  | `db.operation.name` | command name per FR-12 | root, round-trip |
  | `db.namespace` | database name (from `$db`) | root, round-trip, when known |
  | `db.collection.name` | collection name, extracted only per FR-40 | root, round-trip, when FR-40 yields one |
  | `client.address`, `client.port` | client network address | root |
  | `error.type` | error class (FR-29) | any span that ends in error |
  | `corral.error.detail` | error detail (FR-29) | any span that ends in error |
  | `db.response.status_code` | MongoDB server error `code`, as a string | root, round-trip, on server errors |
  | `corral.db.response.code_name` | MongoDB server error `codeName` | root, round-trip, on server errors |

  Optional attributes (may be set; never required):

  | Attribute | Value | Allowed on |
  |---|---|---|
  | `server.address`, `server.port` | upstream MongoDB host and port | round-trip, checkout |
  | `corral.connection.id` | corral's ID for the client connection | root |
  | `corral.upstream.connection.id` | ID of the upstream connection used | round-trip, checkout |
  | `corral.request.id` | wire-protocol `requestID` | root, round-trip |
  | `corral.session.id` | the `id` UUID of the logical session (`lsid`), canonical string form | root, round-trip |
  | `corral.cursor.id` | cursor ID, decimal | root, round-trip |
  | `corral.client.app_name` | client `appName` per FR-25 | root |
  | `corral.client.driver.name`, `corral.client.driver.version` | driver name and version from the client handshake, each per FR-25 | root |

  - `db.query.text` (and any query text or summary) is never set.
- **FR-32:** Span shapes for other exchange types. These rules apply if and
  when another spec makes corral handle the case. This spec does not decide
  whether corral retries, answers locally or pins connections.
  - **Client `OP_MSG` with `moreToCome` (no reply expected):** one root span
    and one round-trip span. Both end when the message has been written
    upstream.
  - **Exhaust cursor (one request, many replies):** one root span and one
    round-trip span. Both end at the final reply (`moreToCome` not set) or
    when the stream ends. There are no per-reply spans.
  - **Command answered locally by corral (no upstream round-trip):** a root
    span with no children.
  - **Retries (several upstream round-trips for one client request):** one
    round-trip child per attempt and one checkout child per checkout, all
    under the same root.
  - **No checkout because a connection is already pinned (cursor or
    transaction):** no checkout child.
  - **Exclusions (no spans; logs allowed under FR-24/25 as below):** these
    exchanges produce no spans, whether they come from clients or from
    corral's own upstream connections:
    - heartbeats and `hello` (including `isMaster` / `ismaster`);
    - connection handshakes;
    - the authentication commands `saslStart`, `saslContinue`,
      `authenticate`, `getnonce` and `logout`.

    They are observed through metrics, which the feature specs define, and
    may produce log records:
    - **Auth failure:** logged at warn with the error class, error detail and,
      when present, `code` / `codeName`. The record never contains the
      username or any authentication payload.
    - **Handshakes, `hello` and heartbeats:** per-exchange records are allowed
      at debug level only.
    - **Auth success:** records are allowed at debug level only, as for
      handshakes and `hello`.
    - **All of the above:** subject to FR-24 and FR-25.

### Data minimisation (applies to logs, metric labels, span names, span attributes, span events, span status, resource attributes and HTTP response bodies)

- **FR-24:** These must never be emitted, at any level, in any signal, except
  fields explicitly allowed by FR-25. Where an FR-25 field would contain a
  username or credential (for example the first value of a user-management
  command), FR-24 takes precedence over FR-25.
  - passwords, keys, tokens or other credentials;
  - authentication payloads of any mechanism (SASL / SCRAM conversation,
    `saslStart` / `saslContinue` / `authenticate` arguments, X.509 material);
  - X.509 client-certificate subject DN and subject alternative names;
  - MongoDB usernames and role names from user- and role-management commands,
    and the `uid` field of a logical session ID (which is derived from the
    username);
  - document contents, query filters, projections, sort specs, update
    documents, aggregation pipelines, the `comment` field, or any other
    command argument values;
  - client handshake metadata, apart from `appName`, driver name and driver
    version (FR-25). This includes OS, platform and environment fields;
  - verbatim MongoDB server error messages (`errmsg`), which can contain
    document values such as duplicate-key values;
  - raw wire-protocol bytes, in any encoding;
  - connection strings and URLs that contain userinfo, including OTLP endpoint
    URLs. MongoDB connection strings are never emitted, with or without
    credentials;
  - the values of `OTEL_EXPORTER_OTLP_HEADERS` and
    `OTEL_EXPORTER_OTLP_TRACES_HEADERS`;
  - TLS private key material, and the contents of certificate and key files;
  - recovered panic values;
  - free text from third-party log sources (FR-28).
- **FR-25:** Only these fields from MongoDB traffic, clients and upstreams are
  allowed (an allowlist), always subject to FR-24's precedence:
  - command name (per FR-12): logs, spans, and metric labels (metric labels
    only as the allowlist value or `other`);
  - database name: logs and spans;
  - collection name, only as extracted per FR-40: logs and spans;
  - client network address (host and port): logs and spans;
  - upstream MongoDB host and port: logs and spans;
  - connection IDs (client and upstream) and wire-protocol request IDs: logs
    and spans;
  - logical session `id` (UUID only) and cursor IDs: logs and spans;
  - client `appName`: logs and spans. Invalid UTF-8 is replaced with U+FFFD,
    and the value is truncated to at most 128 bytes without splitting a UTF-8
    character;
  - driver name and driver version from the client handshake: logs and spans.
    Each is handled separately: invalid UTF-8 is replaced with U+FFFD, then
    the value is truncated to at most 64 bytes without splitting a UTF-8
    character;
  - MongoDB error `code` / `codeName`: logs and spans;
  - durations, byte sizes and counts: all signals.

  None of these except the command name may be used as a metric label
  (FR-11).
- **FR-26:** Error values passed into logs or spans must not carry FR-24 data.
  All errors are reported through the classification and sanitiser in FR-29.

### Configuration

- **FR-27:** The logical settings defined by this spec are listed under
  Interfaces.
  - An invalid value for any of them stops startup with a non-zero exit. The
    error record has `component=config`, a `setting` attribute naming the
    setting, and does not echo the supplied value. This includes
    `admin.listen` with an empty host (FR-16).
  - When tracing is enabled, an invalid value in any `OTEL_*` variable corral
    honours or validates (FR-20, FR-21, FR-30, FR-37) is also fatal at
    startup, under the same `setting` and no-echo rules. `OTEL_SDK_DISABLED`
    is never invalid (FR-19).
  - When tracing is off, including when `OTEL_SDK_DISABLED` turns it off,
    `OTEL_*` variables other than `OTEL_SDK_DISABLED` are ignored and not
    validated.
  - Errors raised before the logger is configured are written to stdout as
    JSON (the default format), with `component=config`.

### Added in later revisions

- **FR-28:** Third-party and global log output is routed through slog to
  stdout, so that corral writes no log output to stderr.
  - **Routing:**
    - `slog.SetDefault` is set to the configured handler;
    - records from Go's standard `log` package and from third-party
      `slog.Default()` calls are routed to a logger with
      `component=thirdparty`. Each such record carries a fixed, constant
      message and never the third-party text;
    - `otel.SetErrorHandler` and `otel.SetLogger` route to a logger with
      `component=otel`;
    - `grpclog.SetLoggerV2` routes to a logger with `component=grpc`;
    - the admin `http.Server.ErrorLog` is built with `slog.NewLogLogger(...)`
      and `component=admin_http`.
  - **Content:** forwarded records never contain the third-party message text
    or its key/value arguments. Each source emits a fixed, constant message
    (one per source and level). Where an error value is available, the record
    also carries the FR-29 sanitised `error` class.
  - **Levels:**
    - grpclog info maps to slog debug, warning to warn, and error and fatal to
      error. For grpclog fatal, the error record is written and flushed to
      stdout before the process exits. This exemption from the rate limit
      below applies to fatal records only;
    - the otel logger's verbosity levels map to debug;
    - the otel error handler maps to warn;
    - the admin `http.Server` error log maps to warn;
    - `thirdparty` records from `slog.Default()` calls keep the level the
      caller gave; records from Go's standard `log` package, which has no
      levels, are info.

    At the default `info` level, only warn and above from the grpc, otel and
    admin_http sources are emitted; `thirdparty` records follow the
    configured level.
  - **Bounding:**
    - all records from the otel error handler and the otel logger go through
      the FR-33 limiter;
    - grpclog records are limited the same way: at most one record per 30
      seconds per level, with a `suppressed_count` of records dropped since the
      last one;
    - admin `http.Server` error-log records are limited the same way;
    - `thirdparty` records are limited the same way, with one limiter per
      source (the standard `log` bridge and the `slog.Default()` bridge) and
      level: at most one record per 30 seconds per source and level, with a
      `suppressed_count` of records dropped since the last one.
  - Crash output from the Go runtime (unrecovered panics, fatal errors) is
    exempt.
- **FR-29:** Error classification and sanitisation.
  - Every error reported in any signal is classified into exactly one value of
    the fixed error-class enum: `timeout`, `network`, `protocol`, `server`,
    `config`, `internal`. Changing the enum requires amending this spec.
  - **Error detail.** Every error also gets exactly one value from the fixed
    detail enum. The key is `corral.error.detail` in both spans and logs.
    Changing the enum requires amending this spec.

    | Detail | Meaning |
    |---|---|
    | `ECONNREFUSED` | connection refused |
    | `ECONNRESET` | connection reset by peer |
    | `ECONNABORTED` | connection aborted |
    | `ETIMEDOUT` | OS-level connection or I/O timeout |
    | `EPIPE` | write to a closed connection |
    | `EHOSTUNREACH` | host unreachable |
    | `ENETUNREACH` | network unreachable |
    | `EADDRINUSE` | address already in use (e.g. admin listener bind) |
    | `deadline_exceeded` | a context or operation deadline expired |
    | `canceled` | the operation was canceled |
    | `eof` | the peer closed the connection: `io.EOF` or `io.ErrUnexpectedEOF` |
    | `tls_handshake` | TLS handshake failed |
    | `dns_resolution` | host name resolution failed |
    | `other` | anything not mapped above, including MongoDB server errors (which use `code` / `codeName`) |
  - This covers server replies, network errors, exporter errors, config errors
    and recovered panics (which are `internal`).
  - **Spans:** a failed span has status Error with an empty description,
    `error.type` set to the class, and `corral.error.detail` set to the
    detail. Server errors also get
    `db.response.status_code` and `corral.db.response.code_name` (FR-23).
    `span.RecordError` is not called, and no `exception` event or
    `exception.*` attribute is recorded on any corral span.
  - **Logs:** the `error` attribute holds the class value produced by a
    sanitiser, never the result of `err.Error()` and never a panic value.
    Every error record also carries `corral.error.detail`. Server errors may
    also carry `db.response.status_code` and
    `corral.db.response.code_name`.
  - **Metrics:** errors are labelled by class, under the label name
    `error_class`. The detail may be used as a label, under the label name
    `error_detail`, only when the catalog declares it (FR-11).
  - **Recovered panics:** the record is logged at error level and may include
    the goroutine stack trace in a `stack` attribute. The panic value is never
    emitted.
- **FR-30:** Resource attributes on exported spans are an allowlist:
  - `service.*`, with `service.name` defaulting to `corral`;
  - `telemetry.sdk.*`;
  - `host.name`;
  - attributes the operator supplies via `OTEL_RESOURCE_ATTRIBUTES` and
    `OTEL_SERVICE_NAME`.

  `process.command_args`, `process.command_line` and `process.owner` are never
  emitted, even when the operator supplies them in `OTEL_RESOURCE_ATTRIBUTES`;
  they are removed. Resource detectors outside the allowlist are not enabled.
- **FR-31:** Every corral metric is declared in one project metric catalog, a
  machine-readable file that lists for each metric its name, type, help text,
  label names, and the allowed values of each label.
  - `corral_` metrics are created only through a constructor backed by the
    catalog, so a metric that is not in the catalog cannot be created.
  - Feature specs add their metrics to the catalog. This spec contributes the
    metric in FR-37.
- **FR-33:** OTLP export failures, and every other record from the otel error
  handler or otel logger, are rate-limited:
  - export failures are logged as warnings with `component=otel`;
  - at most one record per 30 seconds is emitted, and each carries a
    `suppressed_count` of records dropped since the previous one;
  - the error content follows FR-29.
- **FR-34:** On graceful shutdown with tracing on, pending spans are flushed
  for at most 5 seconds; spans not exported by then are dropped.
- **FR-35:** If the admin listener cannot bind at startup, startup fails with a
  non-zero exit. The error record has `component=config` and
  `setting=admin.listen`, and does not contain the address value.
- **FR-36:** The operator documentation states that:
  - the admin listener has no authentication or TLS, and binding it to a
    non-loopback address exposes metrics and health status to anyone who can
    reach it;
  - Kubernetes deployments must set `admin.listen` to a non-loopback address
    (an explicit `0.0.0.0`, `[::]`, or a pod IP) so that probes and scrapes can
    reach it;
  - a blocked stdout can stall the proxy (FR-38).
- **FR-37:** Span export backpressure. corral uses its own bounded-queue span
  processor, not the stock SDK `BatchSpanProcessor`, so that it can count
  drops.
  - Ending a sampled span enqueues it. When the queue is full, the span is
    dropped and `corral_tracing_spans_dropped_total` is incremented.
  - That counter has no labels, is declared in the metric catalog (FR-31),
    and has help text "Spans dropped because the trace export queue was full."
    It counts only queue-full drops. Spans lost to failed exports or to the
    shutdown flush deadline (FR-34) are not counted by it.
  - Span start and end never block on the exporter.
  - Variable mapping (defaults are the OpenTelemetry specification defaults;
    invalid values follow FR-27):

    | Variable | Meaning in corral's processor | Default |
    |---|---|---|
    | `OTEL_BSP_MAX_QUEUE_SIZE` | queue capacity, in spans | 2048 |
    | `OTEL_BSP_SCHEDULE_DELAY` | maximum time between export attempts while spans are queued, in milliseconds | 5000 |
    | `OTEL_BSP_EXPORT_TIMEOUT` | maximum duration of one export call, in milliseconds | 30000 |
    | `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | maximum spans per export call; an export also starts as soon as this many spans are queued; must not exceed the queue capacity | 512 |
- **FR-38:** Log records are written synchronously to stdout. If stdout
  blocks (for example a stalled log pipeline or a full pipe), logging calls
  block, and this can stall proxied operations. This risk is accepted for v1.
- **FR-39:** Log attribute keys and messages.
  - The keys a log record may carry are an allowlist:
    - `time`, `level`, `msg`, `component`, `error`, `trace_id`, `span_id`,
      `stack`, `setting`;
    - the FR-23 attribute keys (required and optional), which include
      `corral.error.detail`;
    - numeric duration, size and count keys: snake_case names ending in
      `_seconds`, `_bytes` or `_count` (e.g. `suppressed_count`), whose value
      is a number.

    No other keys and no slog groups are allowed. Feature specs extend this
    allowlist by amending this spec, the same way as FR-12 and FR-40.
  - `msg` is a compile-time constant string. No runtime value is ever
    formatted into it; runtime values go only in allowed attributes.
  - The `component` key's allowed values are the FR-7 component set,
    including `thirdparty`.
- **FR-40:** Collection-name extraction.
  - The collection name is taken only from a closed list of commands, and only
    when the value is a BSON string:
    - commands whose first element value is the collection name: `find`,
      `insert`, `update`, `delete`, `aggregate` (string form only; the numeric
      form `aggregate: 1` has no collection), `count`, `distinct`,
      `findAndModify`, `create`, `drop`, `createIndexes`, `dropIndexes`,
      `listIndexes`, `collMod`, `killCursors`;
    - `getMore`, where it comes from the `collection` field.
  - Invalid UTF-8 is replaced with U+FFFD.
  - Changing the list requires amending this spec.
  - For any other command, no collection name is recorded. In particular it is
    never extracted from user-, role-management or auth commands (`createUser`,
    `updateUser`, `dropUser`, `dropAllUsersFromDatabase`, `grantRolesToUser`,
    `revokeRolesFromUser`, `usersInfo`, `createRole`, `updateRole`,
    `dropRole`, `dropAllRolesFromDatabase`, `grantPrivilegesToRole`,
    `revokePrivilegesFromRole`, `grantRolesToRole`, `revokeRolesFromRole`,
    `rolesInfo`, `authenticate`, `saslStart`, `saslContinue`, `getnonce`,
    `logout`).
- **FR-41:** TLS settings with a plaintext (`http://`) OTLP endpoint. This
  applies when tracing is on and an OTLP exporter is started (FR-20), for
  both the `http/protobuf` and the `grpc` protocol.
  - In FR-41, a variable is *set* when its value is non-empty after removing
    leading and trailing whitespace (the same rule the SDK applies). An
    endpoint variable that is not set is treated as absent.
  - The *effective traces endpoint* is `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`
    when set, otherwise `OTEL_EXPORTER_OTLP_ENDPOINT` when set. Its scheme is
    compared after trimming leading and trailing whitespace, and
    case-insensitively.
  - The *TLS variables* are `OTEL_EXPORTER_OTLP_CERTIFICATE`,
    `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`, `OTEL_EXPORTER_OTLP_CLIENT_KEY`,
    `OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE`,
    `OTEL_EXPORTER_OTLP_TRACES_CLIENT_CERTIFICATE` and
    `OTEL_EXPORTER_OTLP_TRACES_CLIENT_KEY`.
  - When the effective traces endpoint uses the `http://` scheme:
    - the exporter's effective transport uses no TLS: no CA pool, no client
      certificate and no TLS credentials. corral overrides any TLS
      configuration the SDK derives from the TLS variables and from
      `OTEL_EXPORTER_OTLP_INSECURE` / `OTEL_EXPORTER_OTLP_TRACES_INSECURE`
      (including `false`);
    - this is not a config error: startup continues normally and is not
      fatal;
    - FR-27 validation of the TLS variables still applies (FR-20): a set TLS
      variable whose file is unreadable or not valid PEM is fatal at startup.
      In that case startup fails with the FR-27 error and no FR-41 record is
      emitted.
  - If, in that case, at least one TLS variable is set and startup is not
    stopped by FR-27, corral emits exactly one FR-41 record during startup,
    subject to the configured log level (FR-6), before the `lifecycle`
    startup-complete record:
    - level warn, `component=config`;
    - the fixed message
      `OTLP TLS settings ignored because the traces endpoint uses http://`;
    - a `setting` attribute holding the names of the TLS variables that are
      set, sorted in ascending byte order and joined with `,` (no spaces);
    - no other attributes than `time`, `level`, `msg`, `component` and
      `setting`.

    The record never contains the variable values. Those values are file
    paths, and FR-7 and FR-27 allow only setting names, never values, in
    configuration records. It never contains file contents (FR-24) or the
    endpoint URL.
  - If no TLS variable is set, no FR-41 record is emitted.
  - When the effective traces endpoint does not use the `http://` scheme
    (including `https://`), or neither endpoint variable is set (SDK default
    endpoint), FR-20 applies unchanged and no FR-41 record is emitted.
  - When tracing is off, or `OTEL_TRACES_EXPORTER=none`, no FR-41 record is
    emitted.
  - Out of scope for FR-41, and left to SDK semantics: TLS variables combined
    with `OTEL_EXPORTER_OTLP_INSECURE=true` (or the `_TRACES_` variant) on an
    `https://` or default endpoint, and TLS variables with a gRPC `unix://`
    endpoint (see the Amendment 2 decisions-log row).

(FR-32 is listed under Tracing, next to FR-22.)

## Interfaces

### Logical settings (how they are loaded is deferred to the configuration spec)

| Setting | Type / values | Default |
|---|---|---|
| `log.format` | `json` \| `text` | `json` |
| `log.level` | `debug` \| `info` \| `warn` \| `error` (case-insensitive, no offsets) | `info` |
| `admin.listen` | host:port, non-empty host (`0.0.0.0` / `[::]` for all interfaces) | `127.0.0.1:9187` |
| `tracing.enabled` | bool | `false` (forced off by `OTEL_SDK_DISABLED=true`, case-insensitive) |
| `tracing.sample_ratio` | float, 0.0 to 1.0 | `1.0` (replaced by `OTEL_TRACES_SAMPLER_ARG` when set) |

The `OTEL_*` variables honoured, validated or passed through when tracing is
on are listed in FR-20, FR-21, FR-30 and FR-37. Invalid values are fatal only
when tracing is enabled (FR-27).

### Admin HTTP endpoints (GET and HEAD only; other methods 405)

| Path | Success | Failure | Body |
|---|---|---|---|
| `/metrics` | 200, Prometheus text by default (FR-9) | n/a | metrics |
| `/healthz` | 200, `text/plain; charset=utf-8` | n/a (no response if the process is down) | `ok\n` |
| `/readyz` | 200, `text/plain; charset=utf-8` | 503, `text/plain; charset=utf-8` | `ok\n` on 200; `<reason>\n` on 503, reason from the FR-18 set (`starting`, `shutting_down`, `unknown`, plus reasons added by amending this spec) |
| any other path, including trailing-slash variants | n/a | 404 | n/a |

### Observer and readiness ports

- Observer ports: defined by each domain area in domain terms. Each method
  takes `context.Context` first, and each port has a no-op implementation
  (FR-2, FR-3). Method sets are defined by the feature specs that introduce
  them.
- Readiness port: answers "ready / not ready + fixed reason". It is implemented
  by the component the pooling spec makes responsible, and consumed by the
  admin listener. A reason outside the FR-18 set is reported on `/readyz` as
  `unknown`. The port is not consulted before startup has completed or after
  graceful shutdown has started (FR-18).

### Metrics defined by this spec

| Name | Type | Labels | Help |
|---|---|---|---|
| `corral_tracing_spans_dropped_total` | counter | none | Spans dropped because the trace export queue was full. |

Metric label names reserved by this spec for errors: `error_class` (FR-29
class enum) and `error_detail` (FR-29 detail enum, only where the catalog
declares it).

### Project artifacts owned by this spec

- Domain import allowlist (FR-1), currently empty.
- Logger component set (FR-7).
- Log attribute-key allowlist (FR-39).
- Central command-name allowlist and alias table (FR-12).
- Collection-name command list (FR-40).
- Error-class enum (FR-29).
- `/readyz` reason set (FR-18).
- Metric catalog format, file and catalog-backed constructor (FR-31).
- Bounded-queue span processor (FR-37).
- Marker-scan test harness (AC-33).

## Behavior on failure

- **Invalid observability configuration:** startup fails (FR-27). This covers
  an empty host in `admin.listen`, an unsupported `OTEL_TRACES_EXPORTER` or
  `OTEL_TRACES_SAMPLER`, and any invalid honoured or validated `OTEL_*` value
  when tracing is enabled.
- **TLS variables set with an `http://` OTLP traces endpoint:** not a config
  error. The connection is plaintext, the TLS material is not used, and one
  warn record names the ignored variables (FR-41). Unreadable or non-PEM TLS
  files are still fatal (FR-27).
- **Admin listener cannot bind at startup:** startup fails (FR-35).
- **OTLP collector unreachable or slow:** the export queue fills, and further
  spans are dropped and counted in `corral_tracing_spans_dropped_total`.
  Proxied operations are not blocked by span export (FR-37). Failures are
  logged at most once per 30 seconds with a suppressed count (FR-33).
- **Noisy third-party logging (gRPC connectivity, OTel internals, HTTP server
  errors, Go `log` package and third-party `slog.Default()` output):** fixed
  messages, never the third-party text; level-mapped and rate-limited where
  FR-28 says so (FR-28).
- **Readiness check reports an undefined reason:** `/readyz` returns 503
  `unknown` (FR-18).
- **Graceful shutdown started:** `/readyz` returns 503 `shutting_down`
  (FR-18).
- **Blocked stdout:** logging calls block, and proxied operations can stall.
  This is accepted for v1 (FR-38).
- **Graceful shutdown with tracing on:** pending spans are flushed for at most
  5 seconds (FR-34).
- **Metric cardinality:** any command name outside the allowlist goes to
  `other` (FR-12). No metric outside the catalog can be created (FR-31).
- **Recovered panics:** reported at error level as class `internal`, with an
  optional stack trace and without the panic value (FR-29).

## Non-functional requirements

- **Performance:** with tracing disabled, the per-message tracing calls
  allocate nothing (AC-22), and `Tracer.Start` is not called (FR-19). Span
  export never blocks proxied operations (FR-37). A latency-overhead target is
  out of scope here and moves to a later performance spec (Non-goals).
- **Availability:** synchronous stdout logging can stall the proxy if stdout
  blocks. This is an accepted v1 risk, and it is documented (FR-36, FR-38).
- **Security:** the admin listener has no authentication and no TLS in v1. It
  defaults to loopback, and binding to all interfaces requires an explicit
  address (FR-16). It uses a dedicated mux, so no profiling or expvar handlers
  are exposed by accident (FR-15). Metric labels exclude addresses, IDs, client
  metadata and database or collection names (FR-11), which limits what
  `/metrics` reveals. The documentation requirements are in FR-36.
- **Observability of observability:** the rate-limited export-failure warnings
  (FR-33) and the dropped-spans counter (FR-37).

## Data and compliance

- **Data touched and its classification:**
  - operational metadata: command names, database names, collection names
    (closed extraction list), error codes, upstream host and port, connection,
    request, session and cursor IDs, durations, sizes and counts. Internal,
    non-personal.
  - client network addresses of application servers, client `appName` (capped
    at 128 bytes) and driver name and version (each capped at 64 bytes).
    Internal, operational.
  - MongoDB message contents, credentials, usernames, role names and session
    `uid`s, other handshake metadata (OS, platform, environment), certificate
    identities and third-party log text pass through corral but are never
    emitted (FR-24, FR-28).
- **PII:** none. The Owner confirmed that clients are servers, not end users, so
  client addresses identify infrastructure rather than people. The Owner also
  confirmed that database and collection names are not user data, and allowed
  `appName`, driver metadata, upstream address and IDs in logs and spans.
  - Collection names are extracted only from a closed command list, never from
    user- or role-management commands, whose first value is a username or role
    name (FR-40).
  - FR-24 bans take precedence over FR-25.
  - `appName` is set by the application operator, and is capped to limit
    exposure.
  - The session `uid`, which is derived from the username, is excluded.
  - No wire-dump capability exists (Non-goals).
- **Retention:** corral keeps no telemetry itself. Logs go to stdout, metrics
  are scraped, and spans are exported. Retention is set by the operator's
  downstream systems.

## Acceptance criteria

AC numbers are stable across revisions. AC-20 has been merged into AC-8.

SPEC-001 is approved and accepted on its Part A criteria. Feature specs
inherit the Part B convention checks: each feature spec's acceptance criteria
must re-run them against the logs, metrics, spans and ports it introduces. In
particular, each feature spec must include AC-18-style marker tests (using the
AC-33 harness) for every new log event, metric and span.

### Part A: what SPEC-001 itself builds and verifies (acceptance gate for SPEC-001)

No proxied traffic exists when SPEC-001 is built. Part A tracing criteria are
therefore in-process integration tests of the tracing bootstrap package:

- they bootstrap tracing from the given settings and environment;
- they create test spans through the provider the bootstrap configured;
- they export to an in-process test collector.

Test spans exist only in tests. They are exempt from the FR-23 naming and
attribute rules but are subject to FR-24.

- **AC-1** (FR-1): A unit test runs `go list -deps -json ./internal/domain/...`
  and checks the `Imports` field (non-test files) of every domain package
  against the FR-1 allowlist (stdlib except `log/slog`, domain packages, and
  the explicit list). It fails on any other import. `TestImports` and
  `XTestImports` are not checked. A self-test runs the same check on a
  deliberately violating fixture package in `testdata` and expects a failure.
- **AC-3** (FR-4, FR-5, FR-7, FR-28):
  - Unit: with `log.format=json`, every line written is valid JSON with
    `time`, `level`, `msg` and `component`. With `text`, lines are in slog
    text format.
  - Functional (binary): run corral with default settings plus
    `tracing.enabled=true` and an unreachable collector, send admin requests,
    then shut down gracefully. Every stdout line parses as JSON. Records with
    `component=lifecycle` mark starting, startup-complete and shutting-down.
    Nothing is written to stderr.
  - Integration (tracing bootstrap): with an unreachable collector, test spans
    are created and flushed. Captured stdout lines all parse as JSON, and
    nothing is written to stderr.
- **AC-4** (FR-6): Unit: at each level, records below it are not emitted and
  records at or above it are. The default is `info`.
- **AC-5** (FR-7, FR-39):
  - Loggers can be built only through the factory, which accepts every
    component in the FR-7 set (including `thirdparty`) and rejects components
    outside it (unit).
  - Every test suite wires in a test slog handler that fails the test on any
    record that:
    - has no `component`, or a `component` outside the set;
    - has an attribute key outside the FR-39 allowlist, or a slog group;
    - has a `_seconds`, `_bytes` or `_count` key whose value is not numeric.
  - A static check in the same suite fails if any slog call's message argument
    is not a compile-time constant.
- **AC-6** (FR-8): Integration: with tracing on, records emitted inside a
  sampled test span and inside an unsampled test span both carry `trace_id`
  (32 lowercase hex) and `span_id` (16 lowercase hex) matching the span
  context. With tracing off, neither attribute appears.
- **AC-7** (FR-9, FR-14, FR-15): Functional against the admin listener:
  - `GET /metrics` returns 200 in Prometheus text format and includes `go_`
    and `process_` metrics and `corral_tracing_spans_dropped_total`;
  - `HEAD` on each served path succeeds;
  - `POST`, `PUT` and `DELETE` return 405;
  - `/metrics/`, `/debug/pprof/`, `/debug/vars` and an unknown path return
    404;
  - `/healthz?x=1` behaves like `/healthz`.
- **AC-8** (FR-10, FR-11, FR-25, FR-29, FR-31; includes former AC-20): A unit
  lint test fails when:
  - a catalog entry breaks the naming rules (including non-base unit suffixes
    and `_total` on non-counters), lacks help text, or has a non-snake_case
    label name;
  - a catalog entry uses a forbidden label: addresses, IDs, `appName`, driver
    fields, usernames, database or collection names, error `code` or
    `codeName`;
  - an `error_class` label has values outside the FR-29 class enum;
  - a declared `error_detail` label has values outside the FR-29 detail enum;
  - a catalog entry labels errors under any label name other than
    `error_class` (class) or `error_detail` (detail), including
    `corral.error.detail`.

  A unit test shows that the catalog-backed constructor refuses a metric name
  that is not in the catalog. The live registry contains no `corral_` family
  that is missing from the catalog. `client_golang`'s `testutil/promlint` runs
  over the `corral_` families only, and the test fails on any problem it
  reports.
- **AC-11** (FR-16): Unit/functional:
  - with no `admin.listen` set, the listener is bound to `127.0.0.1:9187`
    only;
  - with a configured address, it is bound there;
  - `0.0.0.0:<port>` binds all IPv4 interfaces.
- **AC-12** (FR-17): Functional: `GET /healthz` returns 200, body `ok\n`,
  `Content-Type: text/plain; charset=utf-8`.
- **AC-13** (FR-18): Unit, after startup has completed, with a readiness-port
  fake:
  - reporting ready, `/readyz` returns 200 with body `ok\n`;
  - reporting not ready with a reason from the defined set, it returns 503
    with body `<reason>\n`;
  - reporting a reason outside the defined set (seeded with a marker), it
    returns 503 with body `unknown\n`, and the marker does not appear in the
    body;
  - every response has `Content-Type: text/plain; charset=utf-8`.
- **AC-14** (FR-19, FR-20): Integration: in each of these setups, test spans
  are created, a flush and shutdown are forced, the provider records no spans
  (except in the `none` case), and a test collector listener pointed to by
  `OTEL_EXPORTER_OTLP_ENDPOINT` accepts zero TCP connections:
  - default settings;
  - `tracing.enabled=true` with `OTEL_SDK_DISABLED=TRUE`;
  - `tracing.enabled=true` with `OTEL_TRACES_EXPORTER=none`.

  Unit: `OTEL_SDK_DISABLED=yes` counts as false and causes no error.
- **AC-15** (FR-20): Integration, with a short `OTEL_BSP_SCHEDULE_DELAY` or a
  forced flush:
  - with tracing on and no protocol or exporter set, test spans arrive at the
    test collector over OTLP HTTP/protobuf;
  - with `OTEL_EXPORTER_OTLP_PROTOCOL=grpc`, they arrive over gRPC;
  - `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` takes precedence over
    `OTEL_EXPORTER_OTLP_PROTOCOL`;
  - `OTEL_TRACES_EXPORTER=otlp` behaves like unset;
  - the endpoint from `OTEL_*` is honoured;
  - `OTEL_EXPORTER_OTLP_COMPRESSION=gzip` results in gzip-encoded requests.
- **AC-16** (FR-21): Unit:
  - with ratio 1.0 all root spans are sampled, and with 0.0 none are;
  - with `OTEL_TRACES_SAMPLER=always_on` and `tracing.sample_ratio=0.0`, all
    spans are sampled;
  - with `OTEL_TRACES_SAMPLER_ARG=0.0` and `tracing.sample_ratio=1.0`, no root
    spans are sampled;
  - each of the six accepted `OTEL_TRACES_SAMPLER` values is accepted;
  - a ratio outside 0.0 to 1.0 is rejected (AC-21).
- **AC-21** (FR-6, FR-16, FR-19, FR-20, FR-21, FR-27, FR-37):
  - Unit: validation rejects an invalid value for each setting, including
    `log.level=debug-4` and `admin.listen=:9187`, and accepts
    `log.level=DEBUG`.
  - Unit: with tracing enabled, validation rejects each of these, and with
    tracing off the same values are accepted and ignored:
    - `OTEL_TRACES_EXPORTER=console`;
    - `OTEL_TRACES_SAMPLER=jaeger_remote`;
    - an unknown `OTEL_EXPORTER_OTLP_PROTOCOL`;
    - malformed `OTEL_EXPORTER_OTLP_HEADERS`;
    - an out-of-range `OTEL_TRACES_SAMPLER_ARG`;
    - a non-PEM `OTEL_EXPORTER_OTLP_CERTIFICATE` file;
    - `OTEL_EXPORTER_OTLP_COMPRESSION=zstd`;
    - `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` larger than `OTEL_BSP_MAX_QUEUE_SIZE`.
  - Every error record carries `setting` with the setting or variable name and
    does not contain the supplied value (checked with a marker value).
  - Functional: starting corral with an invalid setting, and separately with
    tracing enabled plus an invalid `OTEL_*` value, exits non-zero, and the
    error appears on stdout as JSON with `component=config` and `setting`.
- **AC-24** (FR-20, FR-30): Integration: with `OTEL_EXPORTER_OTLP_HEADERS`
  set, the test collector receives the configured header. Exported resource
  attributes include values from `OTEL_RESOURCE_ATTRIBUTES` and
  `OTEL_SERVICE_NAME`. Without `OTEL_SERVICE_NAME`, `service.name` is
  `corral`.
- **AC-25** (FR-21): Unit: children created under a sampled parent are sampled
  and children under an unsampled parent are not, whatever the configured
  ratio.
- **AC-26** (FR-7, FR-28, FR-33): Unit, with a fresh rate limiter per case:
  - an error passed to the OTel global error handler, a message logged via
    `grpclog` at warning, and an admin `http.Server` error-log message each
    produce one slog record with `component` `otel`, `grpc` and `admin_http`
    respectively;
  - a message logged through Go's standard `log` package, and a record logged
    by a third-party caller through `slog.Default()`, each produce one slog
    record with `component` `thirdparty`;
  - each record has that source's fixed message, and the third-party text
    (seeded with a marker, including in the `slog.Default()` record's
    key/value arguments) appears nowhere in it;
  - nothing is written to stderr.

  Also:
  - a `grpclog` info message is emitted at debug, and is not emitted at the
    default `info` level;
  - 10 `grpclog` warnings within 30 seconds (fake clock) produce one record,
    and the next record reports a `suppressed_count` of 9;
  - a standard `log` message produces a record at info; a `slog.Default()`
    record at warn produces a `thirdparty` record at warn;
  - 10 standard `log` messages within 30 seconds (fake clock) produce one
    record, and the next record reports a `suppressed_count` of 9; the same
    holds for 10 `slog.Default()` warn records;
  - limiters are separate per source and level: within the same 30 seconds,
    a `slog.Default()` warn record, a `slog.Default()` error record and a
    standard `log` message each still produce their own record.
- **AC-27** (FR-30): Integration: exported resource attributes all fall within
  the FR-30 allowlist. None of `process.command_args`, `process.command_line`
  or `process.owner` is present, including when they are supplied in
  `OTEL_RESOURCE_ATTRIBUTES`.
- **AC-28** (FR-34): Integration: with an unresponsive collector, the
  bootstrap's shutdown completes within 5 seconds plus a small tolerance
  defined by the test. With a responsive collector, test spans ended just
  before shutdown are received.
- **AC-29** (FR-33): Unit with a fake clock: 10 export failures within 30
  seconds produce one warning. The next failure after 30 seconds produces a
  warning with `suppressed_count` 9. The warnings contain no `err.Error()`
  text.
- **AC-30** (FR-35): Functional: with `admin.listen` set to an address that is
  already in use, startup exits non-zero with an error record carrying
  `setting=admin.listen` and no address value.
- **AC-31** (FR-18): Unit/functional, using a test hook that holds startup
  before its last step:
  - while startup is held, `/readyz` returns 503 with body `starting\n`, and
    this holds even when a registered readiness-check fake reports ready; the
    fake records that it was not consulted;
  - after startup completes, with no readiness check registered, `/readyz`
    returns 200 with body `ok\n`;
  - the `lifecycle` startup-complete record is emitted at that moment;
  - once the `lifecycle` shutting-down record has been emitted (shutdown held
    open by a test hook), `/readyz` returns 503 with body `shutting_down\n`,
    including when a registered readiness-check fake reports ready; the fake
    records that it was not consulted;
  - every response has `Content-Type: text/plain; charset=utf-8`.
- **AC-33** (harness for AC-18, AC-38 and AC-40): The marker-scan harness captures
  stdout, stderr, `/metrics` output, `/readyz` bodies, and the span payloads a
  test collector receives (span names, attributes, events, status
  descriptions, resource attributes). Transport-level request headers received
  by the collector are not scanned.
  - It scans for each marker in raw form and in hex form (lower and upper
    case).
  - It also scans in base64 (standard and URL-safe) at all 3 alignments: the
    marker is encoded with 0, 1 and 2 prefix bytes, and the unstable edge
    characters are dropped from each pattern.
  - Each marker can be scanned in all sources or only in a chosen subset.
  - A self-test plants each encoding of a marker in each source, at every
    byte offset 0 to 2, and expects a detection.
- **AC-35** (FR-29): Unit: errors of each class (including an exporter error
  whose text contains an endpoint URL with a userinfo marker, a `net.OpError`,
  a server reply with a marker in `errmsg`, and a recovered panic with a
  marker value):
  - map to the expected class;
  - produce a log `error` attribute that equals the class value and contains
    no marker;
  - when recorded on a span, yield status Error with an empty description and
    no `exception` events.

  Detail mapping:
  - a refused dial maps to `ECONNREFUSED`;
  - a reset connection maps to `ECONNRESET`;
  - a broken pipe maps to `EPIPE`;
  - an expired context deadline maps to `deadline_exceeded`;
  - a canceled context maps to `canceled`;
  - `io.EOF` and `io.ErrUnexpectedEOF` both map to `eof`;
  - a TLS handshake failure maps to `tls_handshake`;
  - a DNS failure maps to `dns_resolution`;
  - an address already in use maps to `EADDRINUSE`;
  - an unmapped error and a MongoDB server error map to `other`.

  Each log record and span carries `corral.error.detail` with a value from the
  FR-29 enum.

  The recovered panic is logged at error level. It may carry a `stack`
  attribute, and no attribute contains the panic marker.
- **AC-36** (FR-37): Integration with an exporter that blocks until the test
  releases it, and a small `OTEL_BSP_MAX_QUEUE_SIZE`:
  - end more test spans than the queue holds;
  - check that no span start or end call blocks beyond a test-defined bound;
  - release the exporter and force a flush;
  - `corral_tracing_spans_dropped_total` is greater than zero and equals
    spans ended minus spans received by the exporter.
- **AC-37** (FR-12, FR-25): Unit tests of the shared name and metadata
  sanitisers:
  - an unknown command name of more than 64 bytes, including multi-byte
    characters, is kept and truncated to at most 64 bytes on a character
    boundary;
  - invalid UTF-8 is replaced with U+FFFD;
  - an `appName` of more than 128 bytes is truncated to at most 128 bytes on a
    character boundary;
  - a driver name and a driver version, each of more than 64 bytes with
    multi-byte characters and invalid UTF-8, are each replaced-then-truncated
    to at most 64 bytes on a character boundary;
  - `isMaster` and `ismaster` map to `hello` for allowlist matching, and the
    log/span value stays as received;
  - `Find` (wrong case) maps to `other` for metric labels, while `find` maps to
    `find`.
- **AC-38** (FR-24, FR-28, FR-33; moved from AC-18): Integration, using the
  AC-33 harness with tracing on and debug logging. Test spans are exported
  under each of these setups:
  - `OTEL_EXPORTER_OTLP_HEADERS=authorization=<marker>`, with a collector that
    rejects requests;
  - the same header with an unreachable collector;
  - an OTLP endpoint URL containing a userinfo marker, over both
    `http/protobuf` and `grpc`.

  No marker, in any encoding the harness scans, appears in stdout, stderr,
  `/metrics`, `/readyz` bodies or exported span payloads.
- **AC-40** (FR-20, FR-24, FR-27, FR-41): Integration of the tracing
  bootstrap, with tracing on, `log.level=info`, a plaintext (non-TLS) test
  collector reached through an `http://` endpoint, and synthetic test PEM
  files (a generated test CA certificate, client certificate and client key)
  stored under paths that contain a marker. The AC-33 harness scans stdout
  and stderr.
  - Run all 8 combinations (2 × 2 × 2) of:
    - protocol `http/protobuf` and protocol `grpc`;
    - variable form, where the TLS variables and the endpoint vary together:
      both in the general form (`OTEL_EXPORTER_OTLP_*`), or both in the
      `_TRACES_` form (`OTEL_EXPORTER_OTLP_TRACES_*`);
    - TLS material as a CA certificate only, and as a client certificate plus
      client key.
  - In each combination:
    - the bootstrap starts with no config error and does not exit;
    - test spans arrive at the plaintext test collector;
    - exactly one FR-41 record (level warn, `component=config`, the FR-41
      message) is emitted, before the `lifecycle` startup-complete record
      where the test runs the full startup sequence;
    - its `setting` equals the expected sorted, comma-joined names (for
      example `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE,OTEL_EXPORTER_OTLP_CLIENT_KEY`);
    - the path marker, in any encoding the harness scans, and the text
      `-----BEGIN` appear nowhere in stdout or stderr.
  - Endpoint precedence:
    - with `OTEL_EXPORTER_OTLP_ENDPOINT` set to an unreachable `https://`
      endpoint, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set to the plaintext
      collector's `http://` endpoint, and a CA certificate set, spans arrive at
      the plaintext collector and the FR-41 record is emitted;
    - with `OTEL_EXPORTER_OTLP_ENDPOINT` set to an `http://` endpoint,
      `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set to a TLS test collector's
      `https://` endpoint, and a CA certificate that the collector's
      certificate chains to, spans arrive over TLS at that collector and no
      FR-41 record is emitted. This is checked for both protocols.
  - With both `OTEL_EXPORTER_OTLP_CERTIFICATE` and
    `OTEL_EXPORTER_OTLP_TRACES_CERTIFICATE` set and an `http://` endpoint,
    exactly one FR-41 record is emitted, and its `setting` lists both names.
  - An endpoint scheme written `HTTP://` behaves like `http://`.
  - With `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` set to whitespace only and
    `OTEL_EXPORTER_OTLP_ENDPOINT` set to the plaintext collector's `http://`
    endpoint, with a CA certificate set, spans arrive at the plaintext
    collector and the FR-41 record is emitted.
  - With an `http://` endpoint, a CA certificate set and
    `OTEL_EXPORTER_OTLP_INSECURE=false`, the connection is still plaintext and
    spans arrive.
  - With an `http://` endpoint and no TLS variable set, spans arrive and no
    FR-41 record is emitted.
  - Unit: with an `http://` endpoint and tracing on, a non-PEM file in each of
    the six TLS variables is rejected under FR-27, with `setting` naming that
    variable and no value echoed, and no FR-41 record is emitted. With
    tracing off, or with `OTEL_TRACES_EXPORTER=none`, an `http://` endpoint
    with valid TLS variables produces no FR-41 record. With no endpoint
    variable set and a valid CA certificate set, no FR-41 record is emitted.

### Part B: convention checks every feature spec inherits, re-runs and extends

At SPEC-001 time these checks run against whatever ports and signals exist.
They are not part of SPEC-001's acceptance gate.

- **AC-2** (FR-2, FR-3): For each observer port, domain unit tests run the same
  scenarios with the no-op and with a recording fake. Results are the same, and
  the fake records the expected calls, each receiving the caller's
  `context.Context`.
- **AC-9** (FR-12): Unit: recording each command of the feature (after alias
  mapping) yields its allowlist name as the metric label. A command outside
  the allowlist yields `other`.
- **AC-10** (FR-13, FR-31): Unit: for each metric family in the catalog, the
  cartesian product of the allowed label values (excluding `le` and
  `quantile`) is at most 100.
- **AC-17** (FR-22, FR-23, FR-29, FR-32): Functional with an official MongoDB
  driver:
  - the root span for a `find` on collection X is a `SERVER` span named
    `find X`, and it has exactly the child spans {`pool.checkout`
    (`INTERNAL`), round-trip (`CLIENT`, named `find X`)};
  - attributes are only those FR-23 allows, the required ones are present, and
    `db.system.name=mongodb`;
  - a failed operation has status Error with an empty description, `error.type`
    and `corral.error.detail` from their enums, `db.response.status_code`, and
    `corral.db.response.code_name`;
  - no span has `exception` events or a non-empty status description;
  - no span is produced for `hello` / `isMaster`, connection handshakes,
    heartbeats, or any of the FR-32 auth commands;
  - at `info` level, no per-exchange log record is emitted for handshakes,
    `hello`, heartbeats or successful authentication;
  - each FR-32 exchange type that the feature implements produces the shape
    FR-32 defines.

  The number of root spans per driver-level operation is not asserted.
- **AC-18** (FR-24, FR-25, FR-26, FR-29, FR-32, FR-40): Functional, using the AC-33
  harness, with tracing on and debug logging, and distinctive synthetic marker
  values. The run covers:
  - successful SCRAM authentication with a marker username and password;
  - failed SCRAM authentication (wrong marker password);
  - `createUser` with a marker username and password, followed by `usersInfo`
    for that marker username;
  - inserts and finds with marker document values, filters, and a marker in
    the `comment` field;
  - a duplicate-key error on a marker value;
  - a marker `appName`.

  Results:
  - None of the credential, username, document, filter, comment or
    duplicate-key markers appears, in any encoding the harness scans, in
    stdout, stderr, `/metrics`, `/readyz` bodies, span names, attributes,
    events, status descriptions or resource attributes.
  - The server `errmsg` text does not appear.
  - The `appName` marker does not appear in `/metrics` (it is allowed in logs
    and spans per FR-25).
  - The failed SCRAM authentication produces a warn-level record carrying the
    error class, error detail and (when present) `code` / `codeName`. That
    record contains neither the username marker nor the password marker in
    any scanned encoding.
- **AC-19** (FR-23, FR-25, FR-39): Functional, using distinctive synthetic
  database and collection names (e.g. `zzcorraldb_7f3a`, `zzcorralcoll_9c1e`)
  and a distinctive `appName`:
  - root and round-trip spans carry those names in `db.namespace` and
    `db.collection.name`;
  - root spans carry `client.address`;
  - if `corral.client.app_name` is present, it is at most 128 bytes;
  - the database name, collection name, client address, `appName`, upstream
    address and any connection, session, request or cursor ID never appear in
    `/metrics` output;
  - every captured log record uses only FR-39 allowlisted keys.
- **AC-22** (FR-19, NFR performance): Unit: with tracing disabled,
  `testing.AllocsPerRun` (at least 1000 runs) over the per-message tracing
  calls returns 0. A test or review check confirms that `Tracer.Start` is not
  reached when tracing is disabled. Comparing against the no-op tracer does not
  satisfy this criterion.
- **AC-23** (FR-3): Domain unit scenarios run with the real adapters wired to
  in-memory sinks (log buffer, private Prometheus registry, in-memory span
  recorder) give the same domain results as with the no-op.
- **AC-34** (FR-15): Functional: an HTTP request sent to the MongoDB listener
  port gets its connection closed without any HTTP response.
- **AC-39** (FR-40): Unit tests of the collection extractor:
  - each listed command with a string value yields the collection name, and
    `getMore` yields its `collection` field;
  - `aggregate: 1` and non-string values yield none;
  - every listed user-, role-management and auth command, each given a marker
    first value, yields no collection name.

### Verified by review (no automated AC)

- FR-6: format and level are read only once at startup.
- FR-20: `OTEL_EXPORTER_OTLP_TIMEOUT`, `_INSECURE` and the client certificate
  and key variables are passed to the exporter configuration, except that
  when the effective traces endpoint uses `http://`, the exporter's effective
  transport uses no TLS: corral overrides any TLS configuration the SDK
  derives from the certificate and key variables and from
  `OTEL_EXPORTER_OTLP_INSECURE` / `OTEL_EXPORTER_OTLP_TRACES_INSECURE`
  (FR-41).
- FR-24: TLS private key material, certificate file contents and the session
  `uid` never reach any logger, attribute or response (no code path passes
  them to one).
- FR-29: no call site passes `err.Error()` or a panic value to a log or span
  attribute.
- FR-36: the operator documentation contains all three required statements.
- FR-38: log output is synchronous, and the accepted stall risk is documented.

## Open questions

Must be empty before approval.

None.

## Decisions log

| Date | Decision | Reason |
|---|---|---|
| 2026-10-24 | Domain defines observer ports in domain terms; infra adapters implement them; domain imports no slog/Prometheus/OTel (Q1 option a). | Strict DDD boundary per CLAUDE.md; testable with fakes; enforceable by import check. |
| 2026-10-24 | Metrics via `prometheus/client_golang` directly, not the OTel metrics SDK (Q2a). | Mature, fewer dependencies, exact Prometheus semantics; swappable behind ports. |
| 2026-10-24 | OTLP supports `http/protobuf` and `grpc` via standard `OTEL_EXPORTER_OTLP_PROTOCOL`; default `http/protobuf` (Q3c). | Matches OTel convention; HTTP default. |
| 2026-10-24 | Tracing off by default; parent-based trace-ID-ratio sampler, default ratio 1.0 (Q4a). | Hot-path proxy; avoid failing exporter on first run; operators lower ratio. |
| 2026-10-24 | Span model: one root span per proxied exchange, children for pool checkout and upstream round-trip; no connection/session spans (Q5a). | Long-lived connection spans fit poorly with PHP-FPM persistent connections. |
| 2026-10-24 | Command name allowed everywhere (metric labels via allowlist + `other`); database/collection names in logs and spans only. Owner confirmed db/collection names are not user data (Q6a). | Cardinality control and data minimisation. |
| 2026-10-24 | Client network addresses allowed in logs and spans, never metric labels. Owner confirmed clients are servers, not end users (Q7b). | Needed operationally; not personal data in this context. |
| 2026-10-24 | MongoDB usernames never emitted (Q8a). | A username can identify a person; auth data excluded wholesale. |
| 2026-10-24 | No wire-dump / body logging capability; stated non-goal (Q9a). | Incompatible with data minimisation (GDPR). |
| 2026-10-24 | Logs: JSON default, text selectable; level `info` default; stdout; restart-only changes (Q10). | Machine-readable default for log pipelines; simplicity in v1. |
| 2026-10-24 | Single admin listener for `/metrics`, `/healthz`, `/readyz`; configurable bind, loopback default; no auth/TLS in v1, stated non-goal with security note (Q11a). | Simple operations; safe default exposure. |
| 2026-10-24 | This spec defines only the readiness endpoint contract; meaning deferred to the pooling spec (Q12). | Readiness depends on pooling semantics not yet specified. |
| 2026-10-24 | Spec names logical settings only; loading mechanism deferred to the configuration spec; standard `OTEL_*` variables honoured (Q13a). | Avoid fixing config mechanism before its own spec. |
| 2026-10-24 | Metric rules: `corral_` prefix, base units, `_total` counters, enumerated labels only, forbidden label list, max 100 series per metric, `go_*`/`process_*` collectors exposed (Q14). | Prometheus conventions and cardinality control. |
| 2026-10-24 | `trace_id`/`span_id` in logs when tracing is on; zero tracing allocations per message when tracing is off is required (Q15). | Log/trace correlation; protect the hot path. |
| 2026-10-24 | Failure to bind the admin listener at startup is fatal (OPEN 2). | Owner decision. |
| 2026-10-24 | `admin.listen` default is `127.0.0.1:9187` (OPEN 3). | Owner decision. |
| 2026-10-24 | Graceful-shutdown span flush timeout is 5s (OPEN 4). | Owner decision. |
| 2026-10-24 | OTLP export failures are rate-limited to at most one warning per 30s, including a count of failures suppressed since the last warning (OPEN 5). | Owner decision. |
| 2026-10-24 | `OTEL_TRACES_SAMPLER` / `OTEL_TRACES_SAMPLER_ARG` override `tracing.sample_ratio` when set; `OTEL_SDK_DISABLED=true` always disables tracing regardless of `tracing.enabled` (OPEN 6). | Owner decision. |
| 2026-10-24 | `/readyz` with no registered readiness check returns 200 once startup has completed (OPEN 7); confirmed under B1. | Owner decision. |
| 2026-10-24 | This spec owns a single central command-name allowlist; feature specs extend it by amending this spec, so the 100-series cap is checked in one place (OPEN 9). | Owner decision. |
| 2026-10-24 | Backpressure: spans are dropped when the export queue is full and counted in `corral_tracing_spans_dropped_total`; logs stay synchronous to stdout, and a blocked stdout stalling the proxy is accepted for v1 (OPEN 1). | Owner decision. |
| 2026-10-24 | Latency-overhead target moves to a later performance spec and is a non-goal here; the zero-allocation rule stays (OPEN 8). | Owner decision. |
| 2026-10-24 | SPEC-001 is approved on its Part A ACs; feature specs inherit the Part B convention checks; the no-check `/readyz` answer stands (B1). | Owner decision. |
| 2026-10-24 | Allowed in logs and spans, never as metric labels: upstream host and port (`server.address` / `server.port`), connection and request IDs, session (lsid) and cursor IDs, client `appName` capped at 128 bytes with invalid UTF-8 replaced, driver name and version (B2). | Owner decision. |
| 2026-10-24 | An empty host in `admin.listen` is a startup config error; binding to all interfaces requires explicit `0.0.0.0` or `[::]` (B3). | Owner decision. |
| 2026-10-24 | `OTEL_TRACES_EXPORTER` accepts only `otlp` and `none`; any other value, including `console`, is a config error when tracing is enabled (B4). | Owner decision. |
| 2026-10-24 | MongoDB error `code` / `codeName` are banned as metric labels; metrics label by the fixed error class; code/codeName stay allowed in logs and spans (B5). | Owner decision. |
| 2026-10-24 | Heartbeats/`hello`, connection handshakes and auth exchanges: no spans; logs allowed under FR-24/25 as above (B6, amended by OD-1). | Owner decision. |
| 2026-10-24 | Command names outside the allowlist keep their name in logs and spans, capped at 64 bytes with invalid UTF-8 replaced; metric labels use allowlist plus `other`; matching is case-sensitive with an alias table mapping `isMaster` / `ismaster` to `hello` (C1). | Owner decision. |
| 2026-10-24 | `_test.go` files in domain packages are exempt from the import allowlist (C2). | Owner decision. |
| 2026-10-24 | Invalid `OTEL_*` values are fatal at startup when tracing is enabled and ignored when tracing is off (C3). | Owner decision. |
| 2026-10-24 | Stack traces of recovered panics may be logged at error level; the panic value stays banned (C4). | Owner decision. |
| 2026-10-24 | Hello, handshake and auth exchanges get no spans but may produce logs: auth failure at warn with error class and code/codeName, never the username or any auth payload; per-exchange handshake and hello/heartbeat records at debug only (OD-1). | Owner decision. |
| 2026-10-24 | Driver name and driver version are each capped at 64 bytes, with invalid UTF-8 replaced and then truncation on a character boundary, as for `appName` (OD-2). | Owner decision. |
| 2026-10-24 | Errors carry a second, fixed-enum detail attribute (`corral.error.detail`) alongside the 6-value class, with `other` for unmapped errors; it is never a metric label unless declared with bounded values in the catalog (OD-3). | Owner decision. |
| 2026-10-24 | Exemplars are not used in v1 (OD-4). | Owner decision. |
| 2026-10-24 | Auth-success records are logged at debug only, the same as handshake and hello records (OD-5). | Owner decision. |
| 2026-10-24 | Amendment 1: (1) metric label names for errors are `error_class` and `error_detail` (snake_case per FR-10); the span/log attribute stays `corral.error.detail` (FR-11, FR-29, AC-8). (2) Component `thirdparty` added to the FR-7 set; Go standard `log` package records and third-party `slog.Default()` calls are routed with component `thirdparty` and a fixed constant message, never the third-party text (FR-7, FR-28, FR-39, AC-5, AC-26). (3) `/readyz` adds reasons `unknown` (503, a check reported a reason outside the defined set) and `shutting_down` (503, from the start of graceful shutdown); bodies are `<reason>\n` / `ok\n` with `Content-Type: text/plain; charset=utf-8`; before startup completes it returns 503 `starting` without consulting registered checks (FR-18, AC-13, AC-31). (4) `thirdparty` records keep the source's level (std `log` records are info) and are rate-limited per source (std `log` bridge, `slog.Default()` bridge) and level, at most 1 per 30s with `suppressed_count` (FR-28, AC-26). (5) Graceful shutdown starts when corral emits the `lifecycle` shutting-down record; `/readyz` returns 503 `shutting_down` from then (FR-18, AC-31). (6) Feature specs add readiness reasons by amending this spec, as for FR-12 and FR-40 (FR-18). Points 4 to 6 close gaps found while drafting and were relayed by the Orchestrator. | Amendment 1, 2026-10-24, requested by the Orchestrator under the Owner's standing instruction (2026-10-24). Drafted in `specs/drafts/` by the spec-writer, which may not edit approved specs. Approved and promoted to `specs/` by the Owner, 2026-10-24. |
| 2026-10-24 | Amendment 2: when the effective OTLP traces endpoint (`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, else `OTEL_EXPORTER_OTLP_ENDPOINT`) uses `http://`, the exporter's effective transport uses no TLS for both `http/protobuf` and `grpc`: corral overrides any TLS configuration the SDK derives from the certificate, client-certificate and client-key variables (general and `_TRACES_`) and from `OTEL_EXPORTER_OTLP_INSECURE` / `_TRACES_INSECURE`. Startup continues normally (not a config error, not fatal), and exactly one FR-41 warn record (subject to FR-6) is emitted with `component=config`, a fixed constant message, and `setting` listing the names of the ignored variables. Variable values (file paths) are not logged, because FR-7/FR-27 allow only names in configuration records; file contents stay banned (FR-24). FR-27 PEM/readability validation still applies; when it fails, startup fails and no FR-41 record is emitted. "Set" means non-empty after trimming whitespace, as in the SDK. `https://` and no-endpoint defaults are unchanged (FR-7, FR-20, FR-41, AC-33, AC-40, Verified by review). Deliberately out of scope and left SDK-defined: TLS variables with `INSECURE=true` on an `https://` or default endpoint, and TLS variables with a gRPC `unix://` endpoint. Protocol behaviour there is known to diverge, and any startup failure there is reported through the existing startup error rules (FR-27, FR-29); no new behaviour is added. The Owner declined two further proposals for now, and they are not added: a warning when auth headers are sent over a plaintext endpoint, and a wider FR-30 resource-attribute ban list. | Owner decision, 2026-10-24, answering an escalated ambiguity: Go OTel SDK v1.47 behaves inconsistently with TLS settings and an `http://` endpoint (otlptracehttp refuses to start; otlptracegrpc silently uses TLS). Drafted in `specs/drafts/` by the spec-writer; approved and promoted to `specs/` by the Owner, 2026-10-24. |
