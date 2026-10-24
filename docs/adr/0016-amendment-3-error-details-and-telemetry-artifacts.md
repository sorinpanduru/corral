# ADR-0016: SPEC-001 Amendment 3 in code: error details, classes and telemetry artifacts

- Status: Accepted (planner, 2026-10-24, for SPEC-002 and SPEC-001
  Amendment 3)
- Related: SPEC-001 FR-7, FR-11, FR-12, FR-18, FR-23, FR-29, FR-31, FR-39,
  AC-41; SPEC-002 FR-48, FR-49, FR-52, Interfaces ("Synthesized error
  replies"); ADR-0004 (updated by this ADR), ADR-0015

## Context

Amendment 3 adds the class `rejected`, 14 fixed-class details, the
components `proxy`, `pool` and `upstream`, the log key `reason`, six metric
label names with an outcome/cause rule, 25 allowlisted commands and three
readiness reasons. The current `errclass` classifier maps any
`ProtocolError` to `protocol`/`other` and has no way to receive a specific
detail from a domain error. SPEC-001 FR-23/FR-29 now also say that
synthesized errors never carry `db.response.status_code` or
`corral.db.response.code_name`, even when their class is `server`
(`upstream_auth_failed`).

## Decision

### Detailed errors

- New duck-typed contract in `errclass`:
  `DetailedError interface { error; CorralErrorDetail() string }`.
- `errclass.DetailClass(d Detail) (Class, bool)` returns the fixed class of
  each Amendment 3 detail (the SPEC-001 FR-29 table). It is the only place
  that table exists in code.
- Classification precedence over the walked chain becomes:
  recovered panic > **detailed (Amendment 3 detail)** > config > server >
  protocol > errno/sentinels.
  - A `DetailedError` whose token is not an Amendment 3 detail is ignored,
    and classification continues down the list. So a domain bug cannot
    invent a detail.
  - Detailed outranks config because `upstream_incompatible` has class
    `config` but is not a `config.Error`. No load-time `config.Error`
    wraps a detailed error.
- Server attributes (`db.response.status_code`,
  `corral.db.response.code_name`) are added only when the class is `server`
  **and** a `ServerError` node is in the chain. This is unchanged code, but
  it now carries a rule for callers:
  - the error that reports an upstream authentication failure (FR-22) wraps
    the upstream reply's `ServerError` when there is one, so the `upstream
    authentication failed` record carries the upstream code, as SPEC-002
    requires;
  - every synthesized error (SPEC-002 "Synthesized error replies") is a fresh
    domain refusal value (`proxy.Refusal`), never wrapping a `ServerError`.
    Its spans and logs therefore carry no status code (SPEC-001 AC-41, last
    bullet; SPEC-002 AC-27).
- `wire.ProtocolError` implements `DetailedError` with the protocol details
  (`unsupported_opcode`, `message_too_large`, `malformed_message`,
  `checksum_mismatch`, `unexpected_response`). The marker interface
  `ProtocolError()` stays for OTLP collector rejections (`protocol`/`other`).

### Enums and allowlists

- `errclass.Classes()` gains `rejected` (7 values). `errclass.Details()`
  gains the 14 Amendment 3 details (28 values).
- `health.Reasons()` gains `upstream_unavailable`, `upstream_auth_failed`
  and `upstream_incompatible` (6 values). `adminhttp` needs no change: it
  passes any in-set reason through.
- `logging` components gain `Proxy`, `Pool` and `Upstream`.
  `attr.Reason = "reason"` is added to the FR-39 key allowlist.
  `internal/testkit/logcheck` rejects a `reason` attribute whose value kind
  is not `slog.KindString`.
- `sanitize` command allowlist: the 27 FR-12 names, in one file
  (`commands.go`), unchanged API.
- `metrics.AllowedLabelNames()`: `command`, `error_class`, `error_detail`,
  `outcome`, `reason`, `side`, `state`, `opcode`, `kind`. New lint rule: no
  value of `outcome`, `reason`, `state`, `side`, `opcode` or `kind` may equal
  any `errclass.Classes()` or `errclass.Details()` value.
- `sanitize.CollectionForTelemetry(string) string`: replaces invalid UTF-8
  with U+FFFD (SPEC-001 FR-40). The FR-40 command list itself lives in
  `internal/domain/wire`, because the wire parser needs it to extract the
  FR-9 collection field (ADR-0015).

### Catalog additions

The 23 SPEC-002 metrics are added to `catalog.json` exactly as listed in
SPEC-002 Interfaces, with these histogram buckets (seconds):

- `corral_request_duration_seconds`: 0.0005, 0.001, 0.0025, 0.005, 0.01,
  0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60.
- `corral_pool_checkout_wait_seconds`: 0.0001, 0.0005, 0.001, 0.005, 0.01,
  0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30.

Buckets are not series under FR-13. The largest family is
`corral_requests_total` with 84 series.

## Consequences

- Existing `errclass` tests for `ProtocolError` (`protocol`/`other`) stay
  valid. New tests cover each Amendment 3 detail through a fake
  `DetailedError`, an unknown token falling through, and the
  server-attribute rule.
- SPEC-001 AC-41 is implemented by SPEC-002 plan tasks T1 and T2.
- ADR-0004 is updated: the label-name allowlist is now the nine FR-11 names,
  with the outcome/cause rule.
