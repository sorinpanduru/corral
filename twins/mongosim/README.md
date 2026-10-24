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
  are deterministic. `BadServerSignature` still marks the connection
  authenticated server-side.
- OP_COMPRESSED requests and unparsable frames close the connection with a reset.
- `compression` is never advertised; `speculativeAuthenticate` is ignored.
- `localTime` uses the wall clock; everything else is deterministic.
