# SPEC-002: Proxy core (wire listener, handshake, multiplexing pool, upstream)

Status: approved (Owner, 2026-10-24; revision 5); Amendment 1 approved (Owner, 2026-10-24); Amendment 2 approved (Owner, 2026-10-24); Amendment 3 approved (Owner, 2026-10-25); Amendment 4 approved (Owner, 2026-10-25); Amendment 5 approved (Owner, 2026-10-25); Amendment 6 approved (Owner, 2026-10-25); Amendment 7 approved (Owner, 2026-10-25)
Owner: Sorin Panduru
Last updated: 2026-10-25 (Amendment 7)

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
- **Request buffer limit (L):** the fixed maximum number of bytes of one
  client message that corral holds in memory before that message has a
  lease (FR-38). L = 65,536 bytes, counting the 16-byte header (Amendment 1,
  G-10).
- **Reply chunk (K):** the fixed maximum number of bytes of one upstream
  reply that corral holds in memory at a time (FR-58). K = 65,536 bytes
  (Amendment 1, G-10).
- **Inspected field:** a top-level field of a client command body that
  corral reads to make a decision (FR-9, FR-60): the command name (first
  key), `$db`, `lsid`, `txnNumber`, `autocommit`, `startTransaction`, and
  for `aggregate` the `pipeline` field, and for `getMore` the `collection`
  field (Amendment 1). *(Amendment 2)* The command name is included for the
  duplicate check inside the body. It is excluded from the comparison with
  kind-1 section identifiers (FR-60).
- *(Amendment 3)* **Member states (replica-set mode, FR-37).** Each server
  corral monitors is in one of these states:
  - **matching:** its latest successful check reports `setName` equal to
    `upstream.replica_set`;
  - **ghost:** its latest successful check reports `isreplicaset: true`
    without `setName`. This is the state of a member during startup or a
    rolling restart, or of a server that is not yet initiated;
  - **other non-matching:** after a matching member has been found, a known
    member whose latest successful check reports a standalone server (no
    `setName`, no `isreplicaset: true`, no `msg: "isdbgrid"`) or a `mongos`.
    An example is a member restarted as a standalone for maintenance. It is
    treated like a ghost: it stays known, is re-checked on the normal
    schedule, is never the target, and contributes no discovery. Before any
    matching member has been found, such a seed is an FR-18 incompatibility
    instead;
  - **unreachable:** its latest check failed;
  - **foreign:** its latest successful check reports a *different*
    `setName`.

  Matching, ghost, other non-matching and unreachable members stay in the
  known set. Foreign
  servers are ignored as members but are not forgotten (FR-37).

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
    - *(Amendment 4)* The record has level error and the fixed message
      `wire listener bind failed`. It carries `error` and
      `corral.error.detail` per SPEC-001 FR-29 (class `config`, with the
      detail of the bind error, for example `EADDRINUSE`). It carries no
      address, host or port.
    - It mirrors SPEC-003 Amendment 3's `admin listener bind failed`. Because
      it is not an `invalid configuration` record, the message
      `invalid configuration` alone still identifies load-time config errors
      (exit 2), separately from bind failures (exit 1).
  - Binding the wire listener is the last startup step for SPEC-001 FR-18.
    Startup does not wait for the upstream to be reachable (FR-46).
  - *(Amendment 5)* Errors from accepting connections on the bound wire
    listener after startup are handled by FR-66.
- **FR-2:** At most `proxy.max_client_connections` client connections are open
  at once. A connection over the limit is accepted and closed straight away
  with no bytes sent, and is counted (FR-48, `reason=limit_exceeded`).
  - *(Amendment 1, G-15)* When client TLS is on (FR-44), the limit is checked
    before the TLS handshake, so an over-limit connection is closed without
    any TLS bytes being sent.
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
  `messageLength` and `opCode` are signed 32-bit integers (Amendment 1,
  G-1).
  - **Minimum length per opcode** (Amendment 1, G-1). These are the sizes of
    the smallest well-formed message:

    | Opcode | Minimum `messageLength` | Derivation |
    |---|---|---|
    | OP_MSG (2013) | 26 | header 16 + flagBits 4 + section kind 1 + smallest BSON document 5 |
    | OP_QUERY (2004) | 34 | header 16 + flags 4 + shortest cstring 1 + numberToSkip 4 + numberToReturn 4 + smallest BSON document 5 |
    | any other opcode | 16 | header only |

  - A `messageLength` below the minimum for its opcode, including a negative
    value or one below 16, is a client protocol error with detail
    `malformed_message`.
  - A `messageLength` above the effective maximum is a client protocol error
    with detail `message_too_large`. The effective maximum is the upstream
    `maxMessageSizeBytes`, or 48,000,000 bytes while upstream parameters are
    not yet known.
  - **Order of checks** (Amendment 1, G-2). When several faults are present,
    the first failing check in this order decides the `error_detail`:
    1. `messageLength` negative or below 16: `malformed_message`;
    2. `messageLength` above the effective maximum: `message_too_large`;
    3. `opCode` not accepted (FR-6): `unsupported_opcode`;
    4. `messageLength` below the opcode's minimum: `malformed_message`;
    5. body checks, in the order the bytes arrive: OP_MSG flag bits (FR-7),
       then section and BSON structure (FR-7, FR-60), then the OP_QUERY
       structure (FR-8): `malformed_message`;
    6. checksum (FR-7): `checksum_mismatch`. It is reported only when no
       earlier check failed, because it can only be verified once the whole
       message has been read.

    Checks 1 to 4 use only the 16-byte header.
  - A client protocol error closes the client connection with no reply. This
    also covers non-MongoDB traffic, such as an HTTP request sent to the wire
    port (SPEC-001 AC-34). For example, `GET ` read as a length is
    542,393,671, which fails check 2.
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
    (`malformed_message`);
  - *(Amendment 1, G-19)* any BSON document in the body or in a kind-1
    section that fails the FR-60 validation rules (`malformed_message`);
  - *(Amendment 1, G-14)* an inspected-field integrity violation (FR-60,
    `malformed_message`).

  Kind-1 (document sequence) sections are allowed and forwarded unchanged.
  When validation finishes only after forwarding has begun, FR-38 applies.
- **FR-8:** Legacy handshake over OP_QUERY.
  - **Validation** (Amendment 1, G-3). An OP_QUERY must have:
    - a `fullCollectionName` cstring terminated inside the message;
    - `numberToSkip` and `numberToReturn` present;
    - a query document that passes FR-60;
    - optionally one `returnFieldsSelector` document that passes FR-60;
    - no bytes after that.

    The OP_QUERY `flags` field is ignored. Any violation is a client protocol
    error with detail `malformed_message`, and the connection is closed.
  - An OP_QUERY whose full collection name ends in `.$cmd` is a handshake
    when either:
    - its query document's first key is `hello`, `isMaster` or `ismaster`;
      or
    - its query document has a `$query` field holding a document whose first
      key is one of those. Other wrapper fields, such as `$readPreference`,
      are ignored.

    Matching is case-sensitive (FR-13). A handshake is answered locally with
    an OP_REPLY (FR-13 to FR-19).
  - Every OP_REPLY corral sends has `cursorID` 0, `startingFrom` 0 and
    `numberReturned` 1.
    - A handshake reply, and handshake errors with codes 6, 186 and 72, have
      `responseFlags` 0. The document is the FR-14 reply, or the error
      document `{ok: 0, errmsg, code, codeName}`.
    - Any other valid OP_QUERY gets `responseFlags` with `QueryFailure`
      (bit 1) set, and the document `{$err, code: 352, codeName:
      "UnsupportedOpQueryCommand", ok: 0}` with a fixed `$err` text (detail
      `unsupported_opcode`). The client connection stays open.
    - *(Amendment 2, R-1)* This includes an OP_QUERY whose command (first
      key, or first key inside `$query`) is an FR-23 auth command or an FR-33
      blocked command. Over OP_QUERY those get the 352 reply above, not 18,
      115 or 13: the only command corral accepts over OP_QUERY is the
      handshake.
      - The command is still never forwarded.
      - It is not counted in `corral_client_auth_rejected_total` or
        `corral_commands_blocked_total`, and it produces the 352 accounting
        below.
      - FR-61 steps 3 and 4 apply only to OP_MSG.
    - *(Amendment 1, G-3)* This 352 refusal keeps the connection open, so it
      is **not** counted in `corral_protocol_errors_total`, which counts only
      protocol errors that close a connection. It is counted as a request
      with outcome `proxy_error`, and in `corral_request_errors_total` with
      `error_class="protocol"` (FR-48). Its `command` label comes from the
      query document's first key, mapped per SPEC-001 FR-12.
      *(Amendment 2)* When the query document is wrapped in `$query`, the
      label comes from the first key inside `$query`. This is the same key
      that decides handshake status and R-1, so `{$query: {saslStart: 1}}`
      is labelled `saslStart`.
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
      `pipeline` array (FR-33), never its arguments;
    - *(Amendment 3)* for `killSessions` only, whether its argument (the
      value of the `killSessions` field) is an empty array or not an array
      (FR-33). The session IDs in a non-empty array are never decoded or
      kept;
    - *(Amendment 1)* the identifier of each kind-1 section, and whether an
      inspected field's name occurs more than once at the top level of the
      body. Both are used only for the FR-60 inspected-field integrity
      check. BSON structure is validated per FR-60 without decoding values.
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
    *(Amendment 1, G-20)* `writeErrors` is not read. *(Amendment 6)*
    `writeConcernError.codeName` is not read; FR-35 takes the code name of a
    not-primary code from its fixed table.
  - **From corral's own upstream exchanges** (handshake, authentication,
    monitoring and barrier `ping`):
    - the upstream parameters;
    - `isWritablePrimary`, `secondary`, `arbiterOnly`, `msg`, `setName`,
      `hosts`, `passives`, `primary`, `electionId` and `setVersion`;
      *(Amendment 3)* also `isreplicaset`, to recognise a ghost member
      (Terms);
    - `ok`, `code` and `codeName` of authentication replies (FR-22), and of
      the barrier `ping` reply (FR-29);
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
  - *(Amendment 1, G-5)* Matching is exact and case-sensitive. Only these
    three spellings are handshake commands. Any other spelling, such as
    `HELLO`, `Hello` or `IsMaster`, is an ordinary command: it is forwarded
    upstream under the normal rules (over OP_MSG), or refused with 352 (over
    OP_QUERY, FR-8). The same exact matching applies to the FR-23 auth
    commands and the FR-33 blocked commands.
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
  - *(Amendment 1, G-6)* The "first handshake" is the first handshake
    command corral reads on the connection, whatever its outcome. A first
    handshake refused with 6 or 72 still counts as the first, so a later
    handshake carrying `client` gets 186. This keeps the outcome independent
    of upstream state and timing. When a handshake was refused, its `client`
    metadata is still taken for that connection's logs and spans.
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
    - *(Changed by Amendment 3)* The upstream is incompatible when no
      matching member has been found since startup, and at least one
      reachable seed's latest successful check reports one of these:

      | Seed check result | `reason` |
      |---|---|
      | a different `setName` (a foreign server) | `replica_set_name_mismatch` |
      | a standalone server: no `setName`, no `isreplicaset: true` and no `msg: "isdbgrid"` | `standalone` |
      | a `mongos` | `topology_mismatch` |

      When several seeds qualify, the reason comes from the first of them in
      `upstream.hosts` order. A ghost seed (`isreplicaset: true` without
      `setName`) or an unreachable seed never makes the upstream
      incompatible.
    - *(Amendment 5, clarification)* **When seed results are judged.**
      Incompatibility from seed results is judged only once every seed in
      `upstream.hosts` has finished its first check, successfully or not.
      - An unreachable seed counts as finished once its first check fails.
        The FR-35 single retry does not apply, because a seed is not yet a
        usable target.
      - The reason then comes from the first qualifying seed in
        `upstream.hosts` order, as above.
      - Until every seed has finished its first check, the upstream is not
        incompatible from seed results. It has not been reached, so
        readiness is `upstream_unavailable` (FR-46), handshakes and requests
        fail fast with detail `upstream_unavailable` (FR-19, FR-56), and no
        `upstream incompatible` record is emitted.
      - *(Owner decision 2026-10-25)* The first checks of all seeds start
        concurrently at startup (FR-37), and each is bounded by
        `upstream.connect_timeout`. So this period lasts at most about one
        `upstream.connect_timeout`.
      - If a matching member is found first, seed results never make the
        upstream incompatible (Amendment 3 rule above).
      - Rationale: the reason no longer depends on the order in which seed
        checks finish, and a foreign seed that answers first no longer
        causes a spurious `upstream incompatible` record.
      - Single-`mongos` mode has one host, so this changes nothing there.
    - The upstream is also incompatible, with `reason=wire_version`, when the
      current primary fails the wire-version acceptance rule.
    - *(Changed by Amendment 3, Owner decision 2026-10-25)* Servers are no
      longer dropped permanently. FR-37 defines how each state is handled:
      a foreign server is ignored as a member but still re-checked, and is
      re-admitted if it later reports the matching `setName`; ghost and
      unreachable members stay in the known set.
  - The upstream becomes compatible again as soon as a later check result
    no longer meets any of these conditions.
  - *(Changed by Amendment 3)* Once a matching member has been found, seed
    check results no longer make the upstream incompatible. If every
    matching member is later lost, or reports a different `setName`, that is
    transient unavailability (FR-56), not incompatibility.

  - **While incompatible:**
    - readiness is `upstream_incompatible` (FR-46);
    - handshakes and requests fail fast with code 6 and detail
      `upstream_incompatible` (FR-19, FR-56);
    - one `upstream incompatible` error record is emitted on each transition
      into this state, carrying the `reason` (FR-49).
  - **Compatible → incompatible** (for example a rolling change to an
    unsupported version, or a single-`mongos` host re-pointed at a replica
    set):
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
  - *(Amendment 1, G-8)* "Reached" means that a check of the target got a
    successful `hello` reply that passed FR-18, whatever the outcome of the
    authentication on that monitoring connection. From then on the upstream
    parameters are known, so with authentication failing since startup:
    - handshakes succeed;
    - requests fail fast with code 6 and detail `upstream_auth_failed`
      (FR-56);
    - readiness is `upstream_auth_failed` (FR-46).
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
    - *(Amendment 3, clarification)* When a server becomes the target (a new
      primary is selected, or the `mongos` is first reached), corral runs an
      immediate check of it, still at least 500 ms after its previous check
      (FR-37), and authenticates as part of that first check. It does this on
      the existing monitoring connection if that has not yet authenticated,
      or on a new one.
    - Until that check finishes, the authentication state is the one from
      the most recent attempt.
    - A server that stops being the target does not de-authenticate. Its
      later checks simply do not authenticate again.
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
  - *(Amendment 2, R-1)* This applies to OP_MSG. Over OP_QUERY these
    commands get the FR-8 352 reply.
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
  not primary), the upstream connection is discarded. The client connection
  stays open, because an unacknowledged message never reports an outcome to
  the client.

  *(Amendment 1, G-12)*
  - **Not-primary code in the ping reply.** If the barrier `ping` reply
    carries one of the FR-35 not-primary or shutdown codes as its top-level
    `code`, the host is marked exactly as for a forwarded reply (FR-35), and
    the barrier counts as failed. In single-`mongos` mode the FR-35
    single-`mongos` rule applies.
    - *(Amendment 2, R-5)* "Exactly as for a forwarded reply" includes the
      `upstream target marked not primary` warn record (rate-limited, FR-49),
      with `db.response.status_code` from the ping reply. *(Changed by
      Amendment 6)* Its `corral.db.response.code_name` follows the FR-35
      "Code name in the record" rule.
    - The `upstream barrier failed` record is emitted as well; the two
      records report different things (the host state and the barrier
      outcome).
  - **Client disconnect during a barrier.** This does not count as "in
    flight" under FR-41, because nothing is owed to the client. The barrier
    runs to completion, and on success the connection returns to the pool.

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
    - *(Amendment 3, Owner decision 2026-10-25)* It also blocks
      `killSessions` whose argument is an empty array, or is not a non-empty
      array (for example `killSessions: []`, `killSessions: 1` or
      `killSessions: {}`). Under the shared upstream identity that form kills
      every client's sessions, exactly like `killAllSessions`. It gets the
      same reply: code 13 (`Unauthorized`), the same fixed message, and
      detail `command_blocked`.
    - `killSessions` with a non-empty array of session IDs is still
      forwarded (residual exposure, Data and compliance).
    - The "deciding fields must fit" rule of FR-38 applies to the
      `killSessions` argument as it does to the `aggregate` pipeline: if the
      start of the argument is not within L, the message is
      `malformed_message`.
    - Matching is on the command name as received, which is case-sensitive.
    - A blocked command gets a synthesized error with code 13
      (`Unauthorized`), a fixed `errmsg` and detail `command_blocked`. It is
      not forwarded, and is counted and logged (FR-48, FR-49).
    - Only the top-level first pipeline stage is inspected. Nested pipelines
      (for example inside `$unionWith` or `$lookup`) are not inspected, and
      this limitation is documented (FR-51).
    - *(Amendment 1, G-14)* To keep this check from being bypassed, an
      `aggregate` whose `pipeline` arrives in a kind-1 section, and any
      message whose inspected fields are duplicated, is refused per FR-60.
    - *(Amendment 2, R-1)* This applies to OP_MSG. Over OP_QUERY these
      commands get the FR-8 352 reply.
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
    - *(Amendment 3, clarification)* The single retry applies only to check
      failures of class `network` or `timeout`, including TLS failures (SPEC-001
      FR-29). Any other unsuccessful outcome is applied at once, with no
      retry. That includes a `hello` error reply, a reply showing the target
      is no longer primary or is now a ghost, foreign or incompatible server
      (FR-18, FR-37), and an authentication outcome (FR-22).
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
  - *(Amendment 3, clarification)* `writeConcernError.code` is considered
    only in an `ok: 1` reply. In an `ok: 0` reply only the top-level `code`
    counts, and any `writeConcernError` it carries is ignored for marking.
  - *(Amendment 1, G-20)* These codes found only inside `writeErrors[]` do
    not mark the host. `writeErrors` is not read (FR-9).
  - *(Amendment 6, Owner decision 2026-10-25)* **Code name in the record.**
    The `upstream target marked not primary` record (FR-49) carries:
    - `db.response.status_code`: the code that caused the marking. That is
      the top-level `code` for the `ok: 0` form and the barrier `ping`
      reply (FR-29), and `writeConcernError.code` for the `ok: 1` form;
    - `corral.db.response.code_name`: the name of that code in the fixed
      table above, for example 91 gives `ShutdownInProgress`.
    - For the `ok: 1` form the name always comes from the table, because
      FR-9 does not read `writeConcernError.codeName`. FR-9 reads nothing new.
    - *(Owner decision 2026-10-25)* The `ok: 0` form and the barrier `ping`
      reply also take the name from the table, not from the reply's
      `codeName`. So one code always gives the same name in this record,
      whatever the upstream sends. FR-9 still reads `codeName` of those
      replies for the SPEC-001 FR-23 span attributes.
  - **Single `mongos` target** *(replaced by Amendment 1, G-13)*. A `mongos`
    passes through errors from the shards behind it, so most of these codes
    describe a shard, not the `mongos`. In single-`mongos` mode:
    - only a top-level `code` of 91 (`ShutdownInProgress`) or 11600
      (`InterruptedAtShutdown`) in an `ok: 0` reply marks the `mongos`
      unusable, with the steps above;
    - every other listed code, and any code in `writeConcernError`, is
      forwarded unchanged without marking the target;
    - the barrier `ping` reply (FR-29) follows the same rule.
