# Running corral as a MongoDB proxy

corral accepts many client connections on the MongoDB wire protocol and
multiplexes their requests over a small, bounded pool of upstream
connections. This guide covers what operators need to know before pointing
applications at it. The behaviour is specified by SPEC-002. Settings are in
[configuration.md](configuration.md); logging, metrics and readiness plumbing
are in [observability.md](observability.md).

Examples use placeholders only (`<YOUR_DB_PASSWORD>`, `/run/secrets/<name>`).
Never put a real credential in a manifest, a script or a command line.

## What corral is, and is not

- corral speaks to one MongoDB deployment: a replica set (corral follows the
  current primary) or a single `mongos`. A standalone `mongod` is not
  supported: set `upstream.replica_set` for a replica set, or give exactly one
  `mongos` address in `upstream.hosts`.
- Load-balanced mode is not supported. Clients that ask for it in the
  handshake get an error.
- The wire listener speaks OP_MSG. OP_QUERY is accepted only for the
  handshake (`hello` and legacy `isMaster`), which older drivers use first.

## Minimal start

```
corral --upstream.hosts=db1.example:27017,db2.example:27017,db3.example:27017 \
       --upstream.replica_set=rs0 \
       --upstream.auth.username_file=/run/secrets/<name> \
       --upstream.auth.password_file=/run/secrets/<name>
```

Applications connect to `proxy.listen` (default `127.0.0.1:27017`) with a
plain connection string such as `mongodb://corral-host:27017/`. The process
logs `wire listener bound` once the port is open, and `/readyz` answers 200
once an upstream target is reachable.

## One shared upstream identity

All clients share corral's single upstream identity (`upstream.auth.*`).
The database cannot tell the applications apart, and its audit trail shows one
user. corral does what it can to keep one application from reaching another's
operations through the cross-client administrative commands. It blocks, with
code 13:

- `currentOp`, and an `aggregate` whose first pipeline stage is `$currentOp`
  (the top-level stage only: see "Residual exposure" below);
