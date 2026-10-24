# SPEC-002: Proxy core (wire listener, handshake, multiplexing pool, upstream)

Status: approved (Owner, 2026-10-24; revision 5)
Owner: Sorin Panduru
Last updated: 2026-10-24 (revision 5: optional should-fixes from the revision 4 re-review)

## Goal

Make corral do its job: accept many MongoDB client connections (motivating
case: the MongoDB PHP driver under PHP-FPM, one persistent connection per
worker) and multiplex their requests over a small, bounded pool of
authenticated upstream connections to a single MongoDB deployment. The pool
must be transparent enough that official drivers work unmodified, apart from
the connection string pointing at corral.

## Non-goals

- **Routing beyond the minimum.** v1 targets exactly one upstream deployment:
  a replica set (all traffic to the current primary) or a single `mongos`.
  - A standalone `mongod` is not supported (FR-18).
  - Out of scope: secondary reads, read-preference-based server selection,
    tag sets, hedged reads, multi-`mongos` load spreading with host affinity,
    shard awareness and multiple upstream deployments.
  - Read preferences other than `primary` are forwarded unchanged but run on
    the primary (FR-36).
- **Configuration loading.** SPEC-003 owns it. This spec only declares its
  settings under the SPEC-003 contract (FR-55, Interfaces).
- **Performance targets.** No latency, throughput or memory targets in v1.
  They belong to a later performance spec. Correctness under concurrency and
  the hard bounds in this spec are in scope: pool size, wait queue,
  connection limit, per-request buffering and I/O timeouts.
- **Wire compression** on either side (OP_COMPRESSED, snappy, zlib, zstd).
- **Legacy opcodes.** Only OP_MSG is proxied. OP_QUERY is accepted only for
  the handshake (FR-8).
- **Load-balanced mode.** Clients that connect with `loadBalanced=true` are
  refused (FR-17).
- **Streaming server monitoring.** corral does not take part in the
  awaitable/streaming `hello` protocol (FR-15).
- **Exhaust cursors.** corral never lets the upstream stream replies to it
  (FR-30).
- **Connection pinning of any kind.** Ordering after `moreToCome` is preserved
  by a barrier instead (FR-29).
- **Per-client upstream identity.** All upstream connections use one corral
  service identity. No per-user upstream pools, no pass-through of client
  credentials, and no corral-side user database.
- **Forwarding client handshake metadata upstream.** The upstream sees
  corral's own handshake metadata, not the application's `appName`.
- **Inspecting or changing command contents** beyond FR-9: no query
  rewriting, caching, filtering or result inspection. The only command
  authorization is the fixed blocklist in FR-33.
- **Cursor ownership tracking across client connections.** corral relies on
  the server binding cursors to logical sessions (FR-32).
- **Upstream client certificates and X.509 authentication.**
- **Unix-domain-socket listener** in v1, multiple wire listeners, and
  changing settings at runtime (SPEC-003 FR-17). Rotating credentials or
  certificates requires a restart.
- **Any wire-dump or message-body logging**, as SPEC-001 already rules out.

## Users and context

- **Application servers (clients).** PHP-FPM hosts using the MongoDB PHP
  driver (`ext-mongodb` / libmongoc). Each FPM worker keeps its own persistent
  connection, so a fleet of N hosts × W workers opens N×W server
  connections. Pointed at corral, the same fleet should hold at most
  `pool.max_size` upstream connections per corral instance. Other official
  drivers (the Go driver in particular, used by the test suite) must also work
  unmodified.
- **Operators / SREs.** They deploy corral as a sidecar per host or as a
  shared service, and configure it through SPEC-003 (flags and `CORRAL_*`
  environment variables, with secrets from the environment or `_FILE`
  references). They watch `/metrics`, `/readyz` and logs per SPEC-001.
- **DBAs.** They see corral's service identity on the server, with
  `appName=corral` (pool) and `appName=corral-monitor` (monitoring), not the
  individual applications.
- **Factory agents.** They build the domain model (wire message model,
  leases, pooling and upstream-state policy) under the DDD boundary of
  SPEC-001 FR-1.

### Terms

- **Client connection:** a TCP (optionally TLS) connection from a client to
  corral's wire listener.
- **Upstream connection:** an authenticated connection from corral to the
  upstream target, owned by the pool.
- **Monitoring connection:** a connection corral uses only to check upstream
  health and topology (FR-37). It is not counted in the pool.
- **Target:** the upstream server that leases go to: the current replica-set
  primary, or the configured `mongos`.
- **Request / exchange:** one client message and the replies it gets
  (SPEC-001 FR-22 / FR-32). An exchange *starts* when corral reads the first
  byte of a client message, and *ends* as FR-12 defines.
- **Lease:** use of one upstream connection for one exchange. It is checked
  out from the pool and returned when the exchange ends.
- **Upstream parameters:** the limits and versions taken from the target's
  `hello` reply (`maxBsonObjectSize`, `maxMessageSizeBytes`,
  `maxWriteBatchSize`, `logicalSessionTimeoutMinutes`, `minWireVersion`,
  `maxWireVersion`).
- **Synthesized error:** an error reply that corral creates itself instead of
  forwarding one from upstream (Interfaces, "Synthesized error replies").
- **Request buffer limit:** the fixed maximum number of bytes of one client
  message that corral holds in memory before that message has a lease
  (FR-38).

## Functional requirements

### Wire listener and client connections

- **FR-1:** corral runs one MongoDB wire-protocol listener, separate from the
  admin listener. Its bind address is `proxy.listen`.
  - An empty host or port 0 is rejected at load time (SPEC-003 FR-12 and the
    FR-55 rule). Binding to all interfaces requires an explicit `0.0.0.0` or
    `[::]`.
  - Failure to bind exits with code 1 (SPEC-003 FR-24). The error record
    carries `component=config` and `setting=proxy.listen`, and does not
    contain the address.
  - Binding the wire listener is the last startup step for SPEC-001 FR-18.
    Startup does not wait for the upstream to be reachable (FR-46).
- **FR-2:** At most `proxy.max_client_connections` client connections are open
  at once. A connection over the limit is accepted and closed straight away
  with no bytes sent, and is counted (FR-48, `reason=limit_exceeded`).
- **FR-3:** Every client connection gets a connection ID: a positive integer
  that fits in a signed 32-bit integer and is unique among the client
  connections open at that moment.
  - IDs are handed out in increasing order. After 2,147,483,647 they wrap to
    1, skipping IDs still in use.
  - The ID is reported as `connectionId` in the handshake reply (FR-14) and
    used as `corral.connection.id` (SPEC-001 FR-23).
- **FR-4:** An idle client connection (no exchange in progress) is closed
  after `proxy.client_idle_timeout`. The default `0s` disables the timeout,
  because PHP-FPM persistent connections are idle most of the time.

### Message framing and model

- **FR-5:** Framing. Every message starts with the standard 16-byte header
  (`messageLength`, `requestID`, `responseTo`, `opCode`, little-endian).
  - A `messageLength` below the minimum for its opcode is a client protocol
    error with detail `malformed_message`.
  - A `messageLength` above the effective maximum is a client protocol error
    with detail `message_too_large`. The effective maximum is the upstream
    `maxMessageSizeBytes`, or 48,000,000 bytes while upstream parameters are
    not yet known.
  - A client protocol error closes the client connection with no reply. This
    also covers non-MongoDB traffic, such as an HTTP request sent to the wire
    port (SPEC-001 AC-34).
- **FR-6:** Opcodes accepted from clients. Every rejected opcode below is a
  client protocol error with detail `unsupported_opcode`:

  | Opcode | Value | Handling |
  |---|---|---|
  | OP_MSG | 2013 | proxied or handled locally per FR-12 to FR-33 |
  | OP_QUERY | 2004 | handshake commands only (FR-8); anything else gets an OP_REPLY error (FR-8) |
  | OP_COMPRESSED | 2012 | rejected; connection closed |
  | OP_REPLY, OP_UPDATE, OP_INSERT, OP_GET_MORE, OP_DELETE, OP_KILL_CURSORS, any other value | | rejected; connection closed |
- **FR-7:** OP_MSG validation. These are client protocol errors, and the
  connection is closed with no reply:
  - any set flag bit in bits 2–15 (`malformed_message`). Bits 0–15 are
    "required" bits that a receiver must reject if it does not know them;
    only `checksumPresent` (bit 0) and `moreToCome` (bit 1) are defined.
    Optional bits 16–31, including `exhaustAllowed` (bit 16), are allowed;
  - `checksumPresent` set and the CRC-32C does not match
    (`checksum_mismatch`);
  - not exactly one kind-0 (body) section, an unknown section kind, a section
    that overruns the message, or a body that is not a valid BSON document
    (`malformed_message`);
  - a body with no first element, so there is no command name
    (`malformed_message`).

  Kind-1 (document sequence) sections are allowed and forwarded unchanged.
  When validation finishes only after forwarding has begun, FR-38 applies.
- **FR-8:** Legacy handshake over OP_QUERY.
  - An OP_QUERY whose full collection name ends in `.$cmd` is a handshake
    when either:
    - its query document's first key is `hello`, `isMaster` or `ismaster`;
      or
    - its query document has a `$query` field holding a document whose first
      key is one of those. Other wrapper fields, such as `$readPreference`,
      are ignored.

    A handshake is answered locally with an OP_REPLY (FR-13 to FR-19).
  - Every OP_REPLY corral sends has `cursorID` 0, `startingFrom` 0 and
    `numberReturned` 1.
    - A handshake reply, and handshake errors with codes 6, 186 and 72, have
      `responseFlags` 0. The document is the FR-14 reply, or the error
      document `{ok: 0, errmsg, code, codeName}`.
    - Any other OP_QUERY gets `responseFlags` with `QueryFailure` (bit 1)
      set, and the document `{$err, code: 352, codeName:
      "UnsupportedOpQueryCommand", ok: 0}` with a fixed `$err` text (detail
      `unsupported_opcode`). The client connection stays open.
- **FR-9:** Fields corral reads. corral reads only the following fields of
  the messages it handles. Everything else is opaque bytes that corral does
  not decode, interpret, keep beyond the exchange, or emit (SPEC-001 FR-24).
  - **From client requests:**
    - the header and OP_MSG flag bits;
    - the command name (the first key of the body);
    - `$db`, `lsid.id`, `txnNumber`, `autocommit`, `startTransaction`;
    - the collection, per SPEC-001 FR-40;
    - the `getMore` cursor ID (span attribute only);
    - for `aggregate` only, the *name* of the first stage of the top-level
      `pipeline` array (FR-33), never its arguments.
  - **From client handshakes:**
    - the values of `helloOk` and `loadBalanced`;
    - the presence of `topologyVersion` and `maxAwaitTimeMS`;
    - presence only of `client`, `compression`, `speculativeAuthenticate` and
      `saslSupportedMechs`. Their values are never decoded or kept. The one
      exception is `client`, from which the three SPEC-001 FR-25 metadata
      fields are taken: `client.application.name`, `client.driver.name` and
      `client.driver.version`.

    `saslSupportedMechs` holds a username, so it is checked for presence
    only (SPEC-001 FR-24).
  - **From upstream replies forwarded to clients:** `ok`, `code`, `codeName`,
    `writeConcernError.code` (FR-35) and `cursor.id` (span attribute only).
  - **From corral's own upstream exchanges** (handshake, authentication,
    monitoring and barrier `ping`):
    - the upstream parameters;
    - `isWritablePrimary`, `secondary`, `arbiterOnly`, `msg`, `setName`,
      `hosts`, `passives`, `primary`, `electionId` and `setVersion`;
    - `ok`, `code` and `codeName` of authentication replies (FR-22);
    - the authentication conversation fields corral needs for SCRAM. These
      are never emitted (SPEC-001 FR-24).
- **FR-10:** Forwarding is byte-faithful. A request forwarded upstream carries
  exactly the client's sections, byte for byte. A reply forwarded to the
  client carries exactly the upstream's sections, byte for byte. The only
  differences allowed are:
  - the header `requestID` and `responseTo` (FR-11);
  - the `checksumPresent` flag, the checksum and `messageLength`. corral
    either sends no checksum (and `messageLength` then shrinks by 4) or a
    correct one for the message it actually sends;
  - clearing the request's `exhaustAllowed` flag (FR-30).

  corral never adds, removes or changes a field inside a command or reply
  document that it forwards.

### Request/response correlation

- **FR-11:** Correlation.
  - Every message corral sends upstream has a `requestID` that is unique among
    corral's outstanding requests on that upstream connection.
  - An upstream reply whose `responseTo` matches no outstanding request on its
    connection is an upstream protocol error with detail
    `unexpected_response`: the upstream connection is discarded, and the
    affected client follows FR-40.
  - Every reply corral sends to a client has `responseTo` equal to that
    client's original `requestID`, and a `requestID` unique within that
    client connection.
  - Client `requestID` values are never assumed unique across client
    connections. Two clients sending the same `requestID` at the same time
    each get their own reply.
- **FR-12:** Ordering. A client connection has at most one exchange in
  progress at a time. corral handles a client's messages in arrival order and
  does not read the next one until the current exchange has ended. An
  exchange ends when one of these happens:
  - its reply has been fully written to the client;
  - for a forwarded `moreToCome` request, the barrier `ping` has completed
    (FR-29);
  - for a `moreToCome` message handled without forwarding, handling has
    finished (FR-59);
  - the client connection closes.

### Handshake and local commands

- **FR-13:** `hello`, `isMaster` and `ismaster` are always answered locally
  and never forwarded upstream. This holds for OP_MSG and OP_QUERY, and for
  the first and every later `hello` on a connection.
- **FR-14:** Handshake reply contents. corral presents itself as a `mongos`
  (sharded-cluster router), so that drivers see one server, do no host
  discovery and keep sessions, transactions and retryable writes enabled. The
  reply contains exactly these fields:

  | Field | Value |
  |---|---|
  | `isWritablePrimary` (for `hello`) or `ismaster` (for `isMaster` / `ismaster`) | `true` |
  | `helloOk` | `true`, only when the request contained `helloOk: true` |
  | `msg` | `"isdbgrid"` |
  | `maxBsonObjectSize`, `maxMessageSizeBytes`, `maxWriteBatchSize`, `logicalSessionTimeoutMinutes` | from the upstream parameters |
  | `minWireVersion` | the upstream's value, unchanged |
  | `maxWireVersion` | the upstream's value, capped at 25 (FR-18) |
  | `localTime` | corral's current time |
  | `connectionId` | the FR-3 ID |
  | `readOnly` | `false` |
  | `ok` | `1` |

  The reply has no `topologyVersion`, `compression`, `saslSupportedMechs`,
  `speculativeAuthenticate`, `setName`, `hosts`, `serviceId` or
  `$clusterTime`.
