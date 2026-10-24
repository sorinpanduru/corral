# Plan: SPEC-003 Configuration loading

- Spec: `specs/SPEC-003-configuration.md` (approved 2026-10-24, revision 2; Amendment 1 approved and promoted 2026-10-24, commit c4cfcdb). Supersedes ADR-0003.
- Planner date: 2026-10-24 (updated the same day for Amendment 1 and the Owner's answers to P1–P5).
- Scope: every FR and AC of SPEC-003 (AC-1 to AC-22), including the rework of `tracing.Resolve` (FR-19, FR-25), version stamping (FR-22) and `docs/operations/configuration.md` (FR-26).
- Status: wave 1 (T1, T2, T7 and the `symcheck` part of T14) is implemented at 691e9f3. Amendment 1 changes to T1 and T2 are planned as follow-ups T1.1 and T2.1 rather than reopening those tasks.
- Builds on: SPEC-001 (`plans/SPEC-001.md`, ADR-0001 to ADR-0009). Reuses `config.Error` / `LogError`, `logging`, `errclass`, `ratelimit`, `logbridge`, `markerscan`, `otlpcollector`, `logcheck`, `fakeclock`, `importcheck`.
- Forward compatibility: SPEC-002 (proxy core) will add its settings as one `registry.Group` in `internal/app/settings` (ADR-0011). No loader, registry or runtime change is needed for that.
- Dependencies: standard library only for all new production code (SPEC-003 NFR). `golang.org/x/tools` stays test-only (T14).
- Docker: no task needs Docker. **No twin is needed.**

## ADRs written for this plan

- `docs/adr/0010-spec-003-supersedes-adr-0003.md`: ADR-0003 superseded; what is retired, kept and changed.
- `docs/adr/0011-configuration-package-structure.md`: packages and layering, per-spec `registry.Group` registration, value representation, AC-14/AC-15 targets.
- `docs/adr/0012-loader-input-and-failure-model.md`: inputs, source states, winning source, "failed", file-read rule, record groups.
- `docs/adr/0013-startup-modes-output-path-and-exit-codes.md`: entry, mode dispatch, failure and success output paths, SIGHUP watcher, exit codes, version stamping. Amends ADR-0006 steps 0 to 4.
- `docs/adr/0014-corraltest-build-for-binary-config-tests.md`: (Accepted, P1 approved by the Owner) `corraltest` build tag for the AC-6/AC-9/AC-10/AC-19 binary tests.- Status lines updated: ADR-0003 (superseded), ADR-0006 (amended by ADR-0013).

## Rules for every task

- File ownership as ADR-0001: implementers own the non-test files listed under "Implementer owns"; test writers own `*_test.go`, `testdata/` and `tests/**` listed under "Test writer owns". Neither edits the other's files.
- Synthetic data only. Markers are random strings generated per test. Secret fixtures are synthetic text (never real credentials); PEM material is generated at runtime into `t.TempDir()` (no committed `*.pem`/`*.key`).
- Every suite that emits logs uses `internal/testkit/logcheck` (SPEC-001 rule).
- Compliance (FR-11, FR-14, FR-15, SPEC-001 FR-24): no error text, record, help text or test failure message may contain a supplied value, part of one, a file path or file contents. Errors are `config.Error` wrapping a **constant** sentinel. Test failure messages print lengths or setting names, never the raw value (the marker harness scans test output too when captured).
- No new code reads `os.Args`, `os.Getenv`, `os.LookupEnv`, `os.Environ`, `syscall.Getenv`, `syscall.Environ`, `os.ReadFile`, `os.Open`, `os.OpenFile`, `os.Stat` or `os.Lstat` outside `cmd/corral` and `internal/infra/osfile` (AC-15).
- Domain packages (`internal/domain/...`) are not touched by this plan and must not import any new package (FR-16).
- `make build`, `make test` and `make test-functional` are gates for every task from T12 on. Before T12, tasks gate on `make build` and `make test` (the runtime still uses the interim loader until T12 switches it).
- Unit tests use external test packages (`package x_test`) where possible.

## Package map (introduced or changed by this plan)

| Package | Task |
|---|---|
| `internal/infra/config/types` (new) | T1 |
| `internal/infra/config/source` (new), `internal/infra/osfile` (new), `internal/testkit/memfs` (new) | T2 |
| `internal/infra/config/registry` (new) | T3 |
| `internal/infra/config/loader` (new) | T4 (`args*.go`, `mode*.go`), T5 (rest) |
| `internal/infra/telemetry/tracing` (`env*.go`, `tracing.go` Options) | T6 |
| `internal/infra/config` (root, existing) | T7 (records); T12 (removal of the interim API) |
| `internal/infra/config/usage` (new) | T8 |
| `internal/infra/sighup` (new) | T9 |
| `internal/app/settings` (new) | T10 |
| `internal/app/configload` (new) | T11 |
| `internal/app/runtime`, `cmd/corral`, `Makefile` | T12 |
| `internal/testkit/symcheck` (new) | T14 |
| `docs/operations/configuration.md` (new), `docs/operations/observability.md` | T15 |

---

## Tasks

### T1: FR-12 type set

- Goal: one parser per FR-12 type, with the exact accepted syntax, the control-character and UTF-8 rules, and constant errors. This replaces the per-setting parsers in `internal/infra/config/config.go` (removed later in T12).
  - `string`: valid UTF-8, no control character (U+0000–U+001F, U+007F, U+0080–U+009F); empty only with `AllowEmpty`.
  - `text`: valid UTF-8; only `\t`, `\n`, `\r` allowed among control characters; empty only with `AllowEmpty`.
  - `bool`: `true` / `false`, case-insensitive; nothing else.
  - `int`: `-?[0-9]+`, fits int64.
  - `float`: `-?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?`, finite float64. Match the regex first, then `strconv.ParseFloat` (so `Inf`, `NaN`, hex, `_` and `+` never reach `strconv`).
  - `duration`: one or more `<decimal><unit>` groups, units `ns us ms s m h` (ASCII only), no sign, unit required; `> 0` unless `AllowZero` (then any zero with a unit). Validate the grammar before `time.ParseDuration`.
  - `enum`: declared set, case-insensitive unless `EnumCaseSensitive`; returns the declared spelling.
  - `hostport`: logic moved from `validateListen` / `validHostname` and tightened: port decimal digits only, 0–65535, no sign, no leading zeros (`0` alone allowed); host non-empty, IP literal (IPv6 bracketed) or DNS hostname (labels 1–63, letters/digits/`-`, no leading/trailing `-`, total ≤ 253, last label not all-numeric and not `0x`-hex-like). Amendment 1 (P2, resolved): a trailing dot (empty last label) and IPv6 zone identifiers are rejected. T1 shipped before the amendment; T1.1 applies this change.
  - `list` of any scalar except `text`: split on `,`, trim ASCII space/tab around each element, reject empty elements, empty list only with `AllowEmpty`; each element parsed by the element type.
  - Every type rejects invalid UTF-8 first. Exactly one error per value: the first failing check in the type's order (FR-13).
- FRs/ACs: FR-12, FR-1 (type set); AC-4 (type part), AC-17 (type part).
- Implementer owns: `internal/infra/config/types/*.go` (non-test).
- Test writer owns: `internal/infra/config/types/*_test.go`.
- Interfaces defined:
  - `types.Kind` with `KindString, KindText, KindBool, KindInt, KindFloat, KindDuration, KindEnum, KindHostPort, KindList`.
  - `types.Spec{Kind Kind; Elem Kind /* lists */; EnumValues []string; EnumCaseSensitive bool; AllowEmpty bool; AllowZero bool}`.
  - `(types.Spec).Check() error`: the spec itself is well-formed (enum set non-empty and unique, list element is a scalar other than `text`, flags only on kinds that use them).
  - `(types.Spec).Parse(raw string) (any, error)`: dynamic types per ADR-0011 (`string`, `bool`, `int64`, `float64`, `time.Duration`, typed slices for lists).
  - `(types.Spec).Describe() string`: type text for help, for example `enum (json, text)`, `list of duration` (T8 uses it).
  - Constant sentinels: `types.ErrInvalidUTF8`, `ErrControlChar`, `ErrEmpty`, `ErrSyntax`, `ErrRange`, `ErrNotInSet`. No error carries input text.
- Test strategy:
  - Unit, table-driven per type: every AC-4 example (`bool` rejects `1`, `yes`; `int` rejects `+5`, `0x10`, `1_000`; `float` rejects `NaN`, `Inf`, `+0.5`, `0x1p-2`, `1_0` and accepts `.5`, `1.`, `1e-3`; `duration` rejects `5`, `0`, `-1s`, `+1s`, `1µs` and accepts `250ms`, `1m30s`, `0s` only with `AllowZero`; `hostport` rejects `127.0.0.1:09187`, `127.0.0.1:+80`, `:9187`, `0:80`, `0x0:80`, `127.1:80`; `string` rejects U+007F and U+0085; `text` accepts inner `\n`, rejects U+0000; `list` rejects `a,,b`, trims `a, b`; invalid UTF-8 rejected by `string` and `text`).
  - Boundaries: int64 min/max and overflow; float overflow to Inf rejected; port 65535/65536; label 63/64 chars; hostname 253/254 chars.
  - Marker check: for each rejected input carrying a marker, `err.Error()` does not contain it.
  - Fuzz test (`FuzzParse` per kind, short corpus in `make test`): never panics; accepted `float`/`int` inputs match the FR-12 regex.
- Docker: no.
- Depends on: none.
- Status: done (691e9f3), apart from T1.1.

### T1.1: Amendment 1 follow-up to the type set (FR-12, AC-4)

- Goal: bring the implemented T1 in line with Amendment 1.
  - `hostport` rejects a trailing dot (`localhost.:9187`) and IPv6 zone identifiers (`[fe80::1%eth0]:9187`), and still accepts `localhost:9187` and `[::1]:9187`. Remove the old `strings.TrimSuffix(h, ".")` behaviour; reject `%` in a bracketed host before `netip.ParseAddr` (or reject any parsed address with `Zone() != ""`).
  - `list` trimming stays limited to ASCII space and tab, which is deliberately narrower than the FR-7 whitespace set. Confirm the implementation does not use `strings.TrimSpace` for list elements.
- FRs/ACs: FR-12; AC-4 (new hostport and list cases).
- Implementer owns: `internal/infra/config/types/*.go` (non-test).
- Test writer owns: `internal/infra/config/types/*_test.go`. Add the five AC-4 hostport cases. Add the list case: an `int` list `1,<U+00A0>2` is rejected because U+00A0 is not trimmed. Remove any test that accepted a trailing dot or a zone.
- Interfaces: unchanged.
- Docker: no.
- Depends on: T1 (done). Can run in wave 2.

### T2: Input sources: environment snapshot, file-reader contract, OS reader, in-memory reader

- Goal: the injected inputs of FR-25.
  - `source.Env`: built from `KEY=VALUE` entries; first occurrence of a key wins; entries without `=` ignored; key match case-sensitive; `Names()` lists distinct keys in first-occurrence order.
  - `source.IsSet(v string) bool`: false for empty or whitespace-only (`strings.TrimSpace(v) == ""`). Amendment 1 (FR-7, AC-22) fixes the whitespace set as the `strings.TrimSpace` / `unicode.IsSpace` set, which is the one the OpenTelemetry SDK and SPEC-001 FR-41 use. Invalid UTF-8 is not whitespace, so such a value counts as set.
  - `source.ReadFile func(path string, maxBytes int64) ([]byte, error)` with exactly three failure sentinels (`ErrUnreadable` for not found or unreadable, `ErrNotRegular`, `ErrTooLarge`). Constant text.
  - `osfile.Read`: `os.Stat` (follows symlinks) must be a regular file; open `O_RDONLY|O_NONBLOCK`; `Stat` the handle and require regular again; read at most `maxBytes+1` bytes; more than `maxBytes` is `ErrTooLarge`. Any OS error maps to a sentinel; the `*os.PathError` (which contains the path) is never returned or wrapped. This generalises `tracing.readPEMFile`, which T6 deletes.
  - `memfs` (testkit): in-memory `ReadFile` with per-path content or failure kind, and a per-path call counter.
- FRs/ACs: FR-7 (snapshot rules), FR-15 (reader bounds, non-blocking), FR-25; AC-4 (duplicate key, empty value), AC-8 (FIFO and directory with the real reader, 65,536 / 65,537 bytes), AC-10 (unit: call counter support).
- Implementer owns: `internal/infra/config/source/*.go`, `internal/infra/osfile/*.go`, `internal/testkit/memfs/*.go` (non-test).
- Test writer owns: `internal/infra/config/source/*_test.go`, `internal/infra/osfile/*_test.go`, `internal/testkit/memfs/*_test.go`.
- Interfaces defined:
  - `source.NewEnv(entries []string) Env`; `(Env).Lookup(name string) (string, bool)`; `(Env).Names() []string`; `source.IsSet(string) bool`.
  - `source.ReadFile` (type); `source.ErrUnreadable`, `ErrNotRegular`, `ErrTooLarge`; `source.MaxSecretFileBytes = 65536`; `source.MaxPEMFileBytes = 4194304`.
  - `osfile.Read(path string, maxBytes int64) ([]byte, error)` (assignable to `source.ReadFile`).
  - `memfs.New() *FS`; `(*FS).Put(path string, content []byte)`; `(*FS).Fail(path string, err error)`; `(*FS).Read` (a `source.ReadFile`); `(*FS).Calls(path string) int`.
- Test strategy:
  - Unit: duplicate keys (first wins), entry without `=`, `=x` (empty key), case sensitivity, `IsSet` on `""`, `"  "`, `"\t"`, `" a "`.
  - Unit (real reader, Unix build tag where needed): missing path, directory, FIFO (must return within a short bound; run in a goroutine with a timeout), symlink to a regular file accepted, symlink to a directory rejected, unreadable file (skip when root), exactly 65,536 bytes accepted, 65,537 rejected, `maxBytes+1` read bound (a large sparse file returns `ErrTooLarge` quickly).
  - Marker: every returned error's text excludes the path marker.
- Docker: no.
- Depends on: none.
- Status: done (691e9f3), apart from T2.1.

### T2.1: Amendment 1 follow-up to the whitespace set (FR-7, AC-22)

- Goal: prove that `source.IsSet` uses exactly the FR-7 whitespace set. Change the code only if a test fails.
- FRs/ACs: FR-7; AC-22 (unit, `IsSet` part).
- Implementer owns (only on a failing test): `internal/infra/config/source/*.go` (non-test).
- Test writer owns: `internal/infra/config/source/*_test.go`. Values made only of each of U+0009, U+000B, U+000C, U+0085, U+00A0, U+2003 and U+3000 are not set. U+200B alone is set. Invalid UTF-8 bytes alone are set.
- Interfaces: unchanged.
- Docker: no.
- Depends on: T2 (done). Can run in wave 2.

### T3: Settings registry

- Goal: the FR-1 / FR-2 / FR-3 / FR-14 declaration model and build-time checks; per-spec groups (ADR-0011).
  - Name rules: ≥ 2 segments, each `[a-z][a-z0-9_]*`; last segment not `file` and not ending `_file`.
  - Derived names: flag `<name>`, file flag `<name>_file` (secret only), env `CORRAL_` + upper(name) with `.`→`_`, file env `<env>_FILE` (secret only). Two uniqueness sets (flag names, env names) as FR-2.
  - Group checks: every decl's first segment is in its group's `Namespaces`; no namespace claimed by two groups.
  - Presence: `default` (needs `Default` text that parses with type and passes `Rule`), `required`, `optional`. Secrets cannot be `default`.
  - Description non-empty, single line, no control characters.
  - `types.Spec.Check()` must pass.
  - Cross rules: non-empty `Involves`, all registered, `Primary` among them, non-nil `Check`.
  - `New` returns one error listing problem kinds and offending setting names (names are programmer constants, safe to print); it never panics.
- FRs/ACs: FR-1, FR-2, FR-3, FR-14 (declaration rules), FR-15 (reference names), FR-30 (declaration); AC-1 (refusal cases).
- Implementer owns: `internal/infra/config/registry/*.go` (non-test).
- Test writer owns: `internal/infra/config/registry/*_test.go`.
- Interfaces defined:
  - `registry.Presence` (`PresenceDefault`, `PresenceRequired`, `PresenceOptional`).
  - `registry.Decl{Name string; Type types.Spec; Presence Presence; Default string; Secret bool; Description string; Rule func(v any) error}`.
  - `registry.CrossRule{ID string; Involves []string; Primary string; AppliesToUnset bool; Check func(get func(name string) (v any, set bool)) error}`.
  - `registry.Group{Spec string; Namespaces []string; Decls []Decl; Rules []CrossRule}`.
  - `registry.New(groups ...Group) (*Registry, error)`.
  - `(*Registry).Settings() []Setting` (sorted by name); `Lookup(name) (Setting, bool)`; `ByFlag(flag string) (s Setting, fileRef bool, ok bool)`; `ByEnv(env string) (s Setting, fileRef bool, ok bool)`; `CrossRules() []CrossRule` (declaration order).
  - `registry.Setting{Decl; Flag, Env, FileFlag, FileEnv string; DefaultValue any}` (`FileFlag`/`FileEnv` empty for non-secrets).
  - `registry.EnvName(name string) string`; `registry.DerivedNameConflicts(decls []Decl) []string` (exported so AC-1's bypass test can check the derived-name rule without the segment rule).
- Test strategy:
  - Unit: each AC-1 refusal case (duplicate; single segment; uppercase segment; hyphen; `a.b.file`; `a.b_file`; `a.b_c` + `a_b.c`; `a_b.file` with secret `a.b`; secret with default; default failing type; default failing rule; primary not among involved). Plus: namespace outside group, namespace in two groups, empty description, multi-line description, bad `types.Spec`.
  - Unit: `DerivedNameConflicts` on decls that bypass the segment rule (`a.b` secret + `a.b_file`; `a.b` secret + `a_b.file`) reports the `_file` and `_FILE` collisions.
  - Unit: name derivation table (`log.level` → `CORRAL_LOG_LEVEL`, `tracing.sample_ratio` → `CORRAL_TRACING_SAMPLE_RATIO`), `ByFlag` / `ByEnv` for literal and reference names.
- Docker: no.
- Depends on: T1.

### T4: Command-line parsing and control-flag mode selection

- Goal: FR-6, FR-8 (flags), FR-27, and the command-line rows of FR-28, producing positioned inputs and group 1 errors.
  - `SelectMode`: scan for exact help spellings (`--help`, `-help`, `-h`), then version (`--version`, `-version`), then check-config (`--check-config`, `-check-config`), per FR-27 precedence.
  - Parsing (run and check-config modes): an argument not starting with `-` is a positional (`setting=argument`). Otherwise strip one or two leading dashes; split at the first `=`. Exact control spellings are skipped. Malformed (`-`, `--`, `-=x`, empty name, any control character anywhere in the name part) or unknown name (not a setting flag, not a secret's `_file` flag) → `setting=unknown`. A control spelling with `=` → `setting=unknown`.
  - FR-28 occurrence rule (Amendment 1; resolves P4 and P5). It is evaluated per flag name: each setting flag and each `<name>_file` flag separately. An *occurrence* is any argument naming that flag, with or without `=`, in either dash form. Collect every occurrence first, then decide:
    - Non-secret setting flags and every `_file` flag: two or more occurrences give exactly one repeat record at the second occurrence. There is no missing-value record, nothing is validated, and no file is read. One occurrence without `=` gives a missing-value record at its position. One occurrence with `=` is a valid input for T5.
    - Secret setting flags: if any occurrence has `=`, there is exactly one secret-literal record, at the first occurrence that has `=`, and no repeat or missing-value record. Otherwise one bare occurrence gives a missing-value record (not a secret-literal record), and two or more bare occurrences give one repeat record at the second occurrence.
    - Because the setting flag and its `_file` flag are evaluated separately, `--a.b=v --a.b_file=p1 --a.b_file=p2` gives two records, in position order: `a.b` at the literal and `a.b_file` at the second `_file` occurrence.
    - The flag source of the setting is failed whenever its setting flag or its `_file` flag produced a record. The `_file` reference is never read when the setting flag has a secret-literal record (FR-28 row 1) or when the `_file` flag has its own record.
  - In the space-separated form (`--log.level debug`), the missing-value record comes first and the next word is then reported as a positional by the ordinary positional rule.
  - Output records carry their argument position. Values are kept only for valid single occurrences, for T5 to validate.
- FRs/ACs: FR-6, FR-8 (flags), FR-13 (group 1 ordering), FR-14 (literal secret on the command line), FR-27, FR-28 (command-line rows); AC-3; AC-12 (unit part: mode table).
- Implementer owns: `internal/infra/config/loader/args*.go`, `internal/infra/config/loader/mode*.go` (non-test).
- Test writer owns: `internal/infra/config/loader/args*_test.go`, `internal/infra/config/loader/mode*_test.go`.
- Interfaces defined:
  - `loader.Mode` (`ModeRun`, `ModeHelp`, `ModeVersion`, `ModeCheckConfig`); `loader.SelectMode(args []string) Mode`.
  - `loader.FlagInput{Pos int; Setting string /* logical name */; FileRef bool; Value string}`.
  - `loader.ArgProblem{Pos int; Setting string /* record name */; Err error}`.
  - `loader.ParseArgs(r *registry.Registry, args []string) (inputs []FlagInput, problems []ArgProblem, failedFlag map[string]bool)`; `failedFlag` marks settings whose flag source failed at group 1 (ADR-0012).
  - Constant sentinels: `loader.ErrUnknownFlag`, `ErrPositional`, `ErrMissingValue`, `ErrRepeated`, `ErrSecretOnCommandLine`, `ErrControlFlagValue`.
- Test strategy:
  - Unit with a test registry (`t.x` string, `t.secret` secret text, `log.level`-like enum): every AC-3 case, asserting the exact record count, `setting` and position:
    - `--log.level debug` → `log.level`, then `argument`;
    - bare `--tracing.enabled`;
    - `--x=1 --x=2`, and three repeats → one record at the second position;
    - a repeated secret literal → one record at the first position;
    - `--x=1 -x=2`;
    - `--x=1 --x` and `--x --x=1` → one record at the second occurrence, with no missing-value record;
    - bare `--t.secret` → a missing-value record at its position;
    - `--t.secret --t.secret=v` → a secret-literal record at position 2;
    - a positional argument; `--`; `-`;
    - interleaved unknown flags and a positional, in argument order.
  - Unit (FR-28 table): `--t.secret=v --t.secret_file=p` → one record; `--t.secret=v --t.secret_file=p1 --t.secret_file=p2` → two records in the stated order; `--t.secret_file=p1 -t.secret_file=p2` → one `t.secret_file` record. In every one of these cases the flag source of `t.secret` is failed.
  - Unit: every FR-27 combination row through `SelectMode` plus `ParseArgs`. In particular `--check-config=true` → run mode with one `unknown`; `--check-config --help=x` → check-config mode (because `--help=x` is not a help spelling) with one `unknown`.
  - Unit: `--h` unknown; `-h=1` unknown; `--a.b_file=` yields an input with an empty path (T5 reports it); non-secret `--t.x_file=p` unknown.
  - Marker: markers in unknown names, values and positionals never appear in any `ArgProblem` error text.
  - Fuzz: `ParseArgs` never panics, returns at most one problem per argument, and returns at most one problem per registered flag name, whether a setting flag or a `_file` flag (the occurrence rule).
- Docker: no.
- Depends on: T3.

### T5: Loader resolution, validation, cross rules and ordering

- Goal: the rest of the loader contract for CORRAL settings (FR-4 to FR-15, FR-20, FR-28, FR-30), implementing ADR-0012.
  - Environment: for each registered setting, its env literal and (secret) file reference, set per FR-7 (`source.IsSet`, the `unicode.IsSpace` set, Amendment 1); literal and reference both set → one group 2 record under `CORRAL_<NAME>`, neither validated nor read. Values used untrimmed.
  - Unknown env: names with exact prefix `CORRAL_`, set, and not a registered env or file-env name; `OTEL_*` never. Render: any byte outside `[A-Za-z0-9_]` → `CORRAL_<invalid-name>`, then deduplicate, sort ascending by bytes.
  - File references: read through `Inputs.ReadFile` with `source.MaxSecretFileBytes` unless skipped per ADR-0012; strip exactly one trailing `\n` or `\r\n`; validate content. Empty flag path → error under `<name>_file`. Each reference read at most once.
  - Validation: every provided input (all sources) parsed by its `types.Spec` and `Rule`; one record per failing input, `setting` per FR-11.
  - Resolution: winning source, failed state, effective value, unset, defaults, FR-9 missing required (logical name).
  - Cross rules: after all per-input checks, in declaration order; skipped when an involved setting failed, or an involved optional setting is unset (unless `AppliesToUnset`); a failure's `setting` is the primary's name spelled for its effective source (logical for flag, default and env file reference; `CORRAL_*` for env literal).
  - Ordering: group 1 by position, group 2 sorted by `setting`, group 3 by `setting` then rule order (FR-13).
  - Supplied set (FR-20): sorted logical names whose effective value came from flag or env.
  - `Load` does not handle help/version (callers stop before); in check-config mode it ignores control spellings exactly as in run mode.
- FRs/ACs: FR-4, FR-5, FR-7, FR-8 (env), FR-9, FR-10, FR-11 (`setting` naming), FR-13 (groups 1–3), FR-14, FR-15, FR-16 (typed or unset output), FR-17 (read once), FR-20 (supplied set), FR-25, FR-28, FR-30; AC-2 (with test registry), AC-4 (env empty = unset, `_FILE` empty = unset, duplicate key), AC-5 (unit: unknown env list), AC-8, AC-10 (unit: reader called once), AC-15 (unit: determinism), AC-18, AC-22 (loader part, with the test registry: whitespace-only env values unset; U+200B and invalid UTF-8 set and validated).
- Implementer owns: `internal/infra/config/loader/*.go` except T4's `args*.go` and `mode*.go` (non-test).
- Test writer owns: `internal/infra/config/loader/*_test.go` except T4's.
- Interfaces defined:
  - `loader.Inputs{Args []string; Env source.Env; ReadFile source.ReadFile}`.
  - `loader.Source` (`SourceDefault`, `SourceFlag`, `SourceEnv`, `SourceUnset`).
  - `loader.Value{Set bool; V any; Source Source; FileRef bool}`.
  - `loader.Result{Values map[string]Value; Supplied []string; UnknownEnv []string; Errors []error}`; `(Result).Failed(name string) bool`; `(Result).Get(name string) (any, bool)`.
  - `loader.Load(r *registry.Registry, in Inputs) Result`. Every element of `Errors` is a `*config.Error` wrapping a constant sentinel (`loader.ErrInvalidValue`, `ErrMissingRequired`, `ErrLiteralAndFile`, `ErrFileUnreadable`, `ErrFileNotRegular`, `ErrFileTooLarge`, `ErrEmptyPath`, `ErrCrossRule`, plus T4's).
- Test strategy (test registry with `t.secret` secret text optional, `t.req` required string, `t.list` list of string, `t.a`/`t.b` with a cross rule primary `t.b`, `memfs` reader, markers in values and paths):
  - AC-2 shape on test settings: default; flag only; env only; both (flag wins); list in both (flag list only); invalid env with valid flag → `CORRAL_T_*` record.
  - AC-4: env `""` and `"  "` unset (default applies), same for `_FILE`; duplicate env key first wins.
  - AC-5 unit: `CORRAL_SERVICE_HOST`, `CORRAL_ZZ_TYPO`, `CORRAL_`, `corral_lower` → `CORRAL_,CORRAL_SERVICE_HOST,CORRAL_ZZ_TYPO`; empty unknown not listed; a name with `-` or a non-ASCII byte → `CORRAL_<invalid-name>` once, sorted per FR-8; `OTEL_*` never listed.
  - AC-8: every bullet, with exact record counts and `memfs.Calls` assertions. The reader is not called for an env conflict, a secret literal, or a repeated `_file`. Include the Amendment 1 case `--t.secret=<marker> --t.secret_file=p1 --t.secret_file=p2`: exactly two records (`t.secret`, then `t.secret_file`) and zero reader calls. trailing `\n` and `\r\n` stripped once, inner newline kept; file of only `\n` rejected; 65,536 accepted via `memfs`; flag file beats env literal; overridden `CORRAL_T_SECRET_FILE` missing → error under that name; non-secret `_FILE` env is unknown; optional secret unset loads as unset.
  - AC-10 unit: one `Load` reads each reference once; changing the `memfs` content after `Load` does not change the result.
  - AC-18: violation names `t.b` (flag or default) or `CORRAL_T_B` (env literal) or `t.b` (env file ref); skipped when `t.a` invalid or `t.b` repeated; group 3 after every group 2.
  - FR-9: missing required → logical name; required whose flag repeated → no missing-required record.
  - FR-28 skip rules with the occurrence rule: `--t.req --t.req=v` → one repeat record and no missing-required record; for the cross rule, `--t.b=1 --t.b` → no group 3 record.
  - FR-13: a mixed case asserting full order across groups 1–3.
  - AC-15 unit: two `Load` calls with identical inputs give `reflect.DeepEqual` results.
  - Marker scan (`markerscan`) over the rendered records of every case: no value or path marker.
- Docker: no.
- Depends on: T2, T3, T4.

### T6: `tracing.Resolve` rework (FR-19, FR-25)

- Goal: `OTEL_*` validation reports every invalid variable, reads PEM files only through the injected reader, and no longer depends on `config.Settings`.
  - New signature (below). Validation logic and accepted values unchanged (SPEC-001 FR-19 to FR-21, FR-27, FR-30, FR-37, FR-41; D8). Trimming of `OTEL_*` values kept.
  - Collect all errors: one `config.Error` per variable (variable name as `setting`); keep going after a failure.
  - Amendment 1 (FR-19, AC-21): an invalid `OTEL_TRACES_EXPORTER` does not stop validation. Every other honoured or validated variable is validated exactly as when the exporter is valid, whether that value is `otlp` or `none`: protocol, endpoint, headers, timeout, compression, insecure, TLS, `OTEL_BSP_*`, `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`, and `OTEL_RESOURCE_ATTRIBUTES`. On an exporter error, the `cfg` returned is irrelevant because the run fails.
  - Cross-variable checks report under the FR-19 names: batch > queue under `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`; cert without key under the key variable of the same form; key without cert under the cert variable; non-pairing under the key variable.
  - Amendment 1 skip rule (FR-19, AC-20): if **any** variable a check involves has its own error, the check is skipped entirely, with no record and no re-evaluation against that variable's default. So an invalid `OTEL_BSP_MAX_QUEUE_SIZE` skips the batch-size check even when the batch exceeds the default queue size, and a non-PEM client certificate (or key) skips the pairing and presence checks of that form. This replaces the current code's behaviour, which falls back to the default queue size.
  - Whitespace: `OTEL_*` values are trimmed with `strings.TrimSpace` (the FR-7 set, Amendment 1), and a value that is empty after trimming is unset. This is unchanged from today; AC-22 pins it.
  - PEM files through `read` with `source.MaxPEMFileBytes`; read each distinct path at most once per `Resolve` call (cache bytes for the call only, discard on return).
  - Remove `readPEMFile` and all `os` / `syscall` use from `env*.go`.
  - `tracing.Options.Settings` (unused by `Start`) is removed; `Options.Env` stays (used for the resource and the FR-41 plaintext decision; the runtime passes a lookup over the same snapshot).
  - Return errors sorted by `setting` (FR-13 group 4).
- FRs/ACs: FR-19, FR-25; AC-6 (unit part: several `OTEL_*` errors reported), AC-13 (unit part: invalid sampler with tracing on), AC-15 (no globals in `env*.go`), AC-20, AC-21, AC-22 (`OTEL_*` trimming part).
- Implementer owns: `internal/infra/telemetry/tracing/env*.go`, `internal/infra/telemetry/tracing/tracing.go` (Options change only) (non-test), and the interim `Resolve` / `tracing.Options` call site in `internal/app/runtime/runtime.go` (see "Ordering and parallelism").
- Test writer owns: `internal/infra/telemetry/tracing/env*_test.go` (migrate to the new signature, a fake lookup and `memfs`; real-reader cases move to `osfile` tests or use `osfile.Read` explicitly), `internal/infra/telemetry/tracing/otlp_tls_http_test.go` and `sampler_test.go` (signature migration only), `tests/integration/tracing/helpers_test.go` and `bootstrap_extra_test.go` (migrate `Resolve` calls; keep `os.LookupEnv` + `osfile.Read` there because exporters read the real environment, SPEC-001 T15 (b)).
- Interfaces defined:
  - `tracing.ResolveInputs{Enabled bool; SampleRatio float64}`.
  - `tracing.Resolve(in ResolveInputs, env LookupEnv, read source.ReadFile) (enabled bool, cfg EnvConfig, errs []error)`.
  - `tracing.LookupEnv` unchanged (callers pass `source.Env.Lookup`).
- Test strategy:
  - Unit: every existing T10/T23 validation case through the new signature (behaviour parity).
  - Unit: three invalid variables → three errors, sorted; batch > queue reported only when neither has its own error; cert/key pairing matrix for both forms, including one side with its own error (no pairing record).
  - AC-20 (exact):
    - `OTEL_BSP_MAX_QUEUE_SIZE=abc` with a batch size above 2048 → one record, `OTEL_BSP_MAX_QUEUE_SIZE`;
    - a non-PEM `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` with a valid key → one record, the certificate variable;
    - queue 100 with batch 200 → one record, `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`.
  - AC-21 (exact): `OTEL_TRACES_EXPORTER=console` with an invalid `OTEL_EXPORTER_OTLP_PROTOCOL` and malformed `OTEL_EXPORTER_OTLP_HEADERS` → exactly three records, in order `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_PROTOCOL`, `OTEL_TRACES_EXPORTER`. Also run every per-variable check with an invalid exporter, to show parity with `otlp` and `none`.
  - AC-22 (`OTEL_*` part): values made only of U+00A0 (and the other FR-7 whitespace characters) count as unset.
  - Unit: `memfs` call counts: each PEM path read once even when used by individual and pair checks; 4 MiB + 1 rejected; `ErrNotRegular` rejected.
  - Unit: `OTEL_SDK_DISABLED=true` or `Enabled=false` → nothing read (zero `memfs` calls, lookup called only for `OTEL_SDK_DISABLED`).
  - Marker: no value, path or file content in any error.
  - Integration (`tests/integration/tracing`): existing suites pass after migration.
- Docker: no.
- Depends on: T2.

### T7: Configuration records (FR-8, FR-11, FR-20) in `internal/infra/config`

- Goal: the emitters for the configuration records, used by T12, with constant messages and the existing `setting` / errclass attributes only.
  - `LogErrors`: one FR-11 record per error, in the given order (via the existing `LogError`).
  - `LogUnknownEnv`: warn, message `unknown CORRAL_ environment variables ignored`, `setting` = names joined with `,`; no record for an empty list.
  - `LogLoaded`: info, message `configuration loaded`, `setting` = supplied names joined with `,` (empty string when none).
  - Message constants, including `MsgReloadUnsupported = "configuration reload not supported; restart required"` (used by T9).
  - These functions add no `component`; callers pass the bootstrap logger or `Factory.Logger(logging.Config)` (SPEC-001 rule).
- FRs/ACs: FR-8 (record), FR-11, FR-18 (message constant), FR-20 (record); AC-5 (record shape), AC-11 (record content: `admin.listen,log.level`, empty, no values).
- Implementer owns: new files `internal/infra/config/records.go` (and helpers) (non-test). `config.go` stays untouched until T12.
- Test writer owns: `internal/infra/config/records*_test.go`.
- Interfaces defined: `config.MsgInvalid`, `config.MsgUnknownEnv`, `config.MsgLoaded`, `config.MsgReloadUnsupported` (constants); `config.LogErrors(l *slog.Logger, errs []error)`; `config.LogUnknownEnv(l *slog.Logger, names []string)`; `config.LogLoaded(l *slog.Logger, supplied []string)`.
- Test strategy: unit with `logcheck` (keys allowed, component bound by caller), exact JSON fields, level filtering (at `warn`, `LogLoaded` emits nothing), marker check that only names appear.
- Docker: no.
- Depends on: none (uses existing `config.LogError`).

### T8: Help text renderer (FR-21)

- Goal: deterministic plain-text usage built only from the registry.
  - Header line, then one block per setting sorted by name: logical name, flag form (`--<name>=<value>`), environment variable, type (`types.Spec.Describe()`, enums list values), presence (default text verbatim, `(required)`, or `(optional, unset by default)`), the fixed secret text with names filled in for secrets, the description.
  - Control flags section (help, version, check-config spellings).
  - Fixed last line: `OTEL_* environment variables are honoured as described in the operator documentation.` (exact wording fixed in the implementation and documented in T15.)
- FRs/ACs: FR-21; AC-12 (unit part: content), AC-9 (help never contains values).
- Implementer owns: `internal/infra/config/usage/*.go` (non-test).
- Test writer owns: `internal/infra/config/usage/*_test.go`.
- Interfaces defined: `usage.Write(w io.Writer, r *registry.Registry) error`.
- Test strategy: unit with a test registry (secret, required, optional, enum, list default `a,b`, float default `1.0`): every field present per setting; secret text exact; sort order; golden output for the SPEC-001 registry is asserted in T10's tests (avoids a dependency on T10 here).
- Docker: no.
- Depends on: T1, T3.

### T9: SIGHUP watcher (FR-18)

- Goal: a watcher that drains a signal channel from the start of `main`, stays silent until a logger is set, then emits rate-limited warn records. Amendment 1 (FR-18, AC-19): the runtime sets a logger only in a normal run, never in help, version or check-config mode. A watcher without a logger stays silent for its whole life, so check-config output is exactly the FR-23 records.
  - `Reporter`: synchronous; `ratelimit.New(30*time.Second, now)`; on emit, warn `config.MsgReloadUnsupported`, with `suppressed_count` only when greater than zero.
  - `Watcher`: goroutine over the channel; holds an atomic logger pointer; drops signals while it is nil; `SetLogger` installs a `Reporter`; `Stop` ends the goroutine and is idempotent.
  - No `signal.Notify` inside this package (the entry point owns signal registration, ADR-0013).
- FRs/ACs: FR-18; AC-10 (unit: 6 SIGHUPs in 30 s → 1 record; next after 30 s → `suppressed_count=5`), AC-19 (unit part: no logger, no record).
- Implementer owns: `internal/infra/sighup/*.go` (non-test).
- Test writer owns: `internal/infra/sighup/*_test.go`.
- Interfaces defined: `sighup.NewReporter(l *slog.Logger, now func() time.Time) *Reporter`; `(*Reporter).Signal()`; `sighup.Start(ch <-chan os.Signal, now func() time.Time) *Watcher`; `(*Watcher).SetLogger(l *slog.Logger)`; `(*Watcher).Stop()`.
- Test strategy: unit with `fakeclock` and `logcheck` (AC-10 exact counts). Watcher: signals before `SetLogger` produce no record; after it, one record; a watcher that never gets a logger drains any number of signals with no record (the AC-19 precondition); `Stop` returns promptly; race detector clean.
- Docker: no.
- Depends on: T7 (message constant).

### T10: SPEC-001 settings declarations and typed view

- Goal: `internal/app/settings` with the SPEC-001 group exactly as SPEC-003 Interfaces declares it, the typed view the runtime uses, and the list of groups SPEC-002 will extend.
  - Group `SPEC-001`, namespaces `log`, `admin`, `tracing`:
    - `log.format`: enum `json`, `text` (case-insensitive), default `json`.
    - `log.level`: enum `debug`, `info`, `warn`, `error` (case-insensitive; offsets rejected by the enum), default `info`.
    - `admin.listen`: hostport, default `127.0.0.1:9187`.
    - `tracing.enabled`: bool, default `false`.
    - `tracing.sample_ratio`: float, default `1.0`, rule 0.0 ≤ v ≤ 1.0.
    - All non-secret; descriptions as in the Interfaces table; no cross rules.
  - `Observability` typed struct and `ObservabilityFrom(res loader.Result) Observability` (maps enum strings to `logging.Format` / `slog.Level`).
  - `Groups()` returns `[]registry.Group{observabilityGroup()}`; `Registry()` builds it.
  - Setting name constants reuse `config.Setting*` (kept by T12) or are re-declared here; `adminhttp` keeps using `config.SettingAdminListen`.
- FRs/ACs: FR-1 (SPEC-001 table), FR-7, FR-12 (rules narrowing types), FR-16 (typed values); AC-1 (real registry and SPEC-001 settings present), AC-2 (table-driven over every registered non-secret setting, real registry), AC-4 (`--log.level=` rejected), AC-17, AC-22 (`CORRAL_LOG_LEVEL` part).
- Implementer owns: `internal/app/settings/*.go` (non-test).
- Test writer owns: `internal/app/settings/*_test.go`.
- Interfaces defined: `settings.Groups() []registry.Group`; `settings.Registry() (*registry.Registry, error)`; `settings.Observability{LogFormat logging.Format; LogLevel slog.Level; AdminListen string; TracingEnabled bool; SampleRatio float64}`; `settings.ObservabilityFrom(loader.Result) Observability`; `settings.DefaultObservability() Observability`.
- Test strategy:
  - AC-1: real registry builds; every setting has a FR-12 type, valid presence, non-empty description, unique derived names; SPEC-001 settings present with types, presences, defaults.
  - AC-2: table over `Registry().Settings()` (non-secret): default, flag only, env only, both, invalid env with valid flag (`setting=CORRAL_<NAME>`). Valid/invalid sample values per type from a helper map keyed by setting name; the test fails if a registered setting has no sample (forces SPEC-002 to add samples).
  - AC-17: SPEC-001 AC-21 cases via flag and via env: `log.level=debug-4` and `admin.listen=:9187` rejected, `log.level=DEBUG` accepted, each SPEC-001 setting invalid.
  - AC-4: `--log.level=` rejected with `setting=log.level`.
  - AC-22 (`CORRAL_LOG_LEVEL` part, real registry):
    - a value made only of each of U+0009, U+000B, U+000C, U+0085, U+00A0, U+2003 and U+3000 → unset, the default `info` applies, and `log.level` is not in `Supplied`;
    - U+200B alone → set and rejected under `CORRAL_LOG_LEVEL`;
    - invalid UTF-8 bytes alone → set and rejected.
  - Golden help output for the SPEC-001 registry via `usage.Write` (FR-21).
- Docker: no.
- Depends on: T3, T5 (tests use the loader), T8 (golden help test).

### T11: Configuration outcome: loader plus `OTEL_*` validation

- Goal: the SPEC-003 "loader contract" as one app-layer function, joining T5 and T6 without an infra cycle (ADR-0011).
  - `Load`: build `source.Env` from `Inputs.Env`; run `loader.Load`; decide whether `tracing.enabled` is known (`!res.Failed("tracing.enabled")`); if known, run `tracing.Resolve` with the effective `tracing.enabled` and `tracing.sample_ratio` (default if that one failed) over the same snapshot and reader; append its errors as group 4. Compute the FR-41 names (`tracing.TLSIgnoredSettings`) on success.
  - Mode is taken from `loader.SelectMode`; for help and version `Load` returns immediately without reading anything.
  - Outcome is a value; no logging here.
- FRs/ACs: FR-10, FR-13 (all four groups), FR-19 (known / skip logic, same pass, Amendment 1 rules carried through from T6), FR-23 (validation scope), FR-25; AC-6 (unit: ordering across groups, `CORRAL_TRACING_ENABLED=maybe` skips `OTEL_*`), AC-13 (unit: FR-41 names computed in check-config), AC-15 (unit determinism), AC-20 and AC-21 (end-to-end through `Load`, records in group 4 after any CORRAL records), AC-22 (FR-41 part: an `OTEL_EXPORTER_OTLP_ENDPOINT` of only U+00A0 with a CA variable and tracing on → `TLSIgnored` empty).
- Implementer owns: `internal/app/configload/*.go` (non-test).
- Test writer owns: `internal/app/configload/*_test.go`.
- Interfaces defined:
  - `configload.Inputs{Args []string; Env []string; ReadFile source.ReadFile}`.
  - `configload.Outcome{Mode loader.Mode; Result loader.Result; Observability settings.Observability; TracingEnabled bool; Tracing tracing.EnvConfig; TLSIgnored []string; Errors []error}` (`Errors` = groups 1–4 in FR-13 order; `Result.UnknownEnv` and `Result.Supplied` pass through).
  - `configload.Load(r *registry.Registry, in Inputs) Outcome`. The registry must contain `tracing.enabled` and `tracing.sample_ratio` (it returns a programming-error outcome otherwise; test registries include the SPEC-001 group).
- Test strategy (real SPEC-001 group plus test group, fake env entries, `memfs`):
  - AC-6 unit equivalent: unknown flag + two invalid flags + one invalid env + `--tracing.enabled=true` + two invalid `OTEL_*` → exact order and names.
  - `CORRAL_TRACING_ENABLED=maybe`, no flag, invalid `OTEL_*` → only the CORRAL record.
  - Invalid overridden `CORRAL_TRACING_ENABLED` with valid `--tracing.enabled=true` → `OTEL_*` still validated.
  - `--tracing.enabled` group 1 error (repeat, missing value) → `OTEL_*` skipped.
  - `OTEL_SDK_DISABLED=true` → no `OTEL_*` errors, `tracing.enabled` still in `Supplied` when given.
  - `OTEL_*` never in `UnknownEnv` or `Supplied`.
  - Help / version: zero `memfs` calls.
  - Determinism: two runs `reflect.DeepEqual`.
  - AC-20 / AC-21 through `Load`, mixed with one invalid CORRAL flag: the CORRAL record comes first, then the `OTEL_*` records in the order given in T6.
  - AC-22: an endpoint of only U+00A0 plus a valid `OTEL_EXPORTER_OTLP_CERTIFICATE` and tracing on → no errors and an empty `TLSIgnored`.
- Docker: no.
- Depends on: T5, T6, T10.

### T12: Runtime, entry point, Makefile and interim-loader retirement

- Goal: wire SPEC-003 into the binary per ADR-0013; retire ADR-0003's API (ADR-0010).
  - `runtime.Run(ctx, Inputs, Hooks)`: start the SIGHUP watcher first; mode dispatch (help → `usage.Write`, version → `corral <Version>\n`, both exit 0 with no records); `Hooks.BeforeLogger`; `configload.Load`; failure path through `logging.BootstrapLogger` (FR-8 warn, then `config.LogErrors`), exit 2; success path: handler and factory, `logbridge.Install`, FR-8 warn, FR-41 warn (existing message and logic, now from `Outcome.TLSIgnored`), FR-20 info. In check-config mode, run `Hooks.BeforeCheckConfigExit` and then exit 0; the SIGHUP watcher never receives a logger (Amendment 1, FR-18, AC-19). In a normal run, call `watcher.SetLogger(config logger)` right after the FR-20 record, then continue with lifecycle `starting` and ADR-0006 steps 5–9 unchanged. Stop the watcher on return.
  - Exit codes: `exitOK=0`, `exitFailure=1`, `exitConfig=2`. Admin bind failure stays 1.
  - `tracing.Start` gets `Options.Env` = snapshot lookup; exporter keeps the real environment.
  - `cmd/corral/main.go`: `signal.Notify(hup, syscall.SIGHUP)` is the first action of `main` (Amendment 1, FR-18; P3 resolved). Signals before `main` starts are out of scope, so no `init`-time handler is added. Then build `runtime.Inputs` from `os.Args[1:]`, `os.Environ()`, `osfile.Read`, `os.Stdout`, `hup` and `version`; `var version = "devel"`.
  - `cmd/corral/testbuild_off.go` / `testbuild_on.go` per ADR-0014 (Accepted). `testbuild_on.go` maps `CORRALTEST_HOLD_AT=logger` to `Hooks.BeforeLogger` and `CORRALTEST_HOLD_AT=checkconfig` to `Hooks.BeforeCheckConfigExit`, both using the `CORRALTEST_HOLD_DIR` held/release protocol.
  - `Makefile`: `VERSION ?= devel`; `build` uses `go build -ldflags "-X main.version=$(VERSION)" -o bin/corral ./cmd/corral`. Quote the value; document that `VERSION` must not contain whitespace (FR-22 output is `corral <version>` on one line).
  - Remove from `internal/infra/config/config.go`: `Settings`, `Defaults`, `Parse`, `FromArgs`, the per-setting parsers and their sentinels. Keep `Error`, `NewError`, `ConfigSetting`, `LogError`, `Setting*` name constants, `SettingUnknown`, `SettingArgument`. Set `logging.BootstrapLogger`'s level to debug so that it filters nothing (FR-29). Today only warn and error records pass through it, so this is defensive. It is a one-line change in `internal/infra/telemetry/logging/factory.go`, owned here.
- FRs/ACs: FR-10, FR-17, FR-18 (entry and wiring, no record in check-config), FR-19 (JSON for `OTEL_*` errors), FR-21, FR-22, FR-23, FR-24, FR-27 (dispatch), FR-29; AC-5 (unit: warn first, JSON with `--log.format=text`), AC-7 (unit), AC-11 (unit: success order, level `warn` hides FR-20), AC-12 (unit: modes start nothing), AC-13 (unit: check-config binds nothing), AC-19 (in-process).
- Implementer owns: `internal/app/runtime/*.go`, `cmd/corral/*.go` (non-test), `Makefile`, `internal/infra/config/config.go` (removals only), `internal/infra/telemetry/logging/factory.go` (`BootstrapLogger` level only).
- Test writer owns: `internal/app/runtime/*_test.go` (migrate every existing test to `runtime.Inputs`; add SPEC-003 cases), `internal/infra/config/config_test.go` (delete cases for removed API; keep `Error` / `LogError` cases), `internal/infra/adminhttp/listen_test.go` (only if it used removed API), existing `tests/functional/*_test.go` adjustments listed under T13.
- Interfaces defined:
  - `runtime.Inputs{Args []string; Env []string; ReadFile source.ReadFile; Stdout io.Writer; HUP <-chan os.Signal; Version string; Registry *registry.Registry /* nil = settings.Registry() */; Now func() time.Time /* nil = time.Now; SIGHUP limiter clock */}`.
  - `runtime.Hooks` gains `BeforeLogger func(ctx context.Context)` (hold before the configured logger exists, AC-10) and `BeforeCheckConfigExit func(ctx context.Context)` (hold in check-config mode after the FR-20 record and before exit, AC-19).
  - `runtime.Run(ctx context.Context, in Inputs, hooks Hooks) int`.
  - `cmd/corral`: `var version = "devel"` (linker target `main.version`).
- Test strategy (in-process, `logcheck` tee, `markerscan.CaptureStdio`):
  - AC-11: with `--log.level=debug` and `CORRAL_ADMIN_LISTEN` set, record `setting=admin.listen,log.level`; with an unknown env var and an `http://` endpoint plus CA variable and tracing on: FR-8 warn, FR-41 warn, FR-20 info, lifecycle starting, in that order; at `--log.level=warn` no FR-20 record.
  - AC-5: unknown env + invalid setting with `--log.format=text` → warn then error, both JSON.
  - AC-7: invalid setting with the admin port pre-bound → 2, no lifecycle record.
  - Exit codes: config error 2; bind failure 1; graceful 0; check-config success 0, failure 2.
  - Help / version: output text, exit 0, no records, admin port free, `memfs` untouched.
  - SIGHUP in-process: send on `Inputs.HUP` during `BeforeLogger` → no record; after start in a normal run → one warn record, and the process continues.
  - AC-19 (in-process): check-config mode with SIGHUPs sent during `BeforeCheckConfigExit` → exit 0, and the output is exactly the FR-23 success records (FR-8 / FR-41 if applicable, then FR-20), with no reload record. Also cover SIGHUPs sent during `BeforeLogger` in help, version and check-config modes.
  - Regression: every pre-existing runtime test passes after migration.
- Docker: no.
- Depends on: T7, T8, T9, T10, T11.

### T13: Functional (binary) suite for SPEC-003

- Goal: binary-level ACs, plus the minimal adjustments to existing functional tests that SPEC-003 makes necessary. Test-only task; defects go back to the owning task's implementer.
  - `TestMain` builds the default binary (unchanged) and, per ADR-0014, a `-tags corraltest` binary.
  - Existing tests: flag syntax unchanged. Adjust only assertions that count all `component=config` records on a successful start: `TestAC30_AddressInUse` (filter to `msg == "invalid configuration"` or level ERROR) and `tests/functional/otlp_tls_http_test.go` (allow the FR-20 info record). Exit-code assertions `!= 0` stay valid (config errors are now 2).
  - New tests:
    - AC-5: unknown flag marker; the four env variables with marker values (exact warn `setting`); invalid-byte name; `OTEL_*` absent; warn then error as JSON with `--log.format=text`.
    - AC-6: the full mixed input with exact record order, exit 2, empty stderr, marker scan; required setting via `CORRALTEST_REGISTRY=required`; `CORRAL_TRACING_ENABLED=maybe` case.
    - AC-7: invalid setting with admin port pre-bound → 2, no lifecycle record.
    - AC-9: `corraltest` binary, `CORRALTEST_REGISTRY=secret`, `t.secret` via env literal and via file reference (markers in value and path), `--log.level=debug`; full start/stop, help mode, check-config mode; marker scan over stdout, stderr, `/metrics`, `/readyz`; FR-20 lists `t.secret`.
    - AC-10: SIGHUP to a running corral → still serving `/healthz`, one FR-18 warn record, SIGTERM exits 0. Hold case with `CORRALTEST_HOLD_AT=logger` → SIGHUP while held → process alive, no record from it, release, normal start.
    - AC-19: `corraltest` binary, `--check-config`, `CORRALTEST_HOLD_AT=checkconfig` → wait for `held`, send SIGHUP (several times), release → exit 0; stdout is exactly the FR-23 success records, with no reload record and nothing on stderr.
    - AC-12: `--help`, `-help`, `-h` content for every registered setting; `CORRAL_LOG_LEVEL=<marker>` absent from help; every FR-27 combination row; `--version` matches `^corral \S+$`, test build prints `corral devel`; admin port free and no records in both modes. Also a `make build`-style stamped build (`-ldflags -X main.version=<synthetic>`) prints that version.
    - AC-13: `--check-config --log.level=info` valid → 0, FR-20 record, no lifecycle, ports free, test collector (`otlpcollector`) with tracing on sees zero connections; `--log.format=text` success output is slog text; invalid `OTEL_TRACES_SAMPLER` → 2 with JSON record; `http://` endpoint + valid CA → FR-41 before FR-20.
- FRs/ACs: AC-5, AC-6, AC-7, AC-9, AC-10 (functional), AC-12, AC-13, AC-19 (functional).
- Implementer owns: none.
- Test writer owns: `tests/functional/**`.
- Interfaces defined: none.
- Docker: no.
- Depends on: T12.

### T14: Static checks (AC-14, AC-15) and test-build guard

- Goal:
  - AC-14: the domain import check also fails when a domain package imports the registry or loader. The existing rule (stdlib and domain only) already forbids it; add an explicit fixture domain package importing a `.../config/registry`-shaped and a `.../config/loader`-shaped package and assert both are reported.
  - AC-15 static: no reference to `os.Args`, `os.Getenv`, `os.LookupEnv`, `os.Environ`, `syscall.Getenv`, `syscall.Environ`, `os.ReadFile`, `os.Open`, `os.OpenFile`, `os.Stat`, `os.Lstat` in: `internal/infra/config/...`, `internal/app/configload`, and the `env*.go` files of `internal/infra/telemetry/tracing`. Detection by resolved object (`types.Info.Uses`), so import aliases, dot-imports and method values are caught.
  - ADR-0014 guard: the default build's `./cmd/...` closure has no `t.*` setting (check `settings.Registry()` has no namespace `t`), and `cmd/corral/testbuild_on.go` carries the `corraltest` constraint.
- FRs/ACs: FR-16, FR-25; AC-14, AC-15 (static).
- Implementer owns: `internal/testkit/symcheck/*.go` (non-test).
- Test writer owns: `internal/testkit/symcheck/*_test.go` (+ `testdata/`), `tests/lint/config_globals_test.go`, `tests/lint/domain_imports_test.go` and `tests/lint/testdata/fixture/**` (new fixture case), `tests/lint/testbuild_test.go`.
- Interfaces defined: `symcheck.Forbidden(dir string, patterns []string, fileFilter func(path string) bool, symbols []string) ([]Finding, error)`; `symcheck.Finding{Package, File string; Line int; Symbol string}`. Uses `golang.org/x/tools/go/packages` with the hermetic environment of `importcheck` (`GOENV=off`, `GOPACKAGESDRIVER=off`); rejects patterns starting with `-`; an empty match is an error.
- Test strategy: self-test fixture with each forbidden symbol, an aliased `os` import, a method value (`f := os.Getenv`), and a clean file; real-tree test passes; negative control that a file outside the filter is not reported.
- Docker: no.
- Depends on: T5, T6, T11 (real-tree targets exist); T12 for the test-build guard. `symcheck` itself can start in wave 1.

### T15: Operator documentation (FR-26) and AC-16

- Goal: `docs/operations/configuration.md` with every FR-26 topic; update `docs/operations/observability.md`.
  - Reference entry per registered setting with the FR-21 fields (name, flag, env, type, presence, secret note, description).
  - FR-3 mapping, FR-4 precedence, FR-7 empty-value rule and its asymmetry with flags, FR-5 no merging, list syntax.
  - FR-14 / FR-15 secret rules, including the advice to rotate any secret passed on a command line even when rejected; 64 KiB limit; one trailing newline removed; no permission check.
  - FR-17 / FR-18 restart requirement and SIGHUP behaviour: handled from the first action of `main` (a SIGHUP before that may terminate the process); no record before logging is configured, in help, version or check-config mode; rate-limited warn records in a normal run.
  - FR-7 whitespace set (Amendment 1): the `unicode.IsSpace` set, for example non-breaking space U+00A0 counts as whitespace and zero-width space U+200B does not. List trimming covers only ASCII space and tab.
  - FR-8 Kubernetes service-link note and `enableServiceLinks: false`.
  - FR-21 to FR-23 and FR-27 control flags, with the FR-27 combinations table; `make build VERSION=...`.
  - FR-24 exit codes, including the Go runtime panic exit code 2.
  - `OTEL_*`: pointer to `observability.md`; errors now reported as JSON in the same pass.
  - `observability.md`: replace "Interim flag syntax (temporary)" with a short pointer to `configuration.md`; keep the `OTEL_*` reference there.
  - Examples use placeholders only (`<YOUR_DB_PASSWORD>`, `/run/secrets/<name>`), never realistic credentials.
- FRs/ACs: FR-26; AC-16.
- Implementer owns: `docs/operations/configuration.md`, `docs/operations/observability.md`.
- Test writer owns: `internal/app/settings/docs_test.go` (AC-16: every registered setting's logical name and env name appear in the file; reads the file by path relative to the repo root; no `os` restriction applies to test code).
- Interfaces defined: none.
- Docker: no.
- Depends on: T10, T12 (facts stable). Reviewed for the non-AC-16 topics.

---

## Ordering and parallelism

- Wave 1: T1, T2, T7, `symcheck` part of T14 (independent). **Done at 691e9f3.**
- Wave 2: T1.1 (needs T1), T2.1 (needs T2), T3 (needs T1; may start before T1.1 because the registry only calls `types.Spec`), T6 (needs T2), T9 (needs T7).
- Wave 3: T4 (needs T3), T8 (needs T1, T3).
- Wave 4: T5 (needs T2, T3, T4).
- Wave 5: T10 (needs T3, T5, T8).
- Wave 6: T11 (needs T5, T6, T10).
- Wave 7: T12 (needs T7, T8, T9, T10, T11). From here `make test-functional` is a gate again.
- Wave 8: T13, T14 (real-tree and test-build parts), T15 (parallel).

Until T12 lands, the runtime keeps using the interim loader and the old `tracing.Resolve` call. T6 changes `Resolve`'s signature and removes `tracing.Options.Settings`, so **T6 includes a minimal call-site update in `internal/app/runtime/runtime.go`**: pass `ResolveInputs{settings.TracingEnabled, settings.SampleRatio}`, `env` and `osfile.Read`; log every returned error with `config.LogError` as before (exit 1, interim); drop the `Settings:` field from `tracing.Options`. The implementer of T6 owns those lines until T12 replaces them. Existing runtime tests must stay green after T6.

## Coverage table

| AC | Task(s) |
|---|---|
| AC-1 | T3 (refusal cases, derived-name bypass test), T10 (real registry, SPEC-001 settings) |
| AC-2 | T5 (test registry, list case), T10 (every registered non-secret setting) |
| AC-3 | T4 (including the Amendment 1 occurrence-rule cases) |
| AC-4 | T1 (types), T1.1 (hostport trailing dot and zone, list U+00A0), T2 (snapshot), T5 (env empty, `_FILE` empty), T10 (`--log.level=` rejected) |
| AC-5 | T5 (unknown env list), T7 (record), T12 (order, JSON with text format), T13 (functional) |
| AC-6 | T6 (all `OTEL_*` errors), T11 (unit order and skip), T13 (functional) |
| AC-7 | T12 (unit), T13 (functional) |
| AC-8 | T2 (real reader: FIFO, directory, sizes), T4 (two-record literal plus repeated `_file`), T5 (all loader cases with `memfs`, reader-call counts) |
| AC-9 | T8 (help has no values), T13 (functional marker scan) |
| AC-10 | T9 (fake-clock unit), T5 (reader called once), T12 (in-process), T13 (functional, hold hook) |
| AC-11 | T7 (record content), T12 (order, level) |
| AC-12 | T4 (mode table), T8 (help content), T10 (golden help), T12 (unit), T13 (functional) |
| AC-13 | T6, T11 (unit), T12 (unit), T13 (functional) |
| AC-14 | T14 |
| AC-15 | T5, T11 (determinism), T14 (static) |
| AC-16 | T15 |
| AC-17 | T1 (types), T10 (both sources) |
| AC-18 | T5 |
| AC-19 | T9 (no logger, no record), T12 (in-process, `BeforeCheckConfigExit`), T13 (functional, `CORRALTEST_HOLD_AT=checkconfig`) |
| AC-20 | T6 (unit, exact), T11 (through `Load`) |
| AC-21 | T6 (unit, exact), T11 (through `Load`) |
| AC-22 | T2.1 (`IsSet` set), T5 (loader with test registry), T10 (`CORRAL_LOG_LEVEL`), T6 (`OTEL_*` trimming), T11 (U+00A0 endpoint, no FR-41) |

FR coverage summary: FR-1 T3/T10; FR-2 T3; FR-3 T3; FR-4/FR-5 T5; FR-6 T4; FR-7 T2/T2.1/T5/T10; FR-8 T4/T5/T7; FR-9 T5; FR-10 T5/T11/T12; FR-11 T5/T7; FR-12 T1/T1.1/T10; FR-13 T4/T5/T11; FR-14 T3/T4/T5; FR-15 T2/T3/T5; FR-16 T10/T14; FR-17 T5/T12; FR-18 T9/T12; FR-19 T6/T11/T12; FR-20 T5/T7/T12; FR-21 T8/T12; FR-22 T12; FR-23 T11/T12; FR-24 T12; FR-25 T2/T5/T6/T11/T14; FR-26 T15; FR-27 T4/T12; FR-28 T4/T5; FR-29 T12; FR-30 T3/T5.

Review-only items: FR-26 topics other than names (T15); no value, path or content in any error text or record (all tasks; backed by marker scans in T5, T6, T12, T13); standard library only for new production code (all).

## Notes for the Orchestrator

- **Existing functional tests.** Their `-name=value` flags keep working unchanged (FR-3, FR-6). Two existing assertions conflict with the new FR-20 `configuration loaded` info record (`component=config`) on successful starts, and T13 adjusts them: `TestAC30_AddressInUse` counts all `component=config` records and expects exactly one, and `tests/functional/otlp_tls_http_test.go` treats any non-WARN `component=config` record as unexpected. Similar in-process checks in `internal/app/runtime/otlp_tls_http*_test.go` are migrated in T12. This is a spec-required change, not a regression.
- **SIGHUP before `main`.** Out of scope per Amendment 1 (FR-18, Non-goals). SIGHUP is handled from the first action of `main`.
## Proposed for Owner

Nothing is open. P1 to P5 were resolved on 2026-10-24:

- **P1 (resolved: approved by the Owner).** A test-only `-tags corraltest` binary is used for the binary-level cases that need a required setting, a secret setting or a hold hook (AC-6, AC-9, AC-10, and also AC-19). ADR-0014 is Accepted. The production build is unchanged.
- **P2 (resolved by SPEC-003 Amendment 1, FR-12 / AC-4).** `hostport` rejects a trailing dot and IPv6 zone identifiers. Applied in T1.1.
- **P3 (resolved by Amendment 1, FR-18).** The SIGHUP handler is installed as the first action of `main`; the pre-`main` window is out of scope. Also, check-config mode never emits a SIGHUP record (AC-19). Applied in T9, T12 and T13.
- **P4 (resolved by Amendment 1, FR-28).** `--a.b=v --a.b_file=p1 --a.b_file=p2` gives two group 1 records (`a.b` at the literal's position, then `a.b_file` at the second `_file` occurrence), and no file is read. Applied in T4 and T5.
- **P5 (resolved by Amendment 1, FR-28 occurrence rule).** Every occurrence counts toward repeats, with or without `=`. Two or more occurrences give one repeat record at the second occurrence, with no missing-value record. A single bare occurrence gives a missing-value record. For secret flags, any occurrence with `=` gives the single secret-literal record at the first such occurrence. Applied in T4.

Amendment 1 also added AC-19 to AC-22: no SIGHUP record in check-config mode; `OTEL_*` cross-variable checks skipped when an involved variable has its own error; an invalid exporter does not stop other `OTEL_*` checks; the whitespace set. They are mapped in the coverage table.
