# twins/mongosim

Simulated MongoDB upstream for tests. Source contract: `plans/SPEC-002.md`,
"Twins / TW1"; ADR-0022. Runs in the test process on `127.0.0.1:0`; encodes
BSON with `go.mongodb.org/mongo-driver/v2/bson` and its own framing. Synthetic
users and passwords only. Run: `go test -race ./twins/mongosim`.

## API summary

- `Start(tb, Options{Addr, Hello, Users, RequireAuth}) *Server`; `Addr`, `Stop`,
  `Restart` (same port), `RefuseNew(bool)`, `Blackhole(bool)`, `CloseConns()`.
- Topology: `SetHello(HelloSpec)`, `Hello()`. Roles `Standalone` (zero value),
  `Primary`, `Secondary`, `Arbiter`, `Mongos`, `Other`. Answers `hello`,
  `isMaster`, `ismaster` over OP_MSG and OP_QUERY (OP_REPLY). Each `SetHello`
  bumps `topologyVersion.counter`; awaitable hello (`topologyVersion` +
  `maxAwaitTimeMS`) blocks until a change, the timeout, or connection close.
- `StartReplicaSet(tb, n, ClusterOptions) *Cluster`: `Member(i)`, `Arbiter(i)`,
  `Addrs()`, `URI()`, `SetPrimary(i)` (bumps electionId), `NoPrimary()`,
  `StepDown(i)`, `BumpSetVersion()`, `PrimaryIndex()`.
- Monitoring failures: `FailChecks(n, mode)` (appName `corral-monitor`),
  `FailChecksAll`, `FailChecksOn(appName, n, mode)`, `ClearChecks`,
  `ReleaseChecks`. Modes `CheckReset`, `CheckNoReply`, `CheckDelay(d)`,
  `CheckError(code, name)`, `CheckMalformed`. `n < 0` means persistent.
- SCRAM-SHA-256: genuine server (wrong password gives code 18).
  `SetAuthOutcome`, `QueueAuthOutcomes`; outcomes `Succeed`, `Fail18`,
  `Fail334`, `BadServerSignature`, `ErrorCode(code, name)`, `AuthReset`,
  `AuthDelay(d)`; `.AtContinue()` moves a failure to the first `saslContinue`.
  Supports `skipEmptyExchange`.
- Commands: `ping` gives `{ok:1}`; anything else `{ok:1, n:1, corraltestEcho}`.
  `find`/`aggregate` also carry an empty cursor; `corraltestReplyBytes: N`
  pads the first batch (documents of at most 1 MiB, deterministic pattern).
- Rules: `AddRule(Match{Command, Marker, AppName, DB, Nth, Once}, Action)`
  returns `*RuleHandle` (`Release`, `Remove`, `Hits`, `Held`, `WaitHeld`,
  `WaitHits`). Actions: `Hold`, `Delay`, `ResetAfterReceive`, `ReplyError`,
  `ReplyErrorMessage`, `ReplyWriteConcernError`, `ReplyUnknownResponseTo`,
  `ReplyMoreToCome`, `ReplyOversize`, `ReplyChecksum(valid)`, `ReplyRaw`,
  `ReplyMalformed`, `NoReply`. First matching rule wins; every matching rule
  still counts the request towards its `Nth`.
- Recording: `Received()`, `ReceivedCommand(name)`, `WaitReceived`,
  `ResetRecording`, `ConnAttempts()`, `ConnAttemptsBy(appName)`,
  `OpenConns(appName)`, `Conns()`.

## Amendment 1 additions (FR-62, AC-43, AC-46)

- `SendRaw(connIndex, b)`: unsolicited bytes on a connection (AC-46).
  `CloseConn(connIndex, reset)`: close one connection (FIN or RST).
- Idle detection: `ConnInfo.InFlight` is true from receipt of a request until
  it is fully handled (held, delayed, being answered), and also while a partial
  frame or a queued request exists; `IdleConns(appName)`
  lists open, non-in-flight connections with that handshake appName (use
  `"corral"` for idle pool connections).
- Partial messages: `ConnInfo.PartialBytes` (bytes of the incomplete frame
  received so far, 0 when none; kept after a close in the middle of a frame, and
  no message is recorded for it) and `ConnInfo.BytesReceived` (all bytes).