- **FR-15:** No streaming monitoring. Because the reply has no
  `topologyVersion`, drivers poll. A `hello` that carries `topologyVersion`
  and `maxAwaitTimeMS` is still answered at once, with the FR-14 reply.
- **FR-16:** Client metadata. A `client` document is accepted only on the
  first `hello` / `isMaster` of a connection. A later handshake command that
  carries `client` gets an error reply with code 186
  (`ClientMetadataCannotBeMutated`, detail `handshake_rejected`), and the
  connection stays open. Only `appName`, driver name and driver version are
  kept, sanitised per SPEC-001 FR-25, for logs and spans on that connection.
  The rest is dropped at once.
- **FR-17:** A handshake with `loadBalanced: true` gets an error reply with
  code 72 (`InvalidOptions`, detail `handshake_rejected`), and the connection
  stays open.
- **FR-18:** Upstream compatibility. corral decides whether the upstream is
  compatible from the results of its checks (FR-37), and re-decides after
  every check.
  - **Wire-version acceptance rule.** corral accepts an upstream server whose
    wire-version range overlaps 21–25 (MongoDB 7.0 to 8.0): its
    `maxWireVersion` ≥ 21 and its `minWireVersion` ≤ 25. This rule decides
    only whether corral accepts the server. The handshake reply forwards
    `minWireVersion` unchanged and caps `maxWireVersion` at 25 (FR-14).
  - **Single-`mongos` mode** (`upstream.replica_set` unset). The upstream is
    incompatible when the latest successful check of the configured host
    reports one of these:

    | Check result | `reason` |
    |---|---|
    | a standalone server (no `setName` and no `msg: "isdbgrid"`) | `standalone` |
    | a replica-set member (has `setName`) | `topology_mismatch` |
    | a `mongos` that fails the wire-version acceptance rule | `wire_version` |
  - **Replica-set mode** (`upstream.replica_set` set). A *matching member* is
    a checked server whose `setName` equals `upstream.replica_set`.
    - The upstream is incompatible when no matching member has been found
      since the last time one was known (or since startup), and at least one
      reachable seed reports one of these:

      | Seed check result | `reason` |
      |---|---|
      | a different `setName` | `replica_set_name_mismatch` |
      | a standalone server | `standalone` |
      | a `mongos` | `topology_mismatch` |

      When several seeds qualify, the reason comes from the first of them in
      `upstream.hosts` order.
    - The upstream is also incompatible, with `reason=wire_version`, when the
      current primary fails the wire-version acceptance rule.
    - Non-matching servers found alongside at least one matching member are
      ignored, and are not monitored further (FR-37).
  - The upstream becomes compatible again as soon as a later check result
    no longer meets any of these conditions.
  - Once a matching member is known, non-matching seeds stop being checked.
    If every matching member is later lost, that is transient unavailability
    (FR-56), not incompatibility.

  - **While incompatible:**
    - readiness is `upstream_incompatible` (FR-46);
    - handshakes and requests fail fast with code 6 and detail
      `upstream_incompatible` (FR-19, FR-56);
    - one `upstream incompatible` error record is emitted on each transition
      into this state, carrying the `reason` (FR-49).
  - **Compatible → incompatible** (for example a rolling change to an
    unsupported version, or a seed re-pointed at another deployment):
    - open client connections stay open, and their later handshakes and
      requests fail fast as above;
    - waiting requests are failed the same way;
    - idle pool connections to that host are closed at once, and leased ones
      when they are returned (`reason=incompatible`).
  - **Incompatible → compatible:**
    - the new upstream parameters apply from the next handshake reply;
    - clients that handshook earlier pick them up through their periodic
      `hello` polling, because corral does not push changes.
  - The same rule applies to any change in the upstream parameters while the
    upstream stays compatible.
