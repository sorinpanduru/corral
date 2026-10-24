# ADR-0014: `corraltest` build tag for binary-level configuration tests

- Status: Accepted (Owner, 2026-10-24; P1 in `plans/SPEC-003.md` approved).
  Updated the same day for SPEC-003 Amendment 1 (AC-19 check-config hold
  point). Extended by ADR-0022 (SPEC-002): more hold points, pool order,
  panic injection and a loopback probe server, all in the `corraltest`
  build only.
- Related: SPEC-003 AC-6, AC-9, AC-10, AC-19; ADR-0001, ADR-0005; SPEC-001
  T21 (`importcheck.ForbiddenDeps`)

## Context

Three functional (binary-level) ACs need things the production binary does
not have:

- AC-6: "a test registry containing a required setting that is not
  supplied". SPEC-001's settings all have defaults.
- AC-9: a secret setting supplied through an environment literal and a file
  reference. No approved spec declares a secret setting yet (SPEC-002 will).
- AC-10: SIGHUP sent "while startup is held by a test hook before the
  configured logger exists".
- AC-19 (Amendment 1): in check-config mode, SIGHUP sent while a test hook
  holds the process "after the configured logger is built and before exit".

## Decision

- `cmd/corral` gets two files with exclusive build constraints:
  - `testbuild_off.go` (`//go:build !corraltest`): returns no extra
    registry and no hooks. This is what `make build` and every release build
    compile.
  - `testbuild_on.go` (`//go:build corraltest`): reads two variables whose
    prefix is `CORRALTEST_` (not `CORRAL_`, so they are never CORRAL
    variables under FR-8):
    - `CORRALTEST_REGISTRY`: `secret` adds a test group with namespace `t`
      and an optional secret `t.secret` of type `text`; `required` adds a
      required `t.required` of type `string`. Unset: production registry
      only.
    - `CORRALTEST_HOLD_DIR` and `CORRALTEST_HOLD_AT`: if both are set, the
      selected hook creates `<dir>/held` and then waits (bounded, 30 s)
      until `<dir>/release` exists. `CORRALTEST_HOLD_AT=logger` selects
      `Hooks.BeforeLogger`, which runs before the configured logger exists
      (AC-10). `CORRALTEST_HOLD_AT=checkconfig` selects
      `Hooks.BeforeCheckConfigExit`, which runs in check-config mode after
      the FR-20 record and before exit (AC-19). Any other value of
      `CORRALTEST_HOLD_AT` holds nothing.
- The functional `TestMain` builds both binaries: the default one (used by
  every existing test) and `-tags corraltest` (used only by the AC-6
  required case, AC-9, the AC-10 hold case and AC-19).
- The `t` namespace exists only in the test build. It is not owned by any
  spec and must never appear in `settings.Groups()`.
- A lint test (T14) asserts that the default build's dependency closure and
  registry contain no `t.*` setting, and that `testbuild_on.go` carries the
  `corraltest` constraint.

## Consequences

- The production binary is unchanged by the hooks. The test binary differs
  only in the `CORRALTEST_*` behaviours above.
- When SPEC-002 declares real secret settings, AC-9 can move to the
  production binary, and the `secret` registry option can be removed.
- The verifier's scenarios that need a test registry or a hold point use
  the `corraltest` binary; all other scenarios use the default build.
