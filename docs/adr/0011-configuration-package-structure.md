# ADR-0011: Configuration package structure, layering and per-spec registration

- Status: Accepted (planner, 2026-10-24, for SPEC-003)
- Related: SPEC-003 FR-1, FR-2, FR-12, FR-16, FR-19, FR-25, AC-1, AC-14,
  AC-15; ADR-0001 (layering); `plans/SPEC-003.md`

## Context

SPEC-003 needs one mechanism that every feature spec (SPEC-001 now,
SPEC-002 next) uses to declare settings without new loader code (FR-1). The
loader and the `OTEL_*` validation must be pure functions of injected
inputs (FR-25), checked statically (AC-15). Domain packages must never see
the registry or loader (FR-16, AC-14). `tracing` already imports
`internal/infra/config` (for `config.Error`), so the loader must not import
`tracing`, or there is a cycle.

## Decision

### Packages

| Package | Layer | Contents | May use `os` globals |
|---|---|---|---|
| `internal/infra/config` (root, existing) | infra | `config.Error`, `NewError`, `LogError`, record emitters (FR-8, FR-11, FR-20 records), message constants, special setting names (`unknown`, `argument`) | no |
| `internal/infra/config/types` | infra | FR-12 type set: `types.Spec` and its `Parse`; constant error sentinels | no |
| `internal/infra/config/source` | infra | environment snapshot (`source.Env`), file-reader contract (`source.ReadFile` and its three failure sentinels), size constants | no |
| `internal/infra/config/registry` | infra | `Decl`, `CrossRule`, `Group`, `Registry`, name derivation, registry build checks | no |
| `internal/infra/config/loader` | infra | control-flag mode selection, command-line parser, resolution across sources, validation, ordering | no |
| `internal/infra/config/usage` | infra | FR-21 help text renderer | no |
| `internal/infra/osfile` | infra | the real `source.ReadFile` (stat, `O_NONBLOCK` open, fstat, bounded read) | yes (only here) |
| `internal/infra/sighup` | infra | SIGHUP watcher and rate-limited reporter (FR-18) | `os.Signal` type only |
| `internal/app/settings` | app | the declarations of every spec, one `registry.Group` per spec; typed extraction structs | no |
| `internal/app/configload` | app | the SPEC-003 "loader contract": runs the loader, then `tracing.Resolve` when `tracing.enabled` is known; merges FR-13 group 4 | no |
| `internal/app/runtime` | app | mode dispatch, output path, startup sequence (ADR-0013) | no (receives everything through `runtime.Inputs`) |
| `cmd/corral` | entry | binds `os.Args`, `os.Environ()`, `osfile.Read`, `os.Stdout`, the SIGHUP channel and the linker-stamped version | yes |

Dependency direction: `cmd` -> `app/runtime` -> `app/configload` ->
{`app/settings`, `infra/config/loader`, `infra/telemetry/tracing`} ->
`infra/config/{registry,types,source}` -> `infra/config` (root). The loader
does not import `tracing`; `configload` (app layer) joins them.

`tracing.Resolve` takes an injected lookup derived from the same
`source.Env` snapshot and the injected `source.ReadFile` (4 MiB PEM limit).
The OTLP exporters keep reading the real process environment (SPEC-003
FR-19 "injection covers validation only").

### Per-spec registration (for SPEC-002 and later)

- Each spec contributes exactly one `registry.Group{Spec, Namespaces,
  Decls, Rules}` from a file in `internal/app/settings` named after the
  spec's area (`observability.go` for SPEC-001; SPEC-002 adds for example
  `proxy.go`). `settings.Groups()` lists every group; it is the only place a
  new group is wired in.
- `registry.New(groups...)` enforces mechanically what FR-2 leaves to review
  where it can: every declaration's first segment must be one of its group's
  `Namespaces`, and a namespace may be claimed by only one group. Together
  with the derived-name uniqueness check this catches cross-spec collisions
  at test time (AC-1).
- The same group file defines the typed view of its settings (for SPEC-001,
  `settings.Observability` with `logging.Format`, `slog.Level`, ...), built
  from the loader output by a `...From(values)` function. Optional settings
  map to `(T, bool)` or a pointer, never to an empty string (FR-9, FR-16).
- Domain packages receive these typed values from the application layer
  (constructor arguments or domain-owned option structs). They never import
  any `internal/infra/config/...` or `internal/app/settings` package. AC-14
  is enforced by the existing domain import check (stdlib and domain only),
  plus an explicit fixture case for the config packages (T14).
- A new FR-12 type needs a SPEC-003 amendment and a change to
  `internal/infra/config/types`. Feature specs narrow types with a `Rule`
  function on the declaration.

### Value representation

`types.Spec.Parse` returns a typed Go value: `string` (string, text, enum
canonical declared spelling, hostport as validated text), `bool`, `int64`,
`float64`, `time.Duration`, or a typed slice for lists (`[]string`,
`[]int64`, ...). Registry defaults are parsed once at `registry.New` and must
pass type and rule (FR-1). Error values are constant sentinels; nothing
derived from the input is kept in an error.

## Consequences

- The static check of AC-15 targets `internal/infra/config/...` (excluding
  nothing: `osfile` is outside that tree), `internal/app/configload`, and the
  `env*.go` files of `internal/infra/telemetry/tracing`.
- Unit tests build their own registries (for example a `t` namespace with
  `t.secret`) through `registry.New`, so the loader is tested independently
  of SPEC-001's settings.
- SPEC-002 adds settings by adding one group file and one line in
  `settings.Groups()`; no loader, registry or runtime change.
