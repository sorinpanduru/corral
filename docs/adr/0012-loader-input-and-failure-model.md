# ADR-0012: Loader input model, winning source and failure propagation

- Status: Accepted (planner, 2026-10-24, for SPEC-003). Derived from FR-4,
  FR-5, FR-9, FR-13, FR-19, FR-28 and FR-30; no new product behaviour.
  Items the spec does not settle are listed in `plans/SPEC-003.md`
  "Proposed for Owner" and are not decided here.
- Related: SPEC-003 FR-4 to FR-15, FR-19, FR-20, FR-27, FR-28, FR-30

## Context

FR-28 pins outcomes case by case and uses the terms "failed" and "winning
source". The loader needs one internal model from which every row of the
FR-28 table, the FR-9 skip rule, the FR-19 "tracing.enabled is known" rule
and the FR-30 skip rule follow, so implementer and test writer agree.

## Decision

### Inputs

For each registered setting the loader collects up to four *inputs*:

| Input | Source | `setting` name used in its records |
|---|---|---|
| flag literal `--<name>=v` | flag | `<name>` |
| flag file reference `--<name>_file=p` (secret only) | flag | `<name>_file` |
| environment literal `CORRAL_<NAME>` | env | `CORRAL_<NAME>` |
| environment file reference `CORRAL_<NAME>_FILE` (secret only) | env | `CORRAL_<NAME>_FILE` |

An environment input exists only when *set* per FR-7 (not empty, not
whitespace-only). A flag input exists whenever the argument names it, even
with an empty value or a missing `=`.

### Source state

Each source (flag, env) of a setting is in one of: *absent* (no input),
*ok* (exactly one usable input that passed type and rule), or *failed*.

- Flag source failed: any group 1 error for this setting (missing value,
  repeat, secret literal, repeated `_file`), or the single flag input failed
  validation or its file read (group 2).
- Env source failed: literal and file reference both set (FR-28, one record
  `CORRAL_<NAME>`, neither validated nor read), or the single env input
  failed validation or its file read.

### Winning source and effective value

- The *winning source* is the highest-precedence source that is not absent
  (flag > env > default). This holds whether or not that source failed.
- The setting is *failed* iff its winning source failed. A failed setting
  has no effective value, gets no FR-9 missing-required error, makes every
  FR-30 rule involving it skip, and, for `tracing.enabled`, makes it *not
  known* (FR-19), so `OTEL_*` validation is skipped.
- Lower-precedence sources are processed and validated anyway (FR-5,
  FR-10). Their errors are reported but do not make the setting failed.
- With no source present: a default applies; an optional setting is
  *unset*; a required setting yields one group 2 record with the logical
  name.

Every FR-28 row follows from this model, for example: secret literal flag
-> flag failed -> setting failed even if `CORRAL_A_B` is valid; `CORRAL_A_B`
and `CORRAL_A_B_FILE` both set with a valid flag -> env failed, flag wins,
the run still fails because of the env record.

### File reads

A file reference is read only when its source is not already failed by a
command-line error (secret literal, repeated `_file`) or an env conflict.
Each reference is read at most once per load, through the injected reader,
with `source.MaxSecretFileBytes` (64 KiB). Read failures and validation
failures of the content are reported under the reference name, with a
constant cause; paths and contents never enter an error.

### Records and ordering

Each record carries a group (1 to 4) and, for group 1, the argument
position that FR-13 / FR-28 assign. The final list is: group 1 by position;
group 2 sorted by `setting` (byte order); group 3 sorted by `setting` then
rule declaration order; group 4 sorted by `setting`. Each input yields at
most one record; a setting may yield several records from different inputs.

### Supplied set (FR-20)

A setting is *supplied* when its effective value came from the flag or env
source (literal or file reference). Defaults and unset do not count. On the
failure path the set is not used.

## Consequences

- `loader.Result` exposes `Failed(name)`; `configload` uses it alone to
  decide whether `tracing.enabled` is known.
- Tests can derive the expected records for any combination from the tables
  above rather than from case-by-case rules.
