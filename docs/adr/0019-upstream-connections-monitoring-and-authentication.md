# ADR-0019: Upstream connections, monitoring and service authentication

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Updated the same
  day after the Owner's answers: SCRAM uses `github.com/xdg-go/scram` behind
  a thin adapter, and the "planner choice" close reasons (P7) are accepted.
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

Owner decision, 2026-10-24: use the maintained library, not an in-house
client.

- `internal/infra/upstreamconn/scram` is a thin adapter around
  `github.com/xdg-go/scram` v1.2.0, with one `ClientConversation` per
  authentication.
- **Only the password is SASLprepped** (parity with the official Go
  driver). The adapter builds the client as:

  ```go
  scram.SHA256.NewClientUnprepped(username, stringprep.SASLprep.Prepare(password), "")
  ```

  - The username is passed through unchanged, apart from RFC 5802
    escaping. `NewClient` is not used, because it would also SASLprep the
    username, and a username that differs after preparation would then
    authenticate differently from the driver.
  - SASLprep of the password uses `github.com/xdg-go/stringprep`. A
    password that fails preparation gives `scram.ErrSASLprep`.
- The library also does:
  - username escaping (RFC 5802: `=2C`, `=3D`);
  - nonce generation;
  - the PBKDF2 cache: one `Client` per process caches derived keys per
    (salt, iteration count), so there is no PBKDF2 per pool connection.
- **Typed server-signature failure without reading error text** (FR-22,
  SPEC-001 FR-29):
  - before passing the server-final message to `Step`, the adapter checks
    its shape. A message starting with `e=` is a server-reported error,
    and malformed content is a protocol error (`scram.ErrProtocol`).
  - A well-formed `v=<base64>` message is passed to `Step`. If `Step`
    returns an error, or the conversation ends with `Valid() == false`,
    the adapter returns `scram.ErrServerSignature`.
  - The library's error values are never logged or wrapped into records.
    Only the adapter's constant sentinels leave the package.
- The adapter checks the server-first message's iteration count before
  stepping, and returns `scram.ErrIterations` when the count is out of
  range:
  - below 4096 (RFC 7677 minimum);
  - **above an upper bound** (constant `scram.MaxIterations`), so that a malicious or misconfigured upstream
    cannot make corral run an unbounded PBKDF2. The implementer states
    the bound and its reason here when T6 lands: "The upper bound is
    <N> iterations, because <reason>." (placeholder).
- Conversation on the wire (in `upstreamconn`): `saslStart` (mechanism
  `SCRAM-SHA-256`, `options: {skipEmptyExchange: true}`), then
  `saslContinue` until `done`.
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

### Amendment 1 rules for upstream connections

- **Unsolicited data on idle pool connections (FR-62, G-17).**
  - While a pool connection is idle, `upstreamconn` runs a non-consuming
    readability watch on it (the same netpoller peek as ADR-0018, for both
    plaintext and TLS sockets). Readability on an idle connection means
    either bytes or a close.
    - Bytes: the connection is discarded with `reason=error`, counted as
      an upstream protocol error `unexpected_response`, with the rate-limited
      `upstream protocol error` record.
    - Close: discarded with `reason=error`.
  - As a second line, `pool.Conn.CheckIdle()` runs a zero-deadline
    non-consuming probe at checkout, before any request byte is written. On
    failure the pool discards the connection and continues the same
    checkout with another connection: idle, newly opened, or waiting. The
    checkout's wait time and timeout are unchanged.
- **Not-primary marking by mode (FR-35, G-13, G-20).**
  - The engine reports every forwarded reply's top-level `code` and
    `writeConcernError.code`, and every barrier `ping` reply's top-level
    `code`, to the monitor. The monitor applies the mode rule:
    - replica-set mode: the FR-35 code list, top-level or
      `writeConcernError`;
    - single-`mongos` mode: only a top-level 91 or 11600 in an `ok: 0`
      reply.
  - `writeErrors` is never read.
  - A barrier `ping` reply that marks the host also fails the barrier
    (`reason=error`) (FR-29, G-12).
- **"Reached" (FR-19, G-8).** "Reached" is set by the first target check
  whose `hello` succeeded and passed FR-18, whatever the outcome of
  authentication on that monitoring connection.
- **Readiness follows monitoring (FR-46, G-9).** A failed pool connect,
  TLS or timeout attempt never changes readiness directly. It only triggers
  an immediate check. Pool authentication failures still set the
  authentication state.

### Upstream close reasons (accepted by the Owner, P7)

| Cause | `kind=pool` `reason` | `kind=monitor` `reason` |
|---|---|---|
| idle past `upstream.max_idle_time` | `idle` | n/a |
| upstream network or protocol failure, failed barrier (including a not-primary `ping` reply), unsolicited data or peer close on an idle pool connection (FR-62), check failure, replaced monitoring connection | `error` | `error` |
| host marked not primary or unusable, or a primary change | `not_primary` | n/a (monitoring continues) |
| discarded because the client side failed mid-exchange: client closed, client I/O timeout, late request validation failure | `client_abandoned` | n/a |
| FR-47 step 5 | `shutdown` | `shutdown` |
| upstream incompatible, or a non-matching server no longer monitored | `incompatible` | `incompatible` |
| member no longer in the known set | n/a | `idle` |
| recovered panic in a client handler holding a lease | `error` | n/a |

## Consequences

- `go.mod` gains `github.com/xdg-go/scram` v1.2.0 (production, approved by
  the Owner, P2). It brings `github.com/xdg-go/stringprep` v1.0.4 and
  `github.com/xdg-go/pbkdf2` v1.0.0, and `golang.org/x/sys` v0.49.0 becomes
  direct. ADR-0002 is the record.
- The library's key cache holds derived key material in memory. It is never
  emitted and is dropped at process exit.
