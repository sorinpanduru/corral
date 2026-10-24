# ADR-0021: Startup and shutdown with the wire listener

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Amends ADR-0006
  (SPEC-001 Amendment 3, item 10: binding the wire listener is the last
  startup step) and extends the ADR-0013 success path.
  Updated the same day for SPEC-002 Amendment 1 (FR-63 second signal).
- Related: SPEC-002 FR-1, FR-43, FR-46, FR-47, FR-63, AC-1, AC-26, AC-47;
  SPEC-001 FR-18,
  FR-34; ADR-0006, ADR-0013, ADR-0015

## Decision

### Startup (normal run, after the ADR-0013 success path and the lifecycle `starting` record)

5. Metrics registry, catalog lint and registration (unchanged).
6. Bind and serve the admin listener. `/readyz` answers 503 `starting`. The
   readiness port registered with the admin listener is the upstream
   monitor (`proxyapp.Service.Readiness`).
6a. Build the proxy service from `settings.Proxy`: domain objects, ports and
   adapters. No I/O happens here.
6b. Start the upstream monitor and pool maintenance in the background
   (FR-46: startup does not wait for the upstream).
7. Test hook `BeforeLastStep` (corraltest `CORRALTEST_HOLD_AT=laststep`).
8. `tracing.Start` (unchanged).
8a. **Last step: bind the wire listener** on `proxy.listen`.
   - On failure: a `config.Error` for `proxy.listen` that wraps the net
     error as its cause (detail `EADDRINUSE` etc.), with no address. It is
     logged with `component=config`. The monitor, pool, tracing and admin
     are stopped, and the exit code is 1 (FR-1, SPEC-003 FR-24).
   - On success: the `proxy` info record `wire listener bound` (no
     attributes), then the accept loop starts.
9. Mark startup complete, then the `lifecycle` startup-complete record. It
   always follows `wire listener bound` (AC-1).

### Shutdown

From cancellation (SIGINT, SIGTERM or context cancel):

1. `admin.MarkStopping()`, the `lifecycle` shutting-down record, and the
   `Stopping` hook (unchanged). From here `/readyz` is 503 `shutting_down`.
2. `proxyapp.Service.Shutdown`, which drives FR-47:
   - keep serving for `proxy.shutdown.listener_close_delay`;
   - close the listener;
   - close idle client connections, let in-progress exchanges finish, and
     refuse messages started later with code 91;
   - after `proxy.shutdown.drain_timeout`, measured from closing the
     listener, close the remaining client connections and discard their
     upstream connections;
   - close the pool and monitoring connections (`reason=shutdown`).

   Each step has a hold point for the corraltest build (ADR-0022).
3. Admin shutdown (bounded 5 s, unchanged), then tracing shutdown (bounded
   5 s, FR-34, unchanged). Exit 0.

The admin listener stays up during the drain, so `/readyz` keeps answering
`shutting_down` while connections drain.

### Second signal (SPEC-002 FR-63, Amendment 1, G-18)

- `cmd/corral` keeps receiving SIGINT and SIGTERM on a channel after the
  first one has started shutdown. It no longer relies only on
  `signal.NotifyContext`, which stops delivery after the first signal.
- The second signal is passed to the runtime as `Inputs.Force` (a
  `<-chan struct{}` closed on the second signal). The runtime:
  - emits one `lifecycle` warn record, `shutdown forced by second signal`,
    with no attributes;
  - calls `proxyapp.Service.ForceDrainEnd()`, which ends the drain at once
    as if `proxy.shutdown.drain_timeout` had elapsed. Steps 4 and 5 run,
    then admin and tracing shutdown within their bounds (FR-34), and the
    process exits 0.
- Further signals are drained and ignored, with no further record. A second
  signal before shutdown has reached the drain also skips the remaining
  `listener_close_delay`. The record is still emitted once.

### Required upstream settings and existing tests

- `upstream.hosts` is a required setting. The default
  `upstream.auth.mechanism` (`SCRAM-SHA-256`) needs credentials through the
  `upstream-auth-credentials` rule. Once the SPEC-002 group is registered, a
  production run with no input fails with exit 2. The default
  `proxy.listen` (`127.0.0.1:27017`) can also collide with a local `mongod`
  on developer machines and CI.
- `runtime.Run` builds and starts the proxy service only when the registry
  in use contains the SPEC-002 group (`settings.HasProxyGroup(reg)`).
  `settings.Registry()` always contains it, and a unit test asserts this, so
  a production binary cannot run without the proxy. In-process tests of
  SPEC-001 and SPEC-003 behaviour (for example SPEC-003 AC-11, "with no
  input, the record's `setting` is empty") pass `runtime.Inputs.Registry` =
  `settings.RegistryOf(settings.ObservabilityGroup())`. They are otherwise
  unchanged.
- Binary-level tests (SPEC-001, SPEC-003 and SPEC-002 functional suites)
  start corral with the minimal proxy arguments from
  `internal/testkit/corralargs`:
  - `--upstream.hosts=127.0.0.1:<closed port>`;
  - `--upstream.auth.mechanism=none`;
  - `--proxy.listen=127.0.0.1:<free port>`.

  The only exception is the SPEC-002 AC-1 default-address check. The
  extra arguments change the FR-20 `setting` list, so binary tests that
  assert it exactly must include them.

## Consequences

- ADR-0006 steps 7 to 9 change as above. Its readiness and response rules
  are unchanged.
- In the worst case, exit happens after listener_close_delay + drain_timeout
  + admin bound + flush bound. In practice the admin shutdown is
  immediate, which keeps AC-26 ("drain timeout + the FR-34 flush bound + a
  tolerance") achievable. AC-26 measures from closing the listener.