- **FR-19:** Handshake availability.
  - If the upstream has never been reached since startup (no target check
    has succeeded), a handshake gets a synthesized error with code 6 and
    detail `upstream_unavailable`.
  - While the upstream is incompatible, a handshake gets code 6 with detail
    `upstream_incompatible`.
  - Incompatibility takes precedence: when both conditions hold (no target
    check has succeeded, but a seed's check shows incompatibility), the detail
    is `upstream_incompatible`. This matches FR-46 and FR-56.
  - In every other state, including transient unavailability and failing
    authentication, handshakes succeed using the last known upstream
    parameters.
  - The connection stays open in all cases.
- **FR-20:** Compression is not negotiated. A `compression` field in the
  handshake is ignored, and the reply has no `compression` field, so
  compliant drivers do not send OP_COMPRESSED.
- **FR-21:** `speculativeAuthenticate` and `saslSupportedMechs` in a handshake
  are ignored. The reply has neither field.

### Authentication

- **FR-22:** Upstream service identity.
  - Every pool connection is authenticated with corral's single configured
    service identity before it is used for any client request.
  - The mechanism is set by `upstream.auth.mechanism`: `SCRAM-SHA-256` or
    `none`. `none` is for unauthenticated deployments and tests.
  - The authentication source database is `upstream.auth.source`.
  - Of the monitoring connections, only the one to the current target
    authenticates. Monitoring connections to other members do not.
  - **What counts as an authentication failure.** An attempt counts as
    `upstream_auth_failed` only when one of these happens:
    - the upstream replies to an authentication command with code 18
      (`AuthenticationFailed`) or 334 (`MechanismUnavailable`);
    - the upstream's SCRAM server signature fails verification.
  - **Everything else is transient.** Other error replies during
    authentication, including the FR-35 not-primary and shutdown codes, and
    network errors, TLS errors and timeouts, count as transient connection
    failures (FR-56). They keep their own SPEC-001 FR-29 class and detail;
    an error reply is class `server` with detail `other`.
  - **Authentication state.** Outcomes on both pool and monitoring
    connections set the authentication state used by FR-46 and FR-56. It is
    *failing* after an authentication failure and stays failing until an
    authentication succeeds.
  - **Recovery.** While the state is failing:
    - corral makes no pool connection attempts;
    - each target check (every `upstream.heartbeat_interval`, delayed further
      by the FR-56 backoff after consecutive failures) opens a fresh
      monitoring connection to the target and authenticates on it;
    - the first success clears the failing state.
- **FR-23:** Client authentication. Clients do not authenticate to corral.
  - The client-sent commands `saslStart`, `saslContinue` and `authenticate`
    get a synthesized error with code 18 (`AuthenticationFailed`).
  - `getnonce` and `logout` get code 115 (`CommandNotSupported`).
  - All five use detail `client_auth_unsupported`. None is forwarded
    upstream. Each attempt is counted (FR-48) and logged per FR-49.
  - Access to the wire listener is controlled by network placement and,
    optionally, mutual TLS (FR-44).
- **FR-24:** Credential handling.
  - `upstream.auth.username` and `upstream.auth.password` are SPEC-003
    secret settings: they are supplied only through their environment
    variable or a `_FILE` reference, never on the command line, and have no
    default.
  - They never appear in any signal, error record or help output, or in any
    reply that corral synthesizes (SPEC-001 FR-24, SPEC-003 FR-14).
  - Replies that corral forwards from the upstream are upstream data that
    corral does not inspect (FR-9). They may contain the service identity's
    name, for example in `connectionStatus` (FR-33, FR-51).
  - The upstream address is not a connection string. `upstream.hosts` is a
    list of `hostport` values, so a value with userinfo or a `mongodb://`
    URI fails type validation (SPEC-003 FR-12), and the value is not echoed
    (SPEC-003 FR-11). No corral setting carries a connection string.

### Pooling policy

- **FR-25:** Bounds. The pool holds at most `pool.max_size` upstream
  connections, counting connections that are being established or
  authenticated. While the upstream is available, corral keeps at least
  `pool.min_size` connections open. Monitoring connections are not counted.
- **FR-26:** Lease per request. Every forwarded request checks out an upstream
  connection to the target. The lease ends, and the connection goes back to
  the pool, at one of these points:
  - when the full reply has been written to the client;
  - for a `moreToCome` request, when the barrier `ping` completes (FR-29);
  - when FR-38, FR-40, FR-41 or FR-57 discard the connection or give the
    lease back.

  Messages answered or refused locally (FR-8, FR-13 to FR-23, FR-33, FR-59)
  never check out a connection.
- **FR-27:** Wait queue.
  - A request that FR-56 allows to wait, and that finds no free connection,
    waits in a FIFO queue.
  - The queue holds at most `pool.max_waiters` requests. When that setting is
    unset, the bound is `proxy.max_client_connections`, which in effect means
    no separate bound, since each client has at most one exchange in
    progress.
  - A request that arrives while the queue is full gets the FR-39
    `pool_queue_full` error at once.
  - A request still waiting after `pool.checkout_timeout` gets the FR-39
    `pool_wait_timeout` error.
- **FR-28:** Upstream connections are opened on demand, and to keep
  `pool.min_size`, subject to the FR-56 backoff. A pool connection is ready
  for use only after corral's own handshake (`client.application.name =
  "corral"`, `client.driver` name `corral` with the corral version) and
  authentication (FR-22) have succeeded on it, within
  `upstream.connect_timeout`. An idle pool connection is closed after
  `upstream.max_idle_time` (`0s` disables this), as long as `pool.min_size`
  stays satisfied.
  - **Socket liveness.** Every upstream socket (pool and monitoring) enables
    TCP keepalive, with these fixed values, stated in the operator
    documentation:
    - first probe after 30 s idle, then every 10 s, giving up after 3
      unanswered probes, so a silent peer is detected within about 60 s;
    - where the platform supports it (Linux `TCP_USER_TIMEOUT`), a 60 s
      limit on unacknowledged sent data.

    A connection that fails these checks is treated as a network error.
- **FR-29:** Barrier after `moreToCome` (Owner decision M1). When a client
  sends an OP_MSG with `moreToCome` set and corral forwards it, the steps
  are:
  1. corral checks out a connection as for any request, writes the message,
     and sends nothing to the client;
  2. right after the message is written, corral sends its own
     `{ping: 1, $db: "admin"}` OP_MSG on the same upstream connection, with no
     `lsid`, and waits for its reply;
  3. only after that reply does the connection go back to the pool and the
     client's exchange end (FR-12). Because the upstream handles messages on
     one connection in order, the client's `moreToCome` message has then
     been processed before the client's next message is read. This preserves
     the ordering a directly connected client would see.

  The barrier has no timeout of its own, consistent with the Owner's Q8
  decision that there is no per-request upstream timeout: a barrier waits as
  long as the `moreToCome` message takes to run upstream (for example a
  large unacknowledged write). It is bounded only by upstream failure and by
  `proxy.shutdown.drain_timeout` during shutdown (FR-47).

  Barriers in progress on connections to a host are aborted at once when
  that host is marked unknown or not primary (FR-35). A blackholed host is
  also detected at the socket level (FR-28 keepalive settings), so a barrier
  cannot stall for the operating-system default of about 15 minutes.

  If the barrier fails or is aborted (the upstream connection closes or
  errors, an error reply, a protocol error, or the host is marked unknown or
  not primary), the upstream connection is discarded. The
  client connection stays open, because an unacknowledged message never
  reports an outcome to the client.

  The barrier `ping` produces no span, is not counted in
  `corral_requests_total`, and is not a client exchange. There is no
  connection pinning of any kind.
- **FR-30:** No exhaust streaming. corral clears `exhaustAllowed` on every
  request before forwarding it, so the upstream answers with ordinary single
  replies. corral never sends a client a reply with `moreToCome` set.
- **FR-31:** No pinning for transactions or sessions. Requests that belong to
  a logical session or a multi-document transaction are leased per request
  like any other. This is correct because:
  - session and transaction state lives on the server, keyed by `lsid` and
    `txnNumber`, not by connection;
  - all leases go to the same target (FR-34, FR-35);
  - all upstream connections use one identity (FR-22).
- **FR-32:** No cursor pinning or ownership tracking. `getMore` and
  `killCursors` are leased per request like any other command. This relies
  on:
  - cursors on the server being independent of the connection;
  - the server binding cursors created in a session to that session's `lsid`.
- **FR-33:** Blocked and forwarded commands (Owner decision, 2026-10-24).
  - corral blocks these cross-client administrative commands:
    `currentOp`, `killOp`, `killAllSessions`, `killAllSessionsByPattern`,
    and an `aggregate` whose top-level `pipeline` starts with a
    `$currentOp` stage.
    - Matching is on the command name as received, which is case-sensitive.
    - A blocked command gets a synthesized error with code 13
      (`Unauthorized`), a fixed `errmsg` and detail `command_blocked`. It is
      not forwarded, and is counted and logged (FR-48, FR-49).
    - Only the top-level first pipeline stage is inspected. Nested pipelines
      (for example inside `$unionWith` or `$lookup`) are not inspected, and
      this limitation is documented (FR-51).
  - Every other command, apart from FR-13 to FR-23, is forwarded unchanged.
    This includes commands whose result describes corral's upstream
    connection or identity rather than the client's (`connectionStatus`,
    `whatsmyuri`). The operator documentation states this (FR-51).

### Upstream deployment and failover

- **FR-34:** The upstream deployment is set by `upstream.hosts` (one or more
  `host:port` seeds) and `upstream.replica_set` (optional) (Owner decision
  M2):
  - with `upstream.replica_set` set, the upstream is a replica set with that
    name, and the target is its current primary;
  - with it unset, `upstream.hosts` has exactly one entry, which must be a
    `mongos`. The cross-setting rule `upstream-single-host` enforces the
    single entry.
  - A standalone server, or any other mismatch, is incompatible (FR-18).
- **FR-35:** Replica-set primary tracking.
  - **Primary selection:** a member is the primary when its latest check
    reports `isWritablePrimary: true`, its `setName` equals
    `upstream.replica_set`, and its report is not stale.
  - **Stale primary rule** (SDAM, wire version ≥ 17): corral keeps the
    largest `(electionId, setVersion)` pair seen from any primary report,
    comparing `electionId` first and then `setVersion`. A primary report with
    a smaller pair is stale, and that member is treated as not primary. If
    two members claim primary, the one with the larger pair wins.
  - **Failed target check** (SDAM): when a check of a previously usable
    target fails with a network, TLS or timeout error, corral retries that
    check once at once, on a new monitoring connection. Readiness and leases
    are unaffected during the retry. If the retry also fails, the target is
    marked *unknown*. A target that was not usable before its check is marked
    unknown on the first failure. Once the target is unknown, the upstream
    has no usable target. Requests wait under FR-56,
    readiness is `upstream_unavailable` (FR-46), and corral checks all known
    members at the FR-37 fast interval until a target is found again.
  - **Primary change:** new leases go only to the new primary. Pool
    connections to the old primary are closed at once when idle, and when
    returned otherwise (`reason=not_primary`).
  - **Not-primary errors:** when a forwarded reply from the target carries one
    of these codes, either as the top-level `code` of an `ok: 0` reply or as
    `writeConcernError.code` in an `ok: 1` reply:
    10107 (`NotWritablePrimary`), 13435 (`NotPrimaryNoSecondaryOk`), 13436
    (`NotPrimaryOrSecondary`), 11602 (`InterruptedDueToReplStateChange`),
    11600 (`InterruptedAtShutdown`), 189 (`PrimarySteppedDown`), 91
    (`ShutdownInProgress`), then:
    - corral forwards the reply unchanged;
    - corral marks that host not primary (or, for a `mongos` target,
      unusable) at once;
    - corral starts an immediate check (FR-37);
    - corral holds new leases (requests wait under FR-56) until a check
      confirms a primary or a usable target;
    - pool connections to that host are closed as for a primary change.
  - **Single `mongos` target:** the same not-primary handling applies.
- **FR-36:** Read preference. `$readPreference` is forwarded unchanged. All
  requests run on the target, whatever read preference they ask for.
- **FR-37:** Upstream monitoring.
  - **Known members.** The known members are every seed in `upstream.hosts`,
    plus every server learned from the check result of any matching member
    (FR-18), primary or not. corral learns from that member's `hosts` and
    `passives`, and from its `primary` field, which names the member it
    believes is primary. Discovery therefore works while no primary is known.
    - Servers listed only under `arbiters`, or reporting `arbiterOnly:
      true`, are not monitored.
    - A server whose `setName` does not match is not added to the known
      members. A non-matching seed is still checked so that FR-18 can report
      why.
  - **Check intervals.**
    - While a usable target is known, each known member is checked every
      `upstream.heartbeat_interval`.
    - While no usable target is known (never reached, election, target
      marked not primary or unknown), *all* known members are checked
      repeatedly, at least 500 ms apart per member (the SDAM
      `minHeartbeatFrequencyMS`), until a usable target is found.
  - **Connections.** corral uses at most one monitoring connection per host,
    with `client.application.name = "corral-monitor"`. Each check is a
    `hello` exchange limited by `upstream.connect_timeout`. Authentication
    happens only on the target's monitoring connection (FR-22).
  - **Immediate checks.** A failed pool connection attempt, a network error
    on a leased connection, or a not-primary error (FR-35) starts an
    *immediate check*: an immediate check of all known members, each still at
    least 500 ms after its previous check.
  - **Results.** The checks provide the upstream parameters, compatibility
    (FR-18), the current primary (FR-35) and readiness (FR-46).

### Request size, buffering and streaming

- **FR-38:** Bounded buffering of requests.
  - **Request buffer limit.** A fixed value chosen by the planner, at least
    64 KiB, and stated in the operator documentation. It is not a setting.
  - **Before a lease.** While a message has no lease (not yet classified, or
    waiting in the queue), corral holds at most the request buffer limit of
    it in memory. The rest stays unread in the client socket, so TCP flow
    control holds the client back.
  - **Deciding fields must fit.** corral decides whether a message is
    answered locally, refused, blocked or forwarded from the buffered prefix
    alone.
    - If the command name, or for `aggregate` the first pipeline stage name,
      is not within the prefix, the message is a client protocol error with
      detail `malformed_message`.
    - A message that corral answers or refuses locally (handshake, auth
      command, blocked command) must fit entirely within the limit.
      Otherwise it is a client protocol error with detail
      `message_too_large`.
  - **After a lease.** The rest of the message is streamed to the upstream,
    so corral never needs to hold the whole message. Validation (FR-7)
    continues while streaming. If it fails after part of the message has
    been sent upstream, corral discards the upstream connection and closes
    the client connection with no reply. This is counted as a client
    protocol error with the matching detail.
  - **Refused before forwarding.** A message answered with a synthesized
    error before any byte was forwarded (FR-39) is read to its end and
    discarded, subject to FR-57, so the connection stays correctly framed.
    The FR-39 labels are determined from the whole message as it is read.
  - **Memory bound.** Worst-case memory for messages without a lease is
    `proxy.max_client_connections` × the request buffer limit.
  - **Sizes.** corral does not check document or batch size against
    `maxBsonObjectSize` or `maxWriteBatchSize`; the upstream enforces those.

### Failure handling

- **FR-39:** Request not executed → synthesized error reply. When corral knows
  that a request was never sent upstream, the client gets a synthesized error
  reply (Interfaces, "Synthesized error replies"), and its connection stays
  open.
  - For codes 262, 6 and 91 only, error labels let official drivers apply
    their standard retry rules:
    - `RetryableWriteError` when the request is a retryable write (it has
      `txnNumber` and no `autocommit: false`) or is `commitTransaction` /
      `abortTransaction`;
    - `TransientTransactionError` when the request is inside a transaction
      (`autocommit: false`) and is not `commitTransaction` /
      `abortTransaction`.
  - Other synthesized errors carry no labels.
  - A `moreToCome` message that would get a synthesized error gets no reply
    (FR-59).
- **FR-40:** Outcome unknown → close the client connection. corral discards
  the upstream connection and closes the client connection without a reply
  (or without the rest of the reply) when either of these happens after the
  request has been (even partly) sent and before the full reply has been
  forwarded:
  - the upstream connection fails, or breaks the protocol (FR-11, FR-58);
  - validation of the request fails late (FR-38).

  This gives the driver the same network-error signal, and the same
  driver-side retry behaviour, that it would get when connected directly.
- **FR-41:** Client gone mid-exchange.
  - **In flight upstream.** If the client connection closes or fails while
    its request is in flight upstream, corral discards that upstream
    connection rather than waiting for the reply
    (`reason=client_abandoned`).
  - **Queued.** If the client connection closes while its request is still
    waiting in the queue, and corral detects the close, the request is
    removed from the queue and nothing is sent upstream. Detection is best
    effort: corral detects a close only when the peer's close is observable
    on the socket. An undetected close is handled by the in-flight rule once
    the request has been forwarded.
- **FR-42:** A panic recovered while handling one client connection closes
  that client connection and discards any upstream connection it held. Other
  connections and the process continue. It is reported per SPEC-001 FR-29
  (class `internal`).
- **FR-43:** Upstream unavailability never closes existing client connections
  by itself. Clients keep getting handshake replies (FR-19), waits or
  synthesized errors (FR-56) until the upstream recovers.

### TLS

- **FR-44:** Client-side TLS. It is optional and off by default; it is not
  mandatory in any deployment.
  - When `proxy.tls.enabled` is `true`, the wire listener accepts only TLS
    connections, with TLS 1.2 or later. A plaintext connection fails the TLS
    handshake and is closed.
  - The certificate chain comes from `proxy.tls.cert_path`. The private key
    is the secret setting `proxy.tls.key` (PEM text, normally supplied via
    `CORRAL_PROXY_TLS_KEY_FILE` or `--proxy.tls.key_file`).
  - When `proxy.tls.client_ca_path` is set, every client must present a
    certificate that chains to that CA (mutual TLS), or it is refused.
  - A failed handshake is counted (FR-48, `reason=tls_failure`) and logged per
    FR-49. The client certificate's subject and SANs are never emitted
    (SPEC-001 FR-24).
  - Unreadable or invalid certificate or key material, or a key that does not
    match the certificate, is a config error at load time (exit 2, SPEC-003
    FR-24).
  - The certificate, CA and key bytes that were validated at load time are
    exactly the bytes used to build the TLS configuration. corral does not
    read those files again; a file changed after startup has no effect until
    restart.
- **FR-45:** Upstream TLS.
  - `upstream.tls.enabled`, off by default. When `true`, every pool and
    monitoring connection uses TLS 1.2 or later, and verifies the server
    certificate chain and host name. The trust roots are
    `upstream.tls.ca_path` (bytes validated at load time, as in FR-44), or
    the system roots when that is unset.
  - There is no setting that turns off certificate or host-name verification.
  - A failed upstream TLS handshake makes that connection attempt fail, with
    class `network` and detail `tls_handshake` (SPEC-001 FR-29).

### Readiness

- **FR-46:** Readiness meaning. This is the single authoritative definition
  of readiness for SPEC-001 FR-18. After startup has completed and before
  graceful shutdown, corral is ready when all of these hold:
  - a target check has succeeded since startup, and the upstream is
    compatible (FR-18);
  - a usable target is currently known: a non-stale primary, or the
    `mongos`, that has not been marked not primary, unusable or unknown
    (FR-35);
  - the authentication state is not failing (FR-22): the most recent
    authentication attempt to the target, on a pool or a monitoring
    connection, did not end in `upstream_auth_failed`.

  When not ready, exactly one reason is reported, chosen by this precedence:

  | Precedence | Reason | When |
  |---|---|---|
  | 1 | `upstream_incompatible` | FR-18 incompatibility |
  | 2 | `upstream_auth_failed` | the authentication state is failing |
  | 3 | `upstream_unavailable` | anything else: never reached, no usable target, connection failures, TLS failures |

  - Pool saturation (a full pool, waiting requests or timeouts) does not
    affect readiness, so that load does not take instances out of rotation
    and push the load onto the rest.
  - Startup does not wait for the upstream. Upstream authentication failure
    is not fatal at startup or later.

### Graceful shutdown

- **FR-47:** Shutdown sequence. From the start of graceful shutdown (SPEC-001
  FR-18; `/readyz` already returns `shutting_down`):
  1. For `proxy.shutdown.listener_close_delay`, corral keeps accepting and
     serving normally, so that load balancers can see that it is not ready.
  2. The wire listener closes, and no new client connections are accepted.
  3. Each open client connection is handled as follows:
     - **no exchange in progress** (corral has not read any byte of a next
       message): the connection is closed at once. Bytes the client sent
       but corral never read are discarded with it;
     - **exchange in progress**, including a request waiting in the queue:
       it runs to completion under the normal rules (lease, timeout or
       synthesized error), and the connection is then closed;
     - **a message whose first byte corral reads after step 3 has started**
       (only possible on a connection not yet closed): it gets the
       synthesized `shutting_down` error (code 91), unless it is a
       `moreToCome` message (FR-59), and the connection is then closed.
  4. When `proxy.shutdown.drain_timeout` has passed since step 2, the
     remaining client connections are closed, and their upstream connections
     are discarded.
  5. Pool and monitoring connections are closed. corral then continues with
     the SPEC-001 shutdown steps (tracing flush per FR-34) and exits 0.

  The upstream is not sent `endSessions` or `killCursors` for clients'
  sessions or cursors; those belong to the clients.

### Observability (extends SPEC-001)

- **FR-48:** Metrics. corral adds the metrics in Interfaces, "Metrics added by
  this spec", to the SPEC-001 FR-31 catalog. Every metric follows SPEC-001
  FR-10, FR-11 and FR-13, and uses only label names in the SPEC-001 FR-11
  allowlist as amended by Amendment 3.
- **FR-49:** Log events. corral adds the log components and events in
  Interfaces, "Log components and events". Every record follows SPEC-001 FR-7,
  FR-24, FR-25, FR-29 and FR-39.
  - `msg` values are the fixed constants listed.
  - Each `reason` value comes from the enumeration declared for its event.
  - warn- and error-level events marked "rate-limited" are limited to at most
    one record per 30 seconds per event, carrying a `suppressed_count`, as in
    SPEC-001 FR-33.
  - Per-exchange records for handshakes, `hello` and successful upstream
    authentication are emitted at debug level only (SPEC-001 FR-32).
  - Authentication failures are logged at warn (SPEC-001 FR-32).
  - Synthesized errors never carry `db.response.status_code` or
    `corral.db.response.code_name` (SPEC-001 FR-23).
- **FR-50:** Spans. Proxied exchanges produce spans per SPEC-001 FR-22, FR-23
  and FR-32:
  - **ordinary leased exchange:** root, `pool.checkout` and round-trip;
  - **`moreToCome` request:** root, a `pool.checkout` child whenever a
    checkout happens (always, for a forwarded message, since there is no
    pinning), and round-trip. The root and round-trip spans end when the
    message has been written upstream. The barrier `ping` (FR-29) produces no
    span and does not extend either span. This matches SPEC-001 FR-32 as
    amended by Amendment 3;
  - **synthesized error after a checkout attempt** (`pool_wait_timeout`,
    `pool_queue_full`, or a fail-fast error found during checkout): root and
    `pool.checkout`, both ending in error;
  - **synthesized error without a checkout attempt** (`shutting_down`,
    `command_blocked`, unsupported OP_QUERY): root only, ending in error;
  - **outcome unknown (FR-40):** root and round-trip, both ending in error;
  - **no spans:** handshakes (FR-13), the FR-23 auth commands, corral's own
    upstream handshakes, authentication, monitoring and barrier pings
    (SPEC-001 FR-32 exclusions), and client protocol errors that close the
    connection (FR-5 to FR-7, FR-38).
- **FR-51:** Operator documentation states:
  - that all clients share corral's single upstream identity. corral blocks
    `currentOp`, `$currentOp` (top-level stage only), `killOp`,
    `killAllSessions` and `killAllSessionsByPattern`, but other commands such
    as `connectionStatus`, `whatsmyuri` and other session-listing stages are
    forwarded, see and act as that identity, and may reveal its name
    (FR-24, FR-33);
  - that the upstream sees corral's handshake metadata (`appName=corral` /
    `corral-monitor`), not the application's;
  - that read preferences other than `primary` run on the target (FR-36);
  - that a standalone `mongod` is not supported (FR-34);
  - how to size `pool.max_size` against the upstream `maxIncomingConnections`
    across all corral instances;
  - the request buffer limit and the resulting worst-case memory (FR-38);
  - that long-running operations, and slow clients reading large replies,
    hold a lease for their whole duration. `proxy.client_io_timeout` bounds
    only client I/O, not the time spent waiting for the upstream's reply
    (FR-57);
  - the reconnect backoff constants (FR-56) and the upstream socket keepalive
    values (FR-28);
  - that the wire listener does not authenticate clients, and must be
    protected by network placement or mutual TLS (FR-23, FR-44);
  - which options applications must remove from their connection strings
    when they move to corral: credentials, `authSource`, `authMechanism`,
    `replicaSet`, `compressors` and `loadBalanced`;
  - that rotating the service credentials or TLS material requires a restart
    (SPEC-003 FR-17);
  - least-privilege guidance for the service user:
    - grant only the application roles the served applications need;
    - do not grant `clusterMonitor` (or any role with `inprog`,
      `listSessions` or `serverStatus`-style cluster actions);
    - grant no `find` on `system.profile`;
    - grant no `queryStatsRead` / `queryStatsReadTransformed`;
  - the residual cross-client exposure routes listed in Data and compliance,
    and that the least-privilege guidance above closes most of them.

### Required amendment to SPEC-001

- **FR-52:** SPEC-002 cannot be approved until SPEC-001 Amendment 3 has been
  approved and promoted. Amendment 3 contains everything listed in
  Interfaces, "SPEC-001 amendments requested by this spec". The Owner approved
  the package content on 2026-10-24 (Q12). The additions made since then are
  listed in the Amendment 3 decisions log for Owner approval of the text. The
  draft is at `specs/drafts/SPEC-001-observability-conventions.md`.

### Testability

- **FR-53:** The domain model lives in domain packages that obey SPEC-001 FR-1
  and SPEC-003 FR-16, and reports occurrences through observer ports
  (SPEC-001 FR-2, FR-3). Network I/O, TLS, timers and settings stay in
  infrastructure. The domain model covers:
  - the wire message model and validation;
  - correlation;
  - the lease and barrier policy;
  - the wait-queue and fail-fast policy;
  - upstream state, compatibility and the stale-primary rule;
  - readiness evaluation;
  - backoff;
  - the shutdown state.
- **FR-54:** Functional tests run corral as a binary against a containerized
  `mongod`, with official drivers as clients, using synthetic data and
  synthetic credentials only.
  - The matrix: the Go driver, and the PHP driver (`ext-mongodb`) including a
    PHP-FPM setup with persistent connections; MongoDB 7.0 and 8.0; a
    single-node replica set, a 3-node replica set for failover, and a single
    `mongos` test topology for the `mongos` target.
  - Failure cases (upstream reset mid-exchange, slow upstream, unreachable
    upstream, wire-version and topology overrides, not-primary errors) use a
    simulated upstream or a fault-injecting TCP intermediary from `twins/`.

### Settings

- **FR-55:** This spec declares its settings under the SPEC-003 contract, in
  the Interfaces table "Settings declared by this spec". It claims the
  namespaces `proxy`, `pool` and `upstream`.
  - Name mapping, precedence, validation, error reporting (exit 2, no value
    echo), secrets and `_FILE` references all follow SPEC-003.
  - Values reach the domain only as typed values, or as an explicit "not set"
    for optional settings (SPEC-003 FR-16).

### Added in revision 3

- **FR-56:** Upstream unavailability modes (Owner decision M4).
  - **Fail fast.** In these states, a request gets a synthesized error with
    code 6 at once, without queueing, and requests already waiting are failed
    the same way as soon as the state is entered:
    - never reached since startup: detail `upstream_unavailable`;
    - authentication state failing (FR-22): detail `upstream_auth_failed`;
    - incompatible (FR-18): detail `upstream_incompatible`.

    When more than one applies, the detail follows the FR-46 precedence:
    `upstream_incompatible` first, then `upstream_auth_failed`, then
    `upstream_unavailable`.
  - **Wait (transient).** In these states, requests wait in the queue
    (FR-27) and get the `pool_wait_timeout` error (code 262) if no lease is
    available within `pool.checkout_timeout`:
    - the upstream was reached before, and no usable target is currently
      known: an election, a target marked not primary (FR-35), a target
      marked unknown after a failed check (FR-35), or a `mongos` marked
      unusable;
    - pool connection attempts are failing with network, TLS or timeout
      errors.
  - **Reconnect backoff.** After a failed pool connection attempt, the next
    attempt is delayed. The delay starts at 100 ms, doubles after each
    further failure, is capped at 5 s, and has up to ±20 % random jitter. It
    resets after a successful connection. These are fixed constants, stated
    in the operator documentation. While no usable target is known, no pool
    connection attempts are made; corral waits for monitoring (FR-37).
- **FR-57:** Client I/O timeout. `proxy.client_io_timeout` (default 30s)
  bounds client I/O:
  - **Reading:** once corral has read the first byte of a client message,
    the time corral spends reading that message must not exceed the
    timeout. This includes the rest that FR-38 reads after a lease, or reads
    to discard.
    - The time a message spends waiting in the queue or for a lease is not
      counted. The timer pauses when corral stops reading because the
      request buffer limit has been reached, and resumes when reading
      resumes.
    - So `pool.checkout_timeout` and `proxy.client_io_timeout` are
      independent, and no cross-setting rule is needed.
  - **Writing:** a reply or synthesized error must be fully written to the
    client within the timeout.
  - **On expiry:** the client connection is closed (`reason=io_timeout`,
    class `timeout`, detail `deadline_exceeded`). If the request was partly
    sent upstream, or the reply partly forwarded, the upstream connection is
    discarded. If nothing had been sent upstream, the lease is returned.
  - The timeout does not cover the time spent waiting for the upstream's
    reply.
- **FR-58:** Reply forwarding.
  - **Streaming.** Replies are streamed from the upstream to the client.
    corral holds at most a fixed per-reply chunk in memory (chosen by the
    planner and documented), never the whole reply. The FR-9 reply fields
    are read as they pass.
  - **Upstream protocol errors.** corral discards the upstream connection
    and handles the client per FR-40 when an upstream reply:
    - has `moreToCome` set (`unexpected_response`);
    - has a `messageLength` above the upstream `maxMessageSizeBytes`
      (`message_too_large`);
    - has a malformed header or sections (`malformed_message`);
    - fails checksum verification (`checksum_mismatch`). corral may forward a
      checksummed reply without its checksum (FR-10).
  - **Lease.** The lease is held while the reply is being written, bounded by
    FR-57.
- **FR-59:** `moreToCome` messages that are not forwarded. When a client
  OP_MSG with `moreToCome` set is answered or refused locally, no reply is
  sent. This covers handshakes, auth commands, blocked commands, shutdown,
  fail-fast and queue-full or wait-timeout refusals. The client connection
  stays open, and the message is counted in `corral_requests_total` with the
  outcome its reply would have had, and in `corral_requests_dropped_total`
  when it was refused.

## Interfaces

### Settings declared by this spec (SPEC-003 contract)

Namespaces claimed: `proxy`, `pool` and `upstream` (SPEC-003 Interfaces,
"Namespace ownership").

PEM rule (used below): the value names a regular file, read once during
loading through the SPEC-003 FR-25 file reader with a 4 MiB limit, that
contains at least one PEM `CERTIFICATE` block that parses. The bytes read are
kept and used for TLS, and are not read again (FR-44, FR-45).

| Setting | Type | Presence | Secret | Rule | Cross-setting rules | Description |
|---|---|---|---|---|---|---|
| `proxy.listen` | hostport | default `127.0.0.1:27017` | no | port 1 to 65535 | none | MongoDB wire listener address. |
| `proxy.max_client_connections` | int | default `10000` | no | 1 to 1000000 | none | Maximum open client connections. |
| `proxy.client_idle_timeout` | duration | default `0s` | no | zero allowed (0 disables) | none | Close client connections idle this long. |
| `proxy.client_io_timeout` | duration | default `30s` | no | > 0 | none | Maximum time to receive a started client message or to write a reply to a client. |
| `proxy.tls.enabled` | bool | default `false` | no | none | `proxy-tls-material` | Require TLS on the wire listener. |
| `proxy.tls.cert_path` | string | optional | no | PEM rule | `proxy-tls-material`, `proxy-tls-keypair` | Path to the wire listener certificate chain (PEM). |
| `proxy.tls.key` | text | optional | **yes** | a PEM private key (PKCS#1, PKCS#8 or SEC 1 EC) that parses | `proxy-tls-material`, `proxy-tls-keypair` | Wire listener TLS private key (PEM); supply via `CORRAL_PROXY_TLS_KEY_FILE` or `--proxy.tls.key_file`. |
| `proxy.tls.client_ca_path` | string | optional | no | PEM rule | `proxy-tls-mtls` | CA bundle for verifying client certificates; when set, mutual TLS is required. |
| `proxy.shutdown.listener_close_delay` | duration | default `0s` | no | zero allowed | none | Keep serving this long after shutdown starts before closing the listener. |
| `proxy.shutdown.drain_timeout` | duration | default `20s` | no | > 0 | none | Maximum time to drain in-flight exchanges at shutdown. |
| `pool.max_size` | int | default `10` | no | 1 to 10000 | `pool-min-le-max` | Maximum upstream connections in the pool. |
| `pool.min_size` | int | default `0` | no | 0 to 10000 | `pool-min-le-max` | Upstream connections kept open while the upstream is available. |
| `pool.max_waiters` | int | optional | no | 1 to 1000000; unset means `proxy.max_client_connections` | none | Maximum requests waiting for an upstream connection. |
| `pool.checkout_timeout` | duration | default `5s` | no | > 0 | none | Maximum wait for an upstream connection. |
| `upstream.hosts` | list of hostport | required | no | non-empty; ports 1 to 65535; no duplicate entries | `upstream-single-host` | Upstream seed addresses (`host:port`); never a connection string. |
| `upstream.replica_set` | string | optional | no | non-empty | `upstream-single-host` | Replica set name; when set, corral targets that set's current primary. When unset, the single host must be a `mongos`. |
| `upstream.auth.mechanism` | enum `SCRAM-SHA-256`, `none` | default `SCRAM-SHA-256` | no | case-insensitive | `upstream-auth-credentials` | Upstream authentication mechanism. |
| `upstream.auth.source` | string | default `admin` | no | non-empty; none of `/ \ . " $` or space | none | Authentication database for the service identity. |
| `upstream.auth.username` | string | optional | **yes** | non-empty | `upstream-auth-credentials` | Service identity username. |
| `upstream.auth.password` | string | optional | **yes** | non-empty | `upstream-auth-credentials` | Service identity password. |
| `upstream.connect_timeout` | duration | default `10s` | no | > 0 | none | Time limit to connect, handshake and authenticate an upstream connection, and for one monitoring check. |
| `upstream.heartbeat_interval` | duration | default `10s` | no | 500ms to 1h | none | Interval between upstream health and topology checks. |
| `upstream.max_idle_time` | duration | default `5m` | no | zero allowed (0 disables) | none | Close idle pool connections after this long, keeping `pool.min_size`. |
| `upstream.tls.enabled` | bool | default `false` | no | none | `upstream-tls-ca` | Use verified TLS to the upstream. |
| `upstream.tls.ca_path` | string | optional | no | PEM rule | `upstream-tls-ca` | CA bundle for verifying the upstream; unset uses system roots. |

Cross-setting rules (SPEC-003 FR-30):

| Rule | Involves | Primary | Applies to unset | Condition |
|---|---|---|---|---|
| `proxy-tls-material` | `proxy.tls.enabled`, `proxy.tls.cert_path`, `proxy.tls.key` | `proxy.tls.enabled` | yes | when `proxy.tls.enabled=true`, both `proxy.tls.cert_path` and `proxy.tls.key` are set |
| `proxy-tls-keypair` | `proxy.tls.cert_path`, `proxy.tls.key` | `proxy.tls.key` | no | the key matches the leaf certificate's public key |
| `proxy-tls-mtls` | `proxy.tls.client_ca_path`, `proxy.tls.enabled` | `proxy.tls.client_ca_path` | no | when `proxy.tls.client_ca_path` is set, `proxy.tls.enabled=true` |
| `pool-min-le-max` | `pool.min_size`, `pool.max_size` | `pool.min_size` | n/a | `pool.min_size` ≤ `pool.max_size` |
| `upstream-single-host` | `upstream.replica_set`, `upstream.hosts` | `upstream.hosts` | yes | when `upstream.replica_set` is unset, `upstream.hosts` has exactly one entry |
| `upstream-auth-credentials` | `upstream.auth.mechanism`, `upstream.auth.username`, `upstream.auth.password` | `upstream.auth.mechanism` | yes | with `SCRAM-SHA-256`, both username and password are set; with `none`, neither is set |
| `upstream-tls-ca` | `upstream.tls.ca_path`, `upstream.tls.enabled` | `upstream.tls.ca_path` | no | when `upstream.tls.ca_path` is set, `upstream.tls.enabled=true` |

Name mapping examples (SPEC-003 FR-3, FR-15): `--pool.max_size=20` /
`CORRAL_POOL_MAX_SIZE`; secret `upstream.auth.password` →
`CORRAL_UPSTREAM_AUTH_PASSWORD` or `CORRAL_UPSTREAM_AUTH_PASSWORD_FILE` /
`--upstream.auth.password_file=<path>` (never `--upstream.auth.password=`).
None of these names ends in `file` or `_file` (SPEC-003 FR-2).

### Synthesized error replies

A synthesized error is an OP_MSG reply, or an OP_REPLY for OP_QUERY (FR-8).
Its body has `ok: 0`, `code`, `codeName`, a fixed corral-defined `errmsg`
constant (no runtime data, no credentials), and `errorLabels` when FR-39
requires them. It never carries the service identity or any other secret
(FR-24).

Every row has a SPEC-001 FR-29 class and detail (Amendment 3):

| Condition | `code` | `codeName` | Detail | Class | Labels (FR-39) |
|---|---|---|---|---|---|
| wait exceeded `pool.checkout_timeout` | 262 | `ExceededTimeLimit` | `pool_wait_timeout` | `timeout` | yes |
| wait queue full | 262 | `ExceededTimeLimit` | `pool_queue_full` | `rejected` | yes |
| never reached since startup | 6 | `HostUnreachable` | `upstream_unavailable` | `network` | yes |
| authentication state failing | 6 | `HostUnreachable` | `upstream_auth_failed` | `server` | yes |
| upstream incompatible | 6 | `HostUnreachable` | `upstream_incompatible` | `config` | yes |
| shutting down | 91 | `ShutdownInProgress` | `shutting_down` | `rejected` | yes |
| `saslStart`, `saslContinue`, `authenticate` | 18 | `AuthenticationFailed` | `client_auth_unsupported` | `rejected` | no |
| `getnonce`, `logout` | 115 | `CommandNotSupported` | `client_auth_unsupported` | `rejected` | no |
| blocked command (FR-33) | 13 | `Unauthorized` | `command_blocked` | `rejected` | no |
| non-handshake OP_QUERY | 352 | `UnsupportedOpQueryCommand` | `unsupported_opcode` | `protocol` | no |
| repeated client metadata | 186 | `ClientMetadataCannotBeMutated` | `handshake_rejected` | `rejected` | no |
| `loadBalanced: true` handshake | 72 | `InvalidOptions` | `handshake_rejected` | `rejected` | no |

The `upstream_auth_failed` and `upstream_incompatible` conditions
deliberately use `HostUnreachable`, so that clients never learn details of
corral's upstream configuration.

### Domain observer ports (domain terms; method sets are for the planner)

They report:
- client connection opened, closed (with a reason) and rejected;
- handshake answered;
- client auth attempt rejected;
- command blocked;
- client protocol error;
- client I/O timeout;
- request received, completed, failed and dropped;
- checkout started, waited, succeeded and failed;
- barrier completed and failed;
- upstream connection opened, closed and failed (pool or monitoring);
- upstream authentication succeeded and failed;
- upstream availability, compatibility and primary changed;
- target marked not primary.

Every method takes `context.Context` first, and every port has a no-op
(SPEC-001 FR-2, FR-3). The readiness port (`internal/domain/health`) is
implemented by the upstream monitoring component (FR-46).

### Metrics added by this spec

Label names used: `command`, `error_class` and `error_detail` (SPEC-001
original), and `outcome`, `reason`, `side`, `state`, `opcode` and `kind`
(SPEC-001 Amendment 3). No value of an `outcome`, `reason`, `state`, `side`,
`opcode` or `kind` label equals a SPEC-001 FR-29 class or detail value
(SPEC-001 FR-11).

| Name | Type | Labels (values) | Max series |
|---|---|---|---|
| `corral_client_connections` | gauge | none | 1 |
| `corral_client_connections_accepted_total` | counter | none | 1 |
| `corral_client_connections_closed_total` | counter | `reason` (`client_closed`, `idle_timeout`, `io_timeout`, `protocol_error`, `upstream_failure`, `shutdown`, `tls_failure`, `limit_exceeded`, `internal_error`) | 9 |
| `corral_client_auth_rejected_total` | counter | none | 1 |
| `corral_commands_blocked_total` | counter | none | 1 |
| `corral_handshakes_total` | counter | `opcode` (`op_msg`, `op_query`), `outcome` (`ok`, `error`) | 4 |
| `corral_protocol_errors_total` | counter | `side` (`client`, `upstream`), `error_detail` (`unsupported_opcode`, `message_too_large`, `malformed_message`, `checksum_mismatch`, `unexpected_response`) | 10 |
| `corral_requests_total` | counter | `command` (SPEC-001 FR-12 allowlist + `other`), `outcome` (`ok`, `server_error`, `proxy_error`) | 84 |
| `corral_request_duration_seconds` | histogram | `command` | 28 |
| `corral_request_errors_total` | counter | `error_class` (SPEC-001 FR-29 classes) | 7 |
| `corral_requests_dropped_total` | counter | none | 1 |
| `corral_pool_connections` | gauge | `state` (`idle`, `in_use`, `connecting`) | 3 |
| `corral_pool_max_connections` | gauge | none | 1 |
| `corral_pool_waiters` | gauge | none | 1 |
| `corral_pool_checkouts_total` | counter | `outcome` (`ok`, `failed`) | 2 |
| `corral_pool_checkout_failures_total` | counter | `error_detail` (`pool_wait_timeout`, `pool_queue_full`, `upstream_unavailable`, `upstream_auth_failed`, `upstream_incompatible`, `shutting_down`) | 6 |
| `corral_pool_checkout_wait_seconds` | histogram | none | 1 |
| `corral_upstream_connections_opened_total` | counter | `kind` (`pool`, `monitor`) | 2 |
| `corral_upstream_connections_closed_total` | counter | `kind` (`pool`, `monitor`), `reason` (`idle`, `error`, `not_primary`, `client_abandoned`, `shutdown`, `incompatible`) | 12 |
| `corral_upstream_connect_errors_total` | counter | `kind` (`pool`, `monitor`), `error_class` | 14 |
| `corral_upstream_auth_failures_total` | counter | `kind` (`pool`, `monitor`) | 2 |
| `corral_upstream_available` | gauge | none (1 when ready per FR-46, else 0) | 1 |
| `corral_upstream_primary_changes_total` | counter | none | 1 |

Definitions:
- `corral_requests_total` covers every client request, including those
  answered locally and `moreToCome` messages. It does not cover barrier
  pings.
  - `ok`: a reply with `ok: 1` was delivered; or, for `moreToCome`, the
    message was written upstream or handled locally without an error. Write
    errors inside an `ok: 1` reply still count as `ok`.
  - `server_error`: an upstream reply with `ok: 0` was delivered.
  - `proxy_error`: a synthesized error (or, for `moreToCome`, a refusal
    under FR-59), or the client connection was closed during the exchange
    (FR-38, FR-40, FR-57).
- `corral_request_duration_seconds` runs from the moment the first byte of
  the request is read to the moment the reply has been fully written, or for
  `moreToCome`, to the end of the barrier.
- `corral_requests_dropped_total` counts `moreToCome` messages that were
  refused (FR-59).
- Histogram buckets are left to the planner.

### Log components and events

New SPEC-001 FR-7 components: `proxy` (wire listener, client connections,
requests), `pool` (leases, wait queue) and `upstream` (pool and monitoring
connections, authentication, topology).

New SPEC-001 FR-39 attribute key: `reason`. Each event below that carries it
declares its enumeration. Records carrying `error` also carry
`corral.error.detail` (SPEC-001 FR-29).

`reason` enumerations:
- **client close reasons:** `client_closed`, `idle_timeout`, `io_timeout`,
  `protocol_error`, `upstream_failure`, `shutdown`, `tls_failure`,
  `limit_exceeded`, `internal_error`;
- **client reject reasons:** `limit_exceeded`;
- **upstream close reasons:** `idle`, `error`, `not_primary`,
  `client_abandoned`, `shutdown`, `incompatible`;
- **readiness reasons:** `upstream_unavailable`, `upstream_auth_failed`,
  `upstream_incompatible`;
- **incompatibility reasons:** `wire_version`, `standalone`,
  `topology_mismatch`, `replica_set_name_mismatch`.

| Component | Level | `msg` (constant) | Attributes beyond time/level/msg/component | Notes |
|---|---|---|---|---|
| `proxy` | info | `wire listener bound` | none (no address) | startup |
| `proxy` | debug | `client connection opened` | `corral.connection.id`, `client.address`, `client.port` | |
| `proxy` | debug | `client connection closed` | `reason` (client close reasons), `corral.connection.id` | |
| `proxy` | warn | `client connection rejected` | `reason` (client reject reasons) | rate-limited |
| `proxy` | debug | `client handshake answered` | `corral.connection.id`, `corral.client.app_name`, `corral.client.driver.name`, `corral.client.driver.version` (SPEC-001 FR-25) | SPEC-001 FR-32 |
| `proxy` | warn | `client authentication rejected` | `error`, `corral.error.detail`, `corral.connection.id` | rate-limited; never the username or payload |
| `proxy` | warn | `client command blocked` | `error`, `corral.error.detail`, `db.operation.name`, `corral.connection.id` | rate-limited |
| `proxy` | warn | `client protocol error` | `error`, `corral.error.detail`, `corral.connection.id` | rate-limited |
| `proxy` | warn | `client I/O timed out` | `error`, `corral.error.detail`, `corral.connection.id` | rate-limited |
| `proxy` | warn | `client TLS handshake failed` | `error`, `corral.error.detail` | rate-limited; no certificate identity |
| `proxy` | error | `request handler panic recovered` | `error`, `corral.error.detail`, `stack` | SPEC-001 FR-29 |
| `pool` | warn | `pool checkout timed out` / `pool wait queue full` | none | rate-limited |
| `upstream` | debug | `upstream connection opened` / `upstream monitoring connection opened` | `server.address`, `server.port`, `corral.upstream.connection.id` | |
| `upstream` | debug | `upstream connection closed` / `upstream monitoring connection closed` | `reason` (upstream close reasons), `server.address`, `server.port` | |
| `upstream` | warn | `upstream connection failed` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited |
| `upstream` | warn | `upstream authentication failed` | `error`, `corral.error.detail`, `server.address`, `server.port`, and `db.response.status_code` / `corral.db.response.code_name` from the upstream's reply, when there is one | rate-limited; no username; warn per SPEC-001 FR-32 |
| `upstream` | warn | `upstream unavailable` | `reason` (readiness reasons) | on transition only |
| `upstream` | error | `upstream incompatible` | `reason` (incompatibility reasons), `server.address`, `server.port` | on transition only |
| `upstream` | info | `upstream available` | none | on transition only |
| `upstream` | info | `upstream primary changed` | `server.address`, `server.port` | |
| `upstream` | warn | `upstream target marked not primary` | `error` (`server`), `corral.error.detail` (`other`), `server.address`, `server.port`, `db.response.status_code`, `corral.db.response.code_name` (from the forwarded upstream reply, so SPEC-001 FR-23 allows them) | rate-limited; an error record reporting the upstream's error |
| `upstream` | warn | `upstream protocol error` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited |
| `upstream` | warn | `upstream barrier failed` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited |

### SPEC-001 amendments requested by this spec (SPEC-001 Amendment 3; package approved by the Owner, 2026-10-24)

1. **FR-7:** add components `proxy`, `pool` and `upstream`.
2. **FR-12:** add to the command allowlist: `getMore`, `killCursors`,
   `insert`, `update`, `delete`, `findAndModify`, `aggregate`, `count`,
   `distinct`, `bulkWrite`, `createIndexes`, `dropIndexes`, `listIndexes`,
   `listCollections`, `listDatabases`, `create`, `drop`, `ping`, `buildInfo`,
   `endSessions`, `commitTransaction`, `abortTransaction`, `saslStart`,
   `saslContinue`, `authenticate`. With `find`, `hello` and `other` that makes
   28 values, so `corral_requests_total` has at most 84 series (≤ 100).
3. **FR-18:** add readiness reasons `upstream_unavailable`,
   `upstream_auth_failed` and `upstream_incompatible`. Precedence is defined
   only in this spec's FR-46.
4. **FR-29:** add class `rejected`, and the details with fixed classes listed
   in the Synthesized error replies table and the protocol-error metric:
   `pool_wait_timeout`, `pool_queue_full`, `upstream_unavailable`,
   `upstream_auth_failed`, `upstream_incompatible`, `shutting_down`,
   `client_auth_unsupported`, `command_blocked`, `handshake_rejected`,
   `unsupported_opcode`, `message_too_large`, `malformed_message`,
   `checksum_mismatch` and `unexpected_response`. One vocabulary is used in
   both specs.
5. **FR-39:** add the log key `reason`, with values only from the
   enumerations declared above.
6. **FR-11:** add the label names `outcome`, `reason`, `side`, `state`,
   `opcode` and `kind` to the label-name allowlist, with the outcome/cause
   rule.
7. **FR-23 / FR-29:** "server errors" means errors carried in an upstream
   reply. Synthesized errors carry no `db.response.status_code` or
   `corral.db.response.code_name` in any signal.
8. **FR-31:** reworded. These metrics are declared here and need no further
   amendment, because their label names are added by item 6.
9. **FR-40:** no change. Span name and attribute allowlists: no change.
10. **ADR-0006 / FR-18:** binding the wire listener becomes the last startup
    step. SPEC-001 already says this; the planner updates the ADR.

Items 4 (the details beyond the Q12 package), 6, 7 and 8 go beyond the Q12
package and need Owner approval of the Amendment 3 text.

## Behavior on failure

| Situation | Client sees | corral does |
|---|---|---|
| Invalid or missing setting, secret on the command line, cross-setting rule broken, unreadable PEM | n/a | exit 2 with SPEC-003 records, before binding anything (FR-55) |
| Wire listener cannot bind | n/a | exit 1, `setting=proxy.listen` (FR-1) |
| Malformed frame, bad length, OP_COMPRESSED, legacy opcode, bad flags or checksum, invalid body, deciding field beyond the buffer | connection closed, no reply | counts; rate-limited warn (FR-5 to FR-7, FR-38) |
| Non-handshake OP_QUERY | OP_REPLY `QueryFailure`, code 352 | connection stays open (FR-8) |
| Client sends auth command | error 18 / 115 | not forwarded; counts; rate-limited warn (FR-23) |
| Client sends a blocked admin command | error 13 | not forwarded; counts; rate-limited warn (FR-33) |
| Pool full, wait exceeds `pool.checkout_timeout` | error 262 + labels | request read to its end and discarded; checkout span in error (FR-27, FR-38, FR-39) |
| Wait queue full | error 262 + labels, at once | (FR-27, FR-39) |
| Upstream never reached, auth failing, or incompatible | error 6 + labels at once; handshakes fail only when never reached or incompatible | readiness 503 with reason (FR-19, FR-56, FR-46) |
| Election, target marked not primary, connect attempts failing | requests wait, then error 262 | checks the topology; backoff on reconnects (FR-35, FR-56) |
| Not-primary error in a reply (including `writeConcernError`) | the reply, unchanged | marks the host, holds leases, re-checks (FR-35) |
| Upstream connection fails after the request was sent, or late request validation failure | client connection closed | upstream connection discarded (FR-38, FR-40) |
| Upstream protocol violation (unknown `responseTo`, `moreToCome` reply, oversize, bad checksum) | client connection closed | upstream connection discarded (FR-11, FR-58) |
| Client stalls mid-message or reads a reply too slowly | client connection closed after `proxy.client_io_timeout` | lease returned or upstream connection discarded (FR-57) |
| Client disconnects mid-exchange | n/a | upstream connection discarded, or the request is removed from the queue (best effort) (FR-41) |
| Barrier `ping` fails after a `moreToCome` message | nothing | upstream connection discarded; client connection open (FR-29) |
| `moreToCome` message refused | nothing | dropped and counted (FR-59) |
| Panic in a connection handler | that client connection closed | recovered; other clients unaffected (FR-42) |
| Graceful shutdown | in-flight exchanges finish; late messages get error 91; connections closed | FR-47 |
| Client TLS handshake fails, or no client certificate under mutual TLS | connection closed | counts; rate-limited warn (FR-44) |

corral itself never retries a request upstream. Retries are left to the
drivers, which the error codes and labels enable.

## Non-functional requirements

- **Security** *(review by security@emag.ro recommended before approval)*:
  - **Shared identity:** all clients act upstream as one service identity,
    which collapses per-application privileges. The service user should have
    least privilege for the applications served.
  - **Cross-client effects:** partly mitigated by blocking `currentOp`,
    top-level `$currentOp`, `killOp`, `killAllSessions` and
    `killAllSessionsByPattern` (FR-33). The residual routes are listed in
    Data and compliance; least privilege for the service user (FR-51) is
    the mitigation.
  - **No client authentication on the wire listener:** protection comes from
    the loopback default (FR-1), network placement and optional mutual TLS
    (FR-44). Client TLS is not mandatory.
  - **Upstream TLS:** verification cannot be turned off (FR-45).
  - **Secrets:** the service username, password and TLS private key are
    SPEC-003 secrets: never on the command line, never emitted, never in
    synthesized replies, no defaults (FR-24, FR-44).
  - **Message contents:** corral reads only the FR-9 fields of message
    contents.
- **Availability:**
  - Upstream outages and pool saturation do not close client connections by
    themselves (FR-43).
  - Pool saturation does not change readiness (FR-46).
  - Transient unavailability waits instead of failing (FR-56).
  - Reconnects back off (FR-56).
  - Shutdown drains in-flight exchanges (FR-47).
- **Resource bounds:**
  - upstream connections ≤ `pool.max_size` + one monitoring connection per
    monitored host;
  - client connections ≤ `proxy.max_client_connections`;
  - waiting requests ≤ the FR-27 bound;
  - memory for messages without a lease ≤ `proxy.max_client_connections` ×
    the request buffer limit (FR-38);
  - reply memory ≤ `pool.max_size` × the reply chunk (FR-58);
  - client I/O ≤ `proxy.client_io_timeout` (FR-57).
- **Performance:** no targets (Non-goals). The SPEC-001 zero-allocation
  tracing rule (AC-22) applies to the new per-message paths.
- **Observability:** FR-48 to FR-50. SPEC-001 Part B checks re-run (AC-27).

## Data and compliance

- **Data touched and its classification:**
  - *Message contents in transit:* documents, filters, pipelines and command
    arguments travel through corral between client and upstream, and can
    contain personal data held in the upstream database. The Owner classifies
    this as **transit-only processing**:
    - corral reads only the FR-9 fields;
    - it keeps message bytes in memory only for the length of the exchange,
      and at most the FR-38 prefix and the FR-58 chunk at a time;
    - it never writes them to disk and never emits them (SPEC-001 FR-24).
  - *Operational metadata:* as in SPEC-001 (command and database names,
    collection names per FR-40, IDs, client addresses, `appName` and driver
    metadata, upstream host and port).
  - *Secrets:* the upstream service credentials and the wire listener TLS
    private key. They are confidential and held only as SPEC-003 secret
    settings (FR-24, FR-44).
- **Shared identity and access:** all applications reach the database as one
  service identity, so the database's own audit trail cannot tell them apart.
  Blocking the cross-client administrative commands (FR-33) prevents one
  application from listing or killing another's operations and sessions
  through the most direct routes.

  **Residual exposure routes** (flagged for review by security@emag.ro). These
  are not blocked by corral, and they work only if the service user has the
  privileges FR-51 advises against:
  - reading `system.profile`, which can show other clients' operations and
    their filters;
  - the `$queryStats` aggregation stage;
  - `$listSessions` / `$listLocalSessions` with `allUsers: true`;
  - `$currentOp` nested inside a sub-pipeline (for example `$unionWith` or
    `$lookup`), wherever the server allows it, which FR-33 does not inspect;
  - an `aggregate` with `$currentOp` wrapped in `explain`;
  - `connectionStatus`, which reveals the service identity's name.

  The mitigation is the FR-51 least-privilege guidance for the service user.
- **PII:** corral does not need personal data to do its job and emits none.
  Personal data may pass through it in transit only, as classified above.
  - Minimization: only the FR-9 fields are read, and `saslSupportedMechs`
    (which contains a username) is checked for presence only.
  - The session `uid` and client handshake fields outside SPEC-001 FR-25 are
    dropped at once.
  - Client-side TLS is available but not mandatory (Owner decision); where
    traffic crosses untrusted networks, the operator enables it (FR-44).
- **Retention:** none. corral keeps no state that outlives a connection
  except counters, the upstream parameters and topology state. Telemetry
  retention follows SPEC-001.
- **Test data:** synthetic only, including synthetic service credentials and
  synthetic test certificates (FR-54).

## Acceptance criteria

Types:
- *unit:* domain/app, in-process;
- *functional:* corral binary + mongod container + official drivers, build
  tag `functional`;
- *twin:* corral binary + a simulated upstream or fault-injecting
  intermediary from `twins/`.

"Driver matrix" means the Go driver and the PHP driver, against MongoDB 7.0
and 8.0 (FR-54). "The marker harness" means the SPEC-001 AC-33 harness.

- **AC-1** (FR-1, FR-55): Functional:
  - with defaults, corral listens on `127.0.0.1:27017` only;
  - `--proxy.listen=:27017` and `--proxy.listen=127.0.0.1:0` each exit 2 with
    `setting=proxy.listen`;
  - an address already in use exits 1 with `setting=proxy.listen` and
    without the address value;
  - `/readyz` stays `starting` until the wire listener is bound (test hook);
  - the startup-complete record follows the `wire listener bound` record.
- **AC-2** (FR-2, FR-3, FR-48):
  - Functional with `proxy.max_client_connections=3`:
    - a 4th connection is closed without any bytes received;
    - `corral_client_connections_closed_total{reason="limit_exceeded"}`
      increments;
    - the `connectionId` values on the 3 accepted connections are distinct
      and positive.
  - Unit (ID allocator): starting near 2,147,483,647, IDs wrap to 1 and skip
    IDs still in use; no two open connections share an ID.
- **AC-3** (FR-4): Twin/functional with a short `proxy.client_idle_timeout`:
  an idle client connection is closed after the timeout, and one with an
  exchange in progress is not. With the default `0s`, an idle connection
  stays open for longer than a test-chosen period.
- **AC-4** (FR-5, FR-6, FR-7, FR-48): Unit (message model) and twin
  (raw-socket client). Each case closes the connection with no reply and
  increments `corral_protocol_errors_total{side="client",error_detail=…}`:

  | Case | `error_detail` |
  |---|---|
  | `messageLength` of 15 | `malformed_message` |
  | `messageLength` above the maximum | `message_too_large` |
  | an HTTP `GET` request | `message_too_large` |
  | OP_COMPRESSED, each listed legacy opcode, and an unknown opcode | `unsupported_opcode` |
  | an unknown required flag bit | `malformed_message` |
  | a bad CRC-32C checksum | `checksum_mismatch` |
  | two kind-0 sections, an unknown section kind, a truncated section, or an invalid BSON body | `malformed_message` |

  A valid OP_MSG with a correct checksum and a kind-1 section is proxied
  successfully.
- **AC-5** (FR-8, FR-19): Twin (raw-socket client):
  - an OP_QUERY `isMaster` on `admin.$cmd`, and one wrapped as
    `{$query: {isMaster: 1}, $readPreference: {...}}`, each get an OP_REPLY
    with `responseFlags` 0 and the handshake document;
  - an OP_QUERY `find` gets `responseFlags` with `QueryFailure` set and a
    document with `$err`, `code: 352` and `ok: 0`, and the same connection
    then completes an OP_MSG `ping`;
  - with the upstream never reached, an OP_QUERY `isMaster` gets
    `responseFlags` 0 and `{ok: 0, code: 6}`;
  - a second OP_QUERY `isMaster` with `client` gets `responseFlags` 0 and
    code 186.
- **AC-6** (FR-9, FR-10, FR-58): Twin: the simulated upstream records the
  bytes it receives, and returns replies with known bytes, both with and
  without checksums.
  - Forwarded request sections are byte-identical to what the client sent,
    and reply sections the client receives are byte-identical to what the
    upstream sent.
  - The only header or flag differences are: `requestID`, `responseTo`,
    `messageLength` (only when a checksum was removed, by exactly 4), the
    `checksumPresent` flag, the checksum, and `exhaustAllowed`.
- **AC-7** (FR-9, SPEC-001 FR-24): Unit (review-backed):
  - the request and reply parsers expose only the FR-9 fields;
  - a handshake whose `saslSupportedMechs` value is a marker username
    exposes only its presence, and the marker appears in no parser output;
  - a fuzz test of the parser over random and malformed inputs never panics.
- **AC-8** (FR-11, FR-12): Twin:
  - 50 raw clients send requests concurrently, all using the same
    `requestID`, through a pool of size 2. Each receives exactly one reply,
    whose `responseTo` equals its `requestID` and whose body is the reply to
    its own request (the twin echoes a per-request marker);
  - an upstream reply with an unknown `responseTo` makes corral discard that
    upstream connection, close the affected client connection, and increment
    `corral_protocol_errors_total{side="upstream",error_detail="unexpected_response"}`.
- **AC-9** (FR-13, FR-14, FR-15, FR-16, FR-17, FR-20, FR-21): Twin and
  functional:
  - `hello` / `isMaster` / `ismaster` over OP_MSG, and the OP_QUERY handshake,
    never reach the upstream (twin records none);
  - the reply has exactly the FR-14 fields, with values equal to the
    upstream's parameters, except `maxWireVersion`, which is capped at 25;
  - a `hello` with `topologyVersion` and `maxAwaitTimeMS=10000` is answered
    within a test-defined bound well under 10 s;
  - a second `hello` with `client` gets code 186;
  - `loadBalanced: true` gets code 72;
  - a handshake with `compression: ["zstd"]`, `speculativeAuthenticate` and
    `saslSupportedMechs` gets a reply with none of `compression`,
    `speculativeAuthenticate` or `saslSupportedMechs`.
- **AC-10** (FR-18, FR-19, FR-46, FR-49): Twin:
  - with the target reporting wire versions 6–27, the reply reports 6–25
    (`minWireVersion` unchanged, `maxWireVersion` capped);
  - with 21–21 and with 25–30, the upstream is accepted (the ranges overlap
    21–25), and the reply reports 21–21 and 25–25 respectively;
  - with 6–17, and separately with 26–30, readiness is
    `upstream_incompatible`, handshakes and requests get code 6 at once, and
    one `upstream incompatible` record with `reason=wire_version` is emitted;
  - **compatible → incompatible:** with the twin switching its `hello`
    mid-run, an open client connection stays open, its next handshake and
    request get code 6, idle pool connections are closed with
    `reason=incompatible`, and a request waiting at that moment gets code 6;
  - **incompatible → compatible:** the next handshake succeeds with the new
    parameters;
  - with the upstream never reached since startup, a handshake gets code 6;
  - after the upstream has been reached once and then becomes unreachable,
    handshakes succeed with the cached parameters.
- **AC-11** (FR-22, FR-23, FR-24): Functional against mongod with
  authorization enabled and a synthetic service user, whose credentials are
  given through `CORRAL_UPSTREAM_AUTH_USERNAME` and
  `CORRAL_UPSTREAM_AUTH_PASSWORD_FILE`, both markers:
  - driver-matrix clients with no credentials can run CRUD;
  - clients with credentials in their URI fail authentication with code 18,
    and the mongod log shows no authentication attempt from those clients;
  - `getnonce` and `logout` get code 115;
  - `corral_client_auth_rejected_total` increments;
  - no synthesized reply captured by a raw test client contains either
    marker;
  - `--upstream.auth.password=<marker>` exits 2 under SPEC-003 FR-14, with
    the marker absent;
  - `CORRAL_UPSTREAM_HOSTS=mongodb://<marker-user>:<marker-pass>@host:27017`
    exits 2 with `setting=CORRAL_UPSTREAM_HOSTS`, and no marker appears in
    any output scanned by the marker harness.
- **AC-12** (FR-25, FR-26, FR-28, FR-37, FR-48) — **the motivating case**:
  Functional with the PHP driver under PHP-FPM, with ≥ 50 workers holding
  persistent connections, `pool.max_size=4`, driving concurrent CRUD load.
  - The connections from corral with `appName=corral`, counted on mongod
    through `$currentOp` run directly against mongod (not through corral),
    never exceed 4. Connections with `appName=corral-monitor` are counted
    separately and are at most one per monitored host.
  - `corral_pool_connections` summed over states never exceeds 4.
  - All operations succeed.
  - The same holds for the Go driver with 200 concurrent goroutines and
    `maxPoolSize=200`.
- **AC-13** (FR-27, FR-39, FR-50): Twin with `pool.max_size=1` and the twin
  holding the first request open:
  - a second request gets code 262 after `pool.checkout_timeout`, within a
    test-defined tolerance;
  - with `pool.max_waiters=1`, a third request gets code 262 at once;
  - a retryable write (with `txnNumber`) gets `RetryableWriteError`;
  - a request with `autocommit: false` that is not commit/abort gets
    `TransientTransactionError`;
  - a plain read gets no labels;
  - the spans for a timeout are root and `pool.checkout`, both in error, with
    `error.type=timeout` and `corral.error.detail=pool_wait_timeout`;
  - `corral_pool_checkouts_total{outcome="failed"}` and
    `corral_pool_checkout_failures_total{error_detail=…}` match.
- **AC-14** (FR-29, FR-50, FR-12): Twin and functional:
  - **twin:**
    - after a client `moreToCome` message, the twin receives a
      `{ping: 1, $db: "admin"}` on the same upstream connection;
    - while the twin delays the ping reply, that upstream connection is not
      leased to another client, and the same client's next request is not
      forwarded on any connection;
    - after the ping reply, both proceed;
    - when the twin fails the ping (resets the connection), the upstream
      connection is discarded, the client connection stays open, and the
      client's next request succeeds;
    - when the twin delays the ping reply for three times
      `upstream.connect_timeout`, the barrier is not cut off, and completes
      when the reply arrives;
    - when, during a delayed barrier, the twin makes a check of that host
      fail twice (marking it unknown), or returns a not-primary error on
      another connection to it, the barrier is aborted at once: the upstream
      connection is discarded and the client connection stays open;
    - unit/review: upstream sockets have TCP keepalive at 30 s idle, 10 s
      interval and 3 probes, and `TCP_USER_TIMEOUT` of 60 s on Linux;
    - the ping produces no span and is not counted in
      `corral_requests_total`;
  - **functional:** an unacknowledged insert (`w:0`) followed by a `find` on
    the same client connection sees the inserted document, 100 times out of
    100, with `pool.max_size=4` under concurrent load from other clients.
- **AC-15** (FR-30): Twin: a client `getMore` with `exhaustAllowed` reaches
  the twin with the flag cleared, and the client receives one reply without
  `moreToCome`.
- **AC-16** (FR-31, FR-32): Functional, driver matrix, single-node replica
  set, through a fault-injecting intermediary (twin) that records which
  upstream connection carries each command:
  - with `pool.min_size=pool.max_size=2` and a test hook that makes the pool
    hand out idle connections in round-robin order:
    - the commands of one multi-document transaction (commit, and separately
      abort), and the `getMore`s of one cursor (a `find` with `batchSize=2`
      over 100 synthetic documents), alternate between both upstream
      connections;
    - all of them succeed;
  - retryable writes succeed under ≥ 20 concurrent clients;
  - a `getMore` for another session's cursor, sent with a different `lsid`,
    is refused by the server, and the refusal is forwarded unchanged.
- **AC-17** (FR-33, FR-48, FR-49): Twin and functional:
  - `currentOp`, `killOp`, `killAllSessions`, `killAllSessionsByPattern`, and
    `aggregate` with `pipeline: [{$currentOp: {}}]` each get code 13, never
    reach the upstream (twin), increment `corral_commands_blocked_total`, and
    produce a rate-limited `client command blocked` record;
  - `aggregate` with `[{$match: {}}]` is forwarded;
  - `connectionStatus` is forwarded and returns mongod's answer unchanged.
- **AC-18** (FR-18, FR-34, FR-35, FR-36, FR-37): Functional with a 3-node
  replica set, plus twin and unit variants:
  - **functional:** the test sets `pool.checkout_timeout=120s`, so that the
    criteria do not depend on how long the election takes.
    - The background load uses only retryable reads and retryable writes; no
      transactions.
    - Under that continuous driver-matrix load, `replSetStepDown` is run on
      the primary with `force: true`, and the test asserts that the command
      succeeded (or that the connection closed as the node stepped down).
    - The test watches the replica set directly until a new primary is
      elected.
    - After that, without asserting any particular delay, the test checks
      that:
      - new leases go to the new primary;
      - `corral_upstream_primary_changes_total` has incremented;
      - every request that was waiting during the election completed
        successfully, with no 262;
      - drivers' retryable writes succeeded;
    - While no primary is known, sampled `/readyz` responses are 503
      `upstream_unavailable`. To make sampling deterministic, the test holds
      the election open:
      - it runs `replSetFreeze` directly on each secondary (not through
        corral) before the step-down, with a freeze period well under 120 s
        (for example 20 s);
      - it samples readiness;
      - it then unfreezes each secondary directly with `replSetFreeze: 0`;
  - **unit (domain):**
    - the stale-primary rule: given two primary reports, the larger
      `(electionId, setVersion)` wins, and a later report with a smaller pair
      is ignored;
    - discovery: with only a secondary reachable, members named in its
      `hosts`, `passives` and `primary` become known members;
    - while no usable target is known, every known member is scheduled for
      checks at the 500 ms minimum interval (fake clock);
  - **twin, replica-set mode:**
    - a seed reporting a different `setName`, with no matching member found,
      gives `replica_set_name_mismatch`;
    - a seed reporting a standalone gives `standalone`, with an `upstream
      incompatible` error record;
    - a seed reporting a `mongos` gives `topology_mismatch`;
    - a non-matching seed alongside a matching member is ignored, and the
      upstream stays compatible;
  - **twin, single-`mongos` mode:** a host reporting a replica-set member
    gives `topology_mismatch`, and a standalone gives `standalone`;
  - **twin, both modes:**
    - a member listed under `arbiters` gets no monitoring connection;
    - monitoring connections to non-target members perform no
      authentication, and the target's monitoring connection does;
    - a read with `readPreference=secondary` is sent to the primary.
- **AC-19** (FR-38): Twin, with `pool.max_size=1` held by the twin, a request
  buffer limit known to the test, and a test hook exposing the bytes corral
  has read from a given client connection:
  - a 10 MB `insert`: before a lease, the bytes read never exceed the request
    buffer limit; after the twin releases the pool, the twin receives the
    message byte-identical;
  - a waiting large request that times out gets code 262, and the same
    connection then completes a `ping`, so framing is preserved;
  - an `aggregate` whose `pipeline` field starts after the request buffer
    limit gets `malformed_message`, and the connection closes;
  - an OP_MSG `hello` larger than the request buffer limit gets
    `message_too_large`, and the connection closes;
  - a large `insert` whose tail (after the limit) is invalid BSON: the twin's
    upstream connection is closed by corral, the client connection is
    closed, and `corral_protocol_errors_total{side="client",error_detail="malformed_message"}`
    increments.
- **AC-20** (FR-40): Twin:
  - the twin resets the connection after receiving a request: the client
    connection is closed with no reply, and the upstream connection is
    discarded;
  - with the Go and PHP drivers, a retryable write in that situation
    succeeds on the driver's retry;
  - the spans are root and round-trip, both in error.
- **AC-21** (FR-41): Twin:
  - a client disconnects while its request is held by the twin: that upstream
    connection is closed (`reason=client_abandoned`), and a following client
    request succeeds on a new connection;
  - a client whose request fits within the request buffer limit, and is fully
    read, disconnects while queued: the twin never receives its request.
- **AC-22** (FR-42): Unit/twin: a fault-injection hook panics in one
  connection's handler. That client connection closes, a concurrent client
  completes its request, and the error record has class `internal` and no
  panic value.
- **AC-23** (FR-43): Twin: while the upstream is down, open client connections
  stay open. After recovery, the same connections succeed.
- **AC-24** (FR-44, FR-45): Functional, with a synthetic test CA and
  certificates. The key is supplied through `CORRAL_PROXY_TLS_KEY_FILE`:
  - with client TLS on, a driver with `tls=true` works and a plaintext
    driver fails;
  - with `proxy.tls.client_ca_path` set, a client without a certificate fails
    and one with a valid certificate works;
  - with upstream TLS against a TLS-enabled mongod and the right CA,
    requests work;
  - with the wrong CA, readiness is `upstream_unavailable`,
    `corral_upstream_connect_errors_total{error_class="network"}` increments,
    and the log has `corral.error.detail=tls_handshake`;
  - unit: with an injected file reader, the TLS configuration is built from
    the bytes validated at load time, and the reader is called once per file;
  - the marker harness finds no certificate-subject marker, key marker or
    path marker in any output.
- **AC-25** (FR-22, FR-46, FR-48):
  - Unit (readiness evaluation over all input combinations): precedence
    `upstream_incompatible` > `upstream_auth_failed` >
    `upstream_unavailable`, and exactly one reason when not ready.
  - Unit (auth state):
    - code 18, and separately code 334, in reply to `saslStart` on a pool
      connection sets the state to failing;
    - a failed SCRAM server-signature check sets it to failing;
    - error replies with codes 10107, 91 or 11600 during authentication, a
      network error and a timeout do not set it to failing, and are
      transient (`upstream_unavailable`);
    - while failing, no pool connection attempts are made, each target check
      re-attempts authentication on a fresh monitoring connection, and the
      first success clears the state.
  - Functional:
    - a wrong service password gives `upstream_auth_failed` while corral
      keeps running;
    - an unreachable upstream gives `upstream_unavailable`;
    - recovery gives 200;
    - saturating the pool with timeouts leaves readiness at 200;
    - `corral_upstream_available` matches the readiness state.
- **AC-26** (FR-47): Twin, using a shutdown test hook that can hold the
  sequence at each step:
  - during `proxy.shutdown.listener_close_delay`, new connections are
    accepted and served; after step 2, new connections are refused;
  - an exchange in progress at step 3 (held by the twin), and a request
    waiting in the queue at step 3, both complete normally, and each
    connection is closed after its reply;
  - an idle connection is closed at step 3, with no reply;
  - with the hook holding step 3 before an idle connection is closed, a
    request sent on that connection gets code 91, and the connection is then
    closed;
  - after `proxy.shutdown.drain_timeout`, a held exchange is cut off;
  - the process exits 0 within drain timeout + the SPEC-001 FR-34 flush bound
    + a tolerance.
- **AC-27** (FR-24, FR-48, FR-49, FR-50, FR-52, SPEC-001 Part B): Re-run the
  SPEC-001 Part B checks against everything this spec adds:
  - AC-2 / AC-23 for every new observer port;
  - AC-9 for every allowlisted command;
  - AC-10 for every new metric family;
  - AC-17, with the FR-50 span shapes, including `moreToCome`,
    synthesized-error and outcome-unknown;
  - AC-18 through corral, with the driver matrix and marker credentials,
    documents, filters, comments, duplicate keys and `appName`. The marker
    service username and password appear in none of the marker harness's
    sources either. Reply bodies that corral forwards from the upstream to
    clients are upstream data and are not scanned; synthesized replies are
    checked by AC-11;
  - AC-19;
  - AC-22 on the new per-message paths;
  - AC-34.

  Also:
  - every new metric is in the catalog, with exactly the labels and values
    listed;
  - every FR-49 event is emitted with its constant `msg`, its component,
    only its allowed keys, and `reason` values only from its enumeration
    (one marker test per event, marker harness);
  - each rate-limited event, triggered 10 times in 30 s on a fake clock,
    produces one record and then `suppressed_count` 9;
  - no record or span for a synthesized error carries
    `db.response.status_code`.
- **AC-28** (FR-51): Review: the operator documentation contains all FR-51
  statements.
- **AC-29** (FR-53, SPEC-001 FR-1, SPEC-003 FR-16): SPEC-001 AC-1 (as
  extended by SPEC-003 AC-14) passes with the new domain packages. Domain
  unit tests, using a fake clock and fakes with no network, cover the FR-53
  areas.
- **AC-30** (FR-54): Review: functional and twin tests use only synthetic data
  and synthetic credentials, and the driver and server matrix matches FR-54.
- **AC-31** (FR-55, FR-24, FR-44): Unit, through the SPEC-003 loader with an
  injected environment and file reader:
  - every setting in the table is registered with the declared type,
    presence, secrecy and description, in namespace `proxy`, `pool` or
    `upstream` (SPEC-003 AC-1);
  - each declared default applies when no source is given;
  - each per-setting rule rejects a boundary violation, for example:
    `pool.max_size=0`; `upstream.heartbeat_interval=100ms`;
    `proxy.listen` with port 0; an `upstream.hosts` entry with port 0 or a
    duplicate entry; an `upstream.auth.source` containing `$`; and
    `proxy.client_io_timeout=0s`;
  - the PEM rule rejects a missing file, a non-PEM file and a file over
    4 MiB, naming the setting without the path;
  - each cross-setting rule fails once with `setting` = its primary, and
    passes in its valid configuration. This includes:
    `proxy.tls.enabled=true` without a key; a key that does not match the
    certificate; `upstream.hosts` with two entries and no
    `upstream.replica_set`; `SCRAM-SHA-256` without a password; `none` with a
    username; `pool.min_size` > `pool.max_size`; and a CA path with TLS off;
  - `upstream.auth.username`, `upstream.auth.password` and `proxy.tls.key`
    are rejected as command-line literals, and accepted through the
    environment and `_FILE` references;
  - with `pool.max_waiters` unset, the effective queue bound equals
    `proxy.max_client_connections`.
- **AC-32** (FR-56): Twin and unit:
  - **twin:**
    - after the upstream has been reached, the twin stops reporting a
      primary: requests wait and get code 262 after `pool.checkout_timeout`;
      if a primary reappears within the timeout, they succeed;
    - never reached: requests get code 6 at once (within a small test
      bound);
    - auth failing: code 6 at once;
    - incompatible: code 6 at once;
    - requests already queued when auth starts failing get code 6 at once;
    - while no primary is known, the twin receives no pool connection
      attempts;
    - when a single target check fails and the immediate retry succeeds,
      readiness stays 200 and leases continue;
    - when a target check and its retry both fail (the twin stops answering
      on monitoring connections), readiness becomes `upstream_unavailable`,
      and new requests wait instead of being sent to that target;
    - with the upstream never reached and a seed reporting a standalone,
      requests and handshakes get detail `upstream_incompatible`, not
      `upstream_unavailable`;
  - **unit (fake clock):** successive failed pool connection attempts are
    spaced 100 ms, 200 ms, 400 ms and so on, up to 5 s, each within ±20 %,
    and the spacing resets after a success.
- **AC-33** (FR-35): Twin:
  - a write whose reply is `ok: 0` with code 10107 is forwarded unchanged;
    the next requests are not sent to that host until a check reports a
    primary; an immediate check reaches the twin; idle pool connections to
    the host close with `reason=not_primary`; an `upstream target marked not
    primary` record is emitted;
  - the same happens for an `ok: 1` reply with `writeConcernError.code` 91;
  - an `ok: 0` reply with code 11000 does not mark the host.
- **AC-34** (FR-57): Twin:
  - a raw client that sends a header and stalls is closed after
    `proxy.client_io_timeout` (`reason=io_timeout`);
  - a client that stops reading a large reply is closed after the timeout,
    and its upstream connection is discarded;
  - a request whose upstream reply the twin delays for twice the timeout is
    not cut off;
  - with `pool.checkout_timeout` set to three times
    `proxy.client_io_timeout` and the pool held:
    - a small request that waits in the queue for twice
      `proxy.client_io_timeout` is not closed for I/O timeout, and completes
      when the pool frees;
    - a large request stopped at the request buffer limit and waiting for a
      lease is not closed for I/O timeout either.
- **AC-35** (FR-58): Twin:
  - an upstream reply over `maxMessageSizeBytes`, and a reply with
    `moreToCome` set, each close the client connection, discard the upstream
    connection, and increment `corral_protocol_errors_total{side="upstream"}`
    with `message_too_large` and `unexpected_response` respectively;
  - with a test hook exposing the peak bytes held for one reply, a 30 MB
    reply is delivered intact while the peak stays at or below the reply
    chunk bound.
- **AC-36** (FR-59): Twin:
  - a `hello` with `moreToCome` gets no reply, and the connection then
    completes a `ping`;
  - a blocked command with `moreToCome` gets no reply, is never forwarded,
    and increments `corral_requests_dropped_total`.

## Open questions

Must be empty before approval.

None. All product questions were answered by the Owner on 2026-10-24 (see the
Decisions log). Approval still depends on re-review and on SPEC-001
Amendment 3 (FR-52).

## Decisions log

The Owner answered "accept all defaults" on 2026-10-24, adopting the
spec-writer's recommended default for every question, Q1 to Q15 (relayed by
the Orchestrator). Later Owner decisions on 2026-10-24 replaced Q4 (pinning),
part of Q1 (standalone) and Q5 (cross-client commands), and refined Q11/Q9
(unavailability).

