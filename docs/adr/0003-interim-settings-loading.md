# ADR-0003: Interim loading of SPEC-001 logical settings

- Status: Superseded by SPEC-003 and ADR-0010 (2026-10-24). Originally
  Accepted (2026-10-24), decided by Orchestrator under Owner's standing
  instruction, 2026-10-24 (former question Q1, decision D1 in
  `plans/SPEC-001.md`).
- Related: SPEC-001 Non-goals (configuration loading), Interfaces (logical
  settings), FR-27, AC-3, AC-11, AC-21, AC-30

## Context

SPEC-001 names logical settings (`log.format`, `log.level`, `admin.listen`,
`tracing.enabled`, `tracing.sample_ratio`) and defers how they are loaded to a
future configuration spec. Several Part A functional ACs start the corral
binary with specific settings (AC-3 with `tracing.enabled=true`, AC-11 with a
configured `admin.listen`, AC-21 with an invalid setting, AC-30 with an
address in use). The binary needs some way to receive them now.

## Decision

- Settings parsing is split into two layers:
  1. `internal/infra/config` parses and validates a map from logical setting
     name to raw string value. An absent key means "use the default". This
     layer is independent of any loading mechanism and is what unit tests use.
  2. An interim loader turns command-line flags into that map. Each flag is
     named exactly like the logical setting, for example
     `corral -log.level=debug -admin.listen=127.0.0.1:0 -tracing.enabled=true`.
- A missing flag value or a known flag given twice is a config error
  (FR-27) with `setting` set to the known setting name (never the value).
  Unknown flags: see the amendment below.

## Amendment 1 (T9 review)

Decided by Orchestrator under Owner's standing instruction, 2026-10-24,
Owner may override.

- Unknown flag names, unknown keys passed to `config.Parse`, and malformed
  flags (`-`, `--`, `-=x`, an empty name, any control character) report
  `setting=unknown` (`config.SettingUnknown`). The given text is never
  echoed in any log record or error text, because an unknown name may carry
  a secret or a marker.
- Positional (non-flag) arguments report `setting=argument`
  (`config.SettingArgument`), never echoed.
- Both `-name=value` and `--name=value` are accepted. `--` is not an
  end-of-flags marker; on its own it is a malformed flag.
- `admin.listen`: the host must be an IP literal or a valid DNS hostname
  whose last label is not numeric or hex-like. This rejects forms such as
  `0`, `0x0`, `127.1`, hosts containing whitespace, and underscores, which
  resolvers may interpret as unexpected addresses.
- `config.LogError` adds no `component`; callers pass
  `Factory.Logger(logging.Config)` or `logging.BootstrapLogger`. The
  runtime (T18) does so in `main`'s startup path.
- The interim loader is the only part the configuration spec is expected to
  replace. Layer 1 stays.
- `OTEL_*` variables are read from the process environment, as FR-20 requires.

## Consequences

- Functional tests can drive all SPEC-001 settings without an environment
  variable or file convention that the configuration spec may contradict.
- The configuration spec may rename or replace the flags; functional tests
  that use them will need updating at that point.
