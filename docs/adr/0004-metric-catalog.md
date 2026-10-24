# ADR-0004: Metric catalog format and catalog-backed constructors

- Status: Accepted (planner, 2026-10-24). The error-label names were decided
  by Orchestrator under Owner's standing instruction, 2026-10-24, Owner may
  override (former question Q4, decision D4 in `plans/SPEC-001.md`).
- Related: SPEC-001 FR-10, FR-11, FR-13, FR-29, FR-31, FR-37, AC-8, AC-10

## Context

FR-31 requires one machine-readable metric catalog listing name, type, help,
label names and allowed label values, and a constructor that cannot create a
`corral_` metric absent from the catalog. AC-8 lints the catalog.

## Decision

- File: `internal/infra/telemetry/metrics/catalog.json`, embedded with
  `go:embed`. JSON keeps the dependency set to the standard library.
- Schema (one object, key `metrics`, an array). Each entry has:
  `name` (string), `type` (`counter` | `gauge` | `histogram`), `help`
  (string), `labels` (array of `{ "name": string, "values": [string, ...] }`,
  may be empty), and for histograms an optional `buckets` (array of numbers).
  Unknown fields are a parse error.
- Package `internal/infra/telemetry/metrics` provides:
  - catalog parsing and a `Lint` function returning all rule violations
    (FR-10, FR-11, FR-13 series cap, FR-29 enums for error labels);
  - a factory bound to a `prometheus.Registerer` and the catalog. Its
    constructors take a metric *name* only; type, help and labels come from
    the catalog. A name not in the catalog, or a type mismatch, returns an
    error and registers nothing. Vector metrics refuse label values outside
    the declared set;
  - a registry constructor that registers the Go runtime and process
    collectors (FR-14).
- Startup lints the embedded catalog and fails as an `internal` error if it
  has problems, so a bad catalog cannot ship even if a test is skipped.
- Error labels (D4): the label holding the FR-29 error class is named
  `error_class` and the label holding the FR-29 detail is named
  `error_detail`. FR-10 requires snake_case label names, so the span/log key
  `corral.error.detail` cannot be used verbatim as a Prometheus label name.
  The lint checks that values of a label named `error_class` are within the
  FR-29 class enum and values of `error_detail` within the detail enum. The
  spec writer is amending AC-8's wording to use these names.

## Consequences

- Feature specs add metrics by editing `catalog.json` (and amending their
  spec). AC-10 (Part B) can compute the series cap from the file alone.
- promlint (AC-8) runs over gathered `corral_` families from the live
  registry, not over the JSON.
