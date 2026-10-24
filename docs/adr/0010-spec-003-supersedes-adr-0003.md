# ADR-0010: SPEC-003 supersedes ADR-0003 (interim settings loading)

- Status: Accepted (planner, 2026-10-24, for SPEC-003). Records the Owner
  decision in the SPEC-003 Decisions log ("SPEC-003 formally supersedes
  ADR-0003").
- Supersedes: ADR-0003 (Interim loading of SPEC-001 logical settings),
  including its Amendment 1.
- Related: SPEC-003 Goal, FR-3, FR-6, FR-8, FR-12, FR-19, FR-24;
  `plans/SPEC-003.md` T1, T4, T12, T15; ADR-0011, ADR-0012, ADR-0013

## Context

ADR-0003 introduced a flags-only interim loader (`config.FromArgs`) and a
settings validation layer (`config.Parse`, `config.Settings`,
`config.Defaults`) in `internal/infra/config`, to be replaced by a
configuration spec. SPEC-003 is that spec. It keeps the flag spelling
(`-name=value` / `--name=value`) and the `setting=unknown` /
`setting=argument` reporting, and adds environment variables, secrets, file
references, one-pass error collection, control flags and exit code 2.

## Decision

1. ADR-0003 is superseded in full. Where it and SPEC-003 differ, SPEC-003
   governs.
2. Retired (removed in `plans/SPEC-003.md` T12, after the runtime switches
   over):
   - `config.FromArgs` (interim loader);
   - `config.Parse`, `config.Settings`, `config.Defaults` and the per-setting
     parsers in `config.go` (`parseBool`, `parseRatio`, `validateListen`,
     `validHostname`). Their logic moves into the FR-12 type set
     (`internal/infra/config/types`, T1), with the FR-12 tightenings applied:
     no leading `+` on `int`/`float`, no leading zeros or sign on ports,
     strict `true`/`false` only.
   - `tracing.Resolve(config.Settings, LookupEnv)`; replaced by the FR-19 /
     FR-25 form (T6).
3. Kept from ADR-0003 and its amendment, now normative through SPEC-003:
   - flag names equal logical names; `-` and `--` prefixes both accepted;
     `--` is not an end-of-flags marker (FR-3, FR-6);
   - unknown or malformed flags report `setting=unknown`, positional
     arguments `setting=argument`, never echoed (FR-6, FR-8);
   - the `admin.listen` host rules (now the FR-12 `hostport` type);
   - `config.Error`, `config.NewError` and `config.LogError` (the `setting`
     plus errclass record), and the rule that callers supply a
     `component=config` logger.
4. Changed relative to ADR-0003:
   - `OTEL_*` variables are read from the injected environment snapshot, not
     the process environment, for validation (FR-19, FR-25). The OTLP
     exporter still reads the real process environment (SDK semantics).
   - Config errors exit with code 2, not 1 (FR-24). The admin bind failure
     stays exit 1.
   - All config errors are reported in one pass (FR-13), not just the
     first.
   - `OTEL_*` errors go through the bootstrap JSON logger, not the
     configured logger (FR-19, FR-29).

## Consequences

- Existing functional tests that pass settings as `-name=value` keep
  working unchanged in their flag syntax. Tests that count `component=config`
  records on a successful start must allow the new FR-20 `configuration
  loaded` info record (T12 / T13 migration).
- `docs/operations/observability.md` loses its "Interim flag syntax" section
  in favour of `docs/operations/configuration.md` (T15).
- ADR-0003 keeps its text for history; its status line points here.
