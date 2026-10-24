# ADR-0015: Proxy core package structure, layering and ports

- Status: Accepted (planner, 2026-10-24, for SPEC-002)
- Related: SPEC-002 FR-53, FR-48 to FR-50, Interfaces ("Domain observer
  ports"); SPEC-001 FR-1, FR-2, FR-3, FR-18; SPEC-003 FR-16; ADR-0001,
  ADR-0011; `plans/SPEC-002.md`

## Context

SPEC-002 adds the wire protocol model, the client-side request policy, the
pool, upstream monitoring and readiness, plus all the network I/O. CLAUDE.md
and SPEC-001 FR-1 require the domain to import only the standard library
(minus `log/slog`) and other domain packages. FR-53 lists what the domain must
own: the wire message model and validation, correlation, the lease and
barrier policy, the wait-queue and fail-fast policy, upstream state,
compatibility and the stale-primary rule, readiness evaluation, backoff, and
the shutdown state. Timers and settings stay in infrastructure, so the domain
needs an injected clock.

## Decision

### Domain packages (`internal/domain/...`, standard library only)

| Package | Owns |
|---|---|
| `clock` | `Clock` (`Now`, `AfterFunc`) and `Timer` (`Stop`) ports, plus `clock.System()`, a thin `time.AfterFunc` implementation. Every time-dependent domain type takes a `clock.Clock`. |
| `bson` | Hand-written BSON reader, validator, streaming walker and builder (ADR-0017). |
| `wire` | Header, opcodes, OP_MSG / OP_QUERY / OP_REPLY codecs, streaming message validation (FR-5 to FR-8, FR-58), FR-9 request and reply field extraction, the FR-40 collection list, correlation IDs (FR-11), builders for corral's own messages, and `wire.ProtocolError` with the protocol detail tokens. |
| `proxy` | Client-side policy: connection IDs (FR-3), the per-message decision (FR-8, FR-13 to FR-23, FR-33, FR-59), the FR-14 handshake reply, the synthesized-error table and FR-39 labels, request outcome (FR-48), the I/O-time budget (FR-57), the shutdown state machine (FR-47), and the `proxy.Observer` port. |
| `pool` | Lease and wait-queue policy (FR-25 to FR-27), the fail-fast/wait gate (FR-56), backoff, min size and idle expiry (FR-28), host marking and barrier abort (FR-29, FR-35), close reasons, and the `pool.Observer` port. `pool.Pool[C pool.Conn]` is generic over the connection type, so infrastructure gets its concrete connection back without type assertions. |
| `upstream` | Known members, check scheduling, SDAM primary selection and the stale-primary rule, compatibility (FR-18), authentication state (FR-22), availability mode (FR-56), upstream parameters and readiness (FR-46). The monitor implements `health.Readiness`. It defines the `upstream.Checker` port and the `upstream.Observer` port. |
| `health` | Existing readiness port; gains the three Amendment 3 reasons. |

Domain packages talk to each other only through narrow interfaces declared by
the consumer. `pool` does not import `upstream`: it declares `pool.Gate`
(admission: ready with a target host, wait, or fail fast with a detail
token), and the application layer adapts the upstream monitor to it.

Domain errors carry their FR-29 detail as a string token through a method
`CorralErrorDetail() string` (ADR-0016). The domain never imports `errclass`;
infrastructure classifies by duck typing, as ADR-0001 already allows.

### Infrastructure packages

| Package | Owns |
|---|---|
| `internal/infra/upstreamnet` | Upstream dialer with the FR-28 keepalive and `TCP_USER_TIMEOUT` settings, the client-liveness peek (ADR-0018), and the TLS configuration builders for FR-44 and FR-45 (built from bytes validated at load time). |
| `internal/infra/upstreamconn` (+ `scram`) | Upstream connection: dial, TLS, corral's handshake, SCRAM-SHA-256, monitoring checks. Implements `pool.Opener` and `upstream.Checker` (ADR-0019). |
| `internal/infra/proxyserver` | Wire listener: bind, accept, connection limit, client TLS, per-connection goroutine with panic recovery, idle timeout, the FR-47 shutdown driver. |
| `internal/infra/proxyserver/exchange` | Exchange engine: request prefix buffering, local answers and refusals, checkout, streaming forward and reply, barrier, client-gone handling, span calls. |
| `internal/infra/telemetry/proxyobs` | Adapters implementing `proxy.Observer`, `pool.Observer` and `upstream.Observer` as metrics and rate-limited log records. |
| `internal/infra/telemetry/spans` | FR-50 exchange span helper (root, `pool.checkout`, round-trip), with zero allocations when tracing is disabled. |

### Application layer

- `internal/app/settings/proxy.go`: the SPEC-002 `registry.Group` and the
  typed view `settings.Proxy` (ADR-0011, ADR-0020).
- `internal/app/proxyapp`: builds the domain objects from `settings.Proxy`,
  wires the ports (monitor to pool gate, pool events to immediate checks,
  not-primary marks to `pool.MarkHost`), and exposes
  `Service{Start, Bind, Shutdown, Readiness}` to the runtime.
- `internal/app/runtime`: calls `proxyapp` in the ADR-0021 order.

### Observers and spans

- Observer ports carry logs and metrics. Every method takes
  `context.Context` first, and every port has a `Noop` implementation in its
  own domain package (SPEC-001 FR-2, FR-3).
- Spans are created only by the exchange engine (infra), through
  `internal/infra/telemetry/spans`, because span lifetimes follow I/O events
  (written upstream, reply written) that the domain does not see. Observer
  methods do not create spans. This still satisfies SPEC-001 FR-2: domain
  occurrences are reported only through ports.

## Consequences

- The domain import check (SPEC-001 AC-1, SPEC-003 AC-14) covers the new
  packages unchanged. `bson` and `wire` must be implemented without any
  third-party BSON library (ADR-0017).
- Domain behaviour is testable with `fakeclock` (extended with timers) and
  fakes for `Opener`, `Checker` and `Gate`, without network.
- The app layer is the only place where ports are joined, so cross-package
  behaviour (for example "a not-primary reply closes idle pool connections")
  is tested in `internal/app/proxyapp` with fakes, and end to end with twins.