- `killOp`;
- `killAllSessions` and `killAllSessionsByPattern`;
- `killSessions` with an empty or absent session list (which, under a shared
  identity, would kill every client's sessions).

Other commands are forwarded and run as that identity. For example
`connectionStatus` and `whatsmyuri` are forwarded, and `connectionStatus` can
reveal the service identity's name. Session-listing stages such as
`$listSessions` and `$listLocalSessions` are forwarded too.

Handshake, authentication and blocked command names are matched exactly and
case-sensitively. A command with a different capitalisation is not the same
command to corral, and the server decides what it does.

corral also refuses (the connection is closed, reason `protocol_error`,
detail `malformed_message`) a command that carries a duplicate of a field it
inspects, because the server could read a different value than corral did,
and an `aggregate` whose `pipeline` is sent as a kind-1 document sequence
rather than in the command document.

## No client authentication

The wire listener does not authenticate clients. `saslStart`,
`saslContinue` and `authenticate` are answered with code 18
(`AuthenticationFailed`), and `getnonce` and `logout` with code 115
(`CommandNotSupported`). Nothing is forwarded, and applications must not send
credentials. Protect the listener by
network placement (a private network, a firewall, a security group) or with
mutual TLS: set `proxy.tls.enabled=true`, `proxy.tls.cert_path`, `proxy.tls.key` (by
file) and `proxy.tls.client_ca_path`, and only clients with a certificate that
the CA verifies are served.

TLS is optional, and not required by corral. Where traffic crosses networks
you do not control, enable it. corral checks the connection limit before the
TLS handshake, so a client over the limit is closed without any TLS bytes.
The handshake itself is bounded by `proxy.client_io_timeout`.

## Handshake metadata

corral answers the client's `hello` itself, as a `mongos` would, with
parameters taken from the upstream. The upstream sees corral's own metadata,
not the application's: pool connections use `appName=corral`, monitoring
connections use `appName=corral-monitor`. The application's `appName` and
driver name and version appear only in corral's own logs, sanitised. A client
may send its metadata once per connection; a second attempt is refused.

## Changes to application connection strings

Remove these options when moving an application to corral:

- credentials (`user:password@`);
- `authSource` and `authMechanism`;
- `replicaSet` (corral presents itself as a `mongos`);
- `compressors` (compression is not negotiated);
- `loadBalanced`.

Drivers should connect directly to corral, not discover through it.

## Read preferences

corral does not route reads. A read preference other than `primary` is
forwarded and runs on the target (the primary, or the single `mongos`), not on
a secondary. The request still succeeds, and the preference is not honoured.

## Sizing

### Upstream connections

`pool.max_size` bounds the connections one corral instance opens to the
upstream. The upstream accepts at most `maxIncomingConnections` in total.
Size the pool so that:

```
(number of corral instances) x pool.max_size
  + monitoring connections (one per upstream member per instance)
  + connections of other tools
  <= maxIncomingConnections
```

with room for failover, when every instance connects to the new primary at
once. `pool.min_size` connections stay open while the upstream is available.
`pool.max_waiters` bounds the requests queued for a connection, and
`pool.checkout_timeout` bounds each wait. A request that waits too long gets
code 262 (`ExceededTimeLimit`); one that finds the queue full gets 262 at once.

### Memory

Two constants bound what corral holds for a message at once. They are
normative values, and the code uses exactly these:

- L, the request buffer limit, is 65,536 bytes, including the 16-byte message
  header. A client request is read into at most L bytes before corral takes a
  lease. A request larger than L is forwarded in chunks as it is read, and
  the last byte is held back until the whole message has validated.
- K, the reply chunk, is 65,536 bytes. A reply is streamed from the upstream
  to the client one chunk at a time, and the last chunk is held back until the
  whole reply has validated.

The worst-case memory for messages is therefore: each open client connection
holds one buffer of L bytes, so `proxy.max_client_connections` x 64 KiB (with
the default 10,000 connections, about 625 MiB); and each exchange in progress
holds one reply chunk of K bytes, at most `pool.max_size` x 64 KiB at a time.
A barrier reply (see "moreToCome") is read whole, up to 1 MiB, one at a time
per connection in use. Add the Go runtime's own overhead and goroutine stacks.
Lower `proxy.max_client_connections` if the host is small.

## Leases, long operations and slow clients

A request holds an upstream connection (a lease) from checkout until its
reply has been written to the client. So:

- a long-running operation (a large aggregation, a long `getMore` wait with
  `awaitData`, a slow write concern) holds a lease for its whole duration;
- a slow client that reads a large reply slowly also holds a lease for as long
  as it takes to write the reply.

A large reply to a client on a slow network can hit `proxy.client_io_timeout`
before it is fully written: the whole reply must be written within one
timeout, and when it is not, corral closes that client connection
(`io_timeout`) and discards the upstream connection. Raise the timeout, or
use smaller batches, for such clients.

`proxy.client_io_timeout` bounds only client I/O: receiving a started message,
writing a reply (the whole reply shares one budget, so a slow reader cannot
stretch it chunk by chunk), and the TLS handshake. It does not bound the time
spent waiting for the upstream's reply, which is the operation's own time.
Put limits on operations with `maxTimeMS` in the application, and watch
`corral_pool_waiters` and `corral_pool_checkout_failures_total`.

`proxy.client_idle_timeout` closes a connection with no exchange in progress
after that long; the default `0s` never does.

A client that disconnects while its request waits for a lease leaves the
queue, and one that disconnects while the request is at the upstream causes
that upstream connection to be discarded, because its reply can no longer be
matched.

## Upstream connections: backoff and keepalive

When opening an upstream connection fails, corral backs off before the next
attempt. The first delay is 100 ms, each failure doubles it up to a cap of
5 s, and every delay carries jitter of plus or minus 20%. A success resets it.

Every upstream socket (pool and monitoring) has TCP keepalive: the first probe
after 30 s idle, then a probe every 10 s, giving up after 3 probes, and
`TCP_USER_TIMEOUT` of 60 s so that unacknowledged data does not hang a
connection forever. Idle pool connections are also watched for unsolicited
bytes or a close, and are discarded if either happens.

The authentication backoff for a failing service identity is described under
`upstream_auth_failed` below.

## Replica set monitoring

corral monitors every known member with its own connection (`appName=
corral-monitor`), at `upstream.heartbeat_interval`, and sends requests to the
member that is the current primary, using the highest election ID and set
version it has seen to ignore a stale primary.

- **Rolling restarts and reconfigurations.** Members that are down, ghost
  members (listed by the set but not yet initialised) and unreachable members
  stay monitored and are checked again at the normal interval. While the
  primary is lost, requests wait for the checkout timeout rather than failing
  at once. A member that is no longer listed by a current member and is not a
  seed is dropped.
- **A member with a different replica set name** is ignored, but kept and
  re-checked, and re-admitted when it reports the configured name again. Once
  a matching member has been seen, a set-name or topology mismatch never makes
  corral report the upstream as incompatible, and losing all matching members
  is transient unavailability. An incompatible wire version can still be
  reported at any time, because it describes the target itself.
- In replica-set mode, a not-primary or shutdown code (10107, 13435, 13436,
  11602, 11600, 189, 91) as the top-level `code` of an `ok: 0` reply, or as
  `writeConcernError.code` of an `ok: 1` reply, marks the host as not primary
  until the next check confirms otherwise. Codes inside `writeErrors` never
  mark it. With a single `mongos`, only a top-level 91 or 11600 in an `ok: 0`
  reply marks the target. Idle connections to that
  host close with reason `not_primary`. The error reply itself goes to the
  client unchanged, and the driver's own retry logic applies.

## moreToCome (unacknowledged writes)

A request with the moreToCome flag has no reply, so corral cannot tell when
the upstream is done with the connection. It follows such a request with its
own `ping` (a barrier) on the same connection and returns the connection to
the pool only when the `ping` reply arrives. If the barrier fails, the upstream
connection is discarded and the client connection stays open, because nothing
was owed to the client. A barrier reply with a not-primary code marks the host
exactly as a forwarded reply does.

## Readiness

`/readyz` (see [observability.md](observability.md)) is 200 when corral can
reach a usable upstream, and 503 with one of these reasons otherwise:

| Reason | Meaning |
|---|---|
| `starting` | Startup has not completed. |
| `upstream_unavailable` | No usable upstream target is known: no member is reachable, or there is no primary. |
| `upstream_auth_failed` | The upstream rejected corral's credentials. Requests fail fast with code 6. |
| `upstream_incompatible` | The upstream is outside the supported wire version range (at any time), or is a standalone, the wrong topology or the wrong replica set while no matching member has been found since startup. Requests fail fast with code 6. |
| `shutting_down` | Graceful shutdown has begun. |

Startup does not wait for the upstream: corral binds the port as its last
startup step, and answers handshakes with code 6 until an upstream is
available. `corral_upstream_available` is 1 exactly when `/readyz` is 200 for
upstream reasons.

An authentication failure is a code 18 or 334 reply to an authentication
command, or a SCRAM server signature that fails verification; anything else
(network errors, timeouts, other error replies) is transient. While the state
is failing corral opens no pool connections. Each check of the target, every
`upstream.heartbeat_interval` plus the reconnect backoff after consecutive
failures (100 ms growing to 5 s, plus or minus 20% jitter), opens a fresh
monitoring connection and authenticates on it. The first success clears the
state.

## Shutdown

On the first SIGTERM or SIGINT, corral:

1. keeps serving for `proxy.shutdown.listener_close_delay` (default `0s`), so
   a load balancer can take the instance out of rotation;
2. closes the listener;
3. closes idle client connections at once, lets exchanges in progress finish,
   and answers a message that starts after this step with code 91
   (`ShutdownInProgress`), then closes that connection;
4. after `proxy.shutdown.drain_timeout` (default `20s`, measured from closing
   the listener), closes the remaining client connections and discards their
   upstream connections;
5. closes the pool and the monitoring connections, then the admin listener and
   tracing, and exits 0.

`/readyz` answers 503 `shutting_down` for the whole sequence. A second
SIGTERM or SIGINT during shutdown ends the drain at once: if it arrives during
`proxy.shutdown.listener_close_delay` it cuts that delay short too, the
listener closes, all client connections are closed, step 5 follows, and the
process exits 0. corral logs `shutdown forced by second signal` once; further
signals are ignored.

## Credentials and TLS rotation

Rotating the service credentials, the wire listener's TLS material or the CA
bundles requires a restart: they are read once at startup (see "Restart and
SIGHUP" in [configuration.md](configuration.md)). Use a rolling restart behind
the load balancer, and the shutdown sequence above, to keep clients served.

## Least privilege for the service user

All applications run with the service user's privileges, so give it only what
the applications need:

- grant only the application roles the served applications need, on the
  databases they use;
- do not grant `clusterMonitor`, or any role with the `inprog`,
  `listSessions` or `serverStatus`-style cluster actions;
- grant no `find` on `system.profile`;
- grant no `queryStatsRead` or `queryStatsReadTransformed`.

## Residual exposure

corral does not close every route by which one client can see or affect
another's operations under a shared identity. Most of these routes work only
if the service user has privileges the guidance above advises against, but
the session route below does not, and least privilege does not prevent it
(report concerns to security@emag.ro).

**Session enumeration and kill, with no special privilege.** `$listSessions`
and `$listLocalSessions` need no privilege beyond the service user's own
sessions, and every client runs as that one user. So any client can list the
session ids of all clients, and then end them with `killSessions` naming
explicit ids, which corral forwards (only the empty-list form is blocked).
Granting less to the service user does not stop this. The route covers every
client of every corral instance that uses the same upstream identity, so
separate corral instances that share an identity do not isolate anyone. The
upstream identity is the boundary: keep mutually untrusting tenants on
separate upstream identities (separate service users).

The other routes:

- reading `system.profile`, which can show other clients' operations and
  filters;
- the `$queryStats` aggregation stage;
- `$listSessions` and `$listLocalSessions` with `allUsers: true` or another
  user's `users` list, and the `listSessions` route to the same data. These
  need the `listSessions` privilege, which the guidance above advises
  against. (The own-user form is the no-privilege route described above.)
- `$currentOp` nested inside a sub-pipeline (for example `$unionWith` or
  `$lookup`) wherever the server allows it, because corral inspects only the
  top-level stage;
- an `aggregate` with `$currentOp` wrapped in `explain`;
- `connectionStatus`, which reveals the service identity's name;
- `killSessions` with explicit `lsid` values naming another client's
  sessions, and `endSessions`, `abortTransaction` or `commitTransaction` sent
  with another client's session id. The ids are random 128-bit UUIDs, but a
  client can list every client's ids without special privilege (see "Session
  enumeration and kill" above), so do not rely on them being secret;
- session-less cursors, which any client that knows the 64-bit cursor id can
  `getMore` or `killCursors`. Official drivers always use sessions, which bind
  a cursor to its `lsid`.

Keep session and cursor ids out of logs that other tenants can read. corral
puts them only in logs and spans, never in metrics.

## Metrics

Served by `/metrics`; label values come from fixed sets.

| Metric | Type | Description |
|---|---|---|
| `corral_client_connections` | gauge | Open client connections. |
| `corral_client_connections_accepted_total` | counter | Every accepted connection, including those over the limit and those whose TLS handshake failed. |
| `corral_client_connections_closed_total` | counter | Closed connections, by `reason` (`client_closed`, `idle_timeout`, `io_timeout`, `protocol_error`, `upstream_failure`, `shutdown`, `tls_failure`, `limit_exceeded`, `internal_error`). |
| `corral_client_auth_rejected_total` | counter | Client authentication attempts refused. |
| `corral_commands_blocked_total` | counter | Blocked commands refused. |
| `corral_handshakes_total` | counter | Handshakes answered, by opcode and outcome. |
| `corral_protocol_errors_total` | counter | Protocol violations, by `side` (client or upstream) and `error_detail`. |
| `corral_requests_total` | counter | Requests, by command and outcome. |
| `corral_request_duration_seconds` | histogram | Request duration, by command. |
| `corral_request_errors_total` | counter | Failed requests, by `error_class`. |
| `corral_requests_dropped_total` | counter | moreToCome requests that got no reply because corral refused them. |
| `corral_pool_connections` | gauge | Pool connections, by state. |
| `corral_pool_max_connections` | gauge | The configured `pool.max_size`. |
| `corral_pool_waiters` | gauge | Requests waiting for a connection. |
| `corral_pool_checkouts_total` | counter | Checkouts, by outcome. |
| `corral_pool_checkout_failures_total` | counter | Failed checkouts, by detail. |
| `corral_pool_checkout_wait_seconds` | histogram | Time spent waiting for a connection. |
| `corral_upstream_connections_opened_total` | counter | Upstream connections opened, by kind (`pool`, `monitor`). |
| `corral_upstream_connections_closed_total` | counter | Upstream connections closed, by kind and reason. |
| `corral_upstream_connect_errors_total` | counter | Failed connection attempts, by kind and error class. |
| `corral_upstream_auth_failures_total` | counter | Upstream authentication failures, by kind. |
| `corral_upstream_available` | gauge | 1 when corral is ready for upstream reasons, else 0. |
| `corral_upstream_primary_changes_total` | counter | Changes of the target. |

## Log events

Warn and error events are rate-limited, one limiter per event, with a
`suppressed_count` attribute when records were dropped. No record carries
message bytes, credentials, certificate data or client names.

| Component | Message | Level |
|---|---|---|
| `proxy` | `wire listener bound` | info |
| `proxy` | `client connection opened` / `client connection closed` | debug |
| `proxy` | `client handshake answered` | debug |
| `proxy` | `client connection rejected` | warn |
| `proxy` | `client TLS handshake failed` | warn |
| `proxy` | `client authentication rejected` | warn |
| `proxy` | `client command blocked` | warn |
| `proxy` | `client protocol error` | warn |
| `proxy` | `client I/O timed out` | warn |
| `proxy` | `request handler panic recovered` | error |
| `pool` | `pool checkout timed out` | warn |
| `pool` | `pool wait queue full` | warn |
| `upstream` | `upstream connection opened` / `upstream connection closed` | debug |
| `upstream` | `upstream monitoring connection opened` / `upstream monitoring connection closed` | debug |
| `upstream` | `upstream connection failed` | warn |
| `upstream` | `upstream authentication failed` | warn |
| `upstream` | `upstream unavailable` | warn |
| `upstream` | `upstream available` | info |
| `upstream` | `upstream incompatible` | error |
| `upstream` | `upstream primary changed` | info |
| `upstream` | `upstream target marked not primary` | warn |
| `upstream` | `upstream protocol error` | warn |
| `upstream` | `upstream barrier failed` | warn |
| `config` | `wire listener bind failed` | error |
| `lifecycle` | `wire listener serve failed` | error |
| `lifecycle` | `shutdown forced by second signal` | warn |

Transient accept errors on the wire listener (out of descriptors, an aborted
connection, a timeout) are retried after 5 ms, doubling to 1 s, with no record
and no metric. A permanent accept error logs `wire listener serve failed`
once, shuts down without the listener-close delay (`/readyz` answers
`shutting_down`, exchanges in progress finish within the drain timeout) and
exits with code 1. A SIGTERM or SIGINT during that shutdown ends the drain at
once, and the exit code is still 1.

`wire listener bind failed` is the record for a failed bind of the wire
listener: component `config`, the setting `proxy.listen` (never the address)
and the errclass attributes. The process then exits with code 1.