| Date | Decision | Reason |
|---|---|---|
| 2026-10-24 | Q1: the upstream is one replica set (all traffic to the current primary, with failover followed) or one single endpoint. Multi-`mongos` affinity, secondary reads and sharding awareness are out of scope; read preferences are forwarded but run on the primary. *Standalone support was later removed by M2.* | Owner decision. |
| 2026-10-24 | Q2: corral presents itself as a `mongos` (`msg: "isdbgrid"`) and clamps the reported wire range; until the upstream has been reached, handshakes get error 6; after that, cached parameters are used. *Supported versions narrowed to 7.0–8.0 in revision 3; since revision 4 only `maxWireVersion` is capped, and `minWireVersion` is forwarded unchanged (N3).* | Owner decision. |
| 2026-10-24 | Q3: a single upstream service identity using SCRAM-SHA-256 (or `none`); clients do not authenticate; client auth commands are rejected with code 18 (115 for `getnonce` / `logout`); client access is controlled by network placement and optional mutual TLS. | Owner decision. Per-request multiplexing needs one upstream identity. |
| 2026-10-24 | Q4: lease per request; clear `exhaustAllowed`; no pinning or ownership tracking for cursors, transactions or sessions. *The pin-after-`moreToCome` part was replaced by M1.* | Owner decision. |
| 2026-10-24 | Q5: connection-state commands are forwarded unchanged, and the upstream sees `appName=corral` only. *Cross-client admin commands are now blocked; see below.* | Owner decision. |
| 2026-10-24 | Q6: no compression on either side; OP_COMPRESSED and legacy opcodes close the connection; OP_QUERY is allowed only for the handshake, and other OP_QUERY gets error 352. | Owner decision. |
| 2026-10-24 | Q7: client TLS is optional and off by default, TLS 1.2 or later, with optional mutual TLS; upstream TLS has mandatory verification; the wire listener defaults to `127.0.0.1:27017`; no Unix-socket listener in v1. | Owner decision. |
| 2026-10-24 | Q8: limit defaults: `pool.max_size` 10, `pool.min_size` 0, `pool.checkout_timeout` 5s, `pool.max_waiters` = max client connections, max client connections 10000 (accept then close), client idle timeout off, upstream connect timeout 10s, heartbeat 10s, upstream max idle time 5m, no per-request upstream timeout. | Owner decision. |
| 2026-10-24 | Q9: if the request was not executed, a synthesized error with retry labels; if the outcome is unknown, the client connection is closed; the error-code table; the upstream connection is discarded when the client leaves mid-exchange. | Owner decision. |
| 2026-10-24 | Q10: shutdown sequence with `listener_close_delay` (0s) and `drain_timeout` (20s). | Owner decision. |
| 2026-10-24 | Q11: readiness meaning and reasons, with precedence incompatible > auth_failed > unavailable; pool saturation does not affect readiness; startup does not wait for the upstream; upstream auth failure is never fatal. | Owner decision. |
| 2026-10-24 | Q12: the SPEC-001 amendment package is approved as listed, including the new error class `rejected`. | Owner decision. |
| 2026-10-24 | Q13: the test matrix is the Go driver and the PHP driver under PHP-FPM, MongoDB 7.0 and 8.0, single-node and 3-node replica sets, with failure cases through `twins/`. | Owner decision. |
| 2026-10-24 | Q14: messages waiting for a connection are held only up to a fixed request buffer limit; the rest stays unread, so TCP provides the backpressure. | Owner decision. |
| 2026-10-24 | Q15: message contents are transit-only processing (memory only, never persisted or emitted); client TLS is not mandatory. | Owner decision. |
| 2026-10-24 | Alignment with SPEC-003 (approved): settings in the SPEC-003 table format; namespaces `proxy`, `pool` and `upstream`; `*_path` instead of `*_file`; secret `proxy.tls.key`; secret, optional `upstream.auth.username` / `upstream.auth.password` with a cross-setting rule; optional `pool.max_waiters`; exit codes 2 and 1; upstream TLS renumbered to FR-45. Marking the username secret is a conservative reading; the Owner may override it. | Orchestrator instruction relaying the Owner. |
| 2026-10-24 | SPEC-001 Amendment 3 drafted (FR-52); `upstream authentication failed` logged at warn (SPEC-001 FR-32); the server-error clarification was added to the amendment. | Orchestrator instruction. |
| 2026-10-24 | **M1 (replaces the pin model):** after forwarding a `moreToCome` message, corral sends its own `ping` on the same upstream connection and waits for the reply before returning the connection to the pool. There is no pinning at all. Locally answered messages do not take part (FR-29, FR-59). | Owner decision on an escalated challenge-review item. |
| 2026-10-24 | **M2:** supported upstreams are a replica set or a single `mongos`. A standalone is reported as `upstream_incompatible` with a clear log record (FR-18, FR-34). | Owner decision on an escalated challenge-review item. |
| 2026-10-24 | **M4:** wait when transient: no primary or connect attempts failing (for example during an election) → requests wait up to `pool.checkout_timeout`, then 262. Never reached since startup, auth failing or incompatible → fail fast with 6. Pool reconnects use exponential backoff, with bounds defined by the spec-writer: 100 ms initial, doubling, 5 s cap, ±20 % jitter, reset on success (FR-56). | Owner decision on an escalated challenge-review item; the backoff constants are spec-writer values the Owner may override. |
| 2026-10-24 | **Cross-client admin commands are blocked (changes Q5):** corral rejects `currentOp`, a top-level `$currentOp` aggregation stage, `killOp`, `killAllSessions` and `killAllSessionsByPattern` with error 13 (`Unauthorized`) and a fixed message (FR-33). | Owner decision on an escalated challenge-review item. |
| 2026-10-24 | Revision 3, challenge-review fixes. These are spec-writer resolutions, not new product decisions, and the Owner may override any of them: <br>**M3:** handshakes and requests get error 6 while incompatible; incompatibility is defined, including clamped min > max; compatible↔incompatible transitions specified (FR-18, FR-19). <br>**M5:** the not-primary code family (10107, 13435, 13436, 11602, 11600, 189, 91) marks the host at once and holds leases until a primary is confirmed. `writeConcernError.code` in `ok: 1` replies counts, so FR-9 now reads that one field (FR-35). <br>**M6:** `setName` must equal `upstream.replica_set` (a mismatch is *incompatible*); SDAM stale-primary rule by `(electionId, setVersion)` (FR-18, FR-35). <br>**M7:** credentials are banned from replies corral synthesizes; forwarded reply bodies are upstream data and are excluded from AC-27 scans (FR-24, AC-11, AC-27). <br>**SHOULD items:** <br>- shutdown exchange-start definition, and a hook-based AC-26; <br>- monitoring authenticates only to the target, and arbiters are skipped; <br>- pool auth failures feed the auth state, and only auth error replies or SCRAM verification failures count; <br>- `proxy.client_io_timeout` (30s) added; <br>- late validation failure discards the upstream connection and closes the client; <br>- replies are streamed with a bounded chunk; <br>- connection IDs are unique among open connections, with int32 wrap; <br>- `moreToCome` messages that are not forwarded get no reply; <br>- upstream `moreToCome` or oversize replies are upstream protocol errors; <br>- queued-disconnect detection is best effort, and AC-21 uses a request within the buffer limit; <br>- `$query`-wrapped OP_QUERY handshakes are accepted, and OP_REPLY flags are specified; <br>- `saslSupportedMechs` and `compression` are checked for presence only; <br>- SPEC-001 FR-25 references corrected; <br>- every synthesized error has a class and detail, with one protocol vocabulary shared with SPEC-001; <br>- `moreToCome` outcome is defined; <br>- monitoring connections are distinguished by the `kind` label and `appName=corral-monitor`; <br>- the PEM bytes validated are the bytes used; <br>- `proxy.listen` port 1–65535; <br>- shared-identity note in Data and compliance; <br>- AC-16 made deterministic with round-robin and a twin; <br>- AC-19 uses a bytes-read hook; <br>- AC-6 allowed differences include `messageLength` and `checksumPresent`; <br>- supported versions narrowed to MongoDB 7.0–8.0 (wire 21–25) to match the test matrix; <br>- FR-51 items added. <br>**Choices the spec-writer made while resolving these** (Owner may override): <br>- the request buffer limit is at least 64 KiB, and a message whose deciding fields, or whose locally handled body, do not fit in it is a protocol error (FR-38); <br>- `upstream_incompatible` has class `config`; <br>- a failed barrier keeps the client connection open; <br>- requests still waiting at shutdown complete normally; <br>- labels apply only to codes 262, 6 and 91. | Challenge review REQUEST_CHANGES, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Revision 4, re-review fixes. These are spec-writer resolutions, not new product decisions, and the Owner may override any of them: <br>**N1:** discovery uses any matching member's `hosts`, `passives` and `primary` (FR-9 reads `primary`). While no usable target is known, all known members are checked at the 500 ms SDAM minimum. An "immediate check" covers all known members (FR-37). AC-18 no longer depends on election timing: it uses `pool.checkout_timeout=120s` and `replSetFreeze`. <br>**N2:** in replica-set mode, incompatibility is decided from seed check results when no matching member exists (`replica_set_name_mismatch`, `standalone`, `topology_mismatch`; first qualifying seed in `upstream.hosts` order), and `wire_version` from the primary. Single-`mongos` mode uses the configured host (FR-18). <br>**N3:** `minWireVersion` is forwarded unchanged and only `maxWireVersion` is capped at 25. The 21–25 range is only corral's acceptance rule (overlap), and AC-9/AC-10 are fixed. <br>**S1:** the barrier `ping` has no timeout, consistent with Q8 (no per-request upstream timeout). It is bounded by upstream failure and by the shutdown drain. Failure discards the upstream connection and keeps the client connection open (FR-29). `upstream.connect_timeout` no longer covers the barrier. <br>**S2:** `upstream_auth_failed` covers only codes 18 and 334, or a failed SCRAM signature check. Not-primary, shutdown, network, TLS and timeout errors during authentication are transient. While auth is failing there are no pool attempts, and each target check re-authenticates on a fresh connection, with backoff (FR-22). <br>**S3:** a failed target check marks the target unknown, which means a transient wait and readiness `upstream_unavailable` (FR-35, FR-56). <br>**S4:** queue and lease-wait time is excluded from the `proxy.client_io_timeout` read timer, so no cross-setting rule is needed (FR-57). <br>**S5:** the `moreToCome` span shape always has a checkout child when a checkout happens, and mirrors SPEC-001 FR-32 (FR-50). <br>**S6:** least-privilege guidance added (no `clusterMonitor`, no read on `system.profile`, no `queryStatsRead`). Residual routes are listed in Data and compliance and flagged for security@emag.ro. <br>**S7:** stray tag removed. <br>**Also:** the `upstream target marked not primary` record carries `error=server` / `corral.error.detail=other`; the metric no-collision rule now names `side`, `opcode` and `kind`. | Re-review REQUEST_CHANGES, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Revision 5, optional should-fixes from the revision 4 re-review (which returned APPROVE). These are spec-writer resolutions, not new product decisions, and the Owner may override them: <br>(1) Barriers in progress on connections to a host are aborted when that host is marked unknown or not primary (upstream connection discarded, client stays open) (FR-29). Upstream sockets use TCP keepalive (30 s idle, 10 s interval, 3 probes) and Linux `TCP_USER_TIMEOUT` 60 s, so a blackholed peer is detected in about 60 s instead of about 15 min (FR-28, FR-51). <br>(2) Incompatibility takes precedence over "never reached" for the error detail, and multiple fail-fast conditions follow the FR-46 precedence (FR-19, FR-56). <br>(3) AC-18: `replSetStepDown` with `force: true` and its result asserted; `replSetFreeze` run directly on each secondary with a period well under 120 s; background load is retryable operations only, with no transactions. <br>(4) FR-35: one immediate retry of a failed check before a previously usable target is marked unknown, to avoid readiness flapping. <br>(5) Minor: FR-18 notes that once a matching member is known, non-matching seeds stop being checked, so losing all matching members later is transient unavailability. <br>ACs added for (1), (2) and (4) in AC-14 and AC-32. | Re-review APPROVE with optional should-fixes, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | SPEC-002 revision 5 approved by the Owner, including the spec-writer judgement calls listed in the revision 4/5 rows; residual cross-client exposure handled by least privilege only (no further corral blocking), flagged for security@emag.ro review. SPEC-001 Amendment 3 approved the same day. | Owner approval, 2026-10-24. |
