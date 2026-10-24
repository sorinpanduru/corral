# ADR-0017: Domain BSON and wire codec, streaming validation and header rewriting

- Status: Accepted (planner, 2026-10-24, for SPEC-002)
- Related: SPEC-002 FR-5 to FR-11, FR-30, FR-38, FR-58, AC-4, AC-6, AC-7,
  AC-8, AC-19, AC-35; SPEC-001 FR-1, FR-24, FR-40; ADR-0015

## Context

The wire message model is domain (CLAUDE.md, FR-53), and domain packages may
import only the standard library (SPEC-001 FR-1, empty allowlist). corral must
validate client OP_MSG bodies as BSON (FR-7), but must not hold more than the
request buffer limit before a lease (FR-38). It must stream replies with a
bounded chunk while still reading `ok`, `code`, `codeName`,
`writeConcernError.code` and `cursor.id` (FR-58), which may come after a
large `cursor` document. Forwarding must be byte-faithful except for the
header IDs, the checksum and `exhaustAllowed` (FR-10).

## Decision

### Hand-written BSON in `internal/domain/bson`

- No third-party BSON library in production code. The official driver's
  `bson` package is used only by tests and twins, so test encoders are
  independent of the code under test (ADR-0022).
- `Validate(doc)` checks a complete document; the `Walker` is a push parser
  (`Write(chunk)`, `Close()`) that checks the same rules incrementally, with
  an explicit stack instead of recursion, a depth cap of 1024 and constant
  memory per level. It optionally captures configured paths (for example
  `ok`, `code`, `codeName`, `writeConcernError.code`, `cursor.id`, `lsid.id`,
  `pipeline.0` first key). It captures only scalar values, and only up to
  fixed byte bounds (strings up to 256 bytes; longer strings are reported
  as "present, too long" and never kept).
- Rules: document length ≥ 5 and consistent with its content, `0x00`
  terminator, known element types (0x01 to 0x13, 0x7F, 0xFF, including the
  deprecated 0x06, 0x0C and 0x0E), keys are NUL-terminated cstrings, string
  lengths ≥ 1 with a trailing NUL, boolean bytes 0 or 1, nested
  document/array lengths within their parent, binary lengths ≥ 0, and
  code-with-scope consistency. String contents are not checked for UTF-8,
  because the server accepts what drivers send and corral must not reject
  it.
- Errors are constant sentinels with no input bytes.

### `internal/domain/wire`

- `MsgValidator` validates an OP_MSG incrementally: flags (bits 2–15 must be
  zero), section kinds, exactly one kind-0 section, section bounds, each
  kind-0 and kind-1 document through the `bson.Walker`, the trailing CRC-32C
  (`hash/crc32` Castagnoli) when `checksumPresent` is set, and the first
  element (command name). It is used for client requests (fed first with the
  buffered prefix and then with streamed chunks) and for upstream replies.
- `ClassifyPrefix` makes the FR-38 decision from the prefix alone: command
  name, `$db`, `lsid.id`, `txnNumber`, `autocommit`, `startTransaction`, the
  FR-40 collection, `getMore` cursor ID, and the first `pipeline` stage name
  for `aggregate`. A deciding field outside the prefix is
  `malformed_message`. A locally handled message must fit the prefix
  entirely, else `message_too_large`.
- OP_QUERY is parsed only to detect FR-8 handshakes (including the `$query`
  wrapper). Anything else gets the code 352 OP_REPLY.
- The FR-40 collection command list is a domain table (SPEC-001 FR-40 owns
  its contents; changing it requires amending SPEC-001).

### Header rewriting and checksums

- Requests forwarded upstream get a fresh `requestID` from the upstream
  connection's correlator. `responseTo` is 0. Replies to clients get
  `responseTo` = the client's `requestID` and a fresh `requestID` from a
  per-client counter (FR-11).
- **Checksums are always stripped.** corral verifies a client's checksum
  while streaming. It forwards the message without `checksumPresent` and
  with `messageLength` reduced by 4 (FR-10 allows this), so the header can be
  written before the tail is read. A mismatch found at the end is a late
  validation failure (FR-38): the upstream connection is discarded and the
  client connection closed. Upstream reply checksums are verified and
  stripped the same way (FR-58).
- `exhaustAllowed` (bit 16) is cleared on every forwarded request (FR-30).
  An upstream reply with `moreToCome` is `unexpected_response`, detected
  from the reply header before any reply byte is written to the client.
- Upstream `requestID` uniqueness: each upstream connection has at most one
  outstanding request at a time (one lease per exchange, and the barrier
  `ping` is sent after the client's `moreToCome` message, which expects no
  reply). The correlator still tracks the outstanding set and rejects any
  other `responseTo` as `unexpected_response` (FR-11).

## Consequences

- The domain carries a BSON implementation that needs its own fuzz tests
  (AC-7) and cross-checks against the official driver's encoder in unit
  tests (test-only import).
- Stripping checksums keeps one code path for both checksummed and plain
  messages, at the cost of never forwarding a checksum. FR-10 allows this.
