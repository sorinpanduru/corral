# Plan: SPEC-002 Proxy core (wire listener, handshake, multiplexing pool, upstream)

- Spec: `specs/SPEC-002-proxy-core.md` (approved 2026-10-24, revision 5, plus **Amendment 1** approved 2026-10-24, commit e7d0401: FR-60 to FR-65, AC-37 to AC-49, and changes to FR-2, FR-5, FR-7, FR-8, FR-9, FR-13, FR-16, FR-19, FR-29, FR-33, FR-35, FR-38, FR-41, FR-44, FR-46, FR-47, FR-57, FR-58, the metric definitions, AC-19, AC-28 and AC-35). L = K = 65,536 bytes is now normative.
- Depends on spec: `specs/SPEC-001-observability-conventions.md` with Amendment 3 (approved 2026-10-24; SPEC-002 FR-52). The Amendment 3 code changes are in this plan (T1, T2) and also cover SPEC-001 AC-41.
- Depends on plan: `plans/SPEC-003.md`. It is mid-implementation: only wave 1 has landed on the base branch, and there is no `registry` or `loader` package yet. SPEC-002 settings register through the SPEC-003 registry (ADR-0011). Every dependency on a SPEC-003 task is written as **[SPEC-003 Tn]**.
- Planner date: 2026-10-24.
- Scope: every FR (FR-1 to FR-65) and AC (AC-1 to AC-49) of SPEC-002, the SPEC-001 Part B re-runs that AC-27 requires, and SPEC-001 AC-41.
- Docker: needed only by the functional suites (T26, T27, and the functional parts of T28). Twin suites need no Docker.
- Twins needed: **TW1 `twins/mongosim`** (simulated upstream) and **TW2 `twins/faultproxy`** (fault-injecting TCP intermediary in front of a real `mongod`). Contracts are under "Twins".

## ADRs written or updated for this plan

- New:
  - `docs/adr/0015-proxy-core-package-structure-and-ports.md`: domain and infrastructure packages, ports, observers versus spans.
  - `docs/adr/0016-amendment-3-error-details-and-telemetry-artifacts.md`: `DetailedError`, classification precedence, the server-attribute rule for synthesized errors, enum and allowlist changes, catalog buckets.
  - `docs/adr/0017-domain-bson-and-wire-codec.md`: hand-written BSON, streaming validation, checksum stripping, header rewriting.
  - `docs/adr/0018-client-connection-handling-buffers-timeouts-and-accounting.md`: 64 KiB request buffer limit and reply chunk, I/O budget, liveness peek, TLS handshake bound, connection accounting and close reasons.
  - `docs/adr/0019-upstream-connections-monitoring-and-authentication.md`: socket options, handshake metadata, SCRAM through `github.com/xdg-go/scram` behind a thin adapter, monitoring authentication, error outcomes, upstream close reasons.
  - `docs/adr/0020-file-content-settings-for-pem-paths.md`: PEM rule through the SPEC-003 reader (`registry.FileRule`).
  - `docs/adr/0021-startup-and-shutdown-with-the-wire-listener.md`: wire listener as the last startup step, the FR-47 drain inside shutdown, required-setting impact on existing tests.
  - `docs/adr/0022-proxy-test-topology-twins-fixtures-and-hooks.md`: test layers, twins, Docker fixtures, PHP-FPM, test-only driver dependency, corraltest hooks.
- Updated for SPEC-002 Amendment 1:
  - ADR-0017: FR-5 check order, OP_QUERY validation, the nesting-level count, duplicate counts for inspected fields, validate-before-write and hold-back;
  - ADR-0018: L and K normative, the limit checked before TLS, TLS timeout class, no client watch during a barrier, accounting;
  - ADR-0019: FR-62 unsolicited data, not-primary marking by mode, "reached", readiness follows monitoring;
  - ADR-0021: FR-63 second signal;
  - ADR-0022: FR-64, the `shutdown_drain` hold point, `/constants`.
- Updated:
  - ADR-0002: new dependencies.
  - ADR-0004: Amendment 3 label-name allowlist and the outcome/cause rule.
  - ADR-0006: amended by ADR-0021 (SPEC-001 Amendment 3, item 10).
  - ADR-0014: extended by ADR-0022.

## Rules for every task

- **Order inside each task:** the test writer writes the tests first (they fail), then the implementer makes them pass, then review. Test-only tasks (T23 to T29) send defects back to the task that owns the failing code.
- **File ownership** follows ADR-0001. Implementers own the non-test files listed under "Implementer owns". Test writers own `*_test.go`, `testdata/` and `tests/**` as listed. The twin-builder owns `twins/**`. Nobody edits another role's files.
- **Domain rule:** packages under `internal/domain/` import only the standard library (not `log/slog`) and other domain packages (SPEC-001 FR-1, SPEC-003 FR-16). They never read settings, never do network I/O, and take a `clock.Clock` for time.
- **Secrets and data minimisation** (SPEC-001 FR-24, SPEC-002 FR-9, FR-24):
  - Message bytes are never logged, never put in errors, and never kept beyond the exchange.
  - Credentials and TLS key bytes are held only in the typed settings and the SCRAM client, never formatted.
  - Every error put in a log or span goes through `errclass`. Every client-metadata, command or collection string goes through `sanitize`.
  - Test failure messages print lengths and names, never raw values.
- **Synthesized errors** are always fresh `proxy.Refusal` values and never wrap an upstream `ServerError` (ADR-0016).
- **Synthetic data only.** Credentials are random markers, and certificates and keys are generated at runtime into `t.TempDir()`. No `*.pem` or `*.key` file is committed.
- **Log checking:** every suite that emits logs wires in `internal/testkit/logcheck`.
- **Tracing hot path:** per-message tracing calls check `tracing.Enabled()` before `Tracer.Start` (SPEC-001 FR-19, AC-22).
- **Gates:**
  - `make build` and `make test` for every task;
  - plus `make test-twin` from T21 on;
  - plus `make test-functional` (Docker) from T21 on.
- **Test packages:** unit tests use external test packages (`package x_test`) where possible.
- **Test-only dependency:** `go.mongodb.org/mongo-driver/v2` may be imported only in `_test.go` files, `internal/testkit/...`, `tests/...` and `twins/...` (ADR-0022). T29 enforces this.

## Package map (introduced or changed by this plan)

| Package | Task |
|---|---|
| `internal/infra/telemetry/errclass`, `attr`, `logging`, `internal/domain/health`, `internal/testkit/logcheck` | T1 |
| `internal/infra/telemetry/sanitize`, `internal/infra/telemetry/metrics` (`lint.go`) | T2 |
| `internal/domain/clock` (new), `internal/testkit/fakeclock` | T3 |
| `internal/domain/bson` (new) | T4 |
| `internal/infra/upstreamnet` (new) | T5 |
| `internal/infra/upstreamconn/scram` (new) | T6 |
| `internal/testkit/rawwire`, `corralprobe`, `corralproc`, `corralargs` (new) | T7 |
| `internal/testkit/mongofx`, `phpfpm`, `fcgi` (new) | T8 |
| `twins/mongosim`, `twins/faultproxy` (new) | TW1, TW2 |
| `internal/domain/wire` (new) | T9 |
| `internal/domain/upstream` (new) | T10 |
| `internal/domain/pool` (new) | T11 |
| `internal/infra/telemetry/metrics/catalog.json` | T12 |
| `internal/infra/config/registry`, `internal/infra/config/loader` (file-rule extension) | T13 |
| `internal/domain/proxy` (new) | T14 |
| `internal/infra/upstreamconn` (new) | T15 |
| `internal/infra/telemetry/spans` (new) | T16 |
| `internal/app/settings` (`proxy*.go`) | T17 |
| `internal/infra/telemetry/proxyobs` (new) | T18 |
| `internal/infra/proxyserver/exchange` (new) | T19 |
| `internal/infra/proxyserver` (new) | T20 |
| `internal/app/proxyapp` (new), `internal/app/runtime`, `cmd/corral`, `Makefile` | T21 |
| `docs/operations/proxy.md` (new), `docs/operations/configuration.md`, `docs/operations/observability.md` | T22 |

---

## Tasks

### T1: SPEC-001 Amendment 3, part A: error classes and details, readiness reasons, components, `reason` key

- Goal: the Amendment 3 changes to the enums and allowlists that everything else depends on (ADR-0016).
  - `errclass`:
    - add `ClassRejected`, so `Classes()` has 7 values;
    - add the 14 details `pool_wait_timeout`, `pool_queue_full`, `upstream_unavailable`, `upstream_auth_failed`, `upstream_incompatible`, `shutting_down`, `client_auth_unsupported`, `command_blocked`, `handshake_rejected`, `unsupported_opcode`, `message_too_large`, `malformed_message`, `checksum_mismatch`, `unexpected_response`, so `Details()` has 28 values;
    - add `DetailClass(Detail) (Class, bool)` with the fixed classes from SPEC-001 FR-29;
    - add the `DetailedError` contract;
    - new precedence: panic > detailed > config > server > protocol > errno. An unknown detail token falls through.
    - Server attributes still need class `server` and a `ServerError` node in the chain.
  - `health`: add `ReasonUpstreamUnavailable`, `ReasonUpstreamAuthFailed` and `ReasonUpstreamIncompatible`; `Reasons()` returns 6 values.
  - `logging`: add components `Proxy = "proxy"`, `Pool = "pool"` and `Upstream = "upstream"` to `Components()` and `ValidComponent`.
  - `attr`: add `Reason = "reason"`. `logging.AllowedKey` accepts it.
  - `logcheck`: fail on a `reason` attribute whose value kind is not `slog.KindString` (number, bool, group, any).
- FRs/ACs: SPEC-002 FR-52, FR-49 (components, key); SPEC-001 FR-7, FR-18, FR-29, FR-39; SPEC-001 AC-41 (components, readiness set, class and detail enums, detail classes, `reason` handling, synthesized `server` error without status code).
- Implementer owns: `internal/infra/telemetry/errclass/*.go`, `internal/domain/health/health.go`, `internal/infra/telemetry/logging/components.go`, `internal/infra/telemetry/logging/keys.go`, `internal/infra/telemetry/attr/attr.go`, `internal/testkit/logcheck/*.go` (all non-test).
- Test writer owns: `internal/infra/telemetry/errclass/*_test.go`, `internal/infra/telemetry/logging/*_test.go`, `internal/infra/telemetry/attr/*_test.go`, `internal/testkit/logcheck/*_test.go`, `internal/infra/adminhttp/readyz_test.go` (new cases), `internal/domain/health/*_test.go`.
- Interfaces defined:
  - `errclass.ClassRejected`;
  - `errclass.DetailPoolWaitTimeout` … `errclass.DetailUnexpectedResponse` (14 constants);
  - `errclass.DetailedError interface{ error; CorralErrorDetail() string }`;
  - `errclass.DetailClass(d Detail) (Class, bool)`;
  - `health.ReasonUpstreamUnavailable`, `health.ReasonUpstreamAuthFailed`, `health.ReasonUpstreamIncompatible`;
  - `logging.Proxy`, `logging.Pool`, `logging.Upstream`;
  - `attr.Reason`.
- Test strategy (unit):
  - Enum equality: classes are exactly the 7, details exactly the 28, readiness reasons exactly the 6.
  - Classification:
    - each Amendment 3 detail through a fake `DetailedError` gives its declared class;
    - an unknown token falls through to the existing rules;
    - a `DetailedError` wrapping a `net.OpError` still gives the detailed pair;
    - `upstream_auth_failed` wrapping a fake `ServerError` gives `db.response.status_code` in `LogAttrs` and `RecordSpan`;
    - the same detail without a `ServerError` gives no status code (AC-41, last bullet);
    - every existing errclass test stays green.
  - Readiness: an `adminhttp` readiness fake reporting each new reason gives 503 with that body.
  - Logging: the factory accepts `proxy`, `pool` and `upstream`. `logcheck` accepts `reason` with a string value, rejects number, bool, group and any values, and still rejects unknown keys.
- Docker: no.
- Depends on: none. Wave 1.

### T2: SPEC-001 Amendment 3, part B: command allowlist, collection sanitiser, metric label names

- Goal:
  - `sanitize/commands.go` holds exactly the 27 FR-12 names. `sanitize.CollectionForTelemetry` replaces invalid UTF-8 with U+FFFD and does not truncate (SPEC-001 FR-40).
  - `metrics.AllowedLabelNames()` returns the nine FR-11 names.
  - A new lint rule rejects any `outcome`, `reason`, `state`, `side`, `opcode` or `kind` value equal to an FR-29 class or detail value.
- FRs/ACs: SPEC-002 FR-48, FR-52; SPEC-001 FR-11, FR-12, FR-40; SPEC-001 AC-41 (allowlist, label names, lint), AC-8 (rerun), AC-9 Part B, AC-37 (rerun).
- Implementer owns: `internal/infra/telemetry/sanitize/*.go`, `internal/infra/telemetry/metrics/lint.go` (non-test).
- Test writer owns: `internal/infra/telemetry/sanitize/*_test.go`, `internal/infra/telemetry/metrics/*_test.go`, `internal/infra/telemetry/metrics/testdata/**`.
- Interfaces defined: `sanitize.CollectionForTelemetry(string) string`. Existing signatures are unchanged.
- Test strategy (unit):
  - The allowlist equals exactly the 27 names. Each name maps to itself. `GetMore` maps to `other`. `isMaster` and `ismaster` map to `hello`.
  - The label-name allowlist equals exactly the nine names.
  - Lint fixtures:
    - an unknown label name is rejected;
    - `outcome` with value `timeout`, `reason` with `pool_queue_full`, and `state` with `eof` are each rejected;
    - `error_class` including `rejected` is accepted;
    - `error_detail` including every Amendment 3 detail is accepted.
  - Collection sanitiser: invalid UTF-8 replaced; no truncation at 1 KiB.
- Docker: no.
- Depends on: T1. Wave 2.

### T3: Domain clock and fake clock timers

- Goal: the time port for the domain (ADR-0015), and timers in the fake clock.
  - `internal/domain/clock`:
    - `Clock{Now() time.Time; AfterFunc(d time.Duration, f func()) Timer}` and `Timer{Stop() bool}`;
    - `clock.System()`, built on `time.Now` and `time.AfterFunc`.
  - `fakeclock.Clock` satisfies `clock.Clock`:
    - `AfterFunc` registers a timer;
    - `Advance` fires due timers in deadline order (then registration order), outside the lock, and timers registered by a fired callback fire in the same `Advance` when due;
    - `Pending() int`;
    - `WaitForTimers(n int, timeout time.Duration) bool`, for synchronising with goroutines that arm timers.
- FRs/ACs: FR-53 (testability), AC-29.
- Implementer owns: `internal/domain/clock/*.go`, `internal/testkit/fakeclock/*.go` (non-test).
- Test writer owns: `internal/domain/clock/*_test.go`, `internal/testkit/fakeclock/*_test.go`.
- Interfaces defined: as above.
- Test strategy (unit):
  - Fire order. `Stop` before and after firing.
  - A callback that re-arms a timer.
  - Concurrent `AfterFunc` and `Advance` under `-race`.
  - `System()` fires a 1 ms timer.
  - Existing `fakeclock.New` and `Now` users are unaffected.
- Docker: no.
- Depends on: none. Wave 1.

### T4: Domain BSON reader, streaming walker and builder

- Goal: `internal/domain/bson` per ADR-0017.
- FRs/ACs: FR-7 (BSON validity), FR-9 (field reads), FR-58 (fields read as they pass), FR-60 (structural rules, nesting limit, duplicate counts); AC-4 (unit part), AC-7 (fuzz), AC-29, AC-44 (unit: depth and overrun).
- Implementer owns: `internal/domain/bson/*.go` (non-test).
- Test writer owns: `internal/domain/bson/*_test.go`, `internal/domain/bson/testdata/**`. Tests may import `go.mongodb.org/mongo-driver/v2/bson` to build reference documents.
- Interfaces defined:
  - `bson.Type` and the element-type constants.
  - Sentinels `bson.ErrMalformed` and `bson.ErrTooDeep` (constant text, no input bytes).
  - `bson.Validate(doc []byte) error`.
  - `bson.Element` (view into the document, no copy), with:
    - `Key() string`, `Type() Type`;
    - `Int32() (int32, bool)`, `Int64() (int64, bool)`, `Double() (float64, bool)`;
    - `Number() (float64, bool)` (int32, int64 or double);
    - `Bool() (bool, bool)`, `StringValue() (string, bool)`;
    - `Document() ([]byte, bool)`, `Array() ([]byte, bool)`;
    - `Binary() (subtype byte, data []byte, ok bool)`, `ObjectID() ([12]byte, bool)`.
  - `bson.FirstElement(doc []byte) (Element, error)`; `bson.Lookup(doc []byte, path ...string) (Element, bool, error)`.
  - `bson.Capture{Path []string; Mode CaptureMode}`, where the mode is `CaptureValue`, `CaptureFirstKey` (first key of the document at `Path`, for `pipeline.0`) or `CapturePresence`.
  - `bson.NewWalker(captures []Capture) *Walker`; `(*Walker).Write(p []byte) error`; `(*Walker).Close() error`; `(*Walker).Captured(i int) (Value, bool)`; `(*Walker).Consumed() int64`.
    - `Value` holds a type plus a bounded copy of the scalar (strings up to 256 bytes, otherwise `TooLong=true` and no data).
    - **Amendment 1 (FR-60):** `Value.Count int` is the number of occurrences of the key at the top level, for single-segment (top-level) paths. The value kept is the first occurrence (first-wins).
    - `(*Walker).TopLevelKeyEqualsFirst() int` counts later top-level keys equal to the first key (the command name), compared up to the 256-byte bound.
  - Duplicates in general stay accepted by `Validate` and the `Walker`. `Lookup` stays first-wins (ADR-0017).
  - `bson.NewBuilder() *Builder`, with:
    - `AppendInt32`, `AppendInt64`, `AppendDouble`, `AppendBool`, `AppendString`, `AppendDateTime(k string, t time.Time)`, `AppendNull`;
    - `AppendDocument(k string, doc []byte)`, `AppendArray(k string, arr []byte)`, `AppendBinary(k string, subtype byte, b []byte)`;
    - `StartDocument(k)` / `EndDocument()`, `StartArray(k)` / `EndArray()`;
    - `Bytes() []byte`.
