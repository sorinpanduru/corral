# twins/faultproxy

TCP intermediary in front of a real mongod (or `twins/mongosim`). Source
contract: `plans/SPEC-002.md`, "Twins / TW2"; ADR-0022. Frames are relayed
unchanged unless a fault or hello rewrite applies. Own OP_MSG/OP_QUERY/OP_REPLY
parser; BSON from the official driver. Run: `go test -race ./twins/faultproxy`
(no Docker; self-tests use `twins/mongosim`).

## API summary

- `Start(tb, upstream, Options{RewriteHello, RewriteHelloSelf, DropCompression})`;
  `Addr()`, `Upstream()`, `SetRewrite(map)`, `Stop()`.
- Rewrite: in hello / isMaster replies `hosts`, `passives`, `arbiters`,
  `primary`, `me` values found in the map are replaced (empty value = proxy
  address; `RewriteHelloSelf` maps the upstream to the proxy). The rewritten
  frame gets a correct `messageLength` and no checksum.
- Recording: `Commands()` (both directions), `Requests()`, `RequestsBy(name)`,
  `WaitCommands`, `ResetRecording`, `Conns()`, `ConnCount()`, `OpenConns()`.
  `Record` has connection index, handshake appName, direction, command, db,
  `LsidID`, `HasTxnNumber`/`TxnNumber`, `StartTxn`, request/response ids,
  flags, `OK` (replies), time. A reply's `Command` is that of its request.
- Faults: `AddFault(Match{Command, AppName, OnlyConn, ConnIndex, Nth, Once}, f)`
  returns `*FaultHandle` (`Hits`, `WaitHits`, `Remove`); faults
  `ResetAfterRequest()`, `Delay(d)` (request), `DelayReply(d)`. Global:
  `PauseAll`/`ResumeAll`, `KillAll`, `Kill(i)`, `Blackhole(i)`,
  `BlackholeNew(bool)`, `ClearFaults`.

## Known differences from a bare TCP path

- One upstream connection per accepted connection, opened at accept time.
- `OP_COMPRESSED` is relayed but not parsed (no record, no rewrite); use
  `DropCompression` so clients never negotiate it.
- `ResetAfterRequest` closes the client side with a TCP reset and the upstream
  side with a reset right after writing the request; the upstream may or may
  not have executed it.
- Fire-and-forget (`moreToCome`) requests are recorded like any other request.
- `Stop` is safe against a racing Accept. `moreToCome` requests leave no
  pending-reply state.
