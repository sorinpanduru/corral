# ADR-0018: Client connection handling: buffer limits, I/O timeouts, liveness and accounting

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Items marked
  "planner choice" are listed in `plans/SPEC-002.md`, "Proposed for Owner",
  so the Owner can override them.
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
- 64 KiB is the spec minimum. Every message corral answers or refuses
  locally (handshakes, auth commands, blocked commands) is far smaller in
  practice. Real `hello` documents are under 2 KiB, and drivers cap client
  metadata at 512 bytes.

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
  bytes. On Linux it uses `syscall.RawConn.Read` with `recvfrom(MSG_PEEK |
  MSG_DONTWAIT)`. A 0-byte result means the peer closed, and EAGAIN means
  wait for readability through the netpoller. Any pending data means the
  client is alive, so watching stops (the client is pipelining).
- On other platforms, and on TLS connections where the close arrives as a
  `close_notify` record rather than a FIN, detection is not attempted. This
  is the "best effort" FR-41 allows.
- Detected close while queued: the request is removed from the queue and
  nothing is sent. Detected close while in flight: the upstream connection
  is discarded (`client_abandoned`).

### Client TLS handshake (FR-44) (planner choice)

- The TLS handshake runs in the per-connection goroutine, bounded by
  `proxy.client_io_timeout`. A failure or timeout closes the connection with
  `reason=tls_failure` and the `client TLS handshake failed` record.
- `tls.Config` has `MinVersion: tls.VersionTLS12`. With
  `proxy.tls.client_ca_path` set it uses `ClientAuth:
  tls.RequireAndVerifyClientCert`. No certificate field is ever logged.

### Connection accounting (planner choice)

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
