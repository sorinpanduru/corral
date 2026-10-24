# corral

corral is an open-source MongoDB wire-protocol proxy that gives connection
pooling to clients that cannot pool themselves. The motivating case is the
MongoDB PHP driver under PHP-FPM, where every worker holds its own persistent
connection. corral accepts many client connections and multiplexes them over a
small, bounded pool of upstream connections.

Status: early development. The observability foundation (logging, metrics,
tracing, admin listener) is implemented as internal packages; the proxy itself
is not yet built.

License: Apache-2.0 (see `LICENSE`).

## Build and test

Requires Go (see `go.mod`).

- `make build` builds `bin/corral`.
- `make test` runs `go vet` and the unit and integration tests.
- `make test-functional` runs the binary-level tests (builds first); available once the functional suite lands (T18).

## Documentation

- [Observability and operations](docs/operations/observability.md): settings,
  admin endpoints, logging, tracing and security notes.
- Architecture decisions: `docs/adr/`.