- Test strategy (unit):
  - One malformed case per ADR-0017 rule: length < 5, length/content mismatch, missing terminator, unknown type, unterminated key, string length 0, missing string NUL, bool byte 2, nested length overrun, negative binary length, bad code-with-scope.
  - Valid documents built with the driver's encoder for every element type are accepted.
  - Walker equivalence: for each valid and invalid case, `Validate` and the `Walker` agree when the input is split at every byte offset (documents ≤ 4 KiB) and at random chunk sizes (10 MB documents).
  - Captures: `ok` after a 5 MB `cursor` field, `writeConcernError.code`, `cursor.id`, `lsid.id` (binary subtype 4), and the first key of `pipeline.0`. A 1 KiB string capture gives `TooLong` with no data.
  - Depth (FR-60): the top-level document is level 1, and arrays count like documents. A mix of documents and arrays nested 1,024 levels deep is accepted; 1,025 is rejected with `ErrTooDeep`, in both `Validate` and the `Walker`. No recursion (stack-depth test). These tests already exist and match.
  - Duplicate counts (new, Amendment 1):
    - a top-level capture reports `Count` 1, 2 and 3 for one, two and three occurrences, and the value is always the first occurrence;
    - a key that occurs only inside a nested document, or inside an array, does not count toward the top-level `Count`;
    - `Count` is the same under every chunking;
    - `TopLevelKeyEqualsFirst` counts a repeated first key;
    - `Validate` still accepts documents with duplicate keys, and `Lookup` still returns the first occurrence (the existing tests are unchanged).
  - Fuzz (`FuzzValidate`, `FuzzWalker`, short corpus in `make test`): never panics, and `Walker` equals `Validate`.
  - Allocations: `Walker.Write` over a large document allocates O(1) (`testing.AllocsPerRun` bound).
  - Builder: round-trips through the driver's decoder.
- Docker: no.
- Depends on: none. Wave 1.

### T5: Upstream sockets, TLS configuration builders and client liveness peek

- Goal: `internal/infra/upstreamnet` per ADR-0018 and ADR-0019.
  - Dialer with the fixed keepalive values and Linux `TCP_USER_TIMEOUT` (`golang.org/x/sys/unix`).
  - `ClientTLS`: TLS 1.2 or later, verification always on, root CAs from bytes or the system roots.
  - `ServerTLS`: TLS 1.2 or later, key pair from bytes, `RequireAndVerifyClientCert` when CA bytes are given.
  - `WatchClose`: the MSG_PEEK liveness watcher on Linux, a no-op elsewhere and on TLS connections.
- FRs/ACs: FR-28, FR-41 (detection), FR-44, FR-45; AC-14 (unit/review keepalive), AC-24 (unit: TLS configuration from bytes), AC-21 (support).
- Implementer owns: `internal/infra/upstreamnet/*.go` (non-test; `*_linux.go` / `*_other.go` split), `go.mod` / `go.sum` (`golang.org/x/sys` made direct).
- Test writer owns: `internal/infra/upstreamnet/*_test.go`.
- Interfaces defined:
  - Constants `upstreamnet.KeepAliveIdle = 30 * time.Second`, `KeepAliveInterval = 10 * time.Second`, `KeepAliveCount = 3`, `UserTimeout = 60 * time.Second`.
  - `upstreamnet.Dial(ctx context.Context, addr string) (net.Conn, error)`; the deadline comes from `ctx`.
  - `upstreamnet.ClientTLS(caPEM []byte, serverName string) (*tls.Config, error)`; `caPEM == nil` uses the system roots.
  - `upstreamnet.ServerTLS(certPEM, keyPEM, clientCAPEM []byte) (*tls.Config, error)`.
  - `upstreamnet.WatchClose(c net.Conn, stop <-chan struct{}) <-chan struct{}`; the returned channel is closed when a peer close is observed.
  - Amendment 1 (FR-62), for idle upstream connections:
    - `upstreamnet.Peek(c net.Conn) (PeekState, error)`: non-blocking, non-consuming, returning `PeekNone`, `PeekData` or `PeekClosed`. On a `*tls.Conn` it peeks the underlying TCP socket (`NetConn()`), so any TLS record on an idle connection counts as data.
    - `upstreamnet.WatchReadable(c net.Conn, stop <-chan struct{}) <-chan PeekState`.
    - Linux implementation through `RawConn`; other platforms fall back to checking only at checkout, with a zero-deadline read that is treated as a fault when it returns data (the connection is then discarded anyway).
- Test strategy (unit, loopback):
  - After `Dial`, on Linux, `getsockopt` shows `SO_KEEPALIVE=1`, `TCP_KEEPIDLE=30`, `TCP_KEEPINTVL=10`, `TCP_KEEPCNT=3` and `TCP_USER_TIMEOUT=60000` (AC-14).
  - TLS:
    - a server with a synthetic certificate and the right CA connects;
    - with the wrong CA the handshake fails and `errclass.Classify` gives `network/tls_handshake`;
    - a host-name mismatch fails;
    - TLS 1.1 is refused;
    - with a client CA set, a client without a certificate is refused and one with a valid certificate is accepted;
    - certificate and key bytes are used as given (no file access in the package; review).
  - `WatchClose`:
    - a peer close is detected within 100 ms;
    - pending data stops the watch and the byte is still readable afterwards (not consumed);
    - `stop` ends the watcher;
    - on a TLS connection, nothing is detected.
  - `Peek` and `WatchReadable` (Amendment 1): data versus close on plaintext and TLS sockets; data is not consumed; `PeekNone` on a quiet socket returns at once.
- Docker: no.
- Depends on: none. Wave 1.

### T6: SCRAM-SHA-256 adapter around `github.com/xdg-go/scram`

- Goal: `internal/infra/upstreamconn/scram`, a thin adapter around the maintained `github.com/xdg-go/scram` library (Owner decision, 2026-10-24; ADR-0019). No in-house SCRAM.
  - **Only the password is SASLprepped**, for parity with the official Go driver (ADR-0019). The adapter builds the client with:

    ```go
    scram.SHA256.NewClientUnprepped(username, stringprep.SASLprep.Prepare(password), "")
    ```

    - `NewClient` is not used, because it would also prepare the username.
    - The username is passed unchanged, apart from the library's RFC 5802 escaping.
    - A password that fails preparation gives `ErrSASLprep`.
  - The library does username escaping, nonces and the derived-key cache. The adapter holds one library `Client` per process and one `ClientConversation` per authentication.
  - The adapter adds:
    - shape checks on server messages:
      - an iteration count below 4096, or above the upper bound of 1,000,000 (the unexported constant `maxIterations`; rationale in ADR-0019), gives `ErrIterations`. The check runs before any PBKDF2, so a hostile upstream cannot force unbounded work;
      - an `e=` server-final or malformed content gives `ErrProtocol`;
    - mapping of a failed `v=` verification (a `Step` error on a well-formed `v=` message, or `Valid() == false` at the end) to `ErrServerSignature`;
    - constant sentinels as the only errors that leave the package. Library error values are never wrapped or logged.
- FRs/ACs: FR-22 (failure classification), FR-24; AC-25 (unit: failed signature); AC-11 (support).
- Implementer owns: `internal/infra/upstreamconn/scram/*.go` (non-test), `go.mod` / `go.sum` (`github.com/xdg-go/scram` and what it selects; versions recorded in ADR-0002).
- Test writer owns: `internal/infra/upstreamconn/scram/*_test.go`.
- Interfaces defined:
  - `scram.NewClient(username, password []byte) (*Client, error)`. It wraps `scram.SHA256.NewClientUnprepped` with the password prepared by `stringprep.SASLprep.Prepare` and the username unprepared; a preparation failure gives `ErrSASLprep`.
  - Unexported constant `maxIterations = 1_000_000` (the upper bound; reason recorded in ADR-0019). It is not part of the package API.
  - `(*Client).NewConversation() *Conversation`.
  - `(*Conversation).First() ([]byte, error)`; `(*Conversation).Step(server []byte) (reply []byte, done bool, err error)`.
  - Sentinels `scram.ErrServerSignature`, `scram.ErrProtocol`, `scram.ErrSASLprep`, `scram.ErrIterations`. Constant text only.