- **FR-36:** Read preference. `$readPreference` is forwarded unchanged. All
  requests run on the target, whatever read preference they ask for.
- **FR-37:** Upstream monitoring.
  - **Known members** *(changed by Amendment 3, Owner decision 2026-10-25)*.
    The known members are every seed in `upstream.hosts`, plus every server
    learned from the check result of any matching member (FR-18), primary or
    not. corral learns from that member's `hosts` and `passives`, and from
    its `primary` field, which names the member it believes is primary.
    Discovery therefore works while no primary is known.
    - Servers listed only under `arbiters`, or reporting `arbiterOnly:
      true`, are not monitored.
    - **Ghost, other non-matching and unreachable members** (Terms) stay in
      the known set. They
      are re-checked on the normal schedule (the intervals below, including
      the fast interval while no usable target is known). They become usable
      again as soon as a check reports the matching `setName`. They are never
      a target while ghost or unreachable, and they contribute no discovery.
    - **Foreign servers** (a different `setName`) are ignored: they are
      never a target, contribute no discovery, and are excluded from the
      fast-interval checking.
      - A foreign server is not forgotten while it is a seed or still listed
        by a matching member. It is re-checked every
        `upstream.heartbeat_interval`.
      - It is re-admitted as a matching member as soon as a check reports
        the matching `setName`.
      - A foreign seed is still used by FR-18 only while no matching member
        has been found since startup.
    - A server is removed from monitoring only when it is neither a seed nor
      listed by any matching member's latest check, and its own latest check
      did not report the matching `setName`.
  - **Check intervals.**
    - *(Amendment 5, Owner decision 2026-10-25)* At startup, the first
      checks of all seeds in `upstream.hosts` start concurrently. No seed's
      first check waits for another's to finish (FR-18).
    - While a usable target is known, each known member is checked every
      `upstream.heartbeat_interval`.
    - While no usable target is known (never reached, election, target
      marked not primary or unknown), *all* known members are checked
      repeatedly, at least 500 ms apart per member (the SDAM
      `minHeartbeatFrequencyMS`), until a usable target is found.
      *(Amendment 3)* Foreign servers keep the
      `upstream.heartbeat_interval` schedule.
  - **Connections.** corral uses at most one monitoring connection per host,
    with `client.application.name = "corral-monitor"`. Each check is a
    `hello` exchange limited by `upstream.connect_timeout`. Authentication
    happens only on the target's monitoring connection (FR-22), and
    *(Amendment 3)* on the first check after a server becomes the target.
  - **Immediate checks.** A failed pool connection attempt, a network error
    on a leased connection, or a not-primary error (FR-35) starts an
    *immediate check*: an immediate check of all known members, each still at
    least 500 ms after its previous check. *(Amendment 3)* A target change
    starts an immediate authenticating check of the new target (FR-22).
  - **Results.** The checks provide the upstream parameters, compatibility
    (FR-18), the current primary (FR-35) and readiness (FR-46).

### Request size, buffering and streaming

