# ADR-0014: `corraltest` build tag for binary-level configuration tests

- Status: Proposed (planner, 2026-10-24, for SPEC-003). Open for the Owner as
  P1 in `plans/SPEC-003.md`. Tasks T12 and T13 are planned on this basis.
- Related: SPEC-003 AC-6, AC-9, AC-10; ADR-0001, ADR-0005; SPEC-001 T21
  (`importcheck.ForbiddenDeps`)

## Context

Three functional (binary-level) ACs need things the production binary does
not have:

- AC-6: "a test registry containing a required setting that is not
  supplied". SPEC-001's settings all have defaults.
- AC-9: a secret setting supplied through an environment literal and a file
  reference. No approved spec declares a secret setting yet (SPEC-002 will).
- AC-10: SIGHUP sent "while startup is held by a test hook before the
  configured logger exists".

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
    - `CORRALTEST_HOLD_DIR`: if set, `Hooks.BeforeLogger` creates
      `<dir>/held` and then waits (bounded, 30 s) until `<dir>/release`
      exists.
- The functional `TestMain` builds both binaries: the default one (used by
  every existing test) and `-tags corraltest` (used only by AC-6 required,
  AC-9 and AC-10 hold cases).
- The `t` namespace exists only in the test build. It is not owned by any
  spec and must never appear in `settings.Groups()`.
- A lint test (T14) asserts that the default build's dependency closure and
  registry contain no `t.*` setting, and that `testbuild_on.go` carries the
  `corraltest` constraint.

## Consequences

- The production binary is unchanged by the hooks. The test binary differs
  only in the two `CORRALTEST_*` behaviours above.
- When SPEC-002 declares real secret settings, AC-9 can move to the
  production binary, and the `secret` registry option can be removed.
- If the Owner rejects a test build, the alternative is to cover AC-6
  (required part), AC-9 and AC-10 (hold part) in-process through
  `runtime.Run` with an injected registry and hook, and to state that those
  parts are not binary-level.
