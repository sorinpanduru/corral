# ADR-0013: Startup modes, configuration output path, SIGHUP and exit codes

- Status: Accepted (planner, 2026-10-24, for SPEC-003). Amends ADR-0006:
  its steps 0 to 4 are replaced by the sequence below; steps 5 to 9,
  readiness and shutdown are unchanged. Updated 2026-10-24 for SPEC-003
  Amendment 1 (FR-18, AC-19). The SIGHUP watcher now gets a logger only in
  a normal run. The check-config mode has a test hold point. The pre-`main`
  window is out of scope.
- Related: SPEC-003 FR-10, FR-17, FR-18, FR-19, FR-20 to FR-24, FR-27,
  FR-29; SPEC-001 FR-27, FR-41; ADR-0006, ADR-0008, ADR-0009

## Decision

### Entry (`cmd/corral`)

1. First statement of `main`: `signal.Notify(hup, syscall.SIGHUP)` on a
   buffered channel, so the Go default action (terminate) is replaced before
   any argument is looked at (FR-18).
2. `signal.NotifyContext` for SIGINT and SIGTERM (unchanged).
3. `runtime.Run(ctx, runtime.Inputs{Args: os.Args[1:], Env: os.Environ(),
   ReadFile: osfile.Read, Stdout: os.Stdout, HUP: hup, Version: version},
   hooks)`.
4. `version` is a package variable in `cmd/corral`, default `devel`, set by
   `make build` with `-ldflags "-X main.version=$(VERSION)"`, `VERSION ?=
   devel` (FR-22).

SIGHUP during Go runtime start-up and package `init` (before `main`) still
has the default action. SPEC-003 Amendment 1 (FR-18, Non-goals) places this
window out of scope.

### `runtime.Run`

- Step A: start the SIGHUP watcher on `Inputs.HUP` with no logger: every
  signal is drained and dropped without a record.
- Step B: `loader.SelectMode(args)` (FR-27).
  - Help: write `usage.Write(stdout, registry)`; return 0. No logger, no
    record.
  - Version: write `corral <version>\n`; return 0. No logger, no record.
- Step C (`Hooks.BeforeLogger`, tests only): hold point before the
  configured logger exists (AC-10).
- Step D: `configload.Load(registry, inputs)`: loader, then `OTEL_*`
  validation when `tracing.enabled` is known (FR-19). No third-party code
  runs, nothing binds or connects.
- Failure path (any error, FR-29): through `logging.BootstrapLogger(stdout)`
  only: the FR-8 warn record (if any), then every FR-11 record in FR-13
  order. Return 2. No configured logger, no log bridges, no FR-41, no FR-20,
  no lifecycle record.
- Success path:
  1. Build the configured handler and factory (ADR-0006 step 1); wrap trace
     correlation when tracing is enabled.
  2. `logbridge.Install` (ADR-0006 step 2). It may replay early `thirdparty`
     counts (ADR-0008); such records can precede the records below. They are
     not configuration records and FR-29 does not order them.
  3. FR-8 warn record, SPEC-001 FR-41 warn record (if applicable), FR-20
     info record, each through the configured `config` logger and level.
  4. Check-config mode: run `Hooks.BeforeCheckConfigExit` (tests only),
     then return 0. Nothing binds, connects or exports; `tracing.Start` is
     not called. The `sighup` watcher never receives a logger in this mode,
     so a SIGHUP produces no record (SPEC-003 Amendment 1, FR-18, AC-19).
  5. Normal mode only: the `sighup` watcher gets the `config` logger. From
     here, each SIGHUP produces an FR-18 warn record, rate-limited to one
     per 30 s, with `suppressed_count` only when it is non-zero. Then the
     lifecycle `starting` record, then ADR-0006 steps 5 to 9.

  The watcher never receives a logger in help, version or check-config mode
  (SPEC-003 Amendment 1, FR-18, AC-19).

### Exit codes (FR-24)

`runtime` defines `exitOK = 0`, `exitFailure = 1`, `exitConfig = 2`. Exit 2
is returned only from the failure path above. Admin bind failure stays 1
(SPEC-001 FR-35). A panic exiting through the Go runtime also yields 2; that
is documented, not prevented.

### Registry choice

`runtime.Inputs.Registry` selects the registry. `nil` means
`settings.Registry()` (all spec groups). Tests, and the `corraltest` build
(ADR-0014), pass their own.

## Consequences

- `runtime.Run`'s signature changes; in-process tests and integration tests
  migrate to `runtime.Inputs` (T12).
- Bootstrap records are JSON even with `--log.format=text` (FR-19
  behaviour change, AC-5, AC-6).
- The FR-20 record is a new `component=config` info record on every
  successful start at `log.level` info or debug.