- **FR-38:** Bounded buffering of requests.
  - **Request buffer limit.** L = 65,536 bytes, counting the 16-byte header
    (Amendment 1, G-10; previously "at least 64 KiB, chosen by the
    planner"). It is stated in the operator documentation (FR-65). It is not
    a setting.
  - **Before a lease.** While a message has no lease (not yet classified, or
    waiting in the queue), corral holds at most the request buffer limit of
    it in memory. The rest stays unread in the client socket, so TCP flow
    control holds the client back.
  - **Deciding fields must fit.** corral decides whether a message is
    answered locally, refused, blocked or forwarded from the buffered prefix
    alone.
    - If the command name, or for `aggregate` the first pipeline stage name,
      or *(Amendment 3)* for `killSessions` the start of its argument, is not
      within the prefix, the message is a client protocol error with detail
      `malformed_message`.
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
  - *(Amendment 1)* **Validate before writing; hold back the end.**
    - Every streamed byte passes the FR-5, FR-7, FR-8 and FR-60 checks that
      apply to it before corral writes it upstream.
    - corral holds back the final byte or bytes of every forwarded message
      (at least the last byte, and the 4-byte checksum when one is present)
      until every check on the whole message has passed, including the
      checksum.
    - So a message that fails any check is never delivered to the upstream
      complete. The server never executes it, and the partial message dies
      with the discarded upstream connection.
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
    (`reason=client_abandoned`). A barrier (FR-29) is not "in flight" in
    this sense (Amendment 1, G-12).
  - **Queued.** If the client connection closes while its request is still
    waiting in the queue, and corral detects the close, the request is
    removed from the queue and nothing is sent upstream. Detection is best
    effort: corral detects a close only when the peer's close is observable
    on the socket. An undetected close is handled by the in-flight rule once
    the request has been forwarded.
  - *(Amendment 2, R-3)* **Closed mid-message.** If the client closes or
    resets the connection after corral has read at least the first byte of a
    message and before the whole message has been read:
    - the message counts as one request in `corral_requests_total` with
      `outcome="proxy_error"`. Its `command` label is the SPEC-001 FR-12
      label if the command name had been read, otherwise `other`;
    - it is counted once in `corral_request_errors_total` with class
      `network`, and its detail is classified per SPEC-001 FR-29 (for
      example `eof` or `ECONNRESET`);
    - it is not a protocol error, so it is not counted in
      `corral_protocol_errors_total`;
    - the close reason is `client_closed`;
    - it is observed once in `corral_request_duration_seconds` under the
      same `command` label. The duration runs from the first byte read to
      the moment corral detects the close, as for any request that ends
      with the connection closing;
    - if part of the message had already been streamed upstream, the
      FR-38 hold-back means the upstream never receives the complete
      message. That upstream connection is discarded
      (`reason=client_abandoned`). If the message had no lease, nothing is
      sent upstream.
    - A close between messages (no byte of a next message read) is not a
      request.
- **FR-42:** A panic recovered while handling one client connection closes
  that client connection and discards any upstream connection it held. Other
  connections and the process continue. It is reported per SPEC-001 FR-29
  (class `internal`).
  - *(Amendment 7, security review RC5)* The `request handler panic
    recovered` record is rate-limited per FR-49, like the other
    rate-limited records: at most one record per 30 seconds, carrying a
    `suppressed_count`. Otherwise a client that can trigger a recovered
    panic could flood the logs. Only the record is limited. Every panic
    still closes its connection, and every close is still counted
    (`corral_client_connections_closed_total{reason="internal_error"}`,
    FR-48). No metric is limited.
- **FR-43:** Upstream unavailability never closes existing client connections
  by itself. Clients keep getting handshake replies (FR-19), waits or
  synthesized errors (FR-56) until the upstream recovers.

### TLS

- **FR-44:** Client-side TLS. It is optional and off by default; it is not
  mandatory in any deployment.
  - When `proxy.tls.enabled` is `true`, the wire listener accepts only TLS
    connections, with TLS 1.2 or later. A plaintext connection fails the TLS
    handshake and is closed.
  - *(Amendment 1, G-15)* The connection limit is checked before the TLS
    handshake (FR-2). The TLS handshake must finish within
    `proxy.client_io_timeout` (FR-57); otherwise the connection is closed
    with `reason=tls_failure` and class `timeout`, detail
    `deadline_exceeded`.
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
  - *(Amendment 1, G-9)* Readiness follows the monitoring view of the
    target. When pool connection attempts fail with network, TLS or timeout
    errors while monitoring checks succeed:
    - readiness does not change directly;
    - requests wait (FR-56);
    - each failed pool attempt starts an immediate check (FR-37);
    - if those checks also fail (with the FR-35 single retry), the target is
      marked unknown, and readiness becomes `upstream_unavailable`.

    "Connection failures" in the table above means failures of target
    checks, not of pool connection attempts. Pool authentication failures
    still set the authentication state (FR-22), and so do affect readiness.

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
  sessions or cursors; those belong to the clients. A second SIGTERM or
  SIGINT during shutdown, including during step 1, is handled by FR-63
  (Amendments 1 and 2).

  *(Amendment 5)* The accept error that results from closing the listener in
  step 2 is not an error (FR-66). A permanent accept error also ends the
  process. It runs this sequence without step 1 and exits with code 1
  instead of 0 (FR-66).

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
- **FR-51:** Operator documentation (in the file named by FR-65) states:
  - that all clients share corral's single upstream identity. corral blocks
    `currentOp`, `$currentOp` (top-level stage only), `killOp`,
    `killAllSessions`, `killAllSessionsByPattern` and *(Amendment 3)*
    `killSessions` with an empty session list, but other commands such
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
    and that the least-privilege guidance above closes most of them;
  - *(Amendment 4; Owner decision 2026-10-25)* that the guidance does
    **not** close one route:
    - any client can list every client's session IDs with
      `$listSessions` / `$listLocalSessions` for the own user, which needs
      no special privilege and, under the shared identity, covers all
      clients;
    - it can then end those sessions with `killSessions` and explicit IDs
      (or act on them with `endSessions`, `abortTransaction` or
      `commitTransaction`);
    - corral does not block this. Applications that share an upstream
      identity through corral can therefore end each other's sessions,
      and only separate upstream identities isolate them;
  - *(Amendment 1)* the reply chunk size (FR-58); that handshake, auth and
    blocked command names are matched case-sensitively (FR-13); and the
    FR-60 refusal of duplicated inspected fields and of a `pipeline` sent as
    a document sequence;
  - *(Amendment 1)* that a second SIGTERM or SIGINT during shutdown ends the
    drain at once (FR-63);
  - *(Amendment 2)* that it also cuts `proxy.shutdown.listener_close_delay`
    short when it arrives during that delay (FR-63);
  - *(Amendment 3)* how corral treats replica-set members during rolling
    restarts and re-configurations: ghost and unreachable members stay
    monitored, and a member reporting a different replica set name is
    ignored but re-checked and re-admitted when it matches again (FR-37).

### Required amendment to SPEC-001

- **FR-52:** SPEC-002 depends on SPEC-001 Amendment 3, which contains
  everything listed in Interfaces, "SPEC-001 amendments requested by this
  spec". *(Amendment 2)* SPEC-001 Amendment 3 was approved by the Owner and
  promoted to `specs/SPEC-001-observability-conventions.md` on 2026-10-24,
  so this precondition is met.

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
  - Test hooks follow FR-64 (Amendment 1).

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
  - *(Amendment 1, G-15)* **TLS handshake:** when client TLS is on, the
    handshake must also finish within the timeout (FR-44).
  - **On expiry:** the client connection is closed (`reason=io_timeout`,
    class `timeout`, detail `deadline_exceeded`). If the request was partly
    sent upstream, or the reply partly forwarded, the upstream connection is
    discarded. If nothing had been sent upstream, the lease is returned.
  - The timeout does not cover the time spent waiting for the upstream's
    reply.
- **FR-58:** Reply forwarding.
  - **Streaming.** Replies are streamed from the upstream to the client.
    corral holds at most the reply chunk K = 65,536 bytes of one reply in
    memory (Amendment 1, G-10; previously "chosen by the planner"), never
    the whole reply. K is stated in the operator documentation (FR-65). The
    FR-9 reply fields are read as they pass.
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

### Added by Amendment 1

- **FR-60:** BSON validation and inspected-field integrity (G-14, G-19).
  - **Structural validation.** Every BSON document in an OP_MSG body, in
    every kind-1 section, and in an OP_QUERY must be structurally valid,
    recursively through all nested documents and arrays (Owner plan default
    P3: kind-1 documents are fully validated):
    - every length field must be consistent with the bytes that follow;
    - every element type must be a known BSON type;
    - every cstring must be terminated inside its document;
    - every document must end with its terminating byte.

    Nesting deeper than 1024 levels is invalid.
    - Levels are counted with the top-level document as level 1. Each nested
      document or array adds one level, and arrays count like documents.
    - *(Amendment 2, R-6)* Each document in a kind-1 section is a top-level
      document, and is counted from level 1 on its own. The same applies to
      each OP_QUERY document. The body and the sections do not add to each
      other's depth.
    - The server's `maxBSONDepth` defaults to 200 and can be configured
      from 21 to 999, so 1024 never refuses anything any server
      configuration accepts. It only bounds corral's parser.
  - **UTF-8.** String *values* are not checked for UTF-8, so that corral
    never refuses what the server would accept. Values corral emits are
    sanitised per SPEC-001 FR-12 and FR-25.
  - **Duplicate keys.** Duplicate keys in general are left for the server to
    judge, with two exceptions.
  - **Inspected-field integrity.** Each of these is a client protocol error
    with detail `malformed_message`, and the connection is closed:
    - an inspected field (Terms) that appears more than once at the top
      level of the body. *(Amendment 2, R-6)* This includes the command name:
      a body whose first key appears again later at the top level, such as
      `{find: "a", …, find: "b"}`, is refused;
    - a kind-1 section whose identifier equals the name of an inspected
      field other than the command name (for example `pipeline` on an
      `aggregate`, or `$db`, `lsid`, `txnNumber`).

    *(Amendment 2, A-Q1, Owner decision "skip")* Kind-1 section identifiers
    are not compared with the command name. There is no bypass here, because
    the server always takes the command from the first key of the body. Not
    comparing also avoids holding an unbounded command name for the
    comparison.

    Rationale: the server could otherwise read a different value than the
    one corral inspected, for example a second `pipeline` starting with
    `$currentOp`, which would bypass FR-33. Official drivers never produce
    these messages.

    Other kind-1 identifiers that duplicate a body key are not checked by
    corral. The server rejects them, and checking them would need memory
    proportional to the body's key count.
  - **Order.** These checks run as the bytes arrive (FR-5 check 5).
    - Detected before a lease, which is always the case for a message
      within L: nothing is forwarded.
    - Detected after forwarding has begun: FR-38 applies. The upstream
      connection is discarded, and the held-back final bytes ensure the
      upstream never receives the complete message.
- **FR-61:** Order of handling for one message (G-6, G-7). Once FR-5 to FR-8
  and FR-60 have passed for the buffered prefix, corral decides in this
  order, and the first rule that applies wins:
  1. **Shutdown:** a message whose first byte was read after FR-47 step 3
     started gets 91 (`shutting_down`).
  2. **Handshake commands** (FR-13). Within a handshake, the first match
     wins:
     1. `loadBalanced: true`: 72;
     2. repeated `client`: 186 (FR-16);
     3. never reached or incompatible: 6 (FR-19);
     4. otherwise, the FR-14 reply.
  3. **Client auth commands** (FR-23): 18 or 115. OP_MSG only (Amendment 2,
     R-1).
  4. **Blocked commands** (FR-33): 13. OP_MSG only (Amendment 2, R-1).
     *(Amendment 3)* This includes `killSessions` with an empty or non-array
     argument. Over OP_QUERY it gets 352, like any other blocked command.
  5. **Non-handshake OP_QUERY** (FR-8): 352. This includes OP_QUERY auth and
     blocked commands (Amendment 2, R-1).
  6. **Fail-fast states** (FR-56): 6.
  7. **Queue full** (FR-27): 262. Otherwise the message waits for a lease or
     is leased.

  Local refusals (steps 2 to 5) therefore never depend on upstream state,
  except for the handshake's own code 6. Faults that close the connection
  (FR-5 to FR-8, FR-38, FR-60) come before all of these. A local message
  that does not fit the request buffer is `message_too_large` under FR-38.
  For `moreToCome` messages, FR-59 applies to every refusal above.
- **FR-62:** Unsolicited upstream data (G-17).
  - A pool connection with no outstanding request that receives any bytes
    is discarded. This is counted as an upstream protocol error with
    `error_detail="unexpected_response"`, and `reason=error` on close.
  - If it is closed by the peer instead, it is discarded with
    `reason=error`.
  - corral detects this either while the connection is idle, or at the
    latest when it is checked out, before any request is written on it. In
    the checkout case the request is not sent on that connection, and the
    checkout continues with another connection.
- **FR-63:** Second shutdown signal (G-18).
  - A SIGTERM or SIGINT received after graceful shutdown has started ends
    the drain at once, as if `proxy.shutdown.drain_timeout` had elapsed
    (FR-47 step 4). Steps 4 and 5 then run, including the SPEC-001 FR-34
    tracing flush bound, and the process exits 0.
  - *(Amendment 2, R-4)* If the second signal arrives during FR-47 step 1
    (`proxy.shutdown.listener_close_delay`), the rest of the delay is
    skipped as well. corral closes the listener at once (step 2), then
    closes every client connection and discards their upstream connections
    (step 4, with no drain), then runs step 5.
    - Exchanges in progress are cut off as at the end of a drain.
    - The same single record is emitted, and the process exits 0.
  - corral emits one warn record with `component=lifecycle` and the fixed
    message `shutdown forced by second signal`. Further signals are ignored.
  - *(Amendment 5, Owner decision 2026-10-25)* When the shutdown was started
    by a permanent accept error (FR-66), a second signal still exits with
    code 1, not 0.
- **FR-64:** Test hooks (G-11).
  - The test hooks used by the acceptance criteria exist only in binaries
    built with the `corraltest` build tag, as defined by ADR-0014 and
    ADR-0022. They are: start and step holds, pool round-robin order, panic
    injection, and the probe server exposing bytes read before a lease, peak
    reply bytes and the L and K constants.
  - Hooks are enabled only through `CORRALTEST_*` environment variables read
    by that build.
  - The production build (`make build`) contains no hook code, and ignores
    `CORRALTEST_*` variables. These do not start with `CORRAL_`, so SPEC-003
    FR-8 does not report them.
  - The probe server binds to loopback only, and exposes counts and
    constants, never message bytes.
  - *(Amendment 2, R-2)* **Normative hook interface** (from ADR-0014 and
    ADR-0022; JSON field names inside `/conns` stay with ADR-0022):

    | Variable | Effect in the `corraltest` build |
    |---|---|
    | `CORRALTEST_HOLD_DIR` | Directory for hold files and `probe.addr`. Holds take effect only when this and `CORRALTEST_HOLD_AT` are both set. |
    | `CORRALTEST_HOLD_AT` | Comma-separated hold points. At each selected point corral creates `<dir>/held-<point>` and waits, for at most 30 s, until `<dir>/release-<point>` exists. Points: `laststep` (before binding the wire listener), `shutdown_delay` (start of `listener_close_delay`), `shutdown_listener_closed` (after FR-47 step 2), `shutdown_step3` (before idle connections are closed in step 3), `shutdown_drain` (keeps the drain open as if an exchange were in progress), `shutdown_drain_expired` (before step 4), `shutdown_pool_close` (before step 5). The SPEC-003 points `logger` and `checkconfig` keep their files `<dir>/held` and `<dir>/release` (ADR-0014). Unknown point names hold nothing. |
    | `CORRALTEST_POOL_ORDER=roundrobin` | The pool hands out idle connections in round-robin order. The default is most-recently-used. |
    | `CORRALTEST_PANIC_COMMAND=<name>` | The client handler panics after classifying a message whose command name equals `<name>`. |
    | `CORRALTEST_PROBE_LISTEN=<loopback host:port>` | Starts the probe HTTP server on a loopback address only, and writes its bound address to `<CORRALTEST_HOLD_DIR>/probe.addr`. |
    | `CORRALTEST_REGISTRY` | SPEC-003 test registry option (ADR-0014). Not used by SPEC-002. |

    | Probe endpoint | Returns |
    |---|---|
    | `GET /conns` | JSON with, for each open client connection ID: the bytes read for the current message before a lease, the maximum bytes read before a lease, and the peak bytes held for one reply. Counts only. |
    | `GET /constants` | `{"L": 65536, "K": 65536}`. |

    `CORRALTEST_REQUIRE_DOCKER` is read by the test harness, not by corral,
    and is not a hook.
- **FR-65:** Operator documentation file (G-10). The FR-51 statements are in
  `docs/operations/proxy.md`. The FR-38 request buffer limit L and the FR-58
  reply chunk K are both 65,536 bytes; this is normative, and the document
  states both values.

### Added by Amendment 5

- **FR-66:** Wire listener accept errors after startup.
  - **Transient errors are retried.** These accept errors are transient:
    timeouts, errors the platform reports as temporary, `EMFILE`, `ENFILE`,
    `ENOBUFS`, `ENOMEM`, `ECONNABORTED` and `EINTR`. After one, the
    listener stays open, and corral tries to accept again after a delay.
    - The delay starts at 5 ms and doubles after each further consecutive
      transient error, up to a cap of 1 s.
    - It resets after a successful accept.
    - *(Owner decision 2026-10-25)* The delay has no jitter. Transient
      errors produce no record and no metric.
  - **A closed listener during shutdown is not an error.** When shutdown
    closes the wire listener (FR-47 step 2, FR-63), the resulting accept
    error produces no record and does not change the exit code.
  - **Any other accept error is permanent.** corral then:
    - emits exactly one record: level error, `component=lifecycle`, the
      fixed message `wire listener serve failed`, and `error` and
      `corral.error.detail` classified per SPEC-001 FR-29. It has no other
      attributes, and in particular no address, host or port;
    - stops accepting client connections;
    - *(Owner decision 2026-10-25)* runs the FR-47 graceful shutdown
      sequence without step 1. `proxy.shutdown.listener_close_delay` is not
      applied, because the listener can no longer accept:
      - `/readyz` reports `shutting_down` (SPEC-001 FR-18);
      - the wire listener is closed (step 2);
      - client connections are handled and drained as in steps 3 and 4,
        within `proxy.shutdown.drain_timeout`;
      - pool and monitoring connections are closed, and the SPEC-001 FR-34
        tracing flush runs (step 5);
    - exits with code 1, not 0.
    - A second SIGTERM or SIGINT during this shutdown is handled as in FR-63
      (the drain ends at once, then step 5, and one `shutdown forced by
      second signal` record), but the process still exits with code 1.

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
| `proxy.client_io_timeout` | duration | default `30s` | no | > 0 | none | Maximum time to receive a started client message, to write a reply to a client, or to complete a client TLS handshake. |
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
| `saslStart`, `saslContinue`, `authenticate` (OP_MSG) | 18 | `AuthenticationFailed` | `client_auth_unsupported` | `rejected` | no |
| `getnonce`, `logout` (OP_MSG) | 115 | `CommandNotSupported` | `client_auth_unsupported` | `rejected` | no |
| blocked command (FR-33, OP_MSG), including `killSessions` with an empty or non-array argument (Amendment 3) | 13 | `Unauthorized` | `command_blocked` | `rejected` | no |
| non-handshake OP_QUERY, including auth and blocked commands (Amendment 2, R-1) | 352 | `UnsupportedOpQueryCommand` | `unsupported_opcode` | `protocol` | no |
| repeated client metadata | 186 | `ClientMetadataCannotBeMutated` | `handshake_rejected` | `rejected` | no |
| `loadBalanced: true` handshake | 72 | `InvalidOptions` | `handshake_rejected` | `rejected` | no |

The `upstream_auth_failed` and `upstream_incompatible` conditions
deliberately use `HostUnreachable`, so that clients never learn details of
corral's upstream configuration. When more than one row could apply to one
message, FR-61 decides which one is sent (Amendment 1).

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
- target marked not primary;
- *(Amendment 1)* unsolicited upstream data, and shutdown forced by a second
  signal.

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
    (FR-38, FR-40, FR-41 mid-message, FR-57).
- `corral_request_duration_seconds` runs from the moment the first byte of
  the request is read to the moment the reply has been fully written, or for
  `moreToCome`, to the end of the barrier. *(Amendment 2)* For a request
  that ends with the connection closing (FR-38, FR-40, FR-41 mid-message,
  FR-57, and protocol errors after the first byte), it runs until the close
  is detected or performed. Every request counted in `corral_requests_total`
  is observed exactly once.
- `corral_requests_dropped_total` counts `moreToCome` messages that were
  refused (FR-59).
- *(Amendment 1, G-4)* `corral_request_errors_total{error_class}` is
  incremented exactly once for each request counted with
  `outcome="proxy_error"`, labelled with the SPEC-001 FR-29 class of that
  error (the Synthesized error replies table, or the class of the failure
  that closed the connection). Its sum over classes therefore equals
  `corral_requests_total{outcome="proxy_error"}` summed over commands.
  Upstream `ok: 0` replies (`server_error`) are not counted in it.
- *(Amendment 1, G-4)* `corral_client_connections_accepted_total` counts
  every connection the listener accepts. This includes connections then
  closed for being over the limit (FR-2) and connections whose TLS handshake
  fails (FR-44). `corral_client_connections` counts open connections within
  the limit, including those still in the TLS handshake. It never counts
  over-limit connections.
- *(Amendment 1, G-4)* `corral_upstream_connect_errors_total{kind,error_class}`
  counts every failed attempt to make an upstream connection ready: connect,
  TLS, handshake or authentication, including authentication failures
  (class `server`). `corral_upstream_auth_failures_total{kind}` counts the
  subset classified as `upstream_auth_failed` (FR-22). So for each `kind`,
  auth failures ≤ connect errors with `error_class="server"`.
- *(Amendment 1)* **Requests closed by a client protocol error.** When a
  client protocol error (FR-5 to FR-8, FR-38, FR-60) or a client I/O timeout
  (FR-57) closes the connection after corral has read at least the first
  byte of a message, that message counts as one request in
  `corral_requests_total` with `outcome="proxy_error"`.
  - Its `command` label is the SPEC-001 FR-12 label of the command name if
    corral had read it, otherwise `other`. Examples are an HTTP request, an
    unsupported opcode, or a bad length.
  - It is also counted once in `corral_request_errors_total`, with class
    `protocol` (or `timeout` for FR-57).

  Connections closed before any message byte is read (limit exceeded, TLS
  failure, idle timeout, client close) are not requests. This makes the
  AC-39 equality checkable.
- *(Amendment 2, R-3)* **Client closes mid-message.** One request with
  `outcome="proxy_error"`, labelled with the command name if read,
  otherwise `other`. It counts once in `corral_request_errors_total` with
  class `network`, and the close reason is `client_closed`. It is not counted
  in `corral_protocol_errors_total`. It is observed once in
  `corral_request_duration_seconds`, ending when the close is detected
  (FR-41).
- *(Amendment 1, G-3)* `corral_protocol_errors_total` counts only protocol
  errors that close a connection (client side) or discard it (upstream
  side, including FR-62 unsolicited data). The 352 refusal (FR-8) is not
  counted in it.
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
| `config` | error | `wire listener bind failed` | `setting` (`proxy.listen`), `error` (`config`), `corral.error.detail` (for example `EADDRINUSE`); no address, host or port | FR-1 (Amendment 4); once, then exit 1; not an `invalid configuration` record |
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
| `proxy` | error | `request handler panic recovered` | `error`, `corral.error.detail`, `stack` | SPEC-001 FR-29; rate-limited *(Amendment 7, FR-42)* |
| `pool` | warn | `pool checkout timed out` / `pool wait queue full` | none | rate-limited |
| `upstream` | debug | `upstream connection opened` / `upstream monitoring connection opened` | `server.address`, `server.port`, `corral.upstream.connection.id` | |
| `upstream` | debug | `upstream connection closed` / `upstream monitoring connection closed` | `reason` (upstream close reasons), `server.address`, `server.port` | |
| `upstream` | warn | `upstream connection failed` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited |
| `upstream` | warn | `upstream authentication failed` | `error`, `corral.error.detail`, `server.address`, `server.port`, and `db.response.status_code` / `corral.db.response.code_name` from the upstream's reply, when there is one | rate-limited; no username; warn per SPEC-001 FR-32 |
| `upstream` | warn | `upstream unavailable` | `reason` (readiness reasons) | on transition only |
| `upstream` | error | `upstream incompatible` | `reason` (incompatibility reasons), `server.address`, `server.port` | on transition only; *(Amendment 5)* in replica-set mode, not before every seed has finished its first check (FR-18) |
| `upstream` | info | `upstream available` | none | on transition only |
| `upstream` | info | `upstream primary changed` | `server.address`, `server.port` | |
| `upstream` | warn | `upstream target marked not primary` | `error` (`server`), `corral.error.detail` (`other`), `server.address`, `server.port`, `db.response.status_code` (the code that caused the marking: top-level `code`, or `writeConcernError.code` in an `ok: 1` reply), `corral.db.response.code_name` (*changed by Amendment 6:* that code's name from the FR-35 fixed table, never read from `writeConcernError`; the same for every form, Owner decision 2026-10-25). Both come from an upstream reply (forwarded, or the barrier `ping` reply per Amendment 2 R-5), so SPEC-001 FR-23 allows them | rate-limited; an error record reporting the upstream's error |
| `upstream` | warn | `upstream protocol error` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited; also used for FR-62 unsolicited data |
| `upstream` | warn | `upstream barrier failed` | `error`, `corral.error.detail`, `server.address`, `server.port` | rate-limited |
| `lifecycle` | warn | `shutdown forced by second signal` | none | FR-63 (Amendments 1 and 2); once per process |
| `lifecycle` | error | `wire listener serve failed` | `error`, `corral.error.detail` (SPEC-001 FR-29); no address, host or port | FR-66 (Amendment 5); once, on a permanent accept error after startup; then shutdown and exit 1. Not emitted for transient errors or for the listener closed by shutdown |

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

Amendment 1 of this spec needs no further SPEC-001 change. It adds no
metric, label, error detail, readiness reason or log key. Its one new
record uses the existing `lifecycle` component. Amendments 2 and 3 of this
spec need no SPEC-001 change either; neither adds a new signal.
Amendment 4 needs none either: its record uses the existing `config`
component and the existing keys `setting`, `error` and `corral.error.detail`.
Amendment 5 needs none either: its record uses the existing `lifecycle`
component and the existing keys `error` and `corral.error.detail`, and the
FR-18 clarification adds no signal. Transient accept errors produce no
record and no metric (Owner decision 2026-10-25).
Amendment 6 needs none either. It changes only where the value of an
existing key comes from. The record still reports an error carried in an
upstream reply, so SPEC-001 FR-23 (item 7 above) still allows the key.
Amendment 7 needs none either. It applies this spec's existing FR-49 rate
limit, which follows SPEC-001 FR-33, to one more record. SPEC-001 FR-29
still governs that record's content (error level, class `internal`, an
optional `stack`, and never the panic value).

## Behavior on failure

| Situation | Client sees | corral does |
|---|---|---|
| Invalid or missing setting, secret on the command line, cross-setting rule broken, unreadable PEM | n/a | exit 2 with SPEC-003 records, before binding anything (FR-55) |
| Wire listener cannot bind | n/a | one record: level error, `component=config`, message `wire listener bind failed`, `setting=proxy.listen`, class `config` with the bind error's detail, no address; exit 1 (FR-1, Amendment 4) |
| Transient accept error on the wire listener after startup (Amendment 5) | new connections are accepted after a short delay | retried after 5 ms, doubling to a 1 s cap, no jitter, reset after a successful accept; no record, no metric (FR-66) |
| Permanent accept error on the wire listener after startup (Amendment 5) | no new connections; in-flight exchanges finish, then connections are closed, as in graceful shutdown | one record: level error, `component=lifecycle`, message `wire listener serve failed`, `error` and `corral.error.detail` only, no address; FR-47 sequence without `listener_close_delay`; exit 1, also after a second signal (FR-66) |
| Malformed frame, bad length, OP_COMPRESSED, legacy opcode, bad flags or checksum, invalid body, deciding field beyond the buffer | connection closed, no reply | counts; rate-limited warn (FR-5 to FR-7, FR-38) |
| Duplicated inspected field (including the command name), or a kind-1 section named like an inspected field other than the command name (Amendments 1 and 2) | connection closed, no reply | nothing forwarded when detected before a lease; otherwise the upstream connection is discarded and never receives the complete message; `malformed_message` (FR-38, FR-60) |
| Malformed OP_QUERY (Amendment 1) | connection closed, no reply | `malformed_message` (FR-8) |
| Non-handshake OP_QUERY, including auth and blocked commands (Amendment 2) | OP_REPLY `QueryFailure`, code 352 | connection stays open; not a protocol-error count (FR-8) |
| Client sends auth command over OP_MSG | error 18 / 115 | not forwarded; counts; rate-limited warn (FR-23) |
| Client sends a blocked admin command over OP_MSG, including `killSessions` with an empty list (Amendment 3) | error 13 | not forwarded; counts; rate-limited warn (FR-33) |
| Pool full, wait exceeds `pool.checkout_timeout` | error 262 + labels | request read to its end and discarded; checkout span in error (FR-27, FR-38, FR-39) |
| Wait queue full | error 262 + labels, at once | (FR-27, FR-39) |
| Upstream never reached, auth failing, or incompatible | error 6 + labels at once; handshakes fail only when never reached or incompatible | readiness 503 with reason (FR-19, FR-56, FR-46) |
| Replica-set seeds still finishing their first checks at startup, no matching member yet (Amendment 5) | error 6 with detail `upstream_unavailable` | readiness `upstream_unavailable`; incompatibility is judged only after every seed's first check (FR-18) |
| Election, target marked not primary, connect attempts failing | requests wait, then error 262 | checks the topology; backoff on reconnects (FR-35, FR-56) |
| Replica-set member in a ghost state, unreachable, or reporting a different `setName` during a rolling restart or re-configuration (Amendment 3) | nothing directly | ghost and unreachable members stay known and are re-checked; a foreign member is ignored but re-checked every heartbeat and re-admitted when it matches again (FR-37) |
| Not-primary error in a reply (including `writeConcernError` in an `ok: 1` reply) | the reply, unchanged | marks the host, holds leases, re-checks (FR-35); in single-`mongos` mode only top-level 91 / 11600 mark; codes in `writeErrors`, and a `writeConcernError` inside an `ok: 0` reply, never mark (Amendments 1 and 3); the record's code name comes from the FR-35 table (Amendment 6) |
| Upstream connection fails after the request was sent, or late request validation failure | client connection closed | upstream connection discarded (FR-38, FR-40) |
| Upstream protocol violation (unknown `responseTo`, `moreToCome` reply, oversize, bad checksum) | client connection closed | upstream connection discarded (FR-11, FR-58) |
| Unsolicited bytes or close on an idle pool connection (Amendment 1) | nothing | connection discarded; the checkout uses another connection (FR-62) |
| Client stalls mid-message or reads a reply too slowly | client connection closed after `proxy.client_io_timeout` | lease returned or upstream connection discarded (FR-57) |
| Client disconnects mid-exchange | n/a | upstream connection discarded, or the request is removed from the queue (best effort) (FR-41); a barrier still completes (FR-29) |
| Client closes mid-message (Amendment 2) | n/a | one `proxy_error` request, class `network`, `reason=client_closed`; a partly streamed upstream connection is discarded (FR-41) |
| Barrier `ping` fails after a `moreToCome` message | nothing | upstream connection discarded; client connection open; a not-primary code in the ping reply also marks the host and logs it (FR-29) |
| `moreToCome` message refused | nothing | dropped and counted (FR-59) |
| Panic in a connection handler | that client connection closed | recovered; other clients unaffected; rate-limited error record, every close counted (FR-42, Amendment 7) |
| Graceful shutdown | in-flight exchanges finish; late messages get error 91; connections closed | FR-47 |
| Second SIGTERM/SIGINT during shutdown (Amendments 1 and 2) | remaining connections closed | rest of `listener_close_delay` skipped, drain ends at once; exit 0 (FR-63) |
| Client TLS handshake fails, stalls, or no client certificate under mutual TLS | connection closed | counts; rate-limited warn (FR-44) |

corral itself never retries a request upstream. Retries are left to the
drivers, which the error codes and labels enable.

## Non-functional requirements

- **Security** *(review by security@emag.ro recommended before approval)*:
  - **Shared identity:** all clients act upstream as one service identity,
    which collapses per-application privileges. The service user should have
    least privilege for the applications served.
  - **Cross-client effects:** partly mitigated by blocking `currentOp`,
    top-level `$currentOp`, `killOp`, `killAllSessions` and
    `killAllSessionsByPattern` (FR-33), and by refusing the FR-60 bypass
    forms (Amendment 1). The residual routes are listed in Data and
    compliance; least privilege for the service user (FR-51) is the
    mitigation. *(Amendment 4)* One exception: listing sessions for the own
    user and then ending them with `killSessions` and explicit IDs is not
    prevented by least privilege. By Owner decision it is documented only
    (FR-51).
  - **No client authentication on the wire listener:** protection comes from
    the loopback default (FR-1), network placement and optional mutual TLS
    (FR-44). Client TLS is not mandatory.
  - **Upstream TLS:** verification cannot be turned off (FR-45).
  - **Secrets:** the service username, password and TLS private key are
    SPEC-003 secrets: never on the command line, never emitted, never in
    synthesized replies, no defaults (FR-24, FR-44).
  - **Message contents:** corral reads only the FR-9 fields of message
    contents.
  - **Test hooks:** these exist only in the `corraltest` build (FR-64).
- **Availability:**
  - Upstream outages and pool saturation do not close client connections by
    themselves (FR-43).
  - Pool saturation does not change readiness (FR-46).
  - Transient unavailability waits instead of failing (FR-56).
  - Reconnects back off (FR-56).
  - Shutdown drains in-flight exchanges (FR-47).
  - *(Amendment 3)* Rolling restarts and re-configurations never make corral
    forget a replica-set member permanently (FR-37).
  - *(Amendment 5)* Transient accept errors, such as running out of file
    descriptors, do not stop the wire listener; corral backs off and keeps
    accepting (FR-66).
- **Resource bounds:**
  - upstream connections ≤ `pool.max_size` + one monitoring connection per
    monitored host;
  - client connections ≤ `proxy.max_client_connections`;
  - waiting requests ≤ the FR-27 bound;
  - memory for messages without a lease ≤ `proxy.max_client_connections` ×
    L, with L = 65,536 bytes (FR-38);
  - reply memory ≤ `pool.max_size` × K, with K = 65,536 bytes (FR-58);
  - BSON nesting ≤ 1024 levels, with the top-level document as level 1
    (FR-60);
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
  through the most direct routes. *(Amendment 3)* That now includes
  `killSessions` with an empty or absent session list, which under the shared
  identity would kill every client's sessions.

  **Residual exposure routes** (flagged for review by security@emag.ro).
  corral does not block these. The routes in this first list work only if
  the service user has privileges that FR-51 advises against, so the FR-51
  least-privilege guidance mitigates them:
  - reading `system.profile`, which can show other clients' operations and
    their filters;
  - the `$queryStats` aggregation stage;
  - *(corrected by Amendment 4)* `$listSessions` / `$listLocalSessions`
    with `allUsers: true` or with a `users` list naming other users. These
    need the `listSessions` cluster action. The same stages for the caller's
    own user are **not** in this list; see the session-enumeration route
    below;
  - `$currentOp` nested inside a sub-pipeline (for example `$unionWith` or
    `$lookup`), wherever the server allows it, which FR-33 does not inspect;
  - an `aggregate` with `$currentOp` wrapped in `explain`;
  - `connectionStatus`, which reveals the service identity's name.

  *(Amendment 4; Owner decision 2026-10-25, "document only")*
  **Session enumeration and termination, not prevented by least privilege:**
  - `$listSessions` and `$listLocalSessions` for the caller's own user need
    no special privilege. That is their form with no arguments, an empty
    `users` list, or a `users` list naming only the caller.
  - Under the shared upstream identity, the "own user" is the service user.
    Any client can therefore list the session IDs of every client of every
    corral instance that uses that identity.
  - With those IDs, the client can end other clients' sessions through
    `killSessions` with explicit IDs, which FR-33 forwards. It can also act
    on them with `endSessions`, or with `abortTransaction` or
    `commitTransaction` carrying another client's `lsid`.
  - Least privilege does not prevent this. No role can grant the service
    user the application access it needs while denying these
    own-user operations.
  - By Owner decision, corral does not block it. The FR-51 operator
    documentation must state it (Amendment 4).

  *(Amendment 1, G-14; updated by Amendments 3 and 4)* Further routes that
  need no extra privilege, because all clients share one identity:
  - `killSessions` with explicit `lsid`s naming another client's sessions,
    and `endSessions`, `abortTransaction` or `commitTransaction` sent with
    another client's `lsid`. Session IDs are random 128-bit UUIDs and cannot
    be guessed, but *(Amendment 4)* they can be listed through the
    own-user session stages above. These requests are forwarded, unlike the
    empty-list form, which FR-33 blocks;
  - session-less cursors, which any client knowing the 64-bit cursor ID can
    `getMore` or `killCursors`. Official drivers always use (implicit)
    sessions, which bind cursors to an `lsid`;
  - a `pipeline` sent in a kind-1 section, and duplicated inspected fields
    that make corral read a different value than the server. These were
    bypasses of the FR-33 block, and FR-60 **refuses** them (connection
    closed, `malformed_message`; confirmed by the Owner as OPEN-A1). A kind-1
    section named like the command name is not refused (Amendment 2, A-Q1).
    It is not a bypass, because the server takes the command from the body's
    first key.

  For the remaining routes, the mitigations are the FR-51 least-privilege
  guidance for the service user, as far as roles allow, and keeping session
  and cursor IDs out of logs available to other tenants. SPEC-001 already
  allows them only in logs and spans, never in metrics. *(Amendment 4)*
  These mitigations do not prevent the session enumeration and termination
  route above. That route is accepted and documented, not mitigated.
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
and 8.0 (FR-54). "The marker harness" means the SPEC-001 AC-33 harness. "Test
hook" means a `corraltest`-build hook (FR-64).

- **AC-1** (FR-1, FR-55): Functional:
  - with defaults, corral listens on `127.0.0.1:27017` only;
  - `--proxy.listen=:27017` and `--proxy.listen=127.0.0.1:0` each exit 2 with
    `setting=proxy.listen`;
  - an address already in use exits 1. *(Amendment 4)* stdout holds exactly
    one record about it:
    - level error, `component=config` and the message
      `wire listener bind failed`;
    - `setting=proxy.listen`, `error=config` and
      `corral.error.detail=EADDRINUSE`;
    - no address, host or port in any attribute, checked with the marker
      harness using the port number as a marker;
    - no record with the message `invalid configuration` is emitted;
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
  | negative `messageLength` (Amendment 1) | `malformed_message` |
  | OP_MSG with `messageLength` 25, and OP_QUERY with 33 (Amendment 1) | `malformed_message` |
  | `messageLength` above the maximum | `message_too_large` |
  | an HTTP `GET` request | `message_too_large` |
  | OP_COMPRESSED, each listed legacy opcode, and an unknown opcode | `unsupported_opcode` |
  | an unknown opcode with `messageLength` 16 (Amendment 1) | `unsupported_opcode` |
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
      upstream stays compatible. *(Amendment 3)* It is still re-checked on
      the `upstream.heartbeat_interval` schedule (AC-56);
    - *(Amendment 5)* the single-seed cases above are judged as soon as
      that seed's first check finishes. With several seeds, timing follows
      AC-61;
  - **twin, single-`mongos` mode:** a host reporting a replica-set member
    gives `topology_mismatch`, and a standalone gives `standalone`;
  - **twin, both modes:**
    - a member listed under `arbiters` gets no monitoring connection;
    - monitoring connections to non-target members perform no
      authentication, and the target's monitoring connection does;
    - a read with `readPreference=secondary` is sent to the primary.
- **AC-19** (FR-38): Twin, with `pool.max_size=1` held by the twin, the
  request buffer limit L = 65,536 bytes (confirmed through the probe's
  `/constants`, FR-64), and a test hook exposing the bytes corral has read
  from a given client connection:
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
  - *(Amendment 7, FR-49)* **Rate limit.** On a fake clock, the hook panics
    on 10 client connections within 30 s:
    - exactly one `request handler panic recovered` record is emitted in
      that window;
    - an 11th panic after the window emits one record with
      `suppressed_count` 9;
    - all 11 connections are closed, and
      `corral_client_connections_closed_total{reason="internal_error"}`
      increases by exactly 11;
    - a concurrent client still completes its request.
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
    produces one record and then `suppressed_count` 9. *(Amendment 7)*
    This includes `request handler panic recovered` (AC-22);
  - no record or span for a synthesized error carries
    `db.response.status_code`.
- **AC-28** (FR-51, FR-65): Review: the operator documentation file
  `docs/operations/proxy.md` contains all FR-51 statements, and states L and
  K as 65,536 bytes.
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
  - *(Amendment 6)* The code name carried by these records is checked by
    AC-62.
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
    reply is delivered intact while the peak stays at or below K = 65,536
    bytes.
- **AC-36** (FR-59): Twin:
  - a `hello` with `moreToCome` gets no reply, and the connection then
    completes a `ping`;
  - a blocked command with `moreToCome` gets no reply, is never forwarded,
    and increments `corral_requests_dropped_total`.

Added by Amendment 1:

- **AC-37** (FR-5): Unit, table-driven over the 16-byte header and minimal
  bodies:
  - each opcode's minimum length is accepted (26 for OP_MSG with an empty
    body document and no first element, which then fails FR-7 instead), and
    the minimum minus one fails with `malformed_message`;
  - lengths −1, 0, 15 and `INT32_MIN` give `malformed_message`;
  - for every pair of faults combined in one message, the reported detail is
    the one first in FR-5's order. Examples:
    - an unknown opcode with length 10 gives `malformed_message` (check 1
      before check 3);
    - an unknown opcode with length 60,000,000 gives `message_too_large`;
    - an OP_MSG with an unknown required flag and a bad checksum gives
      `malformed_message`;
    - an OP_MSG with a bad checksum and otherwise valid content gives
      `checksum_mismatch`.
