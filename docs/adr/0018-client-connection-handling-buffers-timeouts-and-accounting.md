# ADR-0018: Client connection handling: buffer limits, I/O timeouts, liveness and accounting

- Status: Accepted (planner, 2026-10-24, for SPEC-002). The planner
  choices (P4, P8) were accepted by the Owner, and SPEC-002 Amendment 1
  made them normative (G-4, G-10, G-12, G-15, S4). Updated the same day.
- Related: SPEC-002 FR-2, FR-4, FR-12, FR-38, FR-41, FR-44, FR-48, FR-57,
  FR-58, NFR resource bounds, AC-2, AC-19, AC-21, AC-34, AC-35

## Decision

### Fixed constants (stated in the operator documentation, FR-51)

| Constant | Value | Where |
|---|---|---|
| Request buffer limit (FR-38) | 65,536 bytes, including the 16-byte header | `proxy.RequestBufferLimit` |
| Reply chunk (FR-58) | 65,536 bytes | `exchange.ReplyChunk` |
| Request streaming chunk after a lease | 65,536 bytes | `exchange.ForwardChunk` |

- Worst-case memory for messages without a lease is
  `proxy.max_client_connections` × 64 KiB (640 MiB at the default 10,000).
  Reply memory is at most `pool.max_size` × 64 KiB.
- Prefix and chunk buffers are pooled (`sync.Pool`) and returned at the end
  of each exchange. No buffer outlives an exchange (Data and compliance:
  transit-only).
- Since SPEC-002 Amendment 1 (G-10, FR-38, FR-58, FR-65), L = K = 65,536
  bytes is normative, not a planner choice. L includes the 16-byte header.
  The corraltest probe's `/constants` reports both (AC-48).

### Client I/O timeout (FR-57)

- `proxy.IOBudget` (domain, clock-based) keeps the read time used by the
  current message. Before each socket read the engine sets the read
  deadline to now + remaining budget. After the read it charges the elapsed
  time. No time is charged while the engine is not reading: queue wait,
  lease wait, upstream wait, and the pause at the request buffer limit.
- Writes: each reply or synthesized error gets a write deadline of
  now + `proxy.client_io_timeout` for the whole reply. For a streamed reply
  the deadline is set once, when the first reply byte is written, and covers
  the whole reply.
- Idle timeout (FR-4): between exchanges the read deadline is
  `proxy.client_idle_timeout` (none when `0s`). The first byte read starts
  the exchange and switches to the I/O budget.

### Client liveness while not reading (FR-41)

- While a fully read request waits for a lease, or while its request is in
  flight upstream, the engine watches the client socket without consuming
  bytes. It does **not** watch during a barrier `ping`: a barrier is not
  "in flight" under FR-41 (Amendment 1, G-12). A barrier always runs to
  completion, and on success the connection returns to the pool. On Linux it uses `syscall.RawConn.Read` with `recvfrom(MSG_PEEK |
  MSG_DONTWAIT)`. A 0-byte result means the peer closed, and EAGAIN means
  wait for readability through the netpoller. Any pending data means the
  client is alive, so watching stops (the client is pipelining).
- On other platforms, and on TLS connections where the close arrives as a
  `close_notify` record rather than a FIN, detection is not attempted. This
  is the "best effort" FR-41 allows.
- Detected close while queued: the request is removed from the queue and
  nothing is sent. Detected close while in flight: the upstream connection
  is discarded (`client_abandoned`).

### Client TLS handshake (FR-2, FR-44, FR-57; normative since Amendment 1, G-15)

- The connection limit is checked right after `Accept` and **before** the
  TLS handshake starts. An over-limit connection is closed without any TLS
  byte being sent: the listener accepts plain TCP and wraps it with
  `tls.Server` only after admission.
- The TLS handshake runs in the per-connection goroutine with a deadline of
  `proxy.client_io_timeout`. A failure closes the connection with
  `reason=tls_failure` and the `client TLS handshake failed` record. On
  timeout the record carries class `timeout` and detail
  `deadline_exceeded`.
- `tls.Config` has `MinVersion: tls.VersionTLS12`. With
  `proxy.tls.client_ca_path` set it uses `ClientAuth:
  tls.RequireAndVerifyClientCert`. No certificate field is ever logged.

### Connection and request accounting (normative since Amendment 1, G-4 and S4)

- A message that is closed by a client protocol error (FR-5 to FR-8,
  FR-38, FR-60) or by a client I/O timeout, after its first byte was read,
  counts as one request:
  - `corral_requests_total{outcome="proxy_error"}`, with `command` = the
    FR-12 label of the command name if it was read, else `other`;
  - once in `corral_request_errors_total`, with class `protocol`, or
    `timeout` for FR-57.

  Connections closed before any message byte is read are not requests.

- Every TCP connection accepted by the listener increments
  `corral_client_connections_accepted_total`, including connections over
  the limit. Every such connection later gets exactly one
  `corral_client_connections_closed_total{reason}` increment. So
  `accepted_total = sum(closed_total) + corral_client_connections`.
- `corral_client_connections` counts admitted connections (within the
  limit) that are open, including those still in the TLS handshake.
- Close reason mapping:

| Cause | `reason` |
|---|---|
| peer closed or reset between or during exchanges | `client_closed` |
| idle timeout (FR-4) | `idle_timeout` |
| client I/O timeout (FR-57) | `io_timeout` |
| client protocol error, including late validation (FR-5 to FR-7, FR-38) | `protocol_error` |
| outcome unknown after an upstream failure or upstream protocol error (FR-40, FR-58) | `upstream_failure` |
| FR-47 steps 3 and 4 | `shutdown` |
| TLS handshake failure or timeout | `tls_failure` |
| over `proxy.max_client_connections` (FR-2) | `limit_exceeded` |
| recovered panic (FR-42) | `internal_error` |

## Consequences

- AC-19 and AC-35 observe "bytes read before a lease" and "peak bytes held
  for one reply" through the `corraltest` probe (ADR-0022). Production builds
  do not count per connection beyond the metrics.
- The peek watcher is Linux-specific code with a no-op fallback, so twin
  tests that check queued-disconnect detection (AC-21) run on Linux only.
