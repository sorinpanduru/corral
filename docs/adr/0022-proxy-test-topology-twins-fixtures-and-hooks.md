# ADR-0022: Proxy test topology: twins, Docker fixtures, PHP-FPM and corraltest hooks

- Status: Accepted (planner, 2026-10-24, for SPEC-002). Extends ADR-0014 and
  ADR-0005. The new network access (Docker on Linux with host networking,
  Docker Hub images, PECL, all pinned) was granted by the Owner on
  2026-10-24 (P1 in `plans/SPEC-002.md`). SPEC-002 Amendment 1 (FR-64)
  makes the hook set normative. Updated the same day.
- Related: SPEC-002 FR-54, AC types (unit, twin, functional), AC-1, AC-12,
  AC-14, AC-16, AC-19 to AC-22, AC-26, AC-30, AC-35; ADR-0001, ADR-0005,
  ADR-0014

## Decision

### Test layers and build tags

| Layer | Location | Build tag | Make target | Needs |
|---|---|---|---|---|
| unit / domain / in-process integration | co-located `_test.go`, `tests/integration/...`, `tests/lint/...` | none | `make test` | nothing |
| twin | `tests/twin/...` | `twin` | `make test-twin` (new; depends on `build`) | corral binary (default and `-tags corraltest`), twins in process |
| functional | `tests/functional/...` | `functional` | `make test-functional` (now also runs Docker suites) | corral binary, Docker on Linux |

`make test` stays hermetic. `make test-twin` needs no Docker.

### Twins (built by the twin-builder in `twins/`)

- `twins/mongosim`: simulated MongoDB upstream (wire level, scriptable
  topology, SCRAM server, fault rules, recording).
- `twins/faultproxy`: fault-injecting TCP intermediary in front of a real
  `mongod`, with per-connection command recording and `hello`
  host rewriting.

Contracts are in `plans/SPEC-002.md`, "Twins". Twins are Go packages that
tests import and run **in the test process** on `127.0.0.1:0`. corral runs
as a child process. To keep the test encoder independent of the code under
test, twins encode and decode with the official Go driver's `bson` package
and their own framing. They must not import `internal/domain/...` or
`internal/infra/...`.

### Docker fixtures (functional)

- `internal/testkit/mongofx` drives Docker through the `docker` CLI
  (`os/exec`). No container SDK dependency is added.
- All containers use `--network host` on Linux, with ports chosen by the
  fixture. Replica-set member names are `127.0.0.1:<port>`, so corral, the
  drivers and the PHP container reach every member directly.
- Topologies:
  - single-node replica set;
  - 3-node replica set;
  - sharded test topology with one config-server replica set (1 node), one
    shard replica set (1 node) and one `mongos`.

  Each runs on MongoDB 7.0 and 8.0, with optional authorization (keyfile
  and users generated at runtime) and optional TLS (synthetic CA and
  certificates generated at runtime into `t.TempDir()`).
- Images are pinned by digest in one constants file (`mongo:7.0`,
  `mongo:8.0`, and the PHP base image). The planner records the digests in
  ADR-0002 when the fixture lands.
- `CORRALTEST_REQUIRE_DOCKER=1` (set by `make test-functional`) makes a
  missing Docker daemon a test failure instead of a skip.

### PHP driver under PHP-FPM

- `internal/testkit/phpfpm` builds an image from an embedded Dockerfile:
  the official `php:<8.x>-fpm` base, plus `pecl install mongodb-<pinned>`
  and `pm = static` with a configurable `pm.max_children` (≥ 50 for
  AC-12). It runs with host networking and a mounted script directory
  owned by the test writer (`tests/functional/php/`).
- Tests drive FPM workers directly over FastCGI with
  `internal/testkit/fcgi`, a minimal FastCGI client written for tests. No
  web server is involved. Persistent connections are the ext-mongodb
  default (one libmongoc client per worker, reused across requests).

### Test-only Go dependency

`go.mongodb.org/mongo-driver/v2` (latest v2 release at implementation time,
recorded in ADR-0002) may be imported only by `_test.go` files,
`internal/testkit/...`, `tests/...` and `twins/...`. The production import
lint (SPEC-001 T21) is extended: no production package may import it, or
`twins/...`.

### corraltest hooks (extends ADR-0014)

The `corraltest` build (`cmd/corral/testbuild_on.go`) adds:

| Variable | Effect |
|---|---|
| `CORRALTEST_HOLD_AT` | A comma-separated list is now accepted. New points: `laststep` (before binding the wire listener, AC-1), `shutdown_delay` (start of `listener_close_delay`), `shutdown_listener_closed` (after step 2), `shutdown_step3` (before idle connections are closed in step 3), `shutdown_drain_expired` (before step 4), `shutdown_pool_close` (before step 5). For each new point the files are `<dir>/held-<point>` and `<dir>/release-<point>`. The SPEC-003 points `logger` and `checkconfig` keep `held` / `release`. |
| `CORRALTEST_POOL_ORDER=roundrobin` | The pool hands out idle connections in round-robin order (AC-16). The default is most-recently-used. |
| `CORRALTEST_PANIC_COMMAND=<name>` | The client handler panics after classifying a message whose command name equals `<name>` (AC-22). |
| `CORRALTEST_PROBE_LISTEN=127.0.0.1:0` | Starts a loopback-only HTTP probe server and writes its address to `<CORRALTEST_HOLD_DIR>/probe.addr`. `GET /conns` returns JSON with, per client connection ID, the bytes read for the current message before a lease, the maximum bytes read before a lease, and the peak bytes held for one reply. `GET /constants` returns the request buffer limit and the reply chunk. It exposes counts only, never message bytes. |

Amendment 1 (FR-64, AC-47, AC-48) adds one hold point and makes the
following normative:

- Extra hold point `shutdown_drain`: while held, the drain is kept open as
  if an exchange were in progress. Releasing it lets the drain finish
  normally, and a second signal (FR-63) ends it at once. AC-47 uses this
  point on a real `mongod`.
- `GET /constants` returns `{"L": 65536, "K": 65536}`.
- The production build (`make build`) contains no hook code. It never reads
  any `CORRALTEST_*` variable, starts no probe server and creates no hold
  file. SPEC-003 FR-8 does not report these variables, because they do not
  start with `CORRAL_`.

The production build (`testbuild_off.go`) has none of these. A lint test
(extending SPEC-003 T14) asserts that the default build's closure contains no
probe server and that `testbuild_on.go` carries the `corraltest`
constraint.

## Consequences

- Functional suites need Docker on Linux with host networking, plus network
  access to pull the pinned images and the PECL package. They cannot run on
  macOS Docker Desktop as written.
- The verifier's twin scenarios use the same twins and the corraltest
  binary.