- **AC-38** (FR-8, FR-48): Twin (raw-socket client):
  - each of these closes the connection with `malformed_message`:
    - an OP_QUERY whose `fullCollectionName` has no terminator;
    - one missing `numberToReturn`;
    - one with an invalid query document;
    - one with bytes after `returnFieldsSelector`;
  - a valid non-handshake OP_QUERY gets 352 and leaves
    `corral_protocol_errors_total` unchanged, while
    `corral_requests_total{outcome="proxy_error"}` and
    `corral_request_errors_total{error_class="protocol"}` each increase by 1.
- **AC-39** (metrics definitions, FR-2, FR-44): Functional and twin:
  - with `proxy.max_client_connections=1`, opening 3 connections increases
    `corral_client_connections_accepted_total` by 3 and
    `corral_client_connections` by at most 1;
  - with client TLS on, a connection that fails the TLS handshake increases
    `accepted_total`;
  - across a mixed run of synthesized errors, the sum of
    `corral_request_errors_total` equals the sum of
    `corral_requests_total{outcome="proxy_error"}`;
  - a wrong service password increases both
    `corral_upstream_connect_errors_total{error_class="server"}` and
    `corral_upstream_auth_failures_total` for the same `kind`, and a network
    failure increases only the former.
- **AC-40** (FR-13, FR-16, FR-61): Twin:
  - an OP_MSG `HELLO` and an OP_MSG `IsMaster` are forwarded to the twin
    (not answered locally); over OP_QUERY they get 352;
  - `SaslStart` is forwarded, not refused with 18;
  - with the upstream never reached, a first `hello` with `client` gets 6,
    and a second `hello` with `client` then gets 186;
  - a first `hello` with both `loadBalanced: true` and `client` gets 72,
    and a following `hello` with `client` gets 186;
  - with the upstream never reached:
    - `saslStart` gets 18;
    - `killOp` gets 13;
    - a non-handshake OP_QUERY gets 352;
    - a `find` gets 6;
  - during shutdown (after step 3, hook held), a `saslStart` gets 91.
