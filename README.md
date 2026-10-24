# corral

corral is an open-source MongoDB wire-protocol proxy that gives connection
pooling to clients that cannot pool themselves. The motivating case is the
MongoDB PHP driver under PHP-FPM, where every worker holds its own persistent
connection. corral accepts many client connections and multiplexes them over a
small, bounded pool of upstream connections.

Status: early development. The `corral` binary runs the observability
foundation (logging, metrics, tracing, admin listener); the proxy itself is not
yet built.

License: Apache-2.0 (see `LICENSE`).

## Build and test

Requires Go (see `go.mod`).

- `make build` builds `bin/corral`.
- `make test` runs `go vet` and the unit and integration tests.
- `make test-functional` runs the binary-level tests (builds first).

## Running

```
make build
./bin/corral -log.level=info -admin.listen=127.0.0.1:9187
```

`SIGINT` or `SIGTERM` triggers a graceful shutdown. See the operations guide
below for settings and exit codes.

The admin listener has no authentication or TLS; read Security and operational warnings before binding a non-loopback address.

## Documentation

- [Observability and operations](docs/operations/observability.md): settings,
  admin endpoints, logging, tracing and security notes.
- Architecture decisions: `docs/adr/`.