- Test strategy (unit; a test-side SCRAM server built with the library's server API, `scram.SHA256.NewServer`, with synthetic credentials):
  - A conversation against the library server succeeds.
  - A tampered `v=` gives `ErrServerSignature`.
  - An `e=invalid-proof` server-final gives `ErrProtocol`, not `ErrServerSignature`.
  - A malformed server-first or server-final message, and a nonce that does not extend the client nonce, give `ErrProtocol`.
  - Iteration count 4095 gives `ErrIterations`. 1,000,000 iterations is accepted, and 1,000,001 gives `ErrIterations` before any key derivation; for example, the call returns promptly and the test server's later messages are never requested.
  - A username containing `,` or `=` authenticates.
  - A non-ASCII-space password authenticates after SASLprep; a prohibited character gives `ErrSASLprep`.
  - Username not prepared (driver parity):
    - a username containing a character that SASLprep would map, for example U+00A0, authenticates against a test server whose stored user is exactly that unprepared name;
    - it fails against a server that stores the SASLprep-mapped form.
  - A second conversation with the same salt and iteration count reuses the derived keys. Assert through timing-independent means, for example by counting server-first messages per derived-key computation if the library exposes it; otherwise this is a review item.
  - The marker username and password never appear in any error text (`fmt.Sprint(err)`).
- Docker: no.
- Depends on: none. Wave 1.

### T7: Test kit: raw wire client, process harness, minimal arguments, probe client

- Goal: shared helpers for twin, functional and in-process tests (ADR-0022). The wire encoding is independent of `internal/domain` (it uses the driver's `bson`).
  - `rawwire`:
    - dial (plain or TLS);
    - build OP_MSG (flags, kind-0 body, kind-1 sequences, optional correct or incorrect CRC-32C, chosen `requestID`);
    - build OP_QUERY (full collection name, query document, optional `$query` wrapper);
    - build arbitrary raw bytes (bad lengths, unknown opcodes, HTTP `GET`);
    - send in one write, or split with pauses;
    - `ReadMessage(timeout)` returns the header, flags, documents and raw bytes, or `ErrClosed` / `ErrTimeout`;
    - `ExpectClosedNoBytes(timeout)`.
  - `corralproc`:
    - starts the corral binary (default or corraltest) with arguments and environment;
    - captures stdout JSON records and stderr;
    - waits for a record (`msg` / `component`);
    - sends signals, returns the exit code;
    - reads `/readyz` and `/metrics`;
    - creates and releases `CORRALTEST_HOLD_DIR` hold files (`held-<point>` / `release-<point>`, and the SPEC-003 `held` / `release`).
  - Amendment 1 needs for `rawwire`:
    - bodies with duplicate top-level keys (ordered `bson.D` or raw bytes);
    - kind-1 sections placed before or after the body;
    - documents nested N levels with mixed documents and arrays;
    - OP_QUERY with missing or extra fields;
    - a client TLS dial that sends a partial ClientHello and stalls.
  - `corralproc` must be able to send repeated signals.
  - `corralargs`: `Minimal(t, upstream string) []string` returns the ADR-0021 minimal arguments (`--upstream.hosts`, `--upstream.auth.mechanism=none`, `--proxy.listen=127.0.0.1:<free port>`); `FreePort(t)`.
  - `corralprobe`: client for the corraltest probe (`/conns`, `/constants`).
- FRs/ACs: support for FR-54 and the twin and functional ACs.
- Implementer owns: `internal/testkit/rawwire/*.go`, `internal/testkit/corralproc/*.go`, `internal/testkit/corralargs/*.go`, `internal/testkit/corralprobe/*.go` (non-test), `go.mod` / `go.sum` (`go.mongodb.org/mongo-driver/v2`, test-only use).
- Test writer owns: their `*_test.go` self-tests. The `rawwire` self-test round-trips through a loopback echo. The `corralproc` self-test runs a helper process from a `testdata` main.
- Interfaces defined: as above (exact signatures left to the implementer; documented in package docs).
- Docker: no.
- Depends on: none. Wave 1.

### T8: Test kit: Docker topologies, PHP-FPM fixture, FastCGI client

- Goal: functional fixtures per ADR-0022.
  - `mongofx`:
    - `StartReplicaSet(t, Opts{Version string; Members int; Auth *Auth; TLS *TLSMaterial})` and `StartShardedOneMongos(t, Opts)`;
    - returns member and `mongos` addresses, an admin Go-driver client connected directly (not through corral), `Logs(member)`, `Stop(member)` / `Start(member)` and `Exec(member, cmd)`;
    - `Auth` creates a synthetic root user and a synthetic service user with only `readWrite` on the test databases. The keyfile is generated at runtime.
    - `TLSMaterial` generates a CA, a server certificate for `127.0.0.1` and `localhost`, and a client certificate.
    - Images are pinned by digest in `images.go`.
    - Docker runs through the CLI with `--network host` (Linux).
    - A missing Docker daemon fails when `CORRALTEST_REQUIRE_DOCKER=1`, otherwise skips.
  - `phpfpm`:
    - builds the embedded Dockerfile (PHP FPM base + pinned PECL `mongodb`, `pm=static`);
    - `Start(t, Opts{Workers int; ScriptsDir string})`;
    - `Run(script string, params map[string]string) (stdout []byte, err error)` over FastCGI.
  - `fcgi`: a minimal FastCGI client (BEGIN_REQUEST, PARAMS, STDIN, reading STDOUT, STDERR and END_REQUEST).
- FRs/ACs: FR-54 (matrix); support for AC-11, AC-12, AC-14, AC-16, AC-18, AC-20, AC-24, AC-25 and AC-27.
- Implementer owns: `internal/testkit/mongofx/**`, `internal/testkit/phpfpm/**` (including the embedded `Dockerfile` and `php-fpm.conf`), `internal/testkit/fcgi/**` (non-test).
- Test writer owns: their `*_test.go` (build tag `functional` for the Docker self-tests). The `fcgi` unit test runs against a Go `net/http/fcgi` server.
- Docker: yes (self-tests under `make test-functional`). The `fcgi` unit test does not need it.
- Depends on: T7 (`corralargs.FreePort`). Wave 1 (start with `fcgi`; the rest can follow T7 in the same wave).

### TW1: Twin `twins/mongosim` (simulated MongoDB upstream) [twin-builder]

- Goal: the simulated upstream for every twin AC. The full contract is under "Twins".
- Twin-builder owns: `twins/mongosim/**`, including self-tests that use the official Go driver against it.
- Depends on: none (it uses `go.mongodb.org/mongo-driver/v2`; it may add it to `go.mod` if T7 has not). Wave 1.

### TW2: Twin `twins/faultproxy` (fault-injecting intermediary) [twin-builder]

- Goal: per-connection command recording and fault injection in front of a real `mongod` (AC-16, AC-20 with real drivers). The full contract is under "Twins".
- Twin-builder owns: `twins/faultproxy/**`, including self-tests against `twins/mongosim` (no Docker).
- Depends on: none (self-tests use TW1). Wave 1.

### T9: Domain wire model

- Goal: `internal/domain/wire` per ADR-0017. It covers:
  - header and opcode checks (FR-5, FR-6);
  - OP_MSG streaming validation (FR-7) and FR-38 prefix classification (deciding fields, local-message fit);
  - OP_QUERY handshake detection and the OP_REPLY builder (FR-8);
  - FR-9 request, handshake and reply field extraction, and the FR-40 collection list;
  - correlation (FR-11);
  - header rewriting with checksum stripping and `exhaustAllowed` clearing (FR-10, FR-30);
  - upstream reply checks (FR-58);
  - builders for corral's own messages (hello, `saslStart`, `saslContinue`, `ping`);
  - parsers for `hello` and SASL replies (FR-9 own-exchange fields only).
- **Amendment 1 additions** (ADR-0017):
  - FR-5:
    - signed `messageLength` and `opCode`;
    - per-opcode minimums (OP_MSG 26, OP_QUERY 34, others 16);
    - the fixed check order (< 16 or negative; > maximum; opcode; < opcode minimum; body checks in byte order; checksum last).
  - FR-7 and FR-60:
    - full recursive validation of the body and of every kind-1 document;
    - the nesting limit of 1024 levels (top-level = 1, arrays count);
    - no UTF-8 check;
    - inspected-field integrity:
      - an applicable inspected field (command name, `$db`, `lsid`, `txnNumber`, `autocommit`, `startTransaction`, plus `pipeline` for `aggregate` and `collection` for `getMore`) occurring more than once at the top level is `malformed_message`;
      - a kind-1 identifier equal to an applicable inspected field name (excluding the command name; see "Proposed for Owner" A-Q1) is `malformed_message`;
      - other duplicates are not checked.
  - FR-8: OP_QUERY structural validation (terminated `fullCollectionName`, `numberToSkip` and `numberToReturn` present, a query document and at most one `returnFieldsSelector`, both passing FR-60, no trailing bytes, `flags` ignored). Handshake names are exact and case-sensitive (FR-13).
  - FR-9 and G-20: `writeErrors` is never read; `ReplyFields` has no field for it.
  - FR-38: `MsgValidator` exposes `Done() bool`, true once every check including the checksum has passed, so the engine can release the held-back final byte (T19).
  - New in `wire.Request`: `Kind1Identifiers` is not exposed. Only the integrity verdict leaves the validator (FR-9 minimisation).
- FRs/ACs: FR-5 to FR-11, FR-30, FR-38 (classification part), FR-58 (validation part), FR-60; SPEC-001 FR-40; AC-4 (unit), AC-5 (unit: OP_QUERY detection), AC-6 (unit: rewrite differences), AC-7, AC-8 (unit: correlator), **AC-37**, **AC-38 (unit part)**, **AC-43 (unit: `writeErrors` not read)**, **AC-44 (unit part)**, **AC-49 (unit part)**, SPEC-001 AC-39.
- Implementer owns: `internal/domain/wire/*.go` (non-test).
- Test writer owns: `internal/domain/wire/*_test.go`, `testdata/**` (the driver's `bson` may be used to build inputs).
- Interfaces defined:
  - Header and opcodes:
    - `wire.Header{Length, RequestID, ResponseTo int32; OpCode OpCode}`; `wire.ParseHeader(b []byte) Header`; `(Header).AppendTo(dst []byte) []byte`;
    - `wire.OpMsg`, `OpQuery`, `OpReply`, `OpCompressed`, and the other legacy opcode constants;
    - `wire.DefaultMaxMessageSize = 48_000_000`;
    - `wire.CheckClientHeader(h Header, maxLen int32) error` (FR-5, FR-6); `wire.CheckReplyHeader(h Header, maxLen int32) error`.
  - Errors: `wire.ProtocolError` with `Detail() string` and `CorralErrorDetail() string` (ADR-0016); constructors `wire.ErrUnsupportedOpcode`, `ErrMessageTooLarge`, `ErrMalformed`, `ErrChecksum`, `ErrUnexpectedResponse`.
  - Flags: `wire.FlagChecksumPresent`, `FlagMoreToCome`, `FlagExhaustAllowed`.
  - Request classification:
    - `wire.Request` holds only:
      - `Kind` (`KindOpMsg`, `KindOpQueryHandshake`, `KindOpQueryOther`), `RequestID`, `Flags`, `Command`;
      - `DB`, `HasLSID`, `LSID [16]byte`, `HasTxnNumber`, `AutocommitFalse`, `StartTransaction`;
      - `Collection` (`HasCollection`), `CursorID` (`HasCursorID`, `getMore` only), `FirstStage` (aggregate only);
      - `Handshake *Handshake`, `FitsPrefix bool`.
    - `wire.Handshake` holds only:
      - `Name` (`hello`, `isMaster`, `ismaster`), `HelloOK`, `LoadBalanced`;
      - `HasTopologyVersion`, `HasMaxAwaitTimeMS`;
      - `HasClient`, `HasCompression`, `HasSpeculativeAuth`, `HasSaslSupportedMechs`;
      - `AppName`, `DriverName`, `DriverVersion` (raw, each bounded to 256 bytes);
      - `ViaOpQuery`.
    - `wire.ClassifyPrefix(prefix []byte, total int32) (Request, error)`.
  - Streaming validation and reply scanning:
    - `wire.NewMsgValidator(h Header, isReply bool) *MsgValidator`; `(*MsgValidator).Write(p []byte) error`; `(*MsgValidator).Close() error`;
    - `wire.NewReplyScanner(h Header) *ReplyScanner`, which embeds a validator; `(*ReplyScanner).Fields() ReplyFields`;
    - `wire.ReplyFields{HasOK bool; OK float64; HasCode bool; Code int32; CodeName string; HasWCECode bool; WCECode int32; HasCursorID bool; CursorID int64}`.
  - Header rewriting: `wire.RewriteRequestHeader(hdr []byte, newID int32) (out [20]byte, newLength int32)` (header plus flags, checksum bit cleared, `exhaustAllowed` cleared, length −4 when the checksum is stripped); `wire.RewriteReplyHeader(hdr []byte, newID, responseTo int32) [20]byte`.
  - Correlation: `wire.NewCorrelator() *Correlator` with `Next() int32`, `Match(responseTo int32) error` and `Outstanding() int`; `wire.NewClientIDs() *ClientIDs` with `Next() int32`.
  - Builders:
    - `wire.BuildMsg(requestID, responseTo int32, flags uint32, body []byte) []byte`;
    - `wire.BuildReply(requestID, responseTo, responseFlags int32, doc []byte) []byte`;
    - `wire.PingBody() []byte`;
    - `wire.HelloBody(app, driverVersion, osType string) []byte`;
    - `wire.SaslStartBody(source string, payload []byte) []byte`; `wire.SaslContinueBody(source string, convID int32, payload []byte) []byte`.
  - Reply parsers:
    - `wire.ParseHelloReply(doc []byte) (HelloReply, error)`, reading only the FR-9 own-exchange fields plus the upstream parameters;
    - `wire.ParseSaslReply(doc []byte) (SaslReply, error)` (`OK`, `Code`, `CodeName`, `ConversationID`, `Done`, `Payload`).
  - FR-40: `wire.CollectionCommands() []string`.
- Test strategy (unit):
  - AC-4 table: length 15; above maximum; HTTP `GET`; OP_COMPRESSED; each legacy opcode; an unknown opcode; each of required flag bits 2 to 15; bad CRC; two kind-0 sections; unknown section kind; truncated section; invalid BSON; empty body. Each case gives the exact detail. A valid checksummed OP_MSG with a kind-1 section is accepted.
  - Prefix: command name, or the first `pipeline` stage of `aggregate`, outside a 64 KiB prefix gives `malformed_message`. A `hello` larger than the prefix gives `message_too_large`.
  - OP_QUERY: `admin.$cmd` with `isMaster`, `hello` and `ismaster`; the `$query` wrapper with `$readPreference`; `find` gives `KindOpQueryOther`.
  - FR-9 exposure (AC-7, review-backed):
    - `Request`, `Handshake` and `ReplyFields` have only the listed fields (reflection test on field names);
    - a `saslSupportedMechs` marker username and a marker in `client.os` appear in no output (`fmt.Sprintf("%+v")` scan).
  - Rewrite: byte diff of the original and the rewritten header shows only `requestID`, `responseTo`, the checksum bit, the length (−4) and `exhaustAllowed` (AC-6 unit).
  - Correlator: an unknown `responseTo` gives `unexpected_response`.
  - Reply scanner:
    - `moreToCome` gives `unexpected_response`;
    - `ok` placed after a 5 MB `cursor` is captured under 4 KiB chunks;
    - `writeConcernError.code` is captured.
  - FR-40 (SPEC-001 AC-39): every listed command yields a collection, `getMore` uses `collection`, `aggregate: 1` yields none, and every user, role and auth command with a marker first value yields none.
  - Fuzz: `ClassifyPrefix`, `MsgValidator` and `ReplyScanner` never panic. Validation is the same at every chunking.
  - AC-37 (table-driven over the 16-byte header and minimal bodies):
    - each opcode's minimum length is accepted (OP_MSG 26 with an empty body then fails FR-7), and the minimum − 1 fails with `malformed_message`;
    - lengths −1, 0, 15 and `INT32_MIN` give `malformed_message`;
    - every pair of faults reports the first in FR-5 order. This includes: an unknown opcode with length 10 gives `malformed_message`; an unknown opcode with 60,000,000 gives `message_too_large`; an unknown flag plus a bad checksum gives `malformed_message`; a bad checksum alone gives `checksum_mismatch`.
  - AC-38 (unit part): an unterminated `fullCollectionName`, a missing `numberToReturn`, an invalid query document, and trailing bytes after `returnFieldsSelector` each give `malformed_message`. A valid non-handshake OP_QUERY classifies as `KindOpQueryOther`. `HELLO` and `IsMaster` over OP_QUERY are not handshakes.
  - AC-44 (unit part), each `malformed_message`:
    - `aggregate` with no body `pipeline` and a kind-1 `pipeline` section, in both section orders (before and after the body);
    - two top-level `pipeline` fields;
    - two `$db` fields;
    - a repeated command name;
    - a kind-1 document with an overrunning inner length;
    - 1,025 levels of nesting.

    Accepted: 1,024 levels; an `insert` with kind-1 `documents`; invalid UTF-8 in a field that is not inspected; duplicate keys in a field that is not inspected; a duplicated `collection` on a non-`getMore` command; and `pipeline` duplicated on a non-`aggregate` command.
  - AC-49 (unit part): for a 5 MB `aggregate` whose second `pipeline` is in the last kilobytes, `MsgValidator.Write` fails only on the chunk containing it, and `Done()` is false. With a wrong checksum, `Close()` returns `checksum_mismatch` and `Done()` is false.
  - `ReplyScanner` over a reply with `writeErrors[0].code: 10107` reports no code (AC-43 unit).
- Docker: no.
- Depends on: T4. Wave 2.

### T10: Domain upstream state, monitoring policy and readiness

- Goal: `internal/domain/upstream`. It covers:
  - known members and discovery from any matching member's `hosts`, `passives` and `primary` (FR-37);
  - the check schedule: `upstream.heartbeat_interval` while a target is known; all known members at least 500 ms apart while none is known; immediate checks (FR-37);
  - SDAM primary selection and the `(electionId, setVersion)` stale rule (FR-35);
  - the retry-once rule for a previously usable target, which is then marked unknown (FR-35);
  - compatibility for both modes with the first-qualifying-seed rule, wire-version overlap 21 to 25, transitions, and non-matching seeds dropped once a member is known (FR-18);
  - the authentication state and its recovery (FR-22);
  - availability for requests (FR-56) and for handshakes (FR-19);
  - readiness (FR-46) implementing `health.Readiness`;
  - not-primary marking (FR-35).

  Checks run through the `Checker` port in goroutines. Results are applied under a mutex, and observers are called after state changes, outside the lock.
- **Amendment 1 additions** (ADR-0019):
  - "Reached" (FR-19, G-8): the first target check whose `hello` passed FR-18 sets `Reached` and the parameters, whatever the authentication outcome on that monitoring connection. With authentication failing since startup:
    - `HandshakeAdmission` gives OK with the parameters;
    - `Admission` gives FailFast `upstream_auth_failed`;
    - readiness is `upstream_auth_failed`.
  - Readiness follows monitoring (FR-46, G-9): `PoolConnectFailed` only triggers an immediate check and never changes readiness itself. `PoolAuthResult` still sets the authentication state.
  - Not-primary marking by mode (FR-35, G-13, G-20, FR-29 G-12). `NotPrimary` is replaced by:

    `ReplyCodes(ctx context.Context, host string, src ReplySource /* Forwarded, Barrier */, f ReplyCodeInfo) (marked bool)`

    where `ReplyCodeInfo{HasTop bool; OKZero bool; Top int32; TopName string; HasWCE bool; WCE int32}`.
    - Replica-set mode: the FR-35 list as a top-level `ok: 0` code, or as `writeConcernError.code` in `ok: 1`.
    - Single-`mongos` mode: only a top-level 91 or 11600 in `ok: 0`.
    - Barrier replies: top-level code only, same mode rule.
    - It returns `marked`, so the engine can fail the barrier.
- FRs/ACs: FR-18, FR-19 (decision), FR-22 (state), FR-34, FR-35, FR-37, FR-46, FR-56 (mode decision); AC-10 (unit), AC-18 (unit), AC-25 (unit), AC-32 (unit: modes), **AC-41 (unit)**, **AC-42 (unit: marking from the ping reply)**, **AC-43 (unit)**, AC-2 Part B.
- Implementer owns: `internal/domain/upstream/*.go` (non-test).
- Test writer owns: `internal/domain/upstream/*_test.go`.
- Interfaces defined:
  - Configuration: `upstream.Config{Seeds []string; ReplicaSet string; HasReplicaSet bool; HeartbeatInterval time.Duration; Authenticate bool}`; constants `upstream.MinHeartbeat = 500 * time.Millisecond`, `WireMin = 21`, `WireMax = 25`.
  - Parameters: `upstream.Params{MaxBsonObjectSize, MaxMessageSizeBytes, MaxWriteBatchSize int32; LogicalSessionTimeoutMinutes int32; HasLSTM bool; MinWire, MaxWire int32}`.
  - Check reports: `upstream.Report{Host string; Err error; IsWritablePrimary, Secondary, ArbiterOnly, IsMongos bool; SetName string; HasSetName bool; Hosts, Passives []string; Primary string; ElectionID [12]byte; HasElectionID bool; SetVersion int64; HasSetVersion bool; Params Params}`.
  - `Report` also carries `AuthErr error`: the outcome of authentication on that check, separate from `Err` (Amendment 1, G-8).
  - Checker port: `upstream.CheckRequest{Host string; Authenticate bool; Fresh bool}`; `upstream.Checker interface{ Check(ctx context.Context, req CheckRequest) Report }`.
  - Errors: `upstream.AuthFailed` (error type; `Unwrap` gives the cause; `CorralErrorDetail()` = `upstream_auth_failed`); `upstream.IsAuthFailed(err) bool`.
  - Monitor:
    - `upstream.NewMonitor(cfg Config, chk Checker, clk clock.Clock, obs Observer) *Monitor`;
    - `Start(ctx)`, `Stop(ctx)`;
    - `State() State`;
    - `Readiness(ctx) health.Status`;
    - `Admission() Admission`;
    - `HandshakeAdmission() (Params, string /* "" or detail */)`;
    - `ReplyCodes(ctx, host string, src ReplySource, f ReplyCodeInfo) bool` (Amendment 1; replaces `NotPrimary`);
    - `PoolConnectFailed(ctx, host string, err error)`;
    - `PoolAuthResult(ctx, host string, err error)`;
    - `OnChange(func(Change))`, used by the app to drive `pool.AdmissionChanged` / `CloseHost` / `AbortBarriers`.
  - `upstream.State{Reached, Incompatible bool; IncompatReason Reason; IncompatHost string; AuthFailing bool; Target string; HasTarget bool; Params Params; HasParams bool}`.
  - `upstream.Admission{Kind AdmissionKind /* Ready, Wait, FailFast */; Target string; Detail string}`.
  - `upstream.Change{Kind ChangeKind /* TargetChanged, HostNotPrimary, HostUnknown, BecameIncompatible, AvailabilityChanged */; Host string; Reason string}`.
  - Pure functions: `upstream.EvaluateReadiness(State) health.Status`; `upstream.WireAccepted(min, max int32) bool`.
  - `upstream.Reason` incompatibility tokens: `wire_version`, `standalone`, `topology_mismatch`, `replica_set_name_mismatch`.
  - `upstream.Observer`, with `Noop`:
    - `AvailabilityChanged(ctx, ready bool, reason health.Reason)`;
    - `Incompatible(ctx, reason Reason, host string)`;
    - `Available(ctx)`;
    - `PrimaryChanged(ctx, host string)`;
    - `TargetMarkedNotPrimary(ctx, host string, code int32, codeName string)`;
    - `MonitorConnOpened(ctx, host string, id uint64)`, `MonitorConnClosed(ctx, host string, reason string)`, `MonitorConnFailed(ctx, host string, err error)`;
    - `AuthSucceeded(ctx, kind, host string)`, `AuthFailed(ctx, kind, host string, err error)`.
- Test strategy (unit, fake clock, fake `Checker` returning scripted reports):
  - Readiness over all combinations of (reached, compatible, target known, auth failing): the precedence `upstream_incompatible` > `upstream_auth_failed` > `upstream_unavailable`, and exactly one reason (AC-25).
  - Authentication state:
    - an `AuthFailed` from a pool or monitor sets failing;
    - errors carrying codes 10107, 91 and 11600, a network error and a timeout do not;
    - while failing, each target check asks for `Fresh: true, Authenticate: true`;
    - the first success clears failing (AC-25).
  - Stale primary: the larger `(electionId, setVersion)` wins, and a later smaller report is ignored (AC-18).
  - Discovery from a secondary's `hosts`, `passives` and `primary` (AC-18). `arbiterOnly` members are dropped.
  - While no target is known, each known member is checked at 500 ms intervals on the fake clock (AC-18).
  - Compatibility:
    - `replica_set_name_mismatch`, `standalone` and `topology_mismatch` in replica-set mode, with the reason taken from the first qualifying seed in `upstream.hosts` order;
    - single-`mongos` mode: `topology_mismatch` and `standalone`;
    - wire 6–17 and 26–30 give `wire_version`; 6–27, 21–21 and 25–30 are accepted (AC-10);
    - a non-matching seed next to a matching member is ignored, and is no longer checked;
    - losing all matching members later gives `upstream_unavailable`, not incompatible.
  - Retry once: one failed target check followed by a successful retry leaves the target usable. Two failures mark it unknown and switch to the fast schedule (AC-32 unit).
  - Admission:
    - never reached gives FailFast `upstream_unavailable`;
    - auth failing gives FailFast `upstream_auth_failed`;
    - incompatible gives FailFast `upstream_incompatible` (precedence when combined);
    - no target, or not primary, gives Wait.
  - Handshake admission: auth failing gives OK with the cached parameters; incompatible combined with never reached gives `upstream_incompatible` (FR-19).
  - Not-primary: the host is marked at once, an immediate check is scheduled (still at least 500 ms after the previous check), and admission is Wait until a check confirms a primary.
  - Amendment 1:
    - AC-41 (unit): authentication failing from startup with successful `hello` checks gives handshake OK, request FailFast `upstream_auth_failed` and readiness `upstream_auth_failed`. Pool connect failures with successful checks keep readiness ready and admission Wait while the target is known. With the checks and their retries also failing, readiness becomes `upstream_unavailable`.
    - AC-43 (unit):
      - single-`mongos` mode: top-level `ok: 0` codes 10107, 13435, 189 and 11602, and `writeConcernError` 91, do not mark; top-level 91 and 11600 do.
      - replica-set mode: every FR-35 code marks, top-level or `writeConcernError`.
    - AC-42 (unit): a barrier reply with 10107 or 91 marks in replica-set mode. In single-`mongos` mode only 91 marks.
  - AC-2 Part B: every scenario runs with `Noop` and with a recording fake. The results are the same, and the fake records the calls with the caller's context.
- Docker: no.
- Depends on: T1 (health reasons), T3. Wave 2.

### T11: Domain pool: leases, wait queue, fail-fast gate, backoff, host marking

- Goal: `internal/domain/pool`, generic over the connection type. It covers:
  - FR-25: the maximum counts connections being opened; the minimum is kept while the gate is Ready.
  - FR-26: one lease per checkout; `Return` and `Discard`.
  - FR-27: a FIFO queue bounded by `MaxWaiters`; `pool_queue_full` at once; `pool_wait_timeout` after `CheckoutTimeout`; context cancellation removes the waiter without a failure record (client gone, FR-41).
  - FR-56:
    - a FailFast admission fails new and queued checkouts at once with its detail;
    - a Wait admission queues without opening anything;
    - opens happen only while Ready, to the target;
    - failed opens are delayed by the backoff (100 ms, doubling, 5 s cap, ±20 % jitter, reset on success).
  - FR-28: on-demand opens; idle expiry after `MaxIdleTime` above the minimum.
  - FR-29 and FR-35: `CloseHost(host, reason)` closes idle connections at once, closes leased ones on return, and aborts barriers. `AbortBarriers(host)` only aborts barriers (target unknown). `Lease.Aborted()` exposes the abort.
  - FR-18: incompatible gives `CloseHost(host, incompatible)`.
  - FR-47 step 5: `Close` for shutdown.
  - `OrderRoundRobin` for the AC-16 hook.
- **Amendment 1 addition (FR-62, G-17; ADR-0019):**
  - `pool.Conn` gains `CheckIdle() error`, a non-blocking probe implemented by infrastructure. At checkout, before handing a connection out, the pool calls it. On error, the pool:
    - discards the connection (`reason=error`);
    - reports it (`ConnUnsolicited` for bytes, `ConnClosed` for a peer close);
    - continues the **same** checkout with another connection (idle, newly opened under the bounds, or waiting), with its wait time and `CheckoutTimeout` unchanged.
  - `(*Pool[C]).DiscardIdle(c C, err error)` is for the infrastructure idle watcher (T15). It removes an idle connection at once with `reason=error`. It is a no-op if the connection is no longer idle (leased or already closed).
  - The observer gains `ConnUnsolicited(ctx, host string)`, which drives `corral_protocol_errors_total{side="upstream",error_detail="unexpected_response"}` and the `upstream protocol error` record (T18).
- FRs/ACs: FR-25 to FR-29 (pool side), FR-31, FR-32 (no pinning), FR-35 (pool side), FR-41 (queued), FR-56, **FR-62 (pool side)**; AC-12 (unit bounds), AC-13 (unit), AC-14 (unit abort), AC-16 (unit order), AC-21 (unit), AC-32 (unit backoff), **AC-46 (unit)**, AC-2 Part B.
- Implementer owns: `internal/domain/pool/*.go` (non-test).
- Test writer owns: `internal/domain/pool/*_test.go`.
- Interfaces defined:
  - Ports: `pool.Conn interface{ Host() string; Close(reason CloseReason); CheckIdle() error }` (`CheckIdle` added by Amendment 1, FR-62); `pool.Opener[C Conn] interface{ Open(ctx context.Context, host string) (C, error) }`; `pool.Gate interface{ Admission() Admission }`.
  - `pool.Admission{Kind AdmissionKind /* Ready, Wait, FailFast */; Target string; Detail string}`.
  - `pool.Config{MaxSize, MinSize, MaxWaiters int; CheckoutTimeout, MaxIdleTime time.Duration; Order Order /* OrderMRU, OrderRoundRobin */; Rand func() float64}`.
  - `pool.New[C Conn](cfg Config, op Opener[C], gate Gate, clk clock.Clock, obs Observer) *Pool[C]`.
  - Checkout: `(*Pool[C]).Checkout(ctx context.Context) (*Lease[C], error)`. The error is `*pool.CheckoutError`, whose `Detail()` is one of the six `corral_pool_checkout_failures_total` details and which implements `CorralErrorDetail()`.
  - Lease: `(*Lease[C]).Conn() C`, `Host() string`, `Return()`, `Discard(reason CloseReason)`, `Aborted() <-chan struct{}`.
  - Host and lifecycle control: `(*Pool[C]).AdmissionChanged()`, `CloseHost(host string, reason CloseReason)`, `AbortBarriers(host string)`, `Close(ctx context.Context)`.
  - `(*Pool[C]).Stats() Stats{Idle, InUse, Connecting, Waiters int}`.
  - `pool.CloseReason`: `idle`, `error`, `not_primary`, `client_abandoned`, `shutdown`, `incompatible`.
  - Backoff: `pool.Backoff` (`Next() time.Duration`, `Reset()`) with constants `BackoffInitial = 100 * time.Millisecond`, `BackoffMax = 5 * time.Second`, `BackoffJitter = 0.2`.
  - `pool.Observer`, with `Noop`:
    - `CheckoutStarted(ctx)`, `CheckoutQueued(ctx)`;
    - `CheckoutSucceeded(ctx, wait time.Duration)`, `CheckoutFailed(ctx, detail string, wait time.Duration)`;
    - `ConnOpening(ctx, host string)`, `ConnOpened(ctx, host string, id uint64)`, `ConnOpenFailed(ctx, host string, err error)`;
    - `ConnClosed(ctx, host string, reason CloseReason)`;
    - `StatsChanged(ctx, Stats)`.
- Test strategy (unit, fake clock, fake opener with scripted latency and results, fake gate):
  - Bounds: with `MaxSize=4` and 200 concurrent checkouts, at most 4 connections exist at any instant, counting `Connecting` (AC-12 unit). `MinSize` is kept while Ready and not while Wait.
  - AC-13 unit:
    - `MaxSize=1` with the lease held: a second checkout fails with `pool_wait_timeout` exactly at `CheckoutTimeout`;
    - with `MaxWaiters=1`, a third fails at once with `pool_queue_full`;
    - FIFO order is kept.
  - Fail fast: switching the gate to FailFast(`upstream_auth_failed`) fails every queued waiter at once with that detail (AC-32 unit). In Wait mode no `Open` is called.
  - Backoff: successive failed opens are spaced 100, 200, 400, …, 5000 ms, each within ±20 %, and reset after a success (AC-32 unit, deterministic `Rand`).
  - `CloseHost`: idle connections are closed with the reason at once; a leased one is closed on return; `Aborted()` is closed for leases on that host (AC-14 unit). `AbortBarriers` aborts without closing idle connections.
  - Queued cancel: removed from the queue, no `Open` for it, no failure metric (AC-21 unit).
  - Idle expiry keeps `MinSize`.
  - `OrderRoundRobin` alternates between 2 idle connections (AC-16 unit).
  - FR-62 (AC-46 unit):
    - an idle connection whose `CheckIdle` reports bytes is discarded at checkout, and the checkout succeeds on the second idle connection;
    - with no other idle connection, a new one is opened (within `MaxSize`, counting the discarded one as gone);
    - a peer close is discarded with `reason=error` and no `ConnUnsolicited`;
    - `DiscardIdle` on a leased connection is a no-op;
    - the checkout timeout is not reset by a discard.
  - AC-2 Part B with `Noop` and a recording fake.
- Docker: no.
- Depends on: T3. Wave 2.

### T12: Metric catalog entries for SPEC-002

- Goal: add the 23 SPEC-002 metrics to `catalog.json` exactly as in SPEC-002 Interfaces, with the ADR-0016 buckets, plus name constants.
- Amendment 1 (G-4, G-3, S4): the help text of these metrics states the normative definitions:
  - `corral_request_errors_total`: one per `proxy_error` request, by class; `server_error` requests are not counted;
  - `corral_client_connections_accepted_total`: every accepted connection, including over-limit and TLS-failed ones;
  - `corral_client_connections`: open connections within the limit;
  - `corral_upstream_connect_errors_total`: every failed attempt, including authentication;
  - `corral_upstream_auth_failures_total`: the `upstream_auth_failed` subset;
  - `corral_protocol_errors_total`: only protocol errors that close or discard a connection, not the 352 refusal.

  The counting itself is T18.
- FRs/ACs: FR-48; AC-27 (catalog part), SPEC-001 AC-8 and AC-10 (Part B rerun).
- Implementer owns: `internal/infra/telemetry/metrics/catalog.json`, `internal/infra/telemetry/metrics/names.go` (new, constants only).
- Test writer owns: `internal/infra/telemetry/metrics/catalog_spec002_test.go`.
- Interfaces defined: `metrics.NameClientConnections`, … one constant per SPEC-002 metric.
- Test strategy (unit):
  - Every SPEC-002 metric is present with exactly the declared type, label names and value sets (a table copied from the spec).
  - `Lint` is clean.
  - Per-family series ≤ 100; `corral_requests_total` = 84.
  - promlint is clean over a registry built with `RegisterAll`.
  - Help text is non-empty.
- Docker: no.
- Depends on: T2. Wave 3.

### T13: File-content settings in the SPEC-003 registry and loader (ADR-0020) [SPEC-003 T3, T5]

- Goal: `registry.FileRule` and the loader step that reads a winning path once through `Inputs.ReadFile`, checks the content, reports failures as group 2 records under the input's `setting`, and exposes `registry.FileValue` to cross rules and typed views.
- FRs/ACs: SPEC-002 FR-44, FR-45, FR-55 (PEM rule mechanism); SPEC-003 FR-11, FR-13, FR-17, FR-25; AC-24 (unit: reader called once), AC-31 (PEM rule failures).
- Implementer owns:
  - `internal/infra/config/registry/filerule.go` (new);
  - `internal/infra/config/loader/filerule.go` (new);
  - the call site that invokes the file step in the loader's per-input validation. This is the only edit to a SPEC-003 T5 file; it must be coordinated with the SPEC-003 T5 implementer if T5 is still open.
- Test writer owns: `internal/infra/config/registry/filerule_test.go`, `internal/infra/config/loader/filerule_test.go`.
- Interfaces defined:
  - `registry.FileRule{MaxBytes int64; Check func(content []byte) error}`; `registry.Decl.File *FileRule`;
  - `registry.FileValue{Path string; Content []byte}`;
  - a registry build check: `File` only on non-secret `string` decls.
- Test strategy (unit, test registry `t.pem` with a `FileRule`, `memfs`, marker paths and contents):
  - flag and environment winners are read exactly once, and losing sources are not read;
  - not found, not regular and too large (4 MiB + 1) each give one record under the input's name (`t.pem` / `CORRAL_T_PEM`);
  - a `Check` failure gives one record;
  - records contain no path or content marker;
  - a cross rule receives the `FileValue` content;
  - SPEC-003 group ordering holds with mixed errors;
  - `Supplied` lists only the logical name.
- Docker: no.
- Depends on: [SPEC-003 T3], [SPEC-003 T5]. Wave 3, or as soon as SPEC-003 T5 lands.

### T14: Domain proxy policy

- Goal: `internal/domain/proxy`. It covers:
  - connection IDs (FR-3);
  - the per-message decision: handshakes FR-13 to FR-17 with FR-19 availability, FR-20, FR-21; client auth commands FR-23; blocked commands FR-33 (case-sensitive, top-level first stage); non-handshake OP_QUERY FR-8; the FR-47 late-message refusal; the FR-59 no-reply rule for `moreToCome`;
  - the FR-14 handshake reply (`maxWireVersion` capped at 25, `minWireVersion` unchanged, no forbidden fields) and its OP_REPLY form;
  - the synthesized-error table (code, `codeName`, constant `errmsg`, detail, class) and FR-39 labels;
  - the request outcome (FR-48);
  - the I/O-time budget (FR-57);
  - the shutdown state machine (FR-47);
  - `proxy.Observer`.
- **Amendment 1 additions:**
  - **FR-61 handling order** in `Decide`. It runs only after FR-5 to FR-8 and FR-60 have passed for the prefix (T9); faults that close the connection come first. The first rule that applies wins:
    1. shutdown (first byte read after FR-47 step 3): 91;
    2. handshake commands, in this order: `loadBalanced: true` gives 72; a repeated `client` gives 186; never reached or incompatible gives 6; otherwise the FR-14 reply;
    3. client auth commands: 18 or 115;
    4. blocked commands: 13;
    5. non-handshake OP_QUERY: 352;
    6. otherwise `Forward`.

    Fail-fast (6) and queue-full (262) come from the pool checkout that follows (T11, T19), which keeps steps 6 and 7 after every local refusal. A local message that does not fit L is `message_too_large` before `Decide` (T9). FR-59 `NoReply` applies to every refusal.
  - **FR-13, G-5:** exact, case-sensitive matching for handshake, auth and blocked command names. `HELLO`, `IsMaster` and `SaslStart` are ordinary commands: forwarded over OP_MSG, 352 over OP_QUERY.
  - **FR-16, G-6:** `ConnState.MetadataSeen` is set by the first handshake command read on the connection, whatever its outcome (72, 6 or success). A refused first handshake's `client` metadata is still kept for the connection's logs and spans (returned in `Decision.Metadata`, sanitised by the caller).
- FRs/ACs: FR-3, FR-8, FR-13 to FR-17, FR-19 (handshake side), FR-20, FR-21, FR-23, FR-33, FR-39, FR-47 (state), FR-57 (budget), FR-59, **FR-61**; AC-2 (unit: IDs), AC-5 (unit), AC-9 (unit), AC-10 (unit: cap), AC-13 (unit: labels), AC-17 (unit), AC-26 (unit: state), AC-34 (unit: budget), AC-36 (unit), **AC-40 (unit)**, AC-2 Part B.
- Implementer owns: `internal/domain/proxy/*.go` (non-test).
- Test writer owns: `internal/domain/proxy/*_test.go`.
- Interfaces defined:
  - `proxy.RequestBufferLimit = 65536`.
  - Connection IDs: `proxy.NewConnIDs() *ConnIDs`; `proxy.NewConnIDsAt(next int32) *ConnIDs` (tests); `(*ConnIDs).Acquire() (int32, bool)`; `(*ConnIDs).Release(id int32)`.
  - Refusals:
    - `proxy.Refusal` constants: `PoolWaitTimeout`, `PoolQueueFull`, `UpstreamUnavailable`, `UpstreamAuthFailed`, `UpstreamIncompatible`, `ShuttingDown`, `ClientAuthSASL`, `ClientAuthOther`, `CommandBlocked`, `UnsupportedOpQuery`, `MetadataRepeated`, `LoadBalanced`;
    - methods `Code() int32`, `CodeName() string`, `Errmsg() string`, `Detail() string`, `Class() string`, `Error() string` (constant), `CorralErrorDetail() string`;
    - `proxy.RefusalForDetail(detail string) (Refusal, bool)`, mapping pool and upstream details;
    - `proxy.Labels(r Refusal, req wire.Request) []string`;
    - `proxy.ErrorBody(r Refusal, labels []string) []byte`; `proxy.QueryFailureDoc() []byte`.
  - Handshakes: `proxy.HandshakeReply(h wire.Handshake, p upstream.Params, connID int32, now time.Time) []byte`.
  - Decision:
    - `proxy.ConnState{MetadataSeen bool}`;
    - `proxy.HandshakeView interface{ HandshakeAdmission() (upstream.Params, string) }`;
    - `proxy.Phase` (`PhaseServing`, `PhaseDraining`, `PhaseLate`);
    - `proxy.Decide(req wire.Request, cs *ConnState, hv HandshakeView, phase Phase) Decision`;
    - `proxy.Decision{Kind DecisionKind /* Handshake, HandshakeError, QueryFailure, Refuse, Forward */; Refusal Refusal; NoReply bool; Blocked bool; ClientAuth bool}`.
  - Outcome: `proxy.Outcome` (`OutcomeOK`, `OutcomeServerError`, `OutcomeProxyError`) with `String()` giving `ok` / `server_error` / `proxy_error`; `proxy.OutcomeForReply(f wire.ReplyFields) Outcome`.
  - I/O budget: `proxy.NewIOBudget(limit time.Duration, clk clock.Clock) *IOBudget` with `BeginRead() time.Time /* deadline */`, `EndRead()`, `Remaining() time.Duration` and `Reset()`.
  - Shutdown: `proxy.NewShutdown(delay, drain time.Duration, clk clock.Clock) *Shutdown` with `Begin()`, `ListenerClosed()`, `Phase() Phase`, `DrainExpired() <-chan struct{}`, and `ConnAction(exchangeInProgress, firstByteReadAfterStep3 bool) Action`.
  - `proxy.CloseReason`: the nine client close reasons. `proxy.RejectReason`: `limit_exceeded`.
  - `proxy.Observer`, with `Noop`:
    - `ConnOpened(ctx, ConnInfo)`, `ConnClosed(ctx, id int32, reason CloseReason)`, `ConnRejected(ctx, reason RejectReason)`;
    - `TLSFailed(ctx, err error)`;
    - `HandshakeAnswered(ctx, HandshakeInfo /* id, opcode, ok, sanitised metadata */)`;
    - `ClientAuthRejected(ctx, id int32, r Refusal)`, `CommandBlocked(ctx, id int32, command string)`;
    - `ClientProtocolError(ctx, id int32, err error)`, `ClientIOTimeout(ctx, id int32)`;
    - `RequestReceived(ctx, command string)`;
    - `RequestCompleted(ctx, RequestResult{Command string; Outcome Outcome; Duration time.Duration; Err error; Dropped bool})`;
    - `PanicRecovered(ctx, err error)`.
- Test strategy (unit):
  - IDs: starting at 2,147,483,646, IDs wrap to 1 and skip IDs in use; no duplicates among 10,000 open IDs (AC-2).
  - The handshake reply has exactly the FR-14 fields (`helloOk` only when requested; `isWritablePrimary` versus `ismaster` by command) and none of the forbidden fields (AC-9).
  - Wire versions: (6, 27) gives (6, 25); (21, 21) and (25, 30) give (21, 21) and (25, 25) (AC-10).
  - Decision table:
    - a second handshake with `client` gives 186; a first handshake with `loadBalanced: true` gives 72 (AC-9);
    - `saslStart`, `saslContinue` and `authenticate` give 18; `getnonce` and `logout` give 115 (AC-11 unit);
    - `currentOp`, `killOp`, `killAllSessions`, `killAllSessionsByPattern` and `aggregate` with a first stage `$currentOp` give 13; `CurrentOp` and `$match` are forwarded (AC-17 unit);
    - OP_QUERY `find` gives a query failure; handshakes when never reached give 6 (AC-5 unit);
    - `moreToCome` variants give `NoReply` (AC-36 unit);
    - late messages in shutdown give 91.
  - Labels (AC-13 unit): a retryable write gives `RetryableWriteError`; commit and abort give `RetryableWriteError`; `autocommit: false` gives `TransientTransactionError`; a plain read gets no labels; codes other than 262, 6 and 91 never get labels.
  - The synthesized-error table equals the spec table row by row (code, `codeName`, detail, class). `errmsg` constants contain no runtime data.
  - IOBudget on the fake clock: reads totalling the limit expire; pauses are not charged (AC-34 unit).
  - Shutdown actions per FR-47 step 3 (AC-26 unit).
  - AC-40 (unit, full FR-61 table over every combination of phase × handshake flags × upstream state):
    - with never reached, a first `hello` with `client` gives 6, and a second with `client` gives 186;
    - `loadBalanced: true` plus `client` gives 72, then 186;
    - with never reached: `saslStart` gives 18, `killOp` gives 13, a non-handshake OP_QUERY gives 352, and `find` gives `Forward` (the pool then fails it fast);
    - during the late phase, `saslStart` gives 91;
    - `HELLO`, `IsMaster` and `SaslStart` over OP_MSG give `Forward`; over OP_QUERY they give 352.
  - AC-2 Part B with `Noop` and a recording fake.
- Docker: no.
- Depends on: T9, T10 (`upstream.Params`). Wave 3.

### T15: Upstream connector: pool connections, monitoring checks, authentication

- Goal: `internal/infra/upstreamconn` per ADR-0019.
  - `Opener`: dial, TLS, corral `hello` (`appName=corral`), SCRAM-SHA-256 or `none`, all within `upstream.connect_timeout`.
  - `Checker`:
    - one monitoring connection per host (`appName=corral-monitor`);
    - one `hello` per check bounded by the timeout;
    - authentication only when `CheckRequest.Authenticate`;
    - a fresh connection when `Fresh`.
  - Typed outcomes: `upstream.AuthFailed` for codes 18 and 334 or `scram.ErrServerSignature`; transient otherwise.
  - Upstream connection IDs, a per-connection `wire.Correlator`, the per-connection upstream `maxMessageSizeBytes`, and `RoundTrip` for corral's own small exchanges (`ping`, `hello`, SASL), with a reply bound of `maxMessageSizeBytes`.
  - Monitoring connection open, close and fail events go to `upstream.Observer`; pool connection events go through the pool observer.
- **Amendment 1 additions (ADR-0019):**
  - FR-62:
    - `(*Conn).CheckIdle() error` is a zero-deadline, non-consuming probe: bytes give `ErrUnsolicited`, and a close gives the classified network error.
    - `upstreamconn` runs an idle watcher on each idle pool connection, started when the connection is returned and stopped at checkout. It is built on the T5 readability peek extended to report "data" versus "closed", so it also works on TLS sockets: any readability on an idle connection is a fault. It calls `pool.DiscardIdle`.
  - FR-19, G-8: the checker's `Report` carries the `hello` result even when authentication on the target's monitoring connection then fails. `Report.AuthErr` is a separate field, so the monitor can set "reached" and the authentication state independently.
- FRs/ACs: FR-22, FR-24, FR-28, FR-37 (I/O), FR-45, **FR-62 (detection)**, **FR-19 (G-8 reporting)**; AC-11 (support), AC-18 (twin support: authentication only on the target), AC-24 (support), AC-25 (unit: classification), **AC-41 (support)**, **AC-46 (in-process)**.
- Implementer owns: `internal/infra/upstreamconn/*.go` (non-test; excluding `scram/`).
- Test writer owns: `internal/infra/upstreamconn/*_test.go`. These tests may use `twins/mongosim` in process.
- Interfaces defined:
  - `upstreamconn.Credentials{Mechanism string; Source string; Username, Password []byte}`.
  - `upstreamconn.Config{Version string; Credentials *Credentials; TLS func(host string) (*tls.Config, error); ConnectTimeout time.Duration; Clock clock.Clock}`.
  - `upstreamconn.Conn`:
    - `ID() uint64`, `Host() string`;
    - `NetConn() net.Conn`, `Correlator() *wire.Correlator`;
    - `MaxMessageSize() int32`;
    - `Close(reason pool.CloseReason)`.

    It satisfies `pool.Conn`.
  - `upstreamconn.NewOpener(cfg Config) pool.Opener[*Conn]`.
  - `upstreamconn.NewChecker(cfg Config, obs upstream.Observer) upstream.Checker`.
  - `upstreamconn.RoundTrip(ctx context.Context, c *Conn, body []byte) ([]byte, error)`.
- Test strategy (unit and in-process integration with `twins/mongosim`):
  - The handshake metadata seen by the twin is `corral` / `corral-monitor` with the version.
  - SCRAM succeeds against the twin.
  - Authentication outcomes:
    - twin outcomes code 18, code 334 and bad signature each give `IsAuthFailed`, and the code is reachable through `errclass` `ServerError` for 18 and 334;
    - codes 10107, 91 and 11600, a reset and a delay past the timeout each give transient, with the class and detail from `errclass` (AC-25 unit).
  - `none` sends no SASL.
  - The checker authenticates only when asked, reuses one connection per host, and opens a fresh one when `Fresh`.
  - Correlator mismatch on a scripted reply gives `unexpected_response`.
  - FR-62 (AC-46 in-process):
    - `mongosim` sends 10 unsolicited bytes on an idle pool connection: the watcher discards it through `DiscardIdle`, and `ConnUnsolicited` is reported;
    - `mongosim` closes an idle connection: it is discarded with `reason=error`;
    - `CheckIdle` on a connection with pending bytes fails without consuming them;
    - the same works on a TLS connection.
  - G-8: with `mongosim` answering `hello` and rejecting SCRAM, `Report` has a successful `hello` and `AuthErr` with `IsAuthFailed`.
  - A connect timeout is enforced.
  - The marker password and username appear in no error text and no captured record (`logcheck` and `markerscan`).
- Docker: no.
- Depends on: T5, T6, T9, T10, T11, TW1. Wave 3.

### T16: Exchange spans helper

- Goal: `internal/infra/telemetry/spans` for FR-50.
  - Root (`SERVER`), `pool.checkout` (`INTERNAL`) and round-trip (`CLIENT`) spans.
  - Names `{db.operation.name} {db.collection.name}`.
  - Only FR-23 allowlisted attributes, with values through `sanitize`.
  - Errors through `errclass.RecordSpan`. Server attributes only for upstream replies with `ok: 0`, through a `ServerError` built from `wire.ReplyFields`.
  - With tracing disabled: no `Tracer.Start` call and zero allocations.
- FRs/ACs: FR-50; SPEC-001 FR-19, FR-22, FR-23, FR-32 (as amended); AC-27 (span shapes), SPEC-001 AC-22 Part B.
- Implementer owns: `internal/infra/telemetry/spans/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/spans/*_test.go`.
- Interfaces defined:
  - `spans.New(tr trace.Tracer, enabled bool) *Factory`.
  - `spans.RootInfo{Command, DB, Collection string; HasCollection bool; Client netip.AddrPort; ConnID int32; RequestID int32; SessionID [16]byte; HasSession bool; CursorID int64; HasCursor bool; AppName, DriverName, DriverVersion string}`.
  - `(*Factory).Start(ctx context.Context, info *RootInfo) (context.Context, Exchange)`.
  - `Exchange` is a value type with:
    - `Checkout(ctx) Step`;
    - `RoundTrip(ctx, server netip.AddrPort, upstreamConnID uint64) Step`;
    - `End(err error, reply *wire.ReplyFields)`.
  - `Step` has `End(err error, reply *wire.ReplyFields)`.
  - `spans.ReplyError(f wire.ReplyFields) error`: a `ServerError` for `ok: 0`, nil otherwise.
- Test strategy (unit, `tracetest.SpanRecorder`):
  - Each FR-50 shape:
    - an ordinary exchange (root, checkout, round-trip);
    - `moreToCome` (root and round-trip end at write; checkout present);
    - a synthesized error after a checkout attempt (root and checkout in error, `error.type` and detail set, no status code);
    - a synthesized error without a checkout (root only);
    - outcome unknown (root and round-trip in error);
    - an upstream `ok: 0` reply (status code and `codeName` on root and round-trip).
  - The attribute set is a subset of FR-23 on every span.
  - No `exception` events; empty status descriptions.
  - Disabled: `testing.AllocsPerRun(1000)` over `Start`, `Checkout`, `RoundTrip` and every `End` equals 0, and a counting tracer shows `Start` was never called.
- Docker: no.
- Depends on: T1, T2, T9 (`ReplyFields`). Wave 3.

### T17: SPEC-002 settings group and typed view [SPEC-003 T8, T10]

- Goal: `internal/app/settings/proxy.go`.
  - The `SPEC-002` group (namespaces `proxy`, `pool`, `upstream`) holds every row of the SPEC-002 settings table: type, presence, default, secrecy, rule and description, verbatim.
  - The seven cross rules, each with its primary and its applies-to-unset flag.
  - The PEM rule through `registry.FileRule{MaxBytes: source.MaxPEMFileBytes}`; the key rule; the duplicate-free rule for `upstream.hosts`; the `upstream.auth.source` character rule.
  - `settings.Groups()` includes the group.
  - `settings.HasProxyGroup` and `settings.RegistryOf` (ADR-0021).
  - The typed view `settings.Proxy`. When `pool.max_waiters` is unset, the effective maximum is `proxy.max_client_connections`. Secrets are held in a `settings.Secret` type whose `String`, `GoString`, `Format` and `LogValue` return `[redacted]`.
- FRs/ACs: FR-1 (validation part), FR-24, FR-34 (single-host rule), FR-44, FR-45, FR-55; AC-1 (`--proxy.listen=:27017` and `127.0.0.1:0` give exit 2, unit part), AC-11 (unit: secret literal rejected, connection-string host rejected), AC-24 (unit: reader once, bytes carried), AC-31.
- Implementer owns: `internal/app/settings/proxy.go`, `internal/app/settings/proxy_rules.go`, `internal/app/settings/secret.go`, and the one-line addition in `settings.Groups()` (non-test).
- Test writer owns:
  - `internal/app/settings/proxy*_test.go`;
  - updates to the SPEC-003 T10 tests in `internal/app/settings/*_test.go`: sample values for every new setting in the AC-2 helper map, and the regenerated golden help output.
- Interfaces defined:
  - `settings.ProxyGroup() registry.Group`;
  - `settings.Proxy{Listen string; MaxClientConnections int; ClientIdleTimeout, ClientIOTimeout time.Duration; TLS *ProxyTLS; ListenerCloseDelay, DrainTimeout time.Duration; Pool PoolSettings; Upstream UpstreamSettings}`;
  - `settings.ProxyTLS{CertPEM []byte; Key Secret; ClientCAPEM []byte}`;
  - `settings.PoolSettings{MaxSize, MinSize, MaxWaiters int; CheckoutTimeout time.Duration}`;
  - `settings.UpstreamSettings{Hosts []string; ReplicaSet string; HasReplicaSet bool; Mechanism, Source string; Username, Password Secret; ConnectTimeout, HeartbeatInterval, MaxIdleTime time.Duration; TLS *UpstreamTLS}`;
  - `settings.UpstreamTLS{CAPEM []byte /* nil = system roots */}`;
  - `settings.ProxyFrom(res loader.Result) Proxy`;
  - `settings.HasProxyGroup(r *registry.Registry) bool`;
  - `settings.RegistryOf(groups ...registry.Group) (*registry.Registry, error)`;
  - `settings.ObservabilityGroup() registry.Group`, the exported form of SPEC-003 T10's SPEC-001 group, used by in-process tests (ADR-0021);
  - `settings.Secret` (`Bytes() []byte`).
- Test strategy (unit through the SPEC-003 loader, injected environment, `memfs`):
  - Every AC-31 bullet:
    - registration (type, presence, secrecy, description, namespace);
    - each default;
    - each listed boundary violation (`pool.max_size=0`, `upstream.heartbeat_interval=100ms`, `proxy.listen` port 0 and empty host, an `upstream.hosts` port 0 and a duplicate, `upstream.auth.source` with `$`, `proxy.client_io_timeout=0s`);
    - the PEM rule: missing file, non-PEM file, 4 MiB + 1, each naming the setting without the path;
    - each cross rule failing once with `setting` = its primary, and passing in its valid configuration (the seven listed cases);
    - secrets rejected as command-line literals and accepted through the environment and `_FILE`;
    - the effective `MaxWaiters`.
  - `CORRAL_UPSTREAM_HOSTS=mongodb://<marker>:<marker>@h:27017` gives a record under `CORRAL_UPSTREAM_HOSTS` with no marker (AC-11 unit).
  - The TLS bytes in `settings.Proxy` equal the `memfs` content, and `memfs.Calls(path) == 1` (AC-24 unit).
  - `fmt.Sprintf("%v %+v %#v")` of `settings.Proxy` and `slog` output contain no marker secret.
  - `HasProxyGroup(Registry())` is true.
- Docker: no.
- Depends on: T13, [SPEC-003 T8], [SPEC-003 T10]. Wave 4.

### T18: Telemetry adapters for the domain observer ports

- Goal: `internal/infra/telemetry/proxyobs`.
  - One adapter per port (`proxy`, `pool`, `upstream`). Each turns calls into the SPEC-002 metrics and the FR-49 log events: exact constant `msg`, component, allowed keys, and `reason` from the declared enumerations.
  - Rate-limited events use one `ratelimit.Limiter` (30 s) per event and level, with `suppressed_count`.
  - On-transition-only events (`upstream unavailable`, `upstream incompatible`, `upstream available`) fire once per transition.
  - `corral_upstream_available` follows readiness.
  - Pool gauges follow `StatsChanged`.
  - Debug-only per-exchange records follow SPEC-001 FR-32.
  - Errors go through `errclass.LogAttrs`. Metadata, command names and collections go through `sanitize`.
  - **Metric definitions (normative since Amendment 1, G-3, G-4, S4):**
    - `corral_request_errors_total{error_class}` is incremented exactly once for each request counted with `outcome="proxy_error"`, with that error's class. `server_error` requests are **not** counted (this replaces the earlier P5 default). The sum over classes equals `corral_requests_total{outcome="proxy_error"}` summed over commands.
    - A message closed by a client protocol error or a client I/O timeout after its first byte was read counts as one `proxy_error` request: `command` = the FR-12 label if the command name was read, else `other`; class `protocol` or `timeout`. Connections closed before any message byte are not requests.
    - The 352 refusal is a `proxy_error` request with class `protocol`, and is **not** counted in `corral_protocol_errors_total`.
    - `corral_client_connections_accepted_total` counts every accepted connection, including over-limit and TLS-failed ones. `corral_client_connections` never counts over-limit connections.
    - `corral_upstream_connect_errors_total{kind,error_class}` counts every failed attempt to make an upstream connection ready, including authentication failures (class `server`). `corral_upstream_auth_failures_total{kind}` is the `upstream_auth_failed` subset.
    - FR-62 unsolicited data (`ConnUnsolicited`) gives `corral_protocol_errors_total{side="upstream",error_detail="unexpected_response"}` and the rate-limited `upstream protocol error` record.
  - The domain observer calls carry what these rules need: `RequestCompleted` carries `Err` and whether the command name was read. T14's `ClientProtocolError` and `ClientIOTimeout` are followed by a `RequestCompleted` only when a message byte had been read (T19 guarantees this).
- FRs/ACs: FR-48 (with the Amendment 1 definitions), FR-49, FR-62 (counting); AC-2 (metric part), AC-13 (metric part), AC-17 (metric and record part), AC-27 (events, rate limits, no status code on synthesized errors), **AC-38 (metric part)**, **AC-39 (unit: the equality and subset rules)**, **AC-46 (metric part)**, **AC-49 (metric part)**, SPEC-001 AC-23 Part B.
- Implementer owns: `internal/infra/telemetry/proxyobs/*.go` (non-test).
- Test writer owns: `internal/infra/telemetry/proxyobs/*_test.go`.
- Interfaces defined:
  - `proxyobs.New(logs *logging.Factory, mf *metrics.Factory, now func() time.Time) (*Adapters, error)`;
  - `(*Adapters).Proxy() proxy.Observer`, `Pool() pool.Observer`, `Upstream() upstream.Observer`.
- Test strategy (unit, `logcheck`, private registry, fake clock):
  - One test per FR-49 event: exact `msg`, component, level and key set; `reason` values only from that event's enumeration; a marker test that no value outside FR-25 appears.
  - For each rate-limited event: 10 triggers in 30 s on the fake clock give one record, and the next record after 30 s has `suppressed_count=9`.
  - The transition events fire once per transition.
  - Each metric increments with exactly the declared label values.
  - No record for a synthesized error carries `db.response.status_code`. The `upstream authentication failed` and `upstream target marked not primary` records carry it from upstream replies.
  - SPEC-001 AC-23: the T10, T11 and T14 domain scenarios run with these adapters wired to in-memory sinks give the same domain results as with `Noop`.
  - Amendment 1 (unit):
    - over a mixed sequence of synthesized errors, protocol closes (with and without a known command), I/O timeouts, 352 refusals and `server_error` replies: sum(`request_errors_total`) = sum(`requests_total{outcome="proxy_error"}`); `server_error` is not in `request_errors_total`; a 352 refusal leaves `protocol_errors_total` unchanged (AC-38, AC-39);
    - an auth failure increments both `connect_errors_total{error_class="server"}` and `auth_failures_total` for its `kind`; a network failure increments only the former (AC-39);
    - over-limit and TLS-failed connections increment `accepted_total` but not the gauge (AC-39);
    - `ConnUnsolicited` gives the upstream `unexpected_response` count and the record (AC-46);
    - an HTTP `GET` close counts `command="other"`; a late `aggregate` failure counts `command="aggregate"` (AC-49).
- Docker: no.
- Depends on: T1, T2, T10, T11, T12, T14. Wave 4.

### T19: Exchange engine

- Goal: `internal/infra/proxyserver/exchange`, the per-client-connection message loop.
  1. Between exchanges, read with the idle deadline. The first byte starts the exchange: the FR-57 budget, the request-duration clock and `RequestReceived`.
  2. Read the header, then buffer up to `proxy.RequestBufferLimit` (`wire.CheckClientHeader` uses the current upstream maximum or 48,000,000).
  3. `wire.ClassifyPrefix`, then `proxy.Decide`.
  4. Local answers (handshake, OP_REPLY forms, refusals): read the rest of the message to its end and discard it, within the budget (FR-38), then write the reply within the write timeout, or nothing for `NoReply`.
  5. Forward:
     - start spans;
     - `Checkout`, during which the liveness watcher runs if the message has been fully read (FR-41 queued);
     - on a checkout error, map the detail to a refusal, drain the message, reply with labels (FR-39), or nothing for `moreToCome`;
     - on a lease, write the rewritten header and the prefix, then stream the rest in 64 KiB chunks through the validator. A late validation failure discards the upstream connection with `client_abandoned` and closes the client with `protocol_error` (FR-38).
  6. `moreToCome` requests:
     - end the round-trip span when the message is written;
     - send `wire.PingBody` on the same connection and wait for its reply, with no timeout, while watching `Lease.Aborted()` only, not the client (Amendment 1, G-12);
     - when the ping finishes, the lease returns and the exchange ends;
     - when it fails or is aborted, discard the upstream connection (`error`) and keep the client connection open (FR-29).
  7. Replies:
     - read the upstream header and check it (`unexpected_response` for a `moreToCome` reply, `message_too_large`, correlation);
     - stream through `wire.ReplyScanner` in 64 KiB chunks to the client with the rewritten header, within one write deadline (FR-57);
     - on success, return the lease, then report the top-level and `writeConcernError` codes through `ReplyCodes` (the mode rule is in T10).
  8. Upstream failure or protocol error mid-exchange: discard the upstream connection (`error`) and close the client (`upstream_failure`) (FR-40).
  9. Client gone mid-flight: discard the upstream connection (`client_abandoned`) (FR-41).
  10. Record the outcome and duration (`RequestCompleted`).
  11. Probe hooks (corraltest only): bytes read before a lease, and peak reply bytes held.
- **Amendment 1 changes** (these override the steps above where they differ):
  - **FR-38, validate before writing and hold back the end:**
    - every chunk read after the lease is fed to `MsgValidator` before any byte of it is written upstream;
    - the last byte of the forwarded message is always held back; the client checksum is stripped, never forwarded (ADR-0017);
    - the held byte is written only when `MsgValidator.Close()` has passed and `Done()` is true;
    - on any failure the held byte is never written, the upstream connection is discarded (`client_abandoned`), and the client is closed (`protocol_error`).

    The upstream therefore never receives a complete failing message (AC-49).
  - **FR-29 and FR-41, G-12, barrier:**
    - the engine does **not** watch the client during the barrier `ping`; a client disconnect does not abort it;
    - on success, the connection returns to the pool even when the client has gone;
    - the ping reply's top-level `code` goes to `ReplyCodes(src=Barrier)`, and if it returns `marked`, the barrier fails (`reason=error`);
    - an `ok: 0` ping reply without a marking code also fails the barrier.
  - **FR-35:** after each forwarded reply, the engine calls `ReplyCodes(src=Forwarded)` with the top-level and `writeConcernError` codes. The monitor applies the mode rule (T10). `writeErrors` is never read.
  - **FR-62:** a checkout that hits a faulty idle connection is retried inside the pool (T11). The engine sees only a successful lease or a checkout error.
  - **Request counting (S4):** a client protocol error or I/O timeout after a message's first byte calls `RequestCompleted` with outcome `proxy_error`, the command if read, and the error. Nothing is reported for closes before any byte.
  - **L = K = 65,536** (normative): the constants are taken from `proxy.RequestBufferLimit` and `exchange.ReplyChunk`.
- FRs/ACs: FR-4 (deadline), FR-10, FR-11, FR-12, FR-26, FR-29, FR-30, FR-35 (reporting), FR-36 (no change to `$readPreference`), FR-38 (including validate-before-write), FR-39, FR-40, FR-41, FR-57, FR-58, FR-59, FR-60 (enforcement while streaming), FR-50 (span calls); AC-6, AC-8, AC-14, AC-15, AC-19, AC-20, AC-21, AC-34, AC-35, AC-36, **AC-42 (unit)**, **AC-44 (unit: nothing forwarded)**, **AC-49 (unit)** (unit parts).
- Implementer owns: `internal/infra/proxyserver/exchange/*.go` (non-test).
- Test writer owns: `internal/infra/proxyserver/exchange/*_test.go`. These may use `twins/mongosim` and `internal/testkit/rawwire` in process.
- Interfaces defined:
  - Constants `exchange.ReplyChunk = 65536`, `exchange.ForwardChunk = 65536`.
  - `exchange.Upstream interface{ ID() uint64; Host() string; NetConn() net.Conn; Correlator() *wire.Correlator; MaxMessageSize() int32 }`. `*upstreamconn.Conn` satisfies it.
  - `exchange.Lease interface{ Upstream() Upstream; Return(); Discard(pool.CloseReason); Aborted() <-chan struct{} }`; `exchange.Leaser interface{ Checkout(ctx context.Context) (Lease, error) }`.
  - `exchange.UpstreamView interface{ proxy.HandshakeView; MaxMessageSize() int32 }`.
  - `exchange.Probe interface{ ReadBeforeLease(connID int32, n int); ReplyHeld(connID int32, n int) }` (nil in production).
  - `exchange.Deps{Leaser Leaser; Upstream UpstreamView; Obs proxy.Observer; Spans *spans.Factory; Clock clock.Clock; IOTimeout, IdleTimeout time.Duration; Shutdown *proxy.Shutdown; ReplyCodes func(ctx context.Context, host string, src upstream.ReplySource, f upstream.ReplyCodeInfo) bool; Probe Probe; PanicCommand string}`. `ReplyCodes` replaces `NotPrimary` (Amendment 1).
  - `exchange.New(d Deps) *Engine`.
  - `exchange.ClientConn{Conn net.Conn; ID int32; Addr netip.AddrPort; IsTLS bool}`.
  - `(*Engine).Serve(ctx context.Context, cc *ClientConn) proxy.CloseReason`.
- Test strategy (unit and in-process, loopback sockets, `mongosim` in process, short timeouts):
  - Byte faithfulness, with and without client checksums and upstream checksums (AC-6).
  - 50 concurrent clients with the same `requestID` through a 2-connection leaser (AC-8).
  - An unknown `responseTo` (AC-8).
  - Barrier (AC-14):
    - the ping is sent on the same connection;
    - the connection is not released while the ping is held;
    - the next client message is not read while the ping is held;
    - a ping reset keeps the client open;
    - (Amendment 1) a client disconnect during the ping does not abort it;
    - a ping delayed 3× the connect timeout completes;
    - an abort via `Aborted()` discards the connection.
  - `exhaustAllowed` cleared (AC-15).
  - Buffer bound through the probe; a waiting large request that times out still keeps framing; an `aggregate` pipeline beyond the limit; an oversized `hello`; an invalid tail (AC-19).
  - Reset after the request is sent (AC-20).
  - Disconnect in flight and while queued (AC-21, Linux).
  - I/O timeouts: a stalled header; a slow reader; an upstream delay not cut off; queue waiting not charged (AC-34).
  - An oversized and a `moreToCome` reply; a 30 MB reply with the peak held at or below the chunk (AC-35).
  - `moreToCome` refusals get no reply, and `ping` succeeds after them (AC-36).
  - The FR-50 span shapes per path, using `spans` with a recorder.
  - Amendment 1:
    - AC-49 (unit): a 5 MB `aggregate` with a second `pipeline` near the end, sent with a free lease. `mongosim` records fewer bytes than the rewritten `messageLength` and then a close. The client is closed. One `RequestCompleted{aggregate, proxy_error}`.
    - AC-49 (unit, checksum): a valid body with a wrong checksum gives `checksum_mismatch`, and the upstream never receives the last body byte.
    - AC-44 (unit): every AC-44 malformed message within L is refused before any checkout, and `mongosim` receives nothing.
    - AC-42 (unit):
      - a ping reply with 10107 and with 91 in replica-set mode discards the connection and marks the host;
      - in single-`mongos` mode, 10107 does not fail the barrier and 91 does;
      - a client that disconnects during a delayed ping: the barrier completes, the connection returns to the pool, and the next lease reuses it (no `client_abandoned`).
    - `writeConcernError` and `writeErrors` replies are forwarded byte-identical, with the right `ReplyCodes` calls.
- Docker: no.
- Depends on: T5, T9, T11, T14, T15, T16, TW1 (tests). Wave 4.

### T20: Wire listener and client connection server

- Goal: `internal/infra/proxyserver`. It covers:
  - `Bind`: on failure, a `config.Error` for `proxy.listen` with no address, wrapping the cause (FR-1);
  - the accept loop;
  - the connection limit: accept, close with no bytes, `ConnRejected` and the `limit_exceeded` close reason (FR-2);
  - connection IDs (FR-3);
  - client TLS handshake bounded by `proxy.client_io_timeout`, with `tls_failure` (FR-44, ADR-0018);
  - one goroutine per connection running `Engine.Serve` under a recover that logs the FR-42 record, closes that client (`internal_error`) and discards any held upstream connection;
  - the ADR-0018 accounting invariant;
  - the FR-47 shutdown driver for steps 1 to 4, with hold-point callbacks.
- **Amendment 1 additions (FR-2, FR-44, FR-57, G-15; ADR-0018):**
  - the listener accepts plain TCP, checks the connection limit, and only then wraps the connection with `tls.Server`, so over-limit connections receive no TLS bytes;
  - a TLS handshake timeout closes with `reason=tls_failure`, class `timeout`, detail `deadline_exceeded`;
  - `accepted_total` counts every accepted connection, including over-limit and TLS-failed ones;
  - `ForceDrainEnd()` (FR-63) ends the drain as if `drain_timeout` had elapsed, and skips any remaining listener close delay;
  - the `shutdown_drain` hold point keeps the drain open (ADR-0022).
- FRs/ACs: FR-1, FR-2, FR-3, FR-4, FR-42, FR-43, FR-44, FR-47, **FR-63 (drain end)**; AC-2, AC-3, AC-22, AC-26, **AC-39 (unit: accounting)**, **AC-45 (unit)**, **AC-47 (unit: drain end)** (unit and in-process parts).
- Implementer owns: `internal/infra/proxyserver/*.go` (non-test; excludes `exchange/`).
- Test writer owns: `internal/infra/proxyserver/*_test.go`.
- Interfaces defined:
  - `proxyserver.Options{Addr string; MaxConns int; TLS *tls.Config; IOTimeout time.Duration; Engine *exchange.Engine; Obs proxy.Observer; Shutdown *proxy.Shutdown; IDs *proxy.ConnIDs; Hold func(ctx context.Context, point string)}`;
  - `proxyserver.New(o Options) *Server`;
  - `(*Server).Bind() error`, `Serve(ctx context.Context) error`, `Addr() net.Addr`, `Shutdown(ctx context.Context) error`, `ForceDrainEnd()` (Amendment 1);
  - hold-point names `proxyserver.HoldShutdownDelay`, `HoldListenerClosed`, `HoldStep3`, `HoldDrain` (Amendment 1), `HoldDrainExpired`.
- Test strategy (in-process, fake engine and real engine):
  - With `MaxConns=3`, a 4th connection gets no bytes and closes, and `ConnRejected` fires (AC-2).
  - An idle timeout closes an idle connection but not one with an exchange in progress (AC-3 unit).
  - TLS: plaintext rejected, mutual TLS enforced, failure reason `tls_failure`, no certificate data in records.
  - A panic in one connection closes that one only; a concurrent client completes; the error record has class `internal` and no panic value (AC-22 unit).
  - Shutdown with hold points: the delay keeps serving; after closing the listener, new connections are refused; an idle connection is closed with no reply; an in-progress exchange completes; a late message gets 91; drain expiry cuts the rest off (AC-26 unit).
  - Bind failure on an in-use port: `config.Error` with `ConfigSetting() == "proxy.listen"`, the error text has no address, and the detail is `EADDRINUSE`.
  - The accounting invariant across all of the above.
  - Amendment 1:
    - AC-45 (unit): over the limit with TLS on, the client receives zero bytes (no ServerHello); a partial ClientHello that stalls is closed after the I/O timeout, with `tls_failure` and class `timeout`.
    - AC-39 (unit): with `MaxConns=1`, 3 connections give `accepted` +3 and the gauge ≤ 1; a TLS failure counts in `accepted`.
    - AC-47 (unit): with a drain held open, `ForceDrainEnd` closes the remaining connections at once; a second call is a no-op.
- Docker: no.
- Depends on: T5, T14, T19. Wave 5.

### T21: Application wiring, runtime integration, corraltest hooks, Makefile [SPEC-003 T11, T12]

- Goal:
  - `internal/app/proxyapp`:
    - build `upstream.Monitor`, `pool.Pool[*upstreamconn.Conn]`, the exchange engine and the server from `settings.Proxy`;
    - adapt the monitor to `pool.Gate` and `exchange.UpstreamView`;
    - wire monitor `Change`s to `pool.AdmissionChanged`, `CloseHost(not_primary | incompatible)` and `AbortBarriers(unknown)`;
    - wire pool open failures and auth results to the monitor;
    - wire `ReplyCodes` from the engine to the monitor;
    - wire the FR-62 idle watcher (T15) to `pool.DiscardIdle`;
    - fan observers out to `proxyobs`;
    - expose `Service`.
  - `internal/app/runtime`:
    - the ADR-0021 order: start the service after the admin listener; bind as the last step; the `wire listener bound` record; exit 1 on bind failure; readiness registered; FR-47 inside shutdown;
    - proxy only when `settings.HasProxyGroup`;
    - new `Hooks` fields.
  - `cmd/corral/testbuild_on.go` / `testbuild_off.go`: the ADR-0022 corraltest variables, plus the probe server (loopback, counts only).
  - `Makefile`:
    - `test-twin: build` runs `go test -race -count=1 -tags twin ./tests/twin/...`;
    - `test-functional` exports `CORRALTEST_REQUIRE_DOCKER=1`.
  - Migrate the existing SPEC-001 and SPEC-003 in-process runtime tests to an explicit `settings.RegistryOf(settings.ObservabilityGroup())`, and the existing functional tests to `corralargs.Minimal` (ADR-0021).
  - **Amendment 1:**
    - **FR-63, second signal (ADR-0021):**
      - `cmd/corral` keeps a SIGINT/SIGTERM channel after the first signal, and passes the second as `runtime.Inputs.Force`;
      - the runtime emits the `lifecycle` warn record `shutdown forced by second signal` once, and calls `Service.ForceDrainEnd()`;
      - further signals are ignored;
      - exit 0 after steps 4 and 5 and the FR-34 flush bound.
    - **FR-64, hooks (ADR-0014, ADR-0022):**
      - every hook lives only in `testbuild_on.go` (`corraltest` tag), enabled only by `CORRALTEST_*`;
      - `testbuild_off.go` reads no `CORRALTEST_*` variable;
      - add the `shutdown_drain` hold point;
      - the probe's `/constants` returns L = K = 65,536.
- FRs/ACs: FR-1 (bind, record order), FR-18 (connection closing on incompatibility, wiring), FR-35 (wiring), FR-43, FR-46 (readiness port), FR-47 (sequence), FR-55 (values reach the domain typed), **FR-62 (wiring)**, **FR-63**, **FR-64**; AC-1 (unit part), AC-25 (in-process: `corral_upstream_available` matches readiness), AC-26 (in-process), AC-33 (in-process wiring), **AC-47 (in-process)**, **AC-48 (unit: the default build ignores `CORRALTEST_*`)**.
- Implementer owns: `internal/app/proxyapp/*.go`, `internal/app/runtime/*.go`, `cmd/corral/*.go`, `Makefile` (non-test).
- Test writer owns:
  - `internal/app/proxyapp/*_test.go`, `internal/app/runtime/*_test.go` (including the migration);
  - `tests/functional/*_test.go` (only the migration of existing SPEC-001 and SPEC-003 tests to `corralargs.Minimal`);
  - `tests/integration/**` (only where a test starts the runtime).
- Interfaces defined:
  - `proxyapp.Deps{Logs *logging.Factory; Metrics *metrics.Factory; Tracer trace.Tracer; TracingEnabled bool; Version string; Clock clock.Clock; Hooks Hooks}`;
  - `proxyapp.Hooks{Hold func(ctx context.Context, point string); RoundRobin bool; PanicCommand string; Probe exchange.Probe}`;
  - `proxyapp.New(cfg settings.Proxy, d Deps) (*Service, error)`;
  - `(*Service).Start(ctx)`, `Bind() error`, `Serve(ctx)`, `Readiness() health.Readiness`, `Shutdown(ctx) error`;
  - `runtime.Hooks` gains `Hold func(ctx context.Context, point string)`, `PoolRoundRobin bool`, `PanicCommand string`, `Probe exchange.Probe`;
  - (Amendment 1) `runtime.Inputs.Force <-chan struct{}`; `(*Service).ForceDrainEnd()`.
- Test strategy:
  - `proxyapp` in process with `mongosim`:
    - a not-primary reply closes idle connections to that host with `not_primary` and holds new leases until a check confirms a primary (AC-33 in-process);
    - a pool connect failure triggers an immediate check;
    - a pool auth failure makes readiness `upstream_auth_failed` and fails queued requests with 6;
    - an incompatible switch closes idle connections with `incompatible`.
  - `runtime` in process:
    - record order `wire listener bound` → startup complete;
    - `/readyz` `starting` while `BeforeLastStep` holds;
    - bind failure gives exit 1, `setting=proxy.listen`, no address;
    - shutdown order (FR-47 before admin and tracing);
    - readiness 503 `upstream_unavailable` with an unreachable upstream;
    - `corral_upstream_available` equals readiness.
  - Hooks: each corraltest variable maps to the right hook; the probe listens on loopback only.
  - Regression: every pre-existing runtime test passes after the migration.
  - Amendment 1:
    - AC-47 (in-process): during a held drain, closing `Force` ends the drain at once, emits exactly one `shutdown forced by second signal` record, and returns 0 within the flush bound plus tolerance; a third signal adds nothing.
    - AC-48 (unit): the default-build hook constructor returns no hooks even with `CORRALTEST_PROBE_LISTEN` and `CORRALTEST_HOLD_DIR` set.
    - FR-62 wiring: `mongosim` unsolicited bytes on an idle connection reach `DiscardIdle`.
- Docker: no (gates `make test-twin` and `make test-functional` from here).
- Depends on: T15, T17, T18, T20, [SPEC-003 T11], [SPEC-003 T12]. Wave 6.

### T22: Operator documentation (FR-51) and configuration reference [SPEC-003 T15]

- Goal:
  - `docs/operations/proxy.md` with every FR-51 statement:
    - shared identity, the blocked commands, the top-level-only `$currentOp` limitation, and forwarded identity-revealing commands;
    - handshake metadata;
    - read preferences;
    - no standalone;
    - pool sizing against `maxIncomingConnections`;
    - L = 65,536 bytes (including the 16-byte header) and K = 65,536 bytes, stated as normative values (FR-65), with the worst-case memory;
    - (Amendment 1) that handshake, auth and blocked command names match exactly and case-sensitively; the FR-60 refusal of duplicated inspected fields and of a `pipeline` sent as a kind-1 section; the second-signal behaviour (FR-63); the extended residual exposure list (G-14);
    - long operations and slow clients holding leases, and what `proxy.client_io_timeout` bounds;
    - the backoff constants and keepalive values;
    - no client authentication (network placement or mutual TLS);
    - connection-string options to remove (credentials, `authSource`, `authMechanism`, `replicaSet`, `compressors`, `loadBalanced`);
    - restart for credential and TLS rotation;
    - least-privilege guidance;
    - residual exposure routes.

    It also lists metrics, log events and readiness reasons.
  - `docs/operations/configuration.md`: reference entries for every SPEC-002 setting (SPEC-003 FR-26, AC-16).
  - `docs/operations/observability.md`: pointers to the above.
  - Examples use placeholders only (`<YOUR_DB_PASSWORD>`, `/run/secrets/<name>`).
- FRs/ACs: FR-51, **FR-65** (the file is `docs/operations/proxy.md`); AC-28 (review, including L and K as 65,536); SPEC-003 AC-16 (rerun with SPEC-002 settings).
- Implementer owns: `docs/operations/proxy.md`, `docs/operations/configuration.md`, `docs/operations/observability.md`.
- Test writer owns: `internal/app/settings/docs_test.go` (rerun with the new settings, unchanged logic); `docs/operations` constant check `tests/lint/proxy_doc_test.go` (the request buffer limit, reply chunk, backoff and keepalive constants named in `proxy.md` equal the code constants, and `proxy.md` states `65,536` for both L and K).
- Docker: no.
- Depends on: T17, T21, [SPEC-003 T15]. Wave 7.

### T23: Twin suite A: framing, handshake, forwarding (test-only)

- Goal: binary-level twin tests in `tests/twin/` (build tag `twin`), using `corralproc`, `rawwire`, `mongosim` and the corraltest binary where hooks are needed.
- ACs:
  - AC-4: the full table, the metric per case, and a valid checksummed kind-1 OP_MSG proxied;
  - AC-5;
  - AC-6;
  - AC-8;
  - AC-9: the twin records no handshake; the reply fields; `topologyVersion` with `maxAwaitTimeMS=10000` answered in well under 1 s; 186; 72; the compression and speculative fields;
  - AC-15;
  - AC-17 (twin part);
  - AC-19 (probe);
  - AC-35 (probe);
  - AC-36;
  - SPEC-001 AC-34 (HTTP `GET` to the wire port closes with no response);
  - Amendment 1:
    - **AC-38** (malformed OP_QUERY variants; a 352 refusal leaves `protocol_errors_total` unchanged, while `requests_total{outcome="proxy_error"}` and `request_errors_total{error_class="protocol"}` each increase by 1);
    - **AC-39** (twin part: limit accounting, TLS-failure accounting, the request-errors equality over a mixed run);
    - **AC-40** (FR-61 order and case sensitivity end to end; the shutdown case uses `CORRALTEST_HOLD_AT=shutdown_step3`);
    - **AC-44** (`mongosim.Received()` stays empty for every refused message; the 1,024-level message is forwarded);
    - **AC-49** (`mongosim` per-connection received byte count < `messageLength`, then closed; checksum case; `requests_total` labels `aggregate` / `other`).
- Test writer owns: `tests/twin/framing_test.go`, `handshake_test.go`, `forward_test.go`, `buffer_test.go`, `integrity_test.go` (AC-44, AC-49), `opquery_test.go` (AC-38), `order_test.go` (AC-40), `accounting_test.go` (AC-39), `main_test.go` (builds both binaries), `helpers_test.go`.
- Depends on: T7, TW1, T21 to run. Written in wave 6 alongside T21.

### T24: Twin suite B: pool, failures, timeouts, shutdown (test-only)

- ACs:
  - AC-3;
  - AC-13 (including spans through an in-process OTLP collector with `otlpcollector`);
  - AC-14 (twin part);
  - AC-20 (Go driver against `mongosim`; the spans);
  - AC-21 (Linux);
  - AC-22 (`CORRALTEST_PANIC_COMMAND`);
  - AC-23;
  - AC-26 (`CORRALTEST_HOLD_AT` points; exit 0 within the bound);
  - AC-34;
  - Amendment 1:
    - **AC-42** (barrier ping reply 10107 and 91 in replica-set mode; 10107 versus 91 in single-`mongos` mode; a client disconnect during a delayed barrier, with the same upstream connection reused by the next lease);
    - **AC-45** (client TLS on: over the limit, no TLS bytes; a stalled partial ClientHello closed after `proxy.client_io_timeout` with `tls_failure`);
    - **AC-46** (unsolicited bytes and a close on an idle pool connection; the next request succeeds on another connection; the metric and close reason).
- Test writer owns: `tests/twin/pool_test.go`, `barrier_test.go`, `failure_test.go`, `timeout_test.go`, `shutdown_test.go`, `tls_test.go` (AC-45), `unsolicited_test.go` (AC-46).
- Depends on: T7, TW1, T21 to run. Wave 6.

### T25: Twin suite C: topology, compatibility, availability, authentication (test-only)

- ACs:
  - AC-10;
  - AC-18 (twin parts: replica-set mode and single-`mongos` mode reasons; arbiters not monitored; authentication only on the target's monitoring connection; `readPreference=secondary` sent to the primary);
  - AC-25 (twin: auth codes and readiness);
  - AC-32 (twin parts, including "no pool connection attempts while no primary" through `mongosim.ConnAttempts`, retry once, and incompatible before never reached);
  - AC-33;
  - Amendment 1:
    - **AC-41** (authentication rejected from startup with `hello` answered: handshakes succeed, `find` gets 6 / `upstream_auth_failed`, readiness `upstream_auth_failed`; pool connections refused while checks succeed: readiness 200 and 262 after the timeout, then `upstream_unavailable` once checks and retries fail);
    - **AC-43** (single-`mongos` marking rule; replica-set `writeErrors[0].code` 10107 does not mark).
- Test writer owns: `tests/twin/topology_test.go`, `compat_test.go`, `availability_test.go`, `notprimary_test.go` (with AC-43).
- Depends on: T7, TW1, T21 to run. Wave 6.

### T26: Functional suite A: single-node replica set, auth, TLS, limits, PHP-FPM (Docker, test-only)

- Goal: binary plus a containerised `mongod` plus official drivers (FR-54). The matrix is MongoDB 7.0 and 8.0, with the Go driver and the PHP driver under PHP-FPM.
- ACs:
  - AC-1 (default bind address on `127.0.0.1:27017`; exit 2 cases; exit 1 for an in-use port with no address; `/readyz` `starting` with `CORRALTEST_HOLD_AT=laststep`; record order);
  - AC-2;
  - AC-9 (functional);
  - AC-11 (auth-enabled `mongod`; credentials through `CORRAL_UPSTREAM_AUTH_USERNAME` and `CORRAL_UPSTREAM_AUTH_PASSWORD_FILE` markers; the mongod log shows no client authentication; the marker harness);
  - AC-12 (PHP-FPM with ≥ 50 static workers and persistent connections; `pool.max_size=4`; `$currentOp` counted directly against `mongod`, `appName=corral` ≤ 4 and `corral-monitor` ≤ 1 per host; the gauge ≤ 4; the Go driver with 200 goroutines);
  - AC-14 (functional: `w:0` then `find`, 100 out of 100);
  - AC-16 (through `faultproxy` with hello rewriting; `pool.min_size=pool.max_size=2`; `CORRALTEST_POOL_ORDER=roundrobin`);
  - AC-17 (functional);
  - AC-20 (PHP driver through `faultproxy` reset);
  - AC-24;
  - AC-25 (functional);
  - Amendment 1:
    - **AC-39** (functional part: with a wrong service password, `connect_errors_total{error_class="server"}` and `auth_failures_total` both increase for the same `kind`; a network failure increases only the former; accepted-connection accounting with `proxy.max_client_connections=1`);
    - **AC-47** (corraltest binary with `CORRALTEST_HOLD_AT=shutdown_drain` against a real `mongod`: a second SIGTERM ends the drain, exactly one `shutdown forced by second signal` record, exit 0 within the FR-34 flush bound plus tolerance; a third signal changes nothing);
    - **AC-48** (the production binary with `CORRALTEST_PROBE_LISTEN=127.0.0.1:0` and a hold directory: no probe server, no hold file, no SPEC-003 unknown-variable record; the corraltest binary's `/constants` returns 65,536 for L and K).
- Test writer owns: `tests/functional/proxy_*_test.go`, `tests/functional/shutdown_signal_test.go` (AC-47), `tests/functional/hooks_test.go` (AC-48), `tests/functional/php/**` (PHP scripts).
- Depends on: T7, T8, TW2, T21 to run. Written in wave 6; gate in wave 7.

### T27: Functional suite B: 3-node failover and the single `mongos` (Docker, test-only)

- ACs:
  - AC-18 functional, exactly as specified:
    - `pool.checkout_timeout=120s`;
    - background load of retryable reads and writes only, from the driver matrix;
    - `replSetFreeze` run directly on each secondary;
    - `replSetStepDown` with `force: true` and its result asserted;
    - `/readyz` sampled as 503 `upstream_unavailable` while no primary is known;
    - unfreeze;
    - new leases go to the new primary;
    - `corral_upstream_primary_changes_total` increments;
    - no 262 errors.
  - A smoke test of the single-`mongos` topology on 7.0 and 8.0: CRUD through corral, `/readyz` 200, and the handshake reply with `msg: "isdbgrid"` (FR-34, FR-54).
- Test writer owns: `tests/functional/failover_test.go`, `tests/functional/mongos_test.go`.
- Depends on: T7, T8, T21 to run. Written in wave 6; gate in wave 7.

### T28: Observability Part B rerun (AC-27) (test-only)

- Goal: AC-27 end to end, beyond the unit coverage in T12, T16 and T18.
  - SPEC-001 AC-17: functional span shapes with the Go driver, including `moreToCome`, synthesized-error and outcome-unknown, through `otlpcollector`.
  - SPEC-001 AC-18: the marker harness through corral with the driver matrix, marker credentials (service user and client URI), documents, filters, comments, duplicate keys and `appName`. Forwarded reply bodies are not scanned.
  - SPEC-001 AC-19: database and collection names in spans, never in `/metrics`.
  - SPEC-001 AC-22: already covered in T16; rerun as part of `make test`.
  - SPEC-001 AC-34.
  - Every new metric family in `/metrics` with exactly its labels.
  - Every FR-49 event emitted with its constant `msg`, component, allowed keys and `reason` values, one marker test per event, in a twin or functional run.
  - No record or span for a synthesized error carries `db.response.status_code`.
- Test writer owns: `tests/functional/observability_proxy_test.go`, `tests/twin/events_test.go`.
- Depends on: T7, T8, TW1, T21 to run. Written in wave 6; gate in wave 7.

### T29: Static checks and boundary lint (test-only, plus a small implementer change if needed)

- Goal:
  - the domain import check matches all new domain packages (`bson`, `clock`, `wire`, `proxy`, `pool`, `upstream`) and passes (AC-29);
  - no production package imports `go.mongodb.org/mongo-driver/...`, `twins/...` or `internal/testkit/...` (extends SPEC-001 T21 through `importcheck.Forbidden`);
  - the default build of `./cmd/...` contains no corraltest probe code, and `testbuild_on.go` carries the constraint (extends SPEC-003 T14);
  - (Amendment 1, AC-48 static part, FR-64) every reference to `CORRALTEST_` in non-test Go code is in files with the `corraltest` build constraint; the default build's dependency closure contains no hook or probe package;
  - `constmsg` passes over the new packages;
  - review: no `err.Error()` or panic value reaches a log or span attribute in the new code.
- Test writer owns: `tests/lint/domain_imports_test.go` (assertion on the matched package list), `tests/lint/proxy_imports_test.go` (new), `tests/lint/testbuild_test.go` (extended).
- Implementer owns: none expected. If `importcheck.Forbidden` needs a pattern option, the change goes to `internal/testkit/importcheck/*.go`.
- Depends on: T21. Wave 7.

---

## Twins

Both twins are Go packages under `twins/`, imported by tests and run in the test process on `127.0.0.1:0` (ADR-0022). They:

- encode and decode with `go.mongodb.org/mongo-driver/v2/bson` and their own framing, never with `internal/domain/...`;
- record only what tests assert on;
- use synthetic data;
- are race-clean and stop cleanly through `tb.Cleanup`.

### TW1 `twins/mongosim`: simulated MongoDB upstream

**Purpose:** every twin AC, plus in-process tests in T15, T19 and T21.

**Contract.** Names are indicative; the twin-builder may adjust spelling, but not behaviour.

- **Lifecycle**
  - `Start(tb, Options) *Server`; `(*Server).Addr() string`.
  - `Stop()`: closes the listener and every connection; the address becomes unreachable.
  - `Restart()`: listens again on the same port.
  - `RefuseNew(bool)`: accept, then close at once.
  - `Blackhole(bool)`: accept, but never read or answer.
- **Topology (`hello` and `isMaster` / `ismaster` over OP_MSG and OP_QUERY)**
  - `HelloSpec` fields:
    - `Role` (`Primary`, `Secondary`, `Arbiter`, `Standalone`, `Mongos`, `Other`);
    - `SetName`, `Hosts`, `Passives`, `Arbiters`, `Primary`;
    - `ElectionID` (12 bytes), `SetVersion`;
    - `MinWire`, `MaxWire`;
    - `MaxBsonObjectSize`, `MaxMessageSizeBytes`, `MaxWriteBatchSize`, `LogicalSessionTimeoutMinutes`.
  - `SetHello(HelloSpec)` switches live.
  - `FailChecks(n int, mode)`: the next `n` `hello` messages on connections whose handshake `appName` is `corral-monitor` (or on all connections) fail. Modes: `Reset`, `NoReply` (until released or the connection closes), `Delay(d)`.
  - (Amendment 1, AC-41) `RefuseApp(appName string, on bool)`: refuses new connections once their handshake shows that `appName` (close after the `hello`), so pool connections (`corral`) fail while monitoring (`corral-monitor`) succeeds.
- **Amendment 1 additions**
  - Idle-connection faults (AC-46):
    - `SendUnsolicited(conn int, n int)`: writes `n` bytes on a connection with no outstanding request;
    - `CloseConn(conn int)`;
    - `IdleConns(appName) []int`, listing connections with no outstanding request.
  - Partial-message recording (AC-49): each connection records `BytesReceived` even for incomplete messages, and whether the connection closed before a complete message arrived.
  - Barrier replies (AC-42): rules matching `ping` can `ReplyError(code, codeName)` (10107, 91) as well as `Hold`, `Delay` and `Reset`.
  - `writeErrors` replies (AC-43): `ReplyWriteErrors([]code)` returns `ok: 1` with `writeErrors`.
  - Single-`mongos` mode error replies with any code, top-level or `writeConcernError` (AC-43).
- **Replica-set helper**
  - `StartReplicaSet(tb, n int, opts) *Cluster`: members advertise each other's real addresses.
  - `(*Cluster).SetPrimary(i)`: bumps `electionId`.
  - `NoPrimary()`.
  - `StepDown(i)`.
  - `Member(i) *Server`.
- **Authentication (SCRAM-SHA-256 server, `saslStart` / `saslContinue`, `skipEmptyExchange`)**
  - `Options.Users` (synthetic).
  - `SetAuthOutcome` values:
    - `Succeed`;
    - `Fail18` (`AuthenticationFailed`), `Fail334` (`MechanismUnavailable`);
    - `BadServerSignature` (a wrong `v=`);
    - `ErrorCode(code, codeName)` (for example 10107, 91, 11600);
    - `Reset`;
    - `Delay(d)`.
  - Records whether each connection authenticated, and with which mechanism.
- **Commands**
  - `ping` returns `{ok: 1}`.
  - Any other command returns `{ok: 1, n: 1, corraltestEcho: <value of the request's corraltestMarker field, if present>}`.
  - A `find` with a `corraltestReplyBytes: N` field returns a reply padded to about N bytes with a deterministic pattern (for 30 MB replies).
- **Rules** (`AddRule(Match, Action) *RuleHandle`; `Match` by command name and/or marker, `Once` or persistent)
  - Holding and timing:
    - `Hold`: keep the request until `RuleHandle.Release()`;
    - `Delay(d)`;
    - `ResetAfterReceive`: close the connection after reading the request.
  - Error replies:
    - `ReplyError(code, codeName)` (`ok: 0`);
    - `ReplyWriteConcernError(code)` (`ok: 1`).
  - Protocol faults:
    - `ReplyUnknownResponseTo`;
    - `ReplyMoreToCome`;
    - `ReplyOversize` (header `messageLength` above `maxMessageSizeBytes`);
    - `ReplyChecksum(valid bool)`;
    - `ReplyRaw(sections []byte)` (for byte-faithfulness).
  - `NoReply`.

  Rules apply equally to `ping`, which covers the barrier.
- **Recording**
  - `Received() []Message`. Each entry has:
    - the connection index, the connection's handshake `appName` and its authenticated flag;
    - the opcode, flags, `requestID`, command name and `$db`;
    - lsid presence and `txnNumber` presence;
    - the raw bytes;
    - a timestamp.
  - `ConnAttempts() int` and `ConnAttemptsBy(appName) int` (a connection's `appName` is known after its first `hello`).
  - `OpenConns(appName) int`; `Conns() []ConnInfo`.
- **Self-tests:** the official Go driver connects and runs CRUD against a `Primary` and a `Mongos`; each rule and auth outcome is observable from the driver.

### TW2 `twins/faultproxy`: fault-injecting TCP intermediary

**Purpose:** AC-16 (connection identity per command against a real `mongod`) and AC-20 with real drivers (Go and PHP) against a real `mongod`.

**Contract**

- `Start(tb, upstream string, Options) *Proxy` listens on `127.0.0.1:0`. Each accepted connection opens one connection to `upstream`, and bytes are relayed unchanged unless a fault or the rewrite below applies.
- `Options.RewriteHello map[string]string`: in `hello` / `isMaster` replies, rewrites `hosts`, `passives`, `primary` and `me` values from real member addresses to proxy addresses. A single-node replica set fronted by the proxy is then discovered as the proxy only. Rewritten replies get a correct `messageLength`, and any checksum is dropped.
- **Recording.** It parses OP_MSG frames in both directions with its own parser. `Commands() []Record` holds:
  - the connection index and the connection's handshake `appName`;
  - the command name and database;
  - the lsid `id` bytes and `txnNumber` (needed to group a transaction's commands; this is test-side data only);
  - the direction and a timestamp.
- **Faults** (one-shot or persistent, matched by command name, optionally the n-th match):
  - `ResetAfterRequest`: forward the request, then close both sides before the reply.
  - `Delay(d)`.
  - `Blackhole(conn)`.
  - `PauseAll()` / `ResumeAll()`.
  - `KillAll()`.
- **Self-tests:** against `mongosim` (no Docker); recording of commands per connection; one-shot reset seen by the Go driver as a network error and retried.

---

## Ordering and waves

Inside each task: tests first, then implementation, then review.

| Wave | Tasks | Notes |
|---|---|---|
| 1 | T1, T3, T4, T5, T6, T7, T8, TW1, TW2 | Independent. T8 can start with `fcgi` and pick up `corralargs` from T7 in the same wave. |
| 2 | T2 (T1), T9 (T4), T10 (T1, T3), T11 (T3) | |
| 3 | T12 (T2), T13 [SPEC-003 T3, T5], T14 (T9, T10), T15 (T5, T6, T9, T10, T11, TW1), T16 (T1, T2, T9) | T13 waits for SPEC-003 T5; it is not on the critical path until T17. |
| 4 | T17 (T13, [SPEC-003 T8, T10]), T18 (T1, T2, T10, T11, T12, T14), T19 (T5, T9, T11, T14, T15, T16, TW1) | |
| 5 | T20 (T5, T14, T19) | |
| 6 | T21 (T15, T17, T18, T20, [SPEC-003 T11, T12]); T23 to T28 written in parallel | From T21, `make test-twin` and `make test-functional` are gates. The twin and functional suites are red until T21 lands. |
| 7 | T22 ([SPEC-003 T15], T17, T21), T29 (T21); gate runs of T23 to T28 | Failures go back to the owning implementation task. |

**SPEC-003 dependencies at a glance:**

- T13 needs SPEC-003 T3 (registry) and T5 (loader).
- T17 needs SPEC-003 T8 (golden help) and T10 (settings package).
- T21 needs SPEC-003 T11 (configload) and T12 (runtime, exit codes, `runtime.Inputs`).
- T22 needs SPEC-003 T15 (`configuration.md` and its docs test).

Everything in waves 1 to 3 except T13 can proceed while SPEC-003 is still being implemented.

**Critical path:** T4 → T9 → T14 → T19 → T20 → T21 → gate. In parallel, [SPEC-003 T3 → T5] → T13 → T17 → T21.

## Coverage table

### SPEC-002 acceptance criteria

| AC | Task(s) |
|---|---|
| AC-1 | T17 (exit-2 cases, unit), T20 (bind failure record), T21 (order, `/readyz` hold, exit 1), T26 (functional) |
| AC-2 | T14 (ID allocator wrap, unit), T20 (limit, unit), T18 (metric), T26 (functional) |
| AC-3 | T19 (idle deadline), T20 (unit), T24 (twin) |
| AC-4 | T4, T9 (unit), T18 (metric), T23 (twin) |
| AC-5 | T9, T14 (unit), T23 (twin) |
| AC-6 | T9 (rewrite, unit), T19 (unit), T23 (twin) |
| AC-7 | T4, T9 (unit, fuzz, marker; review-backed) |
| AC-8 | T9 (correlator), T19 (unit), T23 (twin) |
| AC-9 | T14 (unit), T23 (twin), T26 (functional) |
| AC-10 | T10 (compatibility, unit), T14 (cap, unit), T25 (twin) |
| AC-11 | T6, T14, T15, T17 (unit), T26 (functional) |
| AC-12 | T11 (bounds, unit), T26 (functional, PHP-FPM and Go) |
| AC-13 | T11, T14 (unit), T16 (spans), T18 (metrics), T24 (twin) |
| AC-14 | T5 (keepalive, unit/review), T11 (abort, unit), T19 (barrier, unit), T24 (twin), T26 (functional `w:0`) |
| AC-15 | T9, T19 (unit), T23 (twin) |
| AC-16 | T11 (round-robin, unit), T21 (hook), TW2, T26 (functional) |
| AC-17 | T14 (unit), T18 (metric and record), T23 (twin), T26 (functional) |
| AC-18 | T10 (unit), T15 (monitor authentication), T25 (twin), T27 (functional failover) |
| AC-19 | T9 (prefix rules), T19 (unit), T21 (probe), T23 (twin) |
| AC-20 | T19 (unit), T16 (spans), T24 (twin, Go driver), T26 (PHP driver via TW2) |
| AC-21 | T5 (watcher), T11 (queued cancel), T19 (unit), T24 (twin) |
| AC-22 | T20 (unit), T21 (hook), T24 (twin) |
| AC-23 | T20, T21 (FR-43 behaviour), T24 (twin) |
| AC-24 | T5 (TLS builders), T13, T17 (bytes and reader once, unit), T26 (functional) |
| AC-25 | T10, T15 (unit), T21 (in-process), T25 (twin), T26 (functional) |
| AC-26 | T14 (state, unit), T20 (unit), T21 (in-process, hooks), T24 (twin) |
| AC-27 | T12 (catalog), T16 (spans, AC-22), T18 (events, rate limits, AC-23), T28 (end to end) |
| AC-28 | T22 (review, plus the constants check) |
| AC-29 | T3, T4, T9, T10, T11, T14 (domain unit tests with fake clock and fakes), T29 (import check) |
| AC-30 | Review of T23 to T28 (synthetic data, matrix: Go and PHP, 7.0 and 8.0, single-node, 3-node, `mongos`) |
| AC-31 | T13, T17 |
| AC-32 | T10, T11 (unit), T25 (twin) |
| AC-33 | T10 (unit), T19 (detection), T21 (in-process wiring), T25 (twin) |
| AC-34 | T14 (budget, unit), T19 (unit), T24 (twin) |
| AC-35 | T9 (scanner), T19 (unit), T23 (twin) |
| AC-36 | T14, T19 (unit), T23 (twin) |
| AC-37 (Amendment 1) | T9 (unit) |
| AC-38 | T9 (unit: OP_QUERY validation), T18 (counting), T23 (twin) |
| AC-39 | T18 (unit: definitions), T20 (unit: accounting), T23 (twin), T26 (functional: auth and connect errors) |
| AC-40 | T14 (unit: FR-61 table), T23 (twin) |
| AC-41 | T10 (unit), T15 (G-8 reporting), T25 (twin; `mongosim.RefuseApp`) |
| AC-42 | T10 (unit: marking), T19 (unit: barrier), T24 (twin) |
| AC-43 | T9 (`writeErrors` not read), T10 (unit: mode rule), T25 (twin) |
| AC-44 | T4 (depth, overrun), T9 (unit: integrity), T19 (nothing forwarded), T23 (twin) |
| AC-45 | T20 (unit), T24 (twin) |
| AC-46 | T5 (peek), T11 (unit: checkout retry), T15 (idle watcher), T18 (metric), T21 (wiring), T24 (twin) |
| AC-47 | T20 (drain end), T21 (signal handling, in-process), T26 (functional) |
| AC-48 | T21 (unit), T26 (functional), T29 (static) |
| AC-49 | T9 (unit), T19 (unit: hold-back), T18 (counting), T23 (twin) |

### SPEC-001 criteria rerun or implemented here

| AC | Task(s) |
|---|---|
| AC-41 (Amendment 3) | T1, T2 |
| AC-1 (domain imports) | T29 |
| AC-2 Part B (ports: no-op versus fake) | T10, T11, T14 |
| AC-8 (lint rerun) | T2, T12 |
| AC-9 Part B (command labels) | T2, T18 |
| AC-10 Part B (series cap) | T12 |
| AC-17 Part B (span shapes) | T16 (unit), T28 (functional) |
| AC-18, AC-19 Part B (marker scans) | T28 |
| AC-22 Part B (zero allocations) | T16 |
| AC-23 Part B (real adapters) | T18 |
| AC-34 Part B (HTTP on the wire port) | T23, T28 |
| AC-39 Part B (collection extractor) | T9, T2 |

### FR coverage summary

| FRs | Task(s) |
|---|---|
| FR-1 | T17, T20, T21 |
| FR-2, FR-3 | T14, T20 |
| FR-4 | T19, T20 |
| FR-5 to FR-7 | T4, T9, T19 |
| FR-8 | T9, T14 |
| FR-9 | T4, T9 |
| FR-10, FR-11 | T9, T19 |
| FR-12 | T19 |
| FR-13 to FR-17 | T14, T19 |
| FR-18 | T10, T21 |
| FR-19 | T10, T14 |
| FR-20, FR-21 | T14 |
| FR-22 | T6, T10, T15 |
| FR-23 | T14 |
| FR-24 | T6, T14, T15, T17 |
| FR-25, FR-27 | T11 |
| FR-26 | T11, T19 |
| FR-28 | T5, T11, T15 |
| FR-29 | T11, T19 |
| FR-30 | T9, T19 |
| FR-31, FR-32 | T11, T19 (no pinning; verified by T26) |
| FR-33 | T9, T14 |
| FR-34 | T10, T17 |
| FR-35 | T10, T11, T19, T21 |
| FR-36 | T19 |
| FR-37 | T10, T15 |
| FR-38 | T9, T19 |
| FR-39 | T14 |
| FR-40 | T19 |
| FR-41 | T5, T11, T19 |
| FR-42 | T20 |
| FR-43 | T20, T21 |
| FR-44 | T5, T17, T20 |
| FR-45 | T5, T15, T17 |
| FR-46 | T10, T21 |
| FR-47 | T14, T20, T21 |
| FR-48 | T12, T18 |
| FR-49 | T1, T18 |
| FR-50 | T16, T19 |
| FR-51 | T22 |
| FR-52 | T1, T2 |
| FR-53 | T3, T4, T9, T10, T11, T14, T29 |
| FR-54 | T7, T8, TW1, TW2, T23 to T27 |
| FR-55 | T13, T17 |
| FR-56 | T10, T11 |
| FR-57 | T14, T19 |
| FR-58 | T9, T19 |
| FR-59 | T14, T19 |
| FR-60 (Amendment 1) | T4 (structure, nesting, duplicate counts), T9 (integrity), T19 (enforcement while streaming) |
| FR-61 | T14 (with T11 and T19 for steps 6 and 7) |
| FR-62 | T5, T11, T15, T18, T21 |
| FR-63 | T20, T21 (ADR-0021) |
| FR-64 | T21, T29 (ADR-0014, ADR-0022) |
| FR-65 | T22 |

Amended FRs:

| FR | Amendment 1 change | Task(s) |
|---|---|---|
| FR-2 | limit before TLS | T20 |
| FR-5 | minimum lengths and check order | T9 |
| FR-7 | FR-60 checks | T9 |
| FR-8 | OP_QUERY validation; 352 counting | T9, T18 |
| FR-9 | kind-1 identifiers, duplicate presence, no `writeErrors` | T4, T9 |
| FR-13 | exact names | T9, T14 |
| FR-16 | first handshake counts | T14 |
| FR-19 | "reached" | T10, T15 |
| FR-29 | ping-reply marking; no client abort | T10, T19 |
| FR-33 | bypass refusal | T9 |
| FR-35 | mode rule; `writeErrors` | T10, T19 |
| FR-38 | L normative; validate before write and hold back | T9, T19 |
| FR-41 | barrier not in flight | T19 |
| FR-44 | TLS handshake bound | T20 |
| FR-46 | readiness follows monitoring | T10 |
| FR-47 | second signal | T20, T21 |
| FR-57 | TLS handshake | T20 |
| FR-58 | K normative | T19 |
| Metric definitions | G-3, G-4, S4 | T12, T18 |

**SPEC-001 Amendment 3 items:**

| Item | Where |
|---|---|
| 1 (components) | T1 |
| 2 (allowlist) | T2 |
| 3 (readiness reasons) | T1 |
| 4 (`rejected` and details) | T1 |
| 5 (`reason` key) | T1 |
| 6 (label names and rule) | T2 |
| 7 (no status code on synthesized errors) | T1 (rule), T16, T18 (enforcement) |
| 8 (FR-31) | T12 (no amendment needed) |
| 9 | no change |
| 10 (ADR-0006) | ADR-0021 |

## Notes for the Orchestrator

- **Required settings change existing tests (ADR-0021).** Once T17 registers the SPEC-002 group:
  - `upstream.hosts` is required, and the default `SCRAM-SHA-256` mechanism needs credentials;
  - every existing binary test of SPEC-001 and SPEC-003 must pass `corralargs.Minimal` (T7);
  - in-process runtime tests pass a SPEC-001-only registry.

  The migration is in T21's test-writer scope. Until T21 lands, **T17 must not be merged to the integration branch alone**, or `make test-functional` breaks. Merge T17 together with T21, or keep T17 on its task branch until T21 is ready.
- **SPEC-003 artefacts that SPEC-002 touches:**
  - T17 updates the SPEC-003 T10 sample map and golden help;
  - T22 adds the SPEC-002 settings to `configuration.md` (SPEC-003 AC-16 test);
  - T13 adds one call site in a SPEC-003 T5 file.

  `plans/SPEC-003.md` is not changed by this plan. If SPEC-003 T5 is still open when T13 starts, coordinate the call site with its implementer.
- **ADR-0014 follow-up:** the production build now has real secret settings, so SPEC-003 AC-9 could move off the `CORRALTEST_REGISTRY=secret` option. This is optional and not scheduled.
- **Security:** the residual exposure routes in SPEC-002 Data and compliance and the test-only `corraltest` probe server are flagged for review by security@emag.ro (P10). SCRAM is no longer in-house (T6 is an adapter around `github.com/xdg-go/scram`).
- **Platform:** the liveness watcher (AC-21) and the functional suites (host networking) are Linux-only. CI must run `make test-twin` and `make test-functional` on Linux with Docker.

## Owner decisions (2026-10-24)

The Owner answered every item that was proposed. Nothing is open. The items are kept below with their outcome, because tasks refer to them by number.

- **P1 (new access): granted.**
  - Docker on a Linux CI runner with host networking.
  - Docker Hub images (`mongo:7.0`, `mongo:8.0`, a PHP-FPM base) and PECL (`ext-mongodb`), all pinned by digest or version (ADR-0022, recorded in ADR-0002).
  - Affects: T8, T26, T27, T28.
- **P2 (dependencies): approved, with one change.**
  - SCRAM uses the maintained `github.com/xdg-go/scram` library instead of an in-house client. T6 is reworked into a thin adapter (ADR-0019). SASLprep comes from the library's own `xdg-go/stringprep` dependency.
  - Production: `github.com/xdg-go/scram` (and what it selects).
  - Test-only: `go.mongodb.org/mongo-driver/v2`.
  - `golang.org/x/sys` becomes direct.
  - Recorded in ADR-0002, ADR-0019 and ADR-0022.
  - Affects: T5, T6, T7.

P3 to P9 and P11: **defaults accepted**. SPEC-002 Amendment 1 then made P3, P4, P6, P8 and P9 normative. It also **changed P5**: `server_error` requests are not counted in `corral_request_errors_total`. The spec text wins, and T18 follows it.

- **P3 (FR-7 ambiguity: kind-1 documents).**
  - FR-7 requires the kind-0 body to be valid BSON, and says nothing about the documents inside kind-1 sections beyond section bounds.
  - Default: validate each kind-1 document fully as BSON too (`malformed_message` on failure). This is what lets AC-19's "invalid tail" case work, since large inserts carry their documents in kind-1.
  - Alternative: check only kind-1 framing.
  - Affects: T9, T19.
- **P4 (metric semantics).** Default: `corral_client_connections_accepted_total` counts every accepted TCP connection, including those closed for `limit_exceeded`. Then `accepted = sum(closed) + open` (ADR-0018). Affects: T18, T20.
- **P5 (metric semantics).**
  - `corral_request_errors_total{error_class}` has no definition in the spec.
  - Default: one increment per client request whose outcome is `server_error` or `proxy_error`, labelled with the FR-29 class of that error: `server` for upstream `ok: 0` replies; the table class for synthesized errors; `protocol`, `network` or `timeout` for closed connections. Refused `moreToCome` messages are included.
  - Affects: T18.
- **P6 (metric semantics).**
  - Default: `corral_upstream_connect_errors_total{kind,error_class}` counts every pool or monitoring connection attempt that did not become usable, including authentication failures (class `server`).
  - `corral_upstream_auth_failures_total{kind}` counts only the `upstream_auth_failed` subset.
  - Affects: T18.
- **P7 (close reasons at the boundaries).** Default per the ADR-0019 table:
  - `client_abandoned` for every upstream discard caused by the client side, including client I/O timeout and late request validation;
  - `error` for upstream-side failures and for panics;
  - `incompatible` for monitoring connections to non-matching servers that stop being monitored;
  - `idle` for monitoring connections to members that are no longer known.

  Affects: T15, T18, T19.
- **P8 (client TLS handshake bound).** Default: the handshake is bounded by `proxy.client_io_timeout`, and a failure or timeout closes with `reason=tls_failure` (ADR-0018). Affects: T20.
- **P9 (FR-16 literal reading).** Default: a second handshake carrying `client` on the same connection gets 186 even if the first handshake was answered with an error (for example code 6 before the upstream was reached). Affects: T14.
- **P10 (security review): still recommended** for the items below. No in-house SCRAM remains to review.
  - the residual cross-client exposure routes, which are handled by least privilege only;
  - the `corraltest` probe server, which is test-build only and loopback only.

  Affects: merge gating only.
- **P11 (earlier ACs that say "default settings").**
  - SPEC-001 and SPEC-003 functional ACs that start corral "with default settings" can no longer be met literally, because `upstream.hosts` is required (SPEC-002 FR-55).
  - Default reading: "defaults plus the minimum required SPEC-002 settings" (`upstream.hosts` and `upstream.auth.mechanism=none`, on a free `proxy.listen` port; ADR-0021).
  - SPEC-003 AC-11 ("with no input, the record's `setting` is empty") is checked in process with a SPEC-001-only registry.
  - Affects: T21 migration, T26.

## SPEC-002 Amendment 1 (incorporated, e7d0401)

Every Amendment 1 change is mapped to tasks above, each marked "Amendment 1". The coverage tables include FR-60 to FR-65, the amended FRs, and AC-37 to AC-49. **No task was added, removed or re-waved.** Dependencies are unchanged.

Rework for tasks already in progress:

| Task | Change |
|---|---|
| T4 (BSON) | Top-level duplicate `Count` and `TopLevelKeyEqualsFirst` are new. The nesting limit of 1024 (top-level = 1, arrays count), `Validate` accepting duplicates, and `Lookup` first-wins already match the existing tests, which stay. |
| T5 | `Peek` and `WatchReadable` (data versus close, also on TLS) for FR-62. |
| T9 | FR-5 check order and minimums, OP_QUERY validation, FR-60 integrity, no `writeErrors`, `Done()` for hold-back. |
| T10 | "Reached" (G-8), readiness follows monitoring (G-9), `ReplyCodes` with the per-mode marking rule (replaces `NotPrimary`). |
| T11 | `Conn.CheckIdle`, retry inside the same checkout, `DiscardIdle`, `ConnUnsolicited`. |
| T12, T18 | Normative metric definitions (G-3, G-4, S4). P5 is changed: `server_error` is not counted in `request_errors_total`. |
| T14 | The FR-61 order, exact names (G-5), first handshake counts whatever its outcome (G-6). |
| T15 | Idle watcher (FR-62), `Report.AuthErr` (G-8). |
| T19 | Validate before write and hold back the last byte (FR-38); barrier ping reply marking and no client abort (G-12); `ReplyCodes`; S4 request counting. |
| T20 | Limit before TLS, TLS timeout class (G-15), `ForceDrainEnd`, `HoldDrain`. |
| T21 | FR-63 second signal, FR-64 hook isolation, the `shutdown_drain` hold point. |
| T22 | FR-65: `docs/operations/proxy.md` states L = K = 65,536 and the Amendment 1 operator statements. |
| TW1 | `RefuseApp`, unsolicited bytes and close on idle connections, partial-message byte counts, ping error replies, `writeErrors` replies. |
| T7 | Duplicate-key, kind-1-first, deep-nesting, malformed OP_QUERY and partial-ClientHello builders; repeated signals. |
| T23 to T26, T29 | AC-37 to AC-49 as listed. |

## Proposed for Owner

- **A-Q1 (FR-60 detail: the command name as a kind-1 identifier).**
  - Terms list the command name (the first key) as an inspected field, and FR-60 refuses a kind-1 section whose identifier equals an inspected field's name.
  - Comparing kind-1 identifiers with the command name needs unbounded memory when kind-1 sections arrive before the body: every identifier must be kept until the body is read. FR-60 itself rules out such costs for the general duplicate check.
  - Default (ADR-0017): refuse kind-1 identifiers equal to `$db`, `lsid`, `txnNumber`, `autocommit`, `startTransaction`, `pipeline` (on `aggregate`) or `collection` (on `getMore`), in either section order. Do **not** compare kind-1 identifiers with the command name.
  - Rationale: a kind-1 section cannot change the command the server runs (the server takes it from the body's first key), so no FR-33 bypass is possible this way.
  - A duplicate of the command name *inside the body* is still refused.
  - Affects: T9 only. No AC depends on it: AC-44 does not test a kind-1 section named like the command.