- **AC-41** (FR-19, FR-46): Twin:
  - with the twin answering `hello` but rejecting authentication from
    startup: handshakes succeed with the twin's parameters; a `find` gets 6
    with detail `upstream_auth_failed`; readiness is `upstream_auth_failed`;
  - with monitoring checks succeeding while the twin refuses new pool
    connections:
    - readiness stays 200 while the immediate checks succeed;
    - requests wait and get 262 after `pool.checkout_timeout`;
    - when the twin also fails the checks and their retries, readiness
      becomes `upstream_unavailable`.
- **AC-42** (FR-29, FR-35, FR-41): Twin:
  - in replica-set mode, a barrier `ping` reply with code 10107 marks the
    host as not primary, and the upstream connection is discarded. A ping
    reply with code 91 does the same. The twin sends both; a real server
    can answer `ping` on a secondary, but a shutting-down server answers
    91;
  - in single-`mongos` mode, a ping reply with code 10107 does not mark the
    `mongos`, and one with code 91 does;
  - a client that disconnects while its barrier is delayed: the barrier
    completes, and the twin sees the same upstream connection used by the
    next lease (no `client_abandoned` close).
- **AC-43** (FR-35): Twin, single-`mongos` mode:
  - `ok: 0` replies with codes 10107, 13435, 189 and 11602, and an `ok: 1`
    reply with `writeConcernError.code` 91, are forwarded unchanged and do
    not mark the target;
  - an `ok: 0` reply with code 91, and separately 11600, marks it unusable
    with the FR-35 steps.

  Replica-set mode: an `ok: 1` reply whose `writeErrors[0].code` is 10107
  does not mark the host.
