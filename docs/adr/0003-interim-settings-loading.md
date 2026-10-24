# ADR-0003: Interim loading of SPEC-001 logical settings

- Status: Proposed (planner, 2026-10-24). Needs Owner confirmation; see
  question Q1 in `plans/SPEC-001.md`. Superseded by the future configuration
  spec.
- Related: SPEC-001 Non-goals (configuration loading), Interfaces (logical
  settings), FR-27, AC-3, AC-11, AC-21, AC-30

## Context

SPEC-001 names logical settings (`log.format`, `log.level`, `admin.listen`,
`tracing.enabled`, `tracing.sample_ratio`) and defers how they are loaded to a
future configuration spec. Several Part A functional ACs start the corral
binary with specific settings (AC-3 with `tracing.enabled=true`, AC-11 with a
configured `admin.listen`, AC-21 with an invalid setting, AC-30 with an
address in use). The binary needs some way to receive them now.

## Decision (proposed)

- Settings parsing is split into two layers:
  1. `internal/infra/config` parses and validates a map from logical setting
     name to raw string value. An absent key means "use the default". This
     layer is independent of any loading mechanism and is what unit tests use.
  2. An interim loader turns command-line flags into that map. Each flag is
     named exactly like the logical setting, for example
     `corral -log.level=debug -admin.listen=127.0.0.1:0 -tracing.enabled=true`.
- Unknown flags, a missing flag value, or a flag given twice are config errors
  (FR-27) with `setting` set to the flag name as given (never the value).
- The interim loader is the only part the configuration spec is expected to
  replace. Layer 1 stays.
- `OTEL_*` variables are read from the process environment, as FR-20 requires.

## Consequences

- Functional tests can drive all SPEC-001 settings without an environment
  variable or file convention that the configuration spec may contradict.
- The configuration spec may rename or replace the flags; functional tests
  that use them will need updating at that point.
