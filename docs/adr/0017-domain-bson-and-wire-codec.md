# ADR-0017: Domain BSON and wire codec, streaming validation and header rewriting

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Updated the same day
  for SPEC-002 Amendment 1 (FR-5 check order and minimum lengths, FR-8
  OP_QUERY validation, FR-38 validate-before-write and hold-back, FR-60
  nesting limit and inspected-field integrity).
- Related: SPEC-002 FR-5 to FR-11, FR-30, FR-38, FR-58, FR-60, AC-4, AC-6,
  AC-7, AC-8, AC-19, AC-35, AC-37, AC-38, AC-44, AC-49; SPEC-001 FR-1,
  FR-24, FR-40; ADR-0015

## Context

The wire message model is domain (CLAUDE.md, FR-53), and domain packages may
import only the standard library (SPEC-001 FR-1, empty allowlist). corral must:

- validate client OP_MSG and OP_QUERY documents as BSON (FR-7, FR-8,
  FR-60);
- not hold more than L = 65,536 bytes of a message before a lease (FR-38);
- never deliver a failing message complete to the upstream (FR-38,
  Amendment 1);
- refuse duplicated inspected fields (FR-60);
- stream replies with K = 65,536 bytes held at most, while still reading
  `ok`, `code`, `codeName`, `writeConcernError.code` and `cursor.id` (FR-58).
  These may come after a large `cursor` document.

Forwarding must be byte-faithful, except for the header IDs, the checksum
and `exhaustAllowed` (FR-10).

## Decision

### Hand-written BSON in `internal/domain/bson`

- No third-party BSON library in production code. The official driver's
  `bson` package is used only by tests and twins, so test encoders are
  independent of the code under test (ADR-0022).
- `Validate(doc)` checks a complete document. The `Walker` is a push parser
  (`Write(chunk)`, `Close()`) that checks the same rules incrementally. It
  uses an explicit stack instead of recursion, with constant memory per
  level.
- **Nesting limit (FR-60):** 1024 levels. The top-level document is
  level 1, and each nested document or array adds one level (arrays count
  like documents). 1024 levels pass and 1025 fail with `ErrTooDeep`. This is
  above any server `maxBSONDepth` (default 200, configurable 21 to 999), so
  the limit only bounds corral's parser.
- Structural rules:
  - document length ≥ 5 and consistent with its content;
  - a `0x00` terminator;
  - known element types (0x01 to 0x13, 0x7F, 0xFF, including the
    deprecated 0x06, 0x0C and 0x0E);
  - keys are NUL-terminated cstrings inside their document;
  - string lengths ≥ 1, with a trailing NUL;
  - boolean bytes are 0 or 1;
  - nested document and array lengths stay within their parent;
  - binary lengths ≥ 0;
  - code-with-scope is consistent.

  String values are not checked for UTF-8 (FR-60).
- **Duplicates:**
  - `Validate` and the `Walker` accept duplicate keys in general. The
    server judges them (FR-60).
  - `Lookup` and `FirstElement` keep first-match semantics: `Lookup`
    returns the first occurrence.
  - Existing tests for both stay valid.
- **Captures:** the `Walker` captures configured paths, for example `ok`,
  `code`, `codeName`, `writeConcernError.code`, `cursor.id`, `lsid.id`, and
  the first key of `pipeline.0`.
  - It keeps only scalar values, within fixed byte bounds (strings up to
    256 bytes; longer strings are reported as "present, too long" and never
    kept).
  - **New for FR-60:** every capture whose path is a single top-level key
    also reports `Count`, the number of times that key occurs at the top
    level of the document.
    - The captured value is the first occurrence (first-wins, as `Lookup`).
    - Counting needs no memory beyond the configured capture list, so it is
      bounded.
  - Captures never decode or keep values of keys that are not configured.
- Errors are constant sentinels with no input bytes.

### `internal/domain/wire`

- **Header checks in the FR-5 order** (Amendment 1, G-1, G-2).
  `messageLength` and `opCode` are signed 32-bit. The first failing check
  decides the detail:
  1. length < 16, or negative: `malformed_message`;
  2. length > the effective maximum (upstream `maxMessageSizeBytes`, else
     48,000,000): `message_too_large`;
  3. opcode not accepted: `unsupported_opcode`;
  4. length < the opcode minimum (OP_MSG 26, OP_QUERY 34, others 16):
     `malformed_message`.

  Then come the body checks in byte order (FR-5 check 5:
  `malformed_message`). The checksum is checked last (check 6:
  `checksum_mismatch`), and only when no earlier check failed.
- **`MsgValidator`** validates an OP_MSG incrementally:
  - flags (bits 2 to 15 must be zero);
  - section kinds, exactly one kind-0 section, and section bounds;
  - every kind-0 and kind-1 document, fully and recursively, through the
    `bson.Walker` (FR-60, Owner P3);
  - the first element (command name);
  - the trailing CRC-32C (`hash/crc32` Castagnoli) when `checksumPresent`
    is set.

  It is used for client requests (fed first with the buffered prefix and
  then with streamed chunks) and for upstream replies. For replies it does
  not apply the inspected-field rule, and it never reads `writeErrors`
  (FR-9, G-20).