- **AC-44** (FR-60, FR-33): Twin, raw-socket client. Every message below is
  smaller than L, so it is fully checked before any lease. Each closes the
  connection with `malformed_message`, and the twin receives nothing:
  - an `aggregate` whose body has no `pipeline` and whose kind-1 section
    `pipeline` holds `{$currentOp: {}}`;
  - an `aggregate` with two top-level `pipeline` fields, the second starting
    with `$currentOp`;
  - a `find` with two `$db` fields;
  - a kind-1 document with an inner length that overruns it;
  - a body nested 1025 levels deep, counting the top-level document as level
    1 and mixing nested documents and arrays. That is about 8 KiB, within L.

  A body nested exactly 1024 levels deep passes corral and is forwarded,
  whatever the server then answers.

  These are forwarded unchanged:
  - an `insert` with a kind-1 `documents` section;
  - a document whose string value is not valid UTF-8 (in a field corral does
    not inspect);
  - a body with duplicate keys in a field corral does not inspect.
- **AC-45** (FR-2, FR-44, FR-57): Twin, with client TLS on:
  - over the connection limit, a client starting a TLS handshake receives no
    TLS bytes before the connection closes;
  - a client that sends a partial ClientHello and stalls is closed after
    `proxy.client_io_timeout`, with `reason=tls_failure`.
- **AC-46** (FR-62): Twin:
  - the twin sends 10 unsolicited bytes on an idle pool connection: that
    connection is discarded, and
    `corral_protocol_errors_total{side="upstream",error_detail="unexpected_response"}`
    increments;
  - the next request is sent on another connection and succeeds;
  - the same with the twin closing the idle connection gives
    `reason=error`, and the request still succeeds.
- **AC-47** (FR-63): Functional, using a shutdown test hook that holds an
  exchange in the drain: a second SIGTERM ends the drain at once, one
  `shutdown forced by second signal` record is emitted, and the process
  exits 0 within the SPEC-001 FR-34 flush bound plus a tolerance. A third
  signal changes nothing.
- **AC-48** (FR-64): Functional:
  - the production binary from `make build`, started with
    `CORRALTEST_PROBE_LISTEN=127.0.0.1:0` and a hold directory set, starts no
    probe server, creates no hold file, and emits no SPEC-003 unknown-variable
    record for `CORRALTEST_*`;
  - a static check confirms that the hook code is compiled only under the
    `corraltest` tag;
  - the `corraltest` build's probe `GET /constants` returns 65,536 for both
    L and K.
