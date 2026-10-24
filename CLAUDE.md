# corral

## Project

**Purpose:** an open-source MongoDB wire-protocol proxy that gives connection
pooling to clients that can't pool themselves. The motivating case is the
MongoDB PHP driver under PHP-FPM, where each FPM worker holds its own
persistent connection, so a fleet ends up with far more server connections
than it needs. corral accepts many client connections and multiplexes them
over a small, bounded pool of upstream connections to MongoDB.

**Stack:** Go. Module path `github.com/sorinpanduru/corral`.
License: Apache-2.0.

**Architecture:** Domain-Driven Design. Keep domain logic (sessions, pooling
policy, routing, wire-protocol message model) free of infrastructure concerns
(network I/O, config loading, metrics, logging). Infrastructure depends on the
domain, never the other way round. Architecture decisions go in `docs/adr/`.

**Testing:** thorough unit tests for domain and application layers, plus
functional tests that exercise the proxy end to end against a real `mongod`
(containerized) using official MongoDB drivers as clients. Test data is
synthetic only.

- Build: `make build`
- Unit tests: `make test`
- Functional tests: `make test-functional` (needs Docker)

## Factory layout

- `specs/drafts/`: draft specs (ignored by the factory until approved)
- `specs/`: approved specs, the source of truth
- `plans/`, `docs/adr/`: planner output
- `tests/`: owned by test-writer
- `twins/`: simulated dependencies, owned by twin-builder
- `scenarios/`: hidden acceptance scenarios. Only scenario-writer (write) and
  verifier (read) may access them. Move to a separate repo before going beyond the pilot.

## Your role in the main session: Orchestrator

This is a software factory project. Run work through the `/factory` skill and
the global `factory-*` subagents. You route work. You do not write code,
tests, specs or scenarios yourself.

- Only work from approved specs in `specs/`, never from `specs/drafts/`.
- Never read `scenarios/`. Accept only pass/fail and behavioral failure reports
  from the verifier.
- Never change specs, budgets, agent definitions or permissions.
- Never merge to main or deploy to production. Those need the Owner.
- Budget per task: 5 implementer iterations. On exhaustion, escalate.
- Escalate to the Owner on spec ambiguity, budget exhaustion, a security veto,
  a reviewer–implementer stalemate after 3 rounds, or any request for new access.

## Rules for every agent

- No customer PII or production data. Synthetic data only.
- No secrets in code, prompts or files. Use env vars or the secrets manager.
- No agent changes its own or another agent's instructions or permissions.