- **Inspected-field integrity (FR-60).** These are the inspected fields for a
  request body:
  - always: the command name, `$db`, `lsid`, `txnNumber`, `autocommit` and
    `startTransaction`;
  - `pipeline` when the command is `aggregate`;
  - `collection` when the command is `getMore`.

  `MsgValidator` configures top-level count captures for the fixed
  candidate set (`$db`, `lsid`, `txnNumber`, `autocommit`,
  `startTransaction`, `pipeline`, `collection`). After the body document
  ends, any applicable inspected field with `Count > 1` is
  `malformed_message`.

  A duplicate of the command name itself is a later key equal to the first
  key. That duplicate is detected by comparing each top-level key with the
  captured first key while walking. This is one bounded comparison per key
  (up to the 256-byte capture bound), with no extra memory.
- **Kind-1 identifiers (FR-60).** A kind-1 section whose identifier equals
  an applicable inspected field's name is `malformed_message`.
  - Identifiers are compared against the fixed candidate set as they
    arrive. They are kept only as one flag per candidate name, so memory
    stays bounded whatever the number of sections.
  - The applicability of `pipeline` and `collection` is decided once the
    command name is known.
  - The command name is **not** compared with kind-1 identifiers. A kind-1
    section cannot change the command the server runs (the server takes it
    from the body's first key), and this comparison would need unbounded
    memory when sections precede the body. This is listed for confirmation
    in `plans/SPEC-002.md` ("Proposed for Owner").
- **OP_QUERY (FR-8, G-3).** The validator checks:
  - a `fullCollectionName` cstring terminated inside the message;
  - `numberToSkip` and `numberToReturn` present;
  - a query document that passes FR-60;
  - an optional single `returnFieldsSelector` that passes FR-60;
  - no trailing bytes;
  - `flags` ignored.

  Any violation is `malformed_message`. Valid OP_QUERY messages are then
  classified as a handshake (including the `$query` wrapper; exact,
  case-sensitive names) or as a 352 refusal.
- **`ClassifyPrefix`** makes the FR-38 decision from the prefix alone:
  - it reads the command name, `$db`, `lsid.id`, `txnNumber`, `autocommit`,
    `startTransaction`, the FR-40 collection, the `getMore` cursor ID, and
    the first `pipeline` stage name for `aggregate`;
  - a deciding field outside the prefix is `malformed_message`;
  - a locally handled message must fit within L entirely, else
    `message_too_large`.
- The FR-40 collection command list is a domain table. SPEC-001 FR-40 owns
  its contents, and changing it requires amending SPEC-001.

### Header rewriting, checksums and hold-back

- **IDs.** Requests forwarded upstream get a fresh `requestID` from the
  upstream connection's correlator, with `responseTo` 0. Replies to clients
  get `responseTo` = the client's `requestID`, and a fresh `requestID` from a
  per-client counter (FR-11).
- **Checksums are always stripped.**
  - corral forwards the message without `checksumPresent`, and with
    `messageLength` reduced by 4 (FR-10 allows this), so the header can be
    written before the tail is read.
  - Upstream reply checksums are verified and stripped the same way
    (FR-58).
- **Validate before write; hold back the end (FR-38, Amendment 1).**
  - Every chunk passes through the `MsgValidator` before any byte of it is
    written upstream.
  - The forwarder always holds back at least the last byte of the forwarded
    message. The client's 4-byte checksum, when present, is never forwarded
    at all, because it is stripped.
  - The held-back byte is written only after `MsgValidator.Close()` has
    passed for the whole message, including the inspected-field checks that
    complete at the end of the body and the checksum.
  - On any failure the held-back byte is never written. The upstream
    connection is discarded (`client_abandoned`) and the client is closed
    (`protocol_error`). The upstream therefore never receives a complete
    failing message (AC-49).
  - With checksum stripping, "never receives the final 4 bytes" (AC-49)
    holds trivially. The held-back last body byte guarantees that the
    upstream sees fewer bytes than the rewritten `messageLength`.
- `exhaustAllowed` (bit 16) is cleared on every forwarded request (FR-30).
  An upstream reply with `moreToCome` is `unexpected_response`, detected
  from the reply header before any reply byte is written to the client.
- **Upstream `requestID` uniqueness.** Each upstream connection has at most
  one outstanding request at a time: one lease per exchange, and the
  barrier `ping` is sent after the client's `moreToCome` message, which
  expects no reply. The correlator still tracks the outstanding set, and
  rejects any other `responseTo` as `unexpected_response` (FR-11).
  - Bytes arriving with no outstanding request are FR-62 unsolicited data
    (ADR-0019).

## Consequences

- The domain carries a BSON implementation that needs its own fuzz tests
  (AC-7), and cross-checks against the official driver's encoder in unit
  tests (test-only import).
- Stripping checksums keeps one code path for checksummed and plain
  messages, at the cost of never forwarding a checksum. FR-10 allows this.
- New `bson` test items: the per-key `Count` for top-level captures (1, 2
  and 3 occurrences, and a key that occurs only inside a nested document,
  which does not count); `Count` under every chunking; and unchanged
  first-wins values. Existing `Validate` and `Lookup` duplicate-key tests
  stay as they are.