- **AC-49** (FR-38, FR-60, FR-48): Twin, with a pool connection free:
  - a raw client sends a 5 MB `aggregate` whose second top-level `pipeline`
    field (starting with `$currentOp`) sits in the last kilobytes, past L.
    The twin's upstream connection receives some bytes and is then closed
    by corral; the twin never receives a complete message (the bytes it
    received are fewer than the message's `messageLength`). The client
    connection is closed, and
    `corral_protocol_errors_total{side="client",error_detail="malformed_message"}`
    increments;
  - the same with a correct body but a wrong checksum: the twin never
    receives the final 4 bytes, and the detail is `checksum_mismatch`;
  - each of these counts as one `corral_requests_total{command="aggregate",outcome="proxy_error"}`,
    and an HTTP `GET` counts as one with `command="other"`.

Added by Amendment 2:

- **AC-50** (FR-8, FR-23, FR-33, FR-61; R-1): Twin (raw-socket client).
  - Each of these OP_QUERY messages on `admin.$cmd` gets an OP_REPLY with
    `QueryFailure` set and code 352:
    - `{saslStart: 1, …}`;
    - `{getnonce: 1}`;
    - `{currentOp: 1}`;
    - `{$query: {killOp: 1, op: 1}}`.
  - For each, the twin receives nothing, and the connection stays open.
  - The `command` labels are `saslStart`, `other` (`getnonce` is not
    allowlisted), `other` (`currentOp`), and `other` (`killOp`, taken from
    the first key inside `$query`). A `$query`-wrapped `{saslStart: 1}` is
    labelled `saslStart`.
  - `corral_client_auth_rejected_total` and `corral_commands_blocked_total`
    are unchanged.
  - `corral_requests_total{outcome="proxy_error"}` and
    `corral_request_errors_total{error_class="protocol"}` each increase by
    one per message.
- **AC-51** (FR-64; R-2): Functional, with the `corraltest` build:
  - **Hold points:** with `CORRALTEST_HOLD_DIR` set and `CORRALTEST_HOLD_AT`
    listing each FR-64 point in turn, corral creates `held-<point>` at that
    point and proceeds only after `release-<point>` exists. Without the
    release file, it proceeds after at most 30 s.
  - **Unknown point:** `CORRALTEST_HOLD_AT=nosuchpoint` holds nothing.
  - **Probe:** with `CORRALTEST_PROBE_LISTEN=127.0.0.1:0`:
    - `probe.addr` holds a loopback address;
    - `GET /constants` returns exactly `{"L": 65536, "K": 65536}`;
    - `GET /conns` lists each open client connection ID with the three
      counts, and contains no message bytes (marker check).
  - **Pool order and panic:** `CORRALTEST_POOL_ORDER=roundrobin` and
    `CORRALTEST_PANIC_COMMAND` behave as FR-64 states. These are also used by
    AC-16 and AC-22.
- **AC-52** (FR-41, FR-48; R-3): Twin (raw-socket client):
  - **Command name read:** a client sends a header and the first 100 bytes of
    a `find` body (enough for the command name), then closes. The results:
    - `corral_requests_total{command="find",outcome="proxy_error"}` and
      `corral_request_errors_total{error_class="network"}` each increase by
      1;
    - `corral_protocol_errors_total` is unchanged;
    - the close is counted with `reason="client_closed"`;
    - `corral_request_duration_seconds{command="find"}` gains exactly one
      observation.
  - **Command name not read:** a client sends 8 bytes of a header, then
    closes. The request is counted with `command="other"`.
  - **Partly streamed:** a client sends most of a 5 MB `insert` with a pool
    connection free, then resets the connection. The twin's upstream
    connection is closed with `reason=client_abandoned`, and the twin never
    receives a complete message.
- **AC-53** (FR-63; R-4): Twin, using the `corraltest` build with
  `proxy.shutdown.listener_close_delay=60s` and
  `CORRALTEST_HOLD_AT=shutdown_pool_close`:
  - after SIGTERM, a new client connection is still accepted;
  - a raw client starts an exchange whose reply the twin holds;
  - a second SIGTERM 1 s later skips the rest of the delay;
  - while corral is held at `shutdown_pool_close` (after the listener has
    closed and client connections have been closed), a new connection
    attempt is refused, and the held client connection has been closed with
    no reply;
  - after `release-shutdown_pool_close` is created, the process exits 0
    well before the 60 s delay would have ended (within the SPEC-001 FR-34
    flush bound plus a tolerance);
  - exactly one `shutdown forced by second signal` record is emitted.
- **AC-54** (FR-29, FR-49; R-5): Twin, replica-set mode:
  - a barrier `ping` reply with code 10107 produces both an `upstream target
    marked not primary` record, with `db.response.status_code="10107"`, and
    an `upstream barrier failed` record;
  - in a separate run, a ping reply with code 91 produces both records, with
    `db.response.status_code="91"`.

  Both records are subject to their rate limits, so each run starts with a
  fresh limiter.
- **AC-55** (FR-60; A-Q1, R-6): Twin (raw-socket client).
  - **Kind-1 nesting:**
    - an `insert` whose body is shallow but whose single kind-1 `documents`
      document is nested 1025 levels (counted from that document as level
      1) gets `malformed_message`;
    - the same document nested exactly 1024 levels is forwarded.
  - **Duplicate command name:** `{find: "a", filter: {}, find: "b", $db: …}`
    gets `malformed_message`, and the twin receives nothing.
  - **Kind-1 named like the command:** a `find` command with a kind-1 section
    whose identifier is `find` is not refused by corral. It is forwarded to
    the twin, and the twin's reply is returned to the client unchanged.

Added by Amendment 3:

- **AC-56** (FR-18, FR-37; Owner decision 2026-10-25): Unit (domain, fake
  clock) and twin, replica-set mode with three members A (primary), B and C:
  - **Ghost:** B restarts and answers `hello` with `isreplicaset: true` and no
    `setName` for several checks.
    - B stays in the known set and is re-checked on the normal schedule,
      including the 500 ms fast interval while no usable target is known.
    - The upstream does not become incompatible, and no `upstream
      incompatible` record is emitted.
    - When B reports the matching `setName` again, it is matching. If it then
      reports primary, it becomes the target.
  - **Standalone after a match:** C, after A has been found as a matching
    member, answers as a standalone (no `setName`, no `isreplicaset`).
    - C stays known and is re-checked on the normal schedule.
    - It is never the target and adds no discovery.
    - The upstream stays compatible, and no `upstream incompatible` record is
      emitted.
    - The same holds when C answers as a `mongos`.
  - **Unreachable:** C stops answering. It stays in the known set, is
    re-checked on the normal schedule, and is used again as soon as a check
    succeeds with the matching `setName`.
  - **Foreign:** B reports a different `setName`.
    - It is never selected as a target, and its `hosts` are not used for
      discovery.
    - It is re-checked every `upstream.heartbeat_interval`, even while the
      fast interval applies to the others.
    - When it later reports the matching `setName`, it is re-admitted and
      can become the target.
    - Because a matching member had already been found, the upstream stays
      compatible throughout.
  - **All members lost after startup:** A, B and C all become unreachable or
    foreign after a matching member had been found. The result is transient
    unavailability: requests wait, readiness is `upstream_unavailable`, and
    there is no `upstream_incompatible`.
  - **Seed rules at startup unchanged:** with no matching member ever found,
    a ghost or unreachable seed causes no incompatibility, while a foreign,
    standalone or `mongos` seed causes it, as in AC-18.
- **AC-57** (FR-22, FR-35, FR-37; Amendment 3 clarifications): Twin:
  - **Retry only for network, TLS and timeout errors:**
    - a target check that times out, and separately one that fails the TLS
      handshake, is retried once before the target is marked unknown;
    - a target check that returns a `hello` error reply, and separately one
      reporting that the target is no longer primary, is applied at once,
      with no retry. The twin records exactly one check in each case.
  - **`writeConcernError` only in `ok: 1`:** an `ok: 0` reply with top-level
    code 11000 that also carries `writeConcernError.code` 91 does not mark the
    host. An `ok: 1` reply with `writeConcernError.code` 91 does (AC-33).
  - **Authentication on target change:**
    - when the twin makes member B the new primary, the next check of B runs
      at once (at least 500 ms after its previous check) and authenticates on
      B's monitoring connection;
    - the twin records the authentication conversation on that check;
    - before B became the target, its checks carried no authentication.
- **AC-58** (FR-33, FR-9, FR-61; Owner decision 2026-10-25): Twin
  (raw-socket client).
  - `killSessions: []`, `killSessions: 1` and `killSessions: {}` (OP_MSG)
    each get code 13 (`Unauthorized`) with the fixed blocked-command message
    and detail `command_blocked`. The twin receives nothing,
    `corral_commands_blocked_total` increases, and a `client command
    blocked` record is emitted.
  - `killSessions: [{id: <UUID>}]` is forwarded unchanged, and the twin's
    reply is returned unchanged.
  - Over OP_QUERY, `{killSessions: []}` gets the 352 reply (FR-8, R-1).
  - With `moreToCome`, `killSessions: []` gets no reply, is never forwarded,
    and increments `corral_requests_dropped_total` (FR-59).
  - A `killSessions` whose argument starts after L gets `malformed_message`
    (FR-38).
  - The parser exposes only whether the argument is empty or not an array,
    never the session IDs (AC-7).

Added by Amendment 4:

- **AC-59** (FR-1, FR-49): Unit (startup path, with an injected listener
  that fails) and functional (AC-1).
  - When binding the wire listener fails with "address already in use", and
    separately with "permission denied", exactly one record is emitted with:
    - level error, `component=config`, message `wire listener bind failed`;
    - `setting=proxy.listen`, `error=config`, and a `corral.error.detail`
      from the SPEC-001 FR-29 enum (`EADDRINUSE` for the first case,
      `other` for the second).
  - The process exits 1 and emits no `invalid configuration` record.
  - The record contains no host, address or port (marker check, as in
    AC-1).

Added by Amendment 5:

- **AC-60** (FR-66, FR-47, FR-63, FR-49): Unit (accept loop and shutdown
  orchestration, with an injected listener, a fake clock and an injected
  exit-status sink; plus the twin where it says so):
  - **Transient errors:**
    - the listener returns `EMFILE` three times, then a connection. corral
      retries after exactly 5 ms, 10 ms and 20 ms (no jitter), accepts the
      connection, keeps serving, and emits no `wire listener serve failed`
      record and no other record. No metric changes;
    - the next transient error after that success is retried after 5 ms
      again;
    - a long run of transient errors reaches a 1 s delay and stays there;
    - a timeout, a temporary error, `ENFILE`, `ENOBUFS`, `ENOMEM`,
      `ECONNABORTED` and `EINTR` are each retried the same way.
  - **Listener closed by shutdown:** when the shutdown sequence closes the
    listener (FR-47 step 2), and separately on a second signal (FR-63), no
    `wire listener serve failed` record is emitted and the exit status is 0.
  - **Permanent error:** the listener returns an error not in the transient
    list (for example `EBADF` or `EINVAL`). Then:
    - exactly one record is emitted, with level error,
      `component=lifecycle`, message `wire listener serve failed`, and
      `error` and `corral.error.detail` values from the SPEC-001 FR-29
      enums;
    - the record has no attribute other than those, which the AC-27
      allowed-keys check covers;
    - with the listen address's port used as a marker, the marker appears
      in no output (marker harness);
    - no further accept is attempted;
    - **no delay:** with `proxy.shutdown.listener_close_delay=60s`, the
      shutdown sequence starts at once. The FR-47 step 1 delay is not
      waited, and `/readyz` reports `shutting_down`;
    - **drain:** an exchange in progress at the time (held by the twin)
      completes normally and its connection is then closed; an idle client
      connection is closed at once;
    - pool and monitoring connections are closed, and the tracing flush
      runs;
    - the run ends with exit status 1, well before the 60 s delay would
      have ended (within the drain and the SPEC-001 FR-34 flush bound, plus
      a tolerance).
  - **Second signal during that shutdown:** with an exchange held in the
    drain after a permanent accept error, a SIGTERM ends the drain at once.
    Exactly one `shutdown forced by second signal` record and exactly one
    `wire listener serve failed` record are emitted, and the exit status is
    1, not 0.
- **AC-61** (FR-18, FR-19, FR-46, FR-49, FR-56; Amendment 5 clarification):
  Unit (domain, fake clock) and twin, replica-set mode, seeds A then B in
  `upstream.hosts` order, upstream never reached:
  - **Foreign seed answers first, matching seed later:**
    - A answers at once with a different `setName`;
    - B answers with the matching `setName` after a delay shorter than
      `upstream.connect_timeout`;
    - until B answers, readiness is `upstream_unavailable`, and a request
      and a handshake each get code 6 with detail `upstream_unavailable`;
    - no `upstream incompatible` record is emitted at any point.
  - **The reason does not depend on the order checks finish:**
    - B answers at once with a different `setName`, and A answers later as
      a standalone;
    - readiness is `upstream_unavailable` until A answers;
    - then exactly one `upstream incompatible` record is emitted, with
      `reason=standalone`, and requests and handshakes get detail
      `upstream_incompatible`;
    - in a second run where A answers first and B later, the outcome is the
      same: one record with `reason=standalone`, emitted after both checks
      have finished.
  - **Unreachable seed:**
    - A answers at once with a different `setName`, and B never answers;
    - readiness stays `upstream_unavailable` until B's first check fails at
      `upstream.connect_timeout`, with no retry;
    - then the upstream is incompatible, with one record and
      `reason=replica_set_name_mismatch`.
  - **Ghost seed:** with A foreign and B a ghost, the upstream becomes
    incompatible with `reason=replica_set_name_mismatch` once both first
    checks have finished. The ghost seed itself never causes incompatibility
    (FR-18).
  - **Concurrent first checks (FR-37, Owner decision 2026-10-25):**
    - seeds A, B and C never answer, and the twin records when each first
      check connection attempt arrives;
    - all three arrive before any of them reaches `upstream.connect_timeout`;
    - in the unit variant (fake clock), all three first checks are started
      at startup, before any of them completes.

Added by Amendment 6:

- **AC-62** (FR-35, FR-9, FR-29, FR-49; Owner decision 2026-10-25): Unit
  (domain) and twin, replica-set mode. Each run starts with a fresh rate
  limiter.
  - **Table:** the unit test checks that the fixed table maps exactly these
    codes: 10107 `NotWritablePrimary`, 13435 `NotPrimaryNoSecondaryOk`, 13436
    `NotPrimaryOrSecondary`, 11602 `InterruptedDueToReplStateChange`, 11600
    `InterruptedAtShutdown`, 189 `PrimarySteppedDown` and 91
    `ShutdownInProgress`.
  - **`ok: 1` form:**
    - the twin sends an `ok: 1` reply whose `writeConcernError` has
      `code: 91` and a different `codeName` (a marker string);
    - the `upstream target marked not primary` record has
      `db.response.status_code="91"` and
      `corral.db.response.code_name="ShutdownInProgress"`;
    - the marker string appears in no output (marker harness);
    - the same holds with no `codeName` in `writeConcernError`;
    - the reply reaches the client byte-identical (FR-10).
  - **Reads nothing new:** the reply parser exposes `writeConcernError.code`
    but not `writeConcernError.codeName` (AC-7).
  - **`ok: 0` form and barrier `ping` reply** (Owner decision 2026-10-25):
    - an `ok: 0` reply with `code: 10107` and the legacy `codeName:
      "NotMaster"` gives `corral.db.response.code_name="NotWritablePrimary"`;
    - a barrier `ping` reply with `code: 91` and a different `codeName`
      gives `corral.db.response.code_name="ShutdownInProgress"`;
    - the forwarded `ok: 0` reply reaches the client unchanged.

## Open questions

Must be empty before approval.

None for Amendment 7. It declares behaviour the implementation already has,
for the security reason recorded in the Decisions log, so it involves no new
product decision. The Owner approves the text.

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
| 2026-10-24 | Context, Owner plan decisions recorded for reference (no spec text depends on them): SCRAM-SHA-256 for upstream authentication is implemented with the `github.com/xdg-go/scram` library; the new dependencies in the SPEC-002 plan are approved; access for Docker (functional tests) and PECL (PHP `ext-mongodb` in the PHP-FPM fixture) is granted. | Owner decisions on the SPEC-002 plan, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | **Amendment 1**, gap fixes G-1 to G-20 (except G-16, a SPEC-003 matter) found while deriving acceptance tests. These are spec-writer resolutions; items OPEN-A1 to OPEN-A5 need Owner confirmation, and the rest the Owner may override when approving the text: <br>**G-1:** per-opcode minimum `messageLength` (OP_MSG 26, OP_QUERY 34, others 16); lengths are signed, and negative or too short is `malformed_message` (FR-5). <br>**G-2:** a fixed order of framing checks decides the detail; checksum is reported last (FR-5). <br>**G-3:** OP_QUERY structural validation (`malformed_message`); a 352 refusal is not a protocol-error count but is a `proxy_error` request with class `protocol` (FR-8, Metrics). <br>**G-4:** `corral_request_errors_total` is one per `proxy_error` request, by class; `accepted_total` includes over-limit and TLS-failed connections; `connect_errors_total` includes auth failures, with `auth_failures_total` as the subset (planner defaults adopted). <br>**G-5:** handshake, auth and blocked command names match case-sensitively; other spellings are ordinary commands (FR-13). <br>**G-6/G-7:** FR-61 order of handling; a refused first handshake still counts as first (planner default P9); within a handshake: 72, then 186, then 6; local refusals come before fail-fast. <br>**G-8:** "reached" means a successful compatible `hello` check, whatever the auth outcome; with auth failing since startup, handshakes succeed and requests get 6 / `upstream_auth_failed` (FR-19). <br>**G-9:** readiness follows the monitoring view; pool connection failures act only through immediate checks, and pool auth failures still count (FR-46). <br>**G-10:** L = K = 65,536 bytes (L includes the header), normative; operator document `docs/operations/proxy.md` (FR-38, FR-58, FR-65). <br>**G-11:** hooks only in the `corraltest` build, enabled by `CORRALTEST_*` variables per ADR-0014 and ADR-0022 (FR-64). <br>**G-12:** a not-primary code in the barrier ping reply marks the host; a client disconnect during a barrier is not "in flight" (FR-29, FR-41). <br>**G-13:** single-`mongos`: only top-level 91 / 11600 mark the target (FR-35). <br>**G-14:** residual exposure list extended (session and cursor IDs under the shared identity); bypasses of the FR-33 block (kind-1 `pipeline`, duplicated inspected fields) are refused (FR-60). <br>**G-15:** the connection limit is checked before the TLS handshake; the TLS handshake is bounded by `proxy.client_io_timeout` (planner P8) (FR-2, FR-44, FR-57). <br>**G-17:** unsolicited bytes or a close on an idle pool connection: connection discarded, request moved to another connection (FR-62). <br>**G-18:** a second signal ends the drain at once and exits 0, with one `lifecycle` warn record (FR-63). <br>**G-19:** full recursive structural BSON validation of the body and kind-1 documents (planner P3); no UTF-8 check on values; nesting limit 1024 levels, with the top-level document as level 1 and arrays counting like documents. This is above any server's `maxBSONDepth` (default 200, configurable 21–999), so corral never refuses what a server would accept (FR-60; changed from 256 by the challenge review, M1). <br>**G-20:** codes inside `writeErrors[]` do not mark the host (FR-9, FR-35). <br>No SPEC-001 change is needed. ACs AC-37 to AC-48 added; AC-19, AC-28 and AC-35 updated. | Gaps raised while deriving acceptance tests from approved revision 5, relayed by the Orchestrator, 2026-10-24. Approved and promoted by the Owner, 2026-10-24 (see the approval row). |
| 2026-10-24 | Amendment 1, challenge-review fixes. These are spec-writer resolutions, not new product decisions; OPEN-A1 to OPEN-A5 are unchanged: <br>**M1:** the FR-60 nesting limit is 1024 (top-level document = level 1, arrays count like documents), above any server `maxBSONDepth` setting; AC-44 (1025 refused, 1024 forwarded, about 8 KiB within L), the resource bounds and the G-19 entry are updated. <br>**S1:** the general "kind-1 identifier duplicates a body key" check is dropped (unbounded memory, and the server rejects it anyway). Only identifiers equal to an inspected field's name are refused, and FR-9 now reads only inspected names. <br>**S2:** FR-60 says nothing is forwarded when detected before a lease (always the case within L); otherwise FR-38 applies. AC-44 states that its messages are within L. <br>**S3:** FR-38 requires validation before writing upstream, and holds back the final byte(s), including any checksum, until every check has passed, so a failing message never reaches the upstream complete; AC-49 added. <br>**S4:** a client protocol error or I/O timeout that closes the connection after a message's first byte counts as one `proxy_error` request, labelled with its command if known, else `other`, and once in `corral_request_errors_total`. <br>**S5:** AC-42 adds a barrier-ping 91 case in replica-set mode. | Challenge review REQUEST_CHANGES on Amendment 1, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Amendment 1 approved by the Owner and promoted, with OPEN-A1 = close the connection (malformed_message) for duplicated inspected fields or an inspected field sent as a kind-1 section, and OPEN-A2..A5 as proposed (single-mongos marks only on top-level 91/11600; second signal exits 0; handshake order 72→186→6 and a refused first handshake counts; client disconnect does not abort a barrier). | Owner approval, 2026-10-24. |
| 2026-10-24 | **Amendment 2.** <br>**A-Q1 (Owner decision "skip"):** FR-60 does not compare kind-1 section identifiers with the command name. That comparison would need unbounded memory and closes no bypass, because the server takes the command from the body's first key. A duplicated command name inside the body is still refused (FR-60, Terms, AC-55). <br>Recommended defaults, for Owner approval with the text: <br>**R-1:** an OP_QUERY whose command is an auth command or a blocked command gets the FR-8 352 OP_REPLY (`QueryFailure`), not 18, 115 or 13. OP_QUERY carries only the handshake, so FR-61 steps 3 and 4 apply to OP_MSG only. These refusals are not counted in the auth-rejected or blocked counters (FR-8, FR-23, FR-33, FR-61, AC-50). <br>**R-2:** the `corraltest` hook variables (`CORRALTEST_HOLD_DIR`, `CORRALTEST_HOLD_AT` with its points, `CORRALTEST_POOL_ORDER`, `CORRALTEST_PANIC_COMMAND`, `CORRALTEST_PROBE_LISTEN`) and the probe endpoints `/conns` and `/constants` are now normative in FR-64, taken from ADR-0014, ADR-0022 and `plans/SPEC-002.md`; JSON field names inside `/conns` stay with ADR-0022 (AC-51). <br>**R-3:** a client that closes mid-message after the first byte produces one `proxy_error` request (command label if read, else `other`), class `network` with the classified detail, close reason `client_closed`, and no protocol-error count. A partly streamed upstream connection is discarded with `client_abandoned` and never receives the complete message (FR-41, Metrics, AC-52). <br>**R-4:** a second signal during `listener_close_delay` skips the rest of the delay and the drain: listener closed at once, all client connections closed, step 5, one record, exit 0 (FR-63, AC-53). <br>**R-5:** a not-primary code in the barrier ping reply emits the `upstream target marked not primary` record (with that reply's code) as well as `upstream barrier failed` (FR-29, log table, AC-54). <br>**R-6:** each kind-1 document (and each OP_QUERY document) counts nesting from level 1 on its own; a duplicated first key such as `{find: "a", find: "b"}` is a duplicated inspected field (FR-60, AC-55). <br>**Also:** the stale FR-52 sentence pointing at the SPEC-001 Amendment 3 draft is replaced, since Amendment 3 is approved and promoted. No SPEC-001 change is needed. AC-50 to AC-55 added. | Owner decision on A-Q1 and Orchestrator request for R-1 to R-6, 2026-10-24. Promotion awaits Owner approval of the text. |
| 2026-10-24 | Amendment 2, challenge-review fixes (spec-writer resolutions, no new product decision): <br>**M1:** AC-53 is now a twin test on the `corraltest` build. It holds at `shutdown_pool_close` to assert that new connections are refused and the held exchange was closed, then releases the hold and checks exit 0 and the single record. <br>**S1:** a `$query`-wrapped OP_QUERY takes its `command` label from the first key inside `$query`, the same key used for handshake detection and R-1 (FR-8 G-3 note, AC-50). <br>**S2:** every counted request is observed once in `corral_request_duration_seconds`; for requests ending in a close (including a mid-message client close), the duration ends when the close is detected or performed (Metrics definitions, FR-41, AC-52). <br>**S3:** AC-54 adds a ping-reply 91 case. <br>**S4:** AC-55 says the twin's reply is returned unchanged. | Challenge review of Amendment 2, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Amendment 2 (incl. challenge-review fixes M1, S1–S4) approved by the Owner and promoted; R-1 to R-6 resolutions confirmed. | Owner approval, 2026-10-24. |
| 2026-10-25 | **Amendment 3.** <br>**Owner decision, member retention:** non-matching servers are no longer dropped permanently. Only a member reporting a *different* `setName` (foreign) is ignored. It is re-checked every `upstream.heartbeat_interval` and re-admitted when it reports the matching `setName` again. A member reporting no `setName` (ghost or startup state during a rolling restart, recognised by `isreplicaset`) or failing its check (unreachable) stays in the known set and is re-checked on the normal schedule. FR-18's replica-set incompatibility from seed results is kept consistent: it applies only while no matching member has been found since startup; ghost and unreachable seeds never cause it; after a matching member has been found, losing all of them is transient unavailability. This replaces the revision 4/5 text "not monitored further" and "non-matching seeds stop being checked" (Terms, FR-9, FR-18, FR-37, NFR, FR-51, AC-18, AC-56). <br>**Owner decision, `killSessions`:** `killSessions` with an empty array, or an argument that is not a non-empty array, is blocked like `killAllSessions`: code 13 `Unauthorized`, the fixed message, detail `command_blocked`. Under the shared identity it would kill every client's sessions. `killSessions` with explicit session IDs is still forwarded and is listed as residual exposure. FR-9 reads only whether the argument is empty or not an array. FR-38's deciding-field rule covers the argument (FR-9, FR-33, FR-38, FR-51, FR-61, Synthesized error replies, Behavior on failure, Data and compliance, AC-58). The residual-exposure list now states that every remaining route is mitigated by least privilege, and names `$listSessions` / `$listLocalSessions` / `listSessions`, explicit-`lsid` `killSessions`, and `explain`-wrapped or `$unionWith` / `$lookup`-nested `$currentOp`. <br>**Spec-writer clarifications of existing text (wave 2 code review), for Owner approval with the text:** <br>(a) the FR-35 single retry applies only to check failures of class `network` or `timeout` (including TLS); other unsuccessful outcomes are applied at once; <br>(b) `writeConcernError.code` marks a host only in an `ok: 1` reply, and in an `ok: 0` reply only the top-level `code` counts; <br>(c) when a server becomes the target, corral runs an immediate check of it (still at least 500 ms after the previous one) that authenticates on its monitoring connection (FR-22, FR-37). <br>AC-57 covers these. No SPEC-001 change is needed. AC-56 to AC-58 added; AC-18 adjusted. <br>**Challenge-review fixes (APPROVE with should-fixes):** <br>- S1: a ghost is defined exactly as `isreplicaset: true` without `setName`; a standalone-looking or `mongos` member after a match is "other non-matching", kept and re-checked but never the target and with no discovery; AC-56 gains the standalone-after-match case. <br>- S2: the "compatible → incompatible" example is now a single-`mongos` host re-pointed at a replica set. <br>- Nit: a foreign server is kept while it is a seed or still listed by a matching member. | Owner decisions of 2026-10-25 (member retention; `killSessions` block) and Orchestrator request from the wave 2 code review. Promotion awaits Owner approval of the text. |
| 2026-10-25 | Amendment 3 (incl. challenge-review fixes S1, S2) approved by the Owner and promoted; clarifications confirmed. | Owner approval, 2026-10-25. |
| 2026-10-25 | **Amendment 4.** A gap found in code review: FR-1 required a record when the wire listener cannot bind, but did not name it. The record is now declared as the implementation emits it: level error, `component=config`, the fixed message `wire listener bind failed`, `setting=proxy.listen`, `error` (class `config`) and `corral.error.detail` from the bind error (for example `EADDRINUSE`), and no address, host or port; exit 1. It mirrors SPEC-003 Amendment 3's `admin listener bind failed`, so that `invalid configuration` alone still identifies load-time config errors (exit 2). FR-1, the log events table (new `config` row), Behavior on failure, AC-1 (adjusted) and new AC-59 are updated. No SPEC-001 change is needed: the record uses an existing component and existing keys. <br>**Owner decision 2026-10-25 ("document only"), session-enumeration exposure:** <br>- The Amendment 3 residual-exposure text was wrong to say least privilege mitigates `$listSessions` / `$listLocalSessions`. For the caller's own user these stages need no special privilege. <br>- Under the shared upstream identity, any client can therefore list every client's session IDs, then end those sessions through `killSessions` with explicit IDs, which FR-33 forwards. `endSessions` and `abortTransaction` / `commitTransaction` with another client's `lsid` work the same way. <br>- Least privilege does not prevent this. corral does not block it, by Owner decision. FR-51 operator documentation must state it. <br>- Changes: Data and compliance (the first residual list now has only the `allUsers` / other-users forms; a new session enumeration and termination route; the explicit-`lsid` bullet no longer says a leaked ID is needed; the closing mitigation paragraph), NFR Security "Cross-client effects", and a new FR-51 bullet. FR-33 and blocking behaviour are unchanged, and no AC changes, because the change is documentation only. | Gap found in code review, relayed by the Orchestrator, 2026-10-25; Owner decision "document only" on the session-enumeration exposure, relayed by the Orchestrator, 2026-10-25. Promotion awaits Owner approval of the text. |
| 2026-10-25 | Amendment 4 approved by the Owner and promoted (bind-failure record; session enumeration/termination residual exposure documented by Owner decision, flagged for security@emag.ro). | Owner approval, 2026-10-25. |
| 2026-10-25 | **Amendment 5.** Two points from code review. <br>**(1) Wire listener accept errors (new FR-66):** <br>- Transient accept errors (timeouts, temporary errors, `EMFILE`, `ENFILE`, `ENOBUFS`, `ENOMEM`, `ECONNABORTED`, `EINTR`) are retried after 5 ms, doubling to a 1 s cap, and the delay resets after a successful accept. <br>- A listener closed by shutdown is not an error. <br>- Any other accept error is permanent. It produces one record (level error, `component=lifecycle`, fixed message `wire listener serve failed`, `error` and `corral.error.detail` only, no address, host or port), then shutdown and exit 1. This declares a record that was emitted but not declared, in the same way as Amendment 4. <br>- Also updated: FR-1 cross-reference, FR-47 note, log events table (new `lifecycle` row), Behavior on failure (two rows), NFR Availability, and new AC-60. <br>**(2) FR-18 clarification:** <br>- In replica-set mode, incompatibility from seed results is judged only once every seed has finished its first check. An unreachable seed counts once its check fails, with no FR-35 retry. The reason comes from the first qualifying seed in `upstream.hosts` order. <br>- Until then, readiness and fail-fast report `upstream_unavailable`, for at most about one `upstream.connect_timeout`. <br>- Rationale: the reason no longer depends on the order checks finish, and there is no spurious `upstream incompatible` record when a foreign seed answers first. <br>- Also updated: log events table note on `upstream incompatible`, Behavior on failure (one row), AC-18 (note), and new AC-61. <br>No SPEC-001 change is needed. <br>**Owner decisions 2026-10-25 on the open questions:** <br>- **OPEN-B1 = (a):** after a permanent accept error, corral runs the FR-47 graceful sequence without the listener-close delay (`/readyz` `shutting_down`, drain within `proxy.shutdown.drain_timeout`, pools closed, tracing flushed), then exits 1. A second signal during it ends the drain per FR-63 but still exits 1 (FR-47 note, FR-63, FR-66, Behavior on failure, AC-60). <br>- **OPEN-B2:** transient accept errors produce no record and no metric, and the backoff has no jitter (FR-66, SPEC-001 note, Behavior on failure, AC-60). <br>- **OPEN-B3:** every seed's first check starts concurrently at startup, so the FR-18 "not yet judged" period lasts at most about one `upstream.connect_timeout` (FR-18, FR-37, AC-61). | Gaps from code review, relayed by the Orchestrator, 2026-10-25; Owner decisions on OPEN-B1 to OPEN-B3, relayed by the Orchestrator, 2026-10-25. Promotion awaits Owner approval of the text. |
| 2026-10-25 | Amendment 5 (incl. Owner decisions on OPEN-B1 to OPEN-B3) approved by the Owner and promoted. This row was added in Amendment 6 to record an approval that the header already showed. | Owner approval, 2026-10-25, relayed by the Orchestrator. |
| 2026-10-25 | **Amendment 6.** Resolves a conflict between FR-9 and FR-49: the `upstream target marked not primary` row said `corral.db.response.code_name` came "from the forwarded upstream reply", but for the `ok: 1` form with `writeConcernError`, FR-9 reads only `writeConcernError.code`, not its `codeName`. <br>**Owner decision 2026-10-25:** the code name is taken from corral's fixed table of not-primary codes in FR-35 (10107 `NotWritablePrimary`, 13435 `NotPrimaryNoSecondaryOk`, 13436 `NotPrimaryOrSecondary`, 11602 `InterruptedDueToReplStateChange`, 11600 `InterruptedAtShutdown`, 189 `PrimarySteppedDown`, 91 `ShutdownInProgress`). These names match the spec's existing FR-35 list exactly. FR-9 reads nothing new, and now states that `writeConcernError.codeName` is not read. `db.response.status_code` is the code that caused the marking. <br>**OPEN-C1, Owner decision 2026-10-25:** the table applies to every form: the `ok: 0` reply, the `ok: 1` reply with `writeConcernError`, and the barrier `ping` reply. This was the spec-writer's recommendation, for consistency. <br>Changes: FR-9 (one sentence), FR-29 R-5 note, FR-35 (new "Code name in the record" bullet), log events table (`upstream target marked not primary` row), SPEC-001 note, Behavior on failure (not-primary row), AC-33 (pointer), and new AC-62. No SPEC-001 change is needed. | Conflict found in code review, relayed by the Orchestrator, 2026-10-25; Owner decisions of 2026-10-25 (code-name source; OPEN-C1) relayed by the Orchestrator. Promotion awaits Owner approval of the text. |
| 2026-10-25 | Amendment 6 (incl. Owner decision on OPEN-C1) approved by the Owner and promoted. | Owner approval, 2026-10-25, relayed by the Orchestrator. |
| 2026-10-25 | **Amendment 7.** A gap from code review. The implementation rate-limits the `request handler panic recovered` record with the same per-event limiter as the other rate-limited records, but the log events table did not mark that row rate-limited, and FR-49 limits only marked events. <br>**Reason (security review RC5):** a client that can trigger a recovered panic could otherwise flood the logs. <br>**Change:** <br>- the row is marked rate-limited: at most one record per 30 s, with `suppressed_count`, per FR-49 and SPEC-001 FR-33; <br>- FR-42 states that only the record is limited: every panic still closes its connection, and every close is still counted (`corral_client_connections_closed_total{reason="internal_error"}`); no metric is limited; <br>- also updated: Behavior on failure (panic row), the SPEC-001 note, AC-22 (rate-limit case) and AC-27 (names the record). <br>No SPEC-001 change is needed: SPEC-001 FR-29 still governs the record's content. | Gap from code review, security review RC5, relayed by the Orchestrator, 2026-10-25. Text approved by the Owner, 2026-10-25, relayed by the Orchestrator. |
