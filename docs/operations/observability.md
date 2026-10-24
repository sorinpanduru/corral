# Observability and operations

This guide covers corral's logging, metrics, tracing and admin listener, as
implemented in `cmd/corral` and `internal/`. Configuration (flags, environment
variables, secrets, exit codes, control flags) is described in
[configuration.md](configuration.md). The proxy itself is not yet built: today the
binary starts the admin listener and tracing, and serves until it is signalled.

## Running

```
make build
./bin/corral --log.level=info --admin.listen=127.0.0.1:9187
```

Send `SIGINT` or `SIGTERM` for a graceful shutdown (see "Startup and
shutdown").

The admin listener has no authentication or TLS; read Security and operational warnings before binding a non-loopback address.

## Startup and shutdown

Startup order:

1. Load and validate all settings and, when tracing is on, the `OTEL_*`
   variables. Every failure is written to stdout as JSON with
   `component=config` and the process exits with status 2 (see
   [configuration.md](configuration.md)).
2. Build the logger from the settings and install the third-party log bridges
   (`otel`, `grpc`, `slog.Default()`, standard `log`).
3. Emit the configuration records: the unknown `CORRAL_` variable warning, the
   OTLP TLS-ignored warning if it applies, and the `configuration loaded` info
   record.
4. Emit the `lifecycle` record `corral starting`.
5. Register metrics.
6. Bind the admin listener and start serving. `/healthz` is live and `/readyz`
   answers `503 starting`.
7. Start tracing (the last startup step today).
8. Mark startup complete (`/readyz` becomes `200 ok`), then emit the
   `lifecycle` record `corral startup complete`.

A configuration error exits with status 2. Any other startup failure writes an
error record to stdout and exits with status 1.

Shutdown starts on `SIGINT`, `SIGTERM` or an admin listener serve failure.
`/readyz` switches to `503 shutting_down` immediately before the `lifecycle`
record `corral shutting down` is emitted. Then the admin server is shut down
(bounded to 5 seconds) and tracing is shut down (bounded to 5 seconds), so a
shutdown takes about 10 seconds at worst.

Exit codes:

| Code | Meaning |
|---|---|
| 0 | Graceful shutdown, including a signal received during startup (no failure record; `corral shutting down` is still emitted, `corral startup complete` is not if startup had not finished) |
| 2 | Configuration error (the Go runtime also uses 2 for an unrecovered panic) |
| 1 | Startup failure, or the admin listener failed while running (`admin listener serve failed` record, then the normal shutdown) |

A step that fails during shutdown is logged at warn level
(`corral shutdown step failed`) and does not change the exit code.

## Security and operational warnings

- **The admin listener has no authentication and no TLS.** It serves
  `/healthz`, `/readyz` and `/metrics` in plain HTTP. Binding it to a
  non-loopback address exposes metrics and health status to anyone who can reach
  it. Restrict access with network policy or a firewall.
- **Kubernetes:** the default bind is loopback (`127.0.0.1:9187`), which probes
  and scrapers from outside the pod cannot reach. Set `admin.listen`
  explicitly to a non-loopback address: `0.0.0.0:<port>`, `[::]:<port>` or the
  pod IP. An empty host (for example `:9187`) is rejected at startup, so a bind
  to all interfaces must be written out.
- **A blocked stdout can stall the proxy (FR-38).** Log records are written
  synchronously to stdout. If stdout blocks (a stalled log pipeline, a full
  pipe), logging calls block, and this can stall proxied operations. This risk
  is accepted for v1. Make sure whatever consumes stdout keeps reading.
- **OTLP credentials:** use an `https://` endpoint when sending authentication
  headers. Do not set `OTEL_EXPORTER_OTLP_*INSECURE=true` outside a trusted
  network. Supply headers, the CA and the client certificate/key through a
  secrets mechanism, not in manifests or scripts.
- corral writes no log output to stderr; third-party output is routed through
  the same stdout logger (see "Logging").

## Settings

Logical settings and defaults:

| Setting | Values | Default |
|---|---|---|
| `log.format` | `json`, `text` (case-insensitive) | `json` |
| `log.level` | `debug`, `info`, `warn`, `error` (case-insensitive; no offsets such as `info+2`) | `info` |
| `admin.listen` | `host:port` | `127.0.0.1:9187` |
| `tracing.enabled` | `true`, `false` (case-insensitive) | `false` |
| `tracing.sample_ratio` | number in [0, 1] | `1.0` |

Settings are read once at startup. An invalid value stops startup with exit
status 2. The error record has `component=config` and a `setting`
attribute naming the setting. The supplied value is never echoed.

### How settings are supplied

Flags, environment variables and secret file references, their precedence and
syntax, and the error records are described in
[configuration.md](configuration.md). The `OTEL_*` variables below are read
from the environment, not from flags, and are validated in the same pass as the
other settings.

### `admin.listen` rules

- Must be `host:port`, port 0 to 65535.
- The host must not be empty. `:9187` is rejected.
- The host is an IP literal (IPv6 in brackets, for example `[::]:9187`) or a
  valid DNS hostname: letters, digits, `-` and `.`; labels of 1 to 63
  characters that do not start or end with `-`; at most 253 characters in total.
- The last label of a hostname must not be numeric or hex-like. This rejects
  `0`, `127.1` and `0x0`, which resolvers may read as unexpected addresses.
  Hostnames with whitespace or underscores are rejected.
- If the address cannot be bound at startup, startup fails with a non-zero
  exit. The error record has `component=config` and `setting=admin.listen`, and
  does not contain the address.

## Admin endpoints

Only `GET` (and `HEAD`) on these exact paths; any other path returns 404 with
body `not found`, and other methods on these paths get `405` with
`Allow: GET, HEAD`. Bodies are fixed strings: nothing from the request (path,
query, headers) is echoed. Text responses use
`Content-Type: text/plain; charset=utf-8`.

| Endpoint | Response |
|---|---|
| `/healthz` | `200`, body `ok` (liveness only) |
| `/readyz` | `200`, body `ok` when ready; otherwise `503` with the reason as body |
| `/metrics` | `200`, Prometheus text format |

Each body ends with a newline (`ok\n`, `starting\n`, ...).

`/readyz` reasons:

| Body | Meaning |
|---|---|
| `starting` | Startup has not completed. Registered readiness checks are not consulted yet. |
| `shutting_down` | Graceful shutdown has begun. Takes precedence over any other check. |
| `unknown` | A readiness check reported a reason outside the defined set. The reported text is not echoed. |

`/metrics` serves the Prometheus text format only. OpenMetrics is not offered
and no exemplars are emitted. Any query string is ignored. If gathering fails,
the response is `500` with the fixed body `error`; the error text is not
exposed. At most 4 scrapes are served concurrently; beyond that, or past the 10
second timeout, the response is `503` with a fixed body.

Server limits: read-header timeout 5 s, read timeout 10 s, write timeout 30 s,
idle timeout 60 s, maximum request headers 16 KiB. `OPTIONS *` is not
answered by the built-in handler.

Metric labels are drawn from fixed sets. The metric catalog is embedded
in the binary (`internal/infra/telemetry/metrics/catalog.json`).

## Logging

- Output: stdout, synchronous (see the warning above).
- Format: `json` (default) or `text`. Level: `debug`, `info`, `warn`, `error`;
  records below the configured level are not emitted.
- Errors raised before the logger is configured (invalid settings) are written
  to stdout as JSON with `component=config`.
- Every record has a `component` from this fixed set:

| Component | Used for |
|---|---|
| `config` | Configuration errors |
| `lifecycle` | Starting, startup-complete and shutting-down records |
| `admin_http` | Admin listener (including its `http.Server` error log) |
| `tracing` | corral's tracing bootstrap |
| `otel` | OpenTelemetry SDK errors and logs |
| `grpc` | gRPC library logs |
| `thirdparty` | Go standard `log` package and `slog.Default()` callers |

- Record keys are an allowlist: `time`, `level`, `msg`, `component`, `error`,
  `trace_id`, `span_id`, `stack`, `setting`, a fixed set of connection and
  command attributes, and numeric keys ending in `_seconds`, `_bytes` or
  `_count` (for example `suppressed_count`). Messages are constant strings.
- When tracing is enabled, records emitted with a span context carry
  `trace_id` (32 hex characters) and `span_id` (16), also when
  `OTEL_TRACES_EXPORTER=none`. When tracing is disabled these are absent.
- Records about a setting carry `setting=<name>` only, never the value.
- Third-party sources (`otel`, `grpc`, admin HTTP server, standard `log`,
  `slog.Default()`) emit fixed messages: the original text and arguments are
  never forwarded. Each source and level is rate limited to one record per 30
  seconds; the next emitted record carries `suppressed_count`. Levels:
  `otel` informational logs are emitted at debug and `otel` errors at warn;
  `grpc` info, warning and error map to debug, warn and error; admin HTTP
  server errors are emitted at warn; standard `log` records are emitted
  at info and `slog.Default()` records keep the caller's level; both are
  filtered by `log.level`. A gRPC fatal log writes an error record
  (not rate limited) and exits the process with status 1.

### What corral never emits (FR-24)

At any level, in logs, metrics, spans, resource attributes or HTTP bodies:

- passwords, keys, tokens and other credentials; authentication payloads of any
  mechanism (SASL/SCRAM, `authenticate`, X.509 material);
- X.509 client certificate subject DN and subject alternative names;
- MongoDB usernames and role names, and the `uid` of a logical session ID;
- document contents, filters, projections, sorts, updates, pipelines, the
  `comment` field and any other command argument values;
- client handshake metadata other than `appName`, driver name and driver
  version (OS, platform and environment fields are never emitted);
- verbatim MongoDB server error messages;
- raw wire-protocol bytes;
- MongoDB connection strings, with or without credentials, and URLs that
  contain userinfo, including OTLP endpoint
  URLs; the values of `OTEL_EXPORTER_OTLP_HEADERS` and
  `OTEL_EXPORTER_OTLP_TRACES_HEADERS`;
- TLS private key material and the contents of certificate and key files;
- recovered panic values;
- free text from third-party log sources.

What may appear in logs and spans includes (FR-25 allowlist): client IP and
port, upstream host and port, database and collection names, the command name,
connection, request, session (UUID only) and cursor IDs, client `appName`,
driver name and version, the MongoDB error code and `codeName`, and durations,
byte sizes and counts. `host.name` may also appear as a resource attribute on
spans (FR-30).

Errors are reported through a classification (the `error.type` and
`corral.error.detail` keys) rather than their text. The client `appName` (truncated to 128 bytes) and
driver name/version (64 bytes each) may appear in logs and spans.

## Tracing

Tracing is off by default. When off, no spans are created, no exporter is
started and no connection is opened. Enable it with `tracing.enabled=true`.

Tracing is off, whatever `tracing.enabled` says, when `OTEL_SDK_DISABLED` is
`true` (case-insensitive). Any other `OTEL_SDK_DISABLED` value counts as false
and is never an error.

### When `OTEL_*` variables are fatal

- Tracing on: every supported variable below is validated, in both its general
  (`OTEL_EXPORTER_OTLP_*`) and `_TRACES_` (`OTEL_EXPORTER_OTLP_TRACES_*`)
  forms. Every invalid variable is reported (as JSON, sorted by variable name, in
  the same pass as the other configuration errors) and startup stops with exit
  status 2. Each error record has `component=config` and
  `setting=<variable name>`; the value is never echoed. PEM files are read once
  each, at most 4 MiB.
- Tracing off (including via `OTEL_SDK_DISABLED`): all `OTEL_*` variables other
  than `OTEL_SDK_DISABLED` are ignored and not validated.
- An empty or whitespace-only value counts as unset.

### Supported variables and validation

| Variable | Rule | Default |
|---|---|---|
| `OTEL_TRACES_EXPORTER` | `otlp` or `none` (case-insensitive). `none` builds the SDK (sampling, log correlation) but exports nothing. | `otlp` |
| `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_EXPORTER_OTLP_TRACES_PROTOCOL` | `http/protobuf` or `grpc` (case-insensitive) | `http/protobuf` |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `..._TRACES_ENDPOINT` | Absolute `http` or `https` URL with a host; port, if present, at most 65535 | SDK default |
| `OTEL_EXPORTER_OTLP_HEADERS`, `..._TRACES_HEADERS` | Comma-separated `key=value`; keys are non-empty HTTP tokens; values valid percent-encoding | none |
| `OTEL_EXPORTER_OTLP_TIMEOUT`, `..._TRACES_TIMEOUT` | Non-negative integer, milliseconds | SDK default |
| `OTEL_EXPORTER_OTLP_COMPRESSION`, `..._TRACES_COMPRESSION` | `gzip` or `none` (exact, lowercase) | SDK default |
| `OTEL_EXPORTER_OTLP_INSECURE`, `..._TRACES_INSECURE` | `true` or `false` (case-insensitive; see security warnings) | SDK default |
| `OTEL_EXPORTER_OTLP_CERTIFICATE`, `..._TRACES_CERTIFICATE` | Path to a readable PEM file with at least one parsable certificate | none |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` and `..._CLIENT_KEY` (and `_TRACES_` forms) | Both must be set under the same prefix and form a valid key pair. Key formats PKCS#8, PKCS#1, SEC1; encrypted keys are rejected. | none |
| `OTEL_TRACES_SAMPLER` | `always_on`, `always_off`, `traceidratio`, `parentbased_always_on`, `parentbased_always_off`, `parentbased_traceidratio` (case-insensitive) | unset (parent-based ratio) |
| `OTEL_TRACES_SAMPLER_ARG` | Number in [0, 1]; validated only when the effective sampler uses a ratio | unset |
| `OTEL_SERVICE_NAME` | Any non-empty string | see resource rules |
| `OTEL_RESOURCE_ATTRIBUTES` | Comma-separated `key=value`, non-empty keys, valid percent-encoding | none |
| `OTEL_SDK_DISABLED` | See above; never invalid | false |

Certificate and key files must be regular files of at most 4 MiB (not
directories, FIFOs or devices); symlinks are followed. Paths and file contents
are never logged. The exporters read the standard `OTEL_EXPORTER_OTLP_*`
variables themselves from the process environment, so for the TLS and header
variables use your secrets mechanism rather than writing values into manifests
or scripts. Header values are percent-encoded (a space is `%20`). Examples with placeholders only:

```
OTEL_TRACES_EXPORTER=otlp
OTEL_EXPORTER_OTLP_PROTOCOL=grpc
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://<YOUR_COLLECTOR_HOST>:4317
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20<YOUR_API_KEY>
OTEL_EXPORTER_OTLP_CERTIFICATE=<YOUR_CA_PEM_PATH>
OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE=<YOUR_CLIENT_CERT_PEM_PATH>
OTEL_EXPORTER_OTLP_CLIENT_KEY=<YOUR_CLIENT_KEY_PEM_PATH>
```

#### Plaintext `http://` endpoints (FR-41)

The effective traces endpoint is `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` if set,
otherwise `OTEL_EXPORTER_OTLP_ENDPOINT` (values are trimmed; the scheme is
case-insensitive). When it uses `http://`, the connection is plaintext for
both the `http/protobuf` and the `grpc` protocol:

- `OTEL_EXPORTER_OTLP_INSECURE` and its `_TRACES_` form are overridden, even
  when set to `false`.
- The TLS variables (`..._CERTIFICATE`, `..._CLIENT_CERTIFICATE`,
  `..._CLIENT_KEY`, and their `_TRACES_` forms) are not passed to the exporter.
  This is not an error and startup continues. The files are still validated
  (readable, valid PEM); an invalid file is still fatal.
- If at least one TLS variable is set, corral logs one `WARN` record with
  `component=config` before the startup-complete record. Its message is exactly
  `OTLP TLS settings ignored because the traces endpoint uses http://`, and its
  `setting` attribute lists the names of the ignored variables, sorted and
  comma-separated. It never contains values, paths, file contents or the
  endpoint URL.

With an `https://` endpoint, no endpoint, or another scheme, behaviour is
unchanged and there is no such record.

**Caution:** over `http://`, span data and the values of
`OTEL_EXPORTER_OTLP_HEADERS` / `OTEL_EXPORTER_OTLP_TRACES_HEADERS` (which may
carry authentication tokens such as `<YOUR_API_KEY>`) are sent unencrypted.
Use `https://` for any collector outside a trusted network. corral does not
warn about headers sent over `http://`.

### Precedence

- For the protocol, the `_TRACES_` variable wins over the general one. Both are
  validated.
- For the other exporter variables both forms are validated; the exporter
  itself applies the OpenTelemetry precedence (`_TRACES_` over general).
- Sampling: when `OTEL_TRACES_SAMPLER` is unset, the default sampler is
  parent-based with a trace-ID ratio. The ratio is `OTEL_TRACES_SAMPLER_ARG` if
  set, otherwise `tracing.sample_ratio`. `OTEL_TRACES_SAMPLER` set to
  `traceidratio` or `parentbased_traceidratio` uses the ratio the same way.
  `always_on`, `always_off`, `parentbased_always_on` and
  `parentbased_always_off` use no ratio, and `OTEL_TRACES_SAMPLER_ARG` is
  ignored (not validated) with them.

### Span queue and export (BSP variables)

corral uses its own bounded-queue span processor, not the stock batch
processor. Ending a sampled span enqueues it; when the queue is full the span
is dropped and `corral_tracing_spans_dropped_total` is incremented. Span start
and end never block on the exporter.

| Variable | Meaning | Default | Rule |
|---|---|---|---|
| `OTEL_BSP_MAX_QUEUE_SIZE` | Queue capacity, spans | 2048 | positive integer, at most 1048576 |
| `OTEL_BSP_MAX_EXPORT_BATCH_SIZE` | Maximum spans per export; an export also starts as soon as this many are queued | 512 (or the queue size if smaller) | positive integer, at most 1048576, must not exceed the queue size |
| `OTEL_BSP_SCHEDULE_DELAY` | Maximum time between export attempts while spans are queued, ms | 5000 | positive integer |
| `OTEL_BSP_EXPORT_TIMEOUT` | Maximum duration of one export call, ms | 30000 | positive integer |

On graceful shutdown, pending spans are flushed for at most 5 seconds (after
the admin server's own 5 second bound); spans not exported by then are dropped.

### Resource attributes

- Detectors kept: `service.*`, `telemetry.sdk.*` and `host.name`. Output of
  any other detector is discarded.
- `OTEL_RESOURCE_ATTRIBUTES` can add other attributes (values are
  percent-decoded), except the banned keys below.
- `service.name` is `corral` unless set by `OTEL_RESOURCE_ATTRIBUTES` or
  `OTEL_SERVICE_NAME`; `OTEL_SERVICE_NAME` wins over both.
- Banned keys, removed from exported spans even if the operator sets them (raw
  or percent-encoded): `process.command_args`, `process.command_line`,
  `process.owner`.
- Do not put credentials or personal data in `OTEL_RESOURCE_ATTRIBUTES`; only
  the three keys above are filtered.

## Metrics

| Metric | Type | Description |
|---|---|---|
| `corral_tracing_spans_dropped_total` | counter, no labels | Spans dropped because the trace export queue was full |

This counter counts only queue-full drops. Spans lost to failed exports or to
the shutdown flush deadline are not counted. Metrics are registered in a
private registry and served by `/metrics`. Names must be in the embedded
catalog; label values come from fixed sets.