- Refusal with attribution: `RefuseApp(appName, bool)` reads that appName's
  hello, records it (so `ConnAttemptsBy(appName)` counts it), then resets the
  connection without a reply, while other appNames (the monitor) are answered
  normally. `RefuseAfterHello(bool)` does the same for every appName.
- `ReplyWriteErrors(code)`: `{ok:1, n:0, writeErrors:[{index:0, code, errmsg}]}`.
- Error replies to `ping` (barrier) use `ReplyError(code, name)` with
  `Match{Command: "ping"}`, for example 10107 or 91.
- `hello` replies carry `isWritablePrimary` only; legacy `isMaster` replies
  carry `ismaster` only.
- A non-handshake OP_QUERY gets `{ok:0, code:352, codeName:"UnsupportedOpQueryCommand"}`.
- `BadServerSignature` does not mark the connection authenticated; it sets
  `ConnInfo.SignatureForged`. `Fail18` carries errmsg "Authentication failed.".
- `Stop` is safe against a racing Accept (a late connection is closed, never
  registered); `Cluster` mutators publish atomically under a cluster lock.

## Upstream TLS (optional; proposed change, needs Owner approval)

Source contract: SPEC-002 FR-45, AC-24. Off by default; with `Options.TLS`
unset behaviour is unchanged.

- `Options{TLS: m, Authority: a}`; modes `TLSOff` (zero), `TLSValid`,
  `TLSExpired` (CA-signed, expired 24 h ago), `TLSHostnameMismatch` (valid,
  only SAN `mismatch.corral-test.invalid`), `TLSLegacyVersion` (valid cert,
  TLS 1.0 to 1.1 only). `TLSValid` has SANs 127.0.0.1, ::1, localhost.
- `NewAuthority(tb)` makes a synthetic CA ("corral-test-ca"); omit `Authority`
  to get a fresh one per server. `Server.Authority()`, `Server.CAPEM()`,
  `Server.CAFile()` (PEM in the test's TempDir, for `upstream.tls.ca_path`).
  A second `NewAuthority` gives a "wrong CA" for the client to trust.
- `Server.TLSHandshakeFailures()` counts failed server-side handshakes.
- Keys and certificates are generated in memory at test time (ECDSA P-256);
  only the public CA certificate is ever written, to a TempDir.
- Differences: the handshake runs in the connection's read goroutine, with a
  5 s limit; failed handshakes are not recorded in `Conns()` messages. Refuse
  and Blackhole skip the handshake. Legacy mode relies on Go's server honouring
  an explicit `MinVersion` below 1.2 (verified in the self-test).

### Live TLS switching, handshake record, per-connection stall (proposed, needs Owner approval)

- `SetTLS(mode)` changes the mode for connections accepted from then on;
  existing connections are unaffected. Includes `TLSOff` (back to plaintext)
  and plaintext to TLS. `TLSMode()` reads it. `Options.Authority` may be set
  with TLS off so the CA exists before the first switch; otherwise `SetTLS`
  creates one on first need (read it with `CAFile()` after).
- `TLSHandshakes()` returns `[]TLSHandshake{ConnIndex, Remote, Start, End, OK,
  Err, Version, Mode}` in completion order, failed attempts included (a
  plaintext client against a TLS listener is a failed attempt). Plaintext
  connections are not recorded. `WaitTLSHandshakes(timeout, pred)`,
  `ResetTLSHandshakes()`. `Start` is when the server began the handshake.
- `StallNextHello(connIndex, d)`: delays the reply to the next hello /
  isMaster on that one open connection, once. Existing equivalent:
  `FailChecksOn(appName, n, CheckDelay(d))`, which matches by appName, not by
  connection.

## Known differences from a real mongod

- Commands other than the ones above are echoed, never executed; there is no
  storage. `find` always returns an empty cursor (plus padding on request).
- `ConnAttemptsBy(appName)` counts a connection when its first hello arrives,
  so refused and black-holed connections appear only in `ConnAttempts()`.
- An empty `Match.Command` skips the handshake family (hello, isMaster,
  saslStart, saslContinue); name them to match them.
- Hold, Delay and the check faults block the connection (requests on one
  connection are served in order, as on mongod).
- SASLprep is not applied (use ASCII passwords). Salt and server nonce suffix
  are deterministic.
- OP_COMPRESSED requests and unparsable frames close the connection with a reset.
- `compression` is never advertised; `speculativeAuthenticate` is ignored.
- `localTime` uses the wall clock; everything else is deterministic.
