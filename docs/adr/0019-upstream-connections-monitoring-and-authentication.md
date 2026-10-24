# ADR-0019: Upstream connections, monitoring and service authentication

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Items marked
  "planner choice" are listed in `plans/SPEC-002.md`, "Proposed for Owner".
- Related: SPEC-002 FR-18, FR-22, FR-24, FR-25, FR-28, FR-35, FR-37, FR-45,
  FR-46, FR-56; ADR-0002 (dependency pins), ADR-0015

## Decision

### Sockets (FR-28)

- Every upstream dial (pool and monitoring) uses `net.Dialer` with
  `KeepAliveConfig{Enable: true, Idle: 30s, Interval: 10s, Count: 3}`. On
  Linux, a `Control` function also sets `TCP_USER_TIMEOUT` to 60,000 ms
  through `golang.org/x/sys/unix`. That module is already in the module
  graph at v0.49.0 (ADR-0002) and becomes a direct requirement at the same
  version.
- The values are exported constants (`upstreamnet.KeepAliveIdle`, ...), so
  the unit and review check in AC-14 can assert them, and the operator
  documentation repeats them.

### TLS (FR-45)

- `tls.Config{MinVersion: TLS 1.2, RootCAs: pool from the validated
  upstream.tls.ca_path bytes, or nil for system roots, ServerName: host part
  of the seed or member address}`. `InsecureSkipVerify` is never set. No
  code path or setting can set it.

### Handshake on upstream connections

- First message on every upstream connection: OP_MSG `{hello: 1, helloOk:
  true, client: {application: {name: "corral" | "corral-monitor"}, driver:
  {name: "corral", version: <build version>}, os: {type: <GOOS>}}, $db:
  "admin"}`. There is no `compression`, `saslSupportedMechs` or
  `speculativeAuthenticate`.
- The whole connect, handshake and authentication sequence of a pool
  connection is bounded by `upstream.connect_timeout`. Each monitoring check
  (one `hello`) is bounded by it as well.

### SCRAM-SHA-256 (FR-22)

- corral implements the SCRAM-SHA-256 client itself
  (`internal/infra/upstreamconn/scram`) with the standard library
  (`crypto/pbkdf2`, `crypto/hmac`, `crypto/sha256`, `crypto/rand`). Username
  escaping (`=2C`, `=3D`) follows RFC 5802, and the password is prepared
  with SASLprep (RFC 4013) as the MongoDB authentication specification
  requires.
- SASLprep uses `github.com/xdg-go/stringprep` (pinned at v1.0.4, no
  transitive dependencies; the same library the official Go driver uses).
  This is a new production dependency, recorded here and in ADR-0002.
  - Reason for not using a full SCRAM library: corral must tell a failed
    **server signature** check apart from every other failure without
    reading error text (FR-22, SPEC-001 FR-29). Owning the conversation
    gives a typed `scram.ErrServerSignature`.
- Conversation: `saslStart` (mechanism `SCRAM-SHA-256`, `options:
  {skipEmptyExchange: true}`), `saslContinue` until `done`. The salted
  password is cached per (salt, iteration count) in memory for the process
  lifetime, to avoid one PBKDF2 per pool connection.
- Secret handling: the username and password come only from
  `settings.Proxy` (typed, secret). They are never put in an error, a log
  attribute or a span, and are held as `[]byte`. Conversation payloads are
  never logged (SPEC-001 FR-24).

### Error outcome of an attempt (FR-22, FR-56)

`upstreamconn` returns typed errors that the domain understands:

| Cause | Domain error | Class / detail |
|---|---|---|
| reply code 18 or 334 to `saslStart` or `saslContinue` | `upstream.AuthFailed` wrapping the reply's `ServerError` | `server` / `upstream_auth_failed`, with the upstream code |
| `scram.ErrServerSignature` | `upstream.AuthFailed` | `server` / `upstream_auth_failed` |
| any other error reply during handshake or authentication | transient, wrapping the reply's `ServerError` | `server` / `other` |
| network, TLS, timeout | transient, wrapping the cause | per SPEC-001 FR-29 (`tls_handshake`, `ECONNREFUSED`, `deadline_exceeded`, ...) |

### Monitoring (FR-37, FR-22)

- One monitoring connection per known host, `appName=corral-monitor`.
  Checks use `hello` only (no streaming, no `topologyVersion`).
- Only the current target's monitoring connection authenticates. When a host
  becomes the target, corral authenticates on its existing monitoring
  connection before the next check. While the authentication state is
  failing, each target check opens a fresh monitoring connection and
  authenticates on it (FR-22 recovery).
- FR-9 limits what is read from `hello` replies: corral does not read
  `arbiters`, so arbiter-only servers are never learned and never
  monitored. A server that reports `arbiterOnly: true` is dropped from the
  known members.

### Upstream close reasons (planner choice for the boundary cases)

| Cause | `kind=pool` `reason` | `kind=monitor` `reason` |
|---|---|---|
| idle past `upstream.max_idle_time` | `idle` | n/a |
| upstream network or protocol failure, failed barrier, check failure, replaced monitoring connection | `error` | `error` |
| host marked not primary or unusable, or a primary change | `not_primary` | n/a (monitoring continues) |
| discarded because the client side failed mid-exchange: client closed, client I/O timeout, late request validation failure | `client_abandoned` | n/a |
| FR-47 step 5 | `shutdown` | `shutdown` |
| upstream incompatible, or a non-matching server no longer monitored | `incompatible` | `incompatible` |
| member no longer in the known set | n/a | `idle` |
| recovered panic in a client handler holding a lease | `error` | n/a |

## Consequences

- `go.mod` gains `github.com/xdg-go/stringprep v1.0.4` (production) and
  makes `golang.org/x/sys` direct. ADR-0002's "no other third-party
  dependency" rule is satisfied by this ADR.
- The PBKDF2 cache holds derived key material in memory. It is never
  emitted and is dropped at process exit.
