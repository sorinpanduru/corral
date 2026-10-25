<p align="center">
  <img src="logo.png" alt="corral" width="320">
</p>

# corral

corral is an open-source MongoDB wire-protocol proxy that gives connection
pooling to clients that cannot pool themselves. The motivating case is the
MongoDB PHP driver under PHP-FPM, where every worker holds its own persistent
connection. corral accepts many client connections and multiplexes them over a
small, bounded pool of upstream connections.

Status: early development. The proxy core is built: corral pools upstream
connections for replica sets and sharded clusters (through `mongos`), with
optional TLS on both sides, SCRAM authentication to the upstream, and logging,
metrics, tracing and an admin listener. It has not yet been used in production.

License: Apache-2.0 (see `LICENSE`).

## Build and test

Requires Go (see `go.mod`).

- `make build` builds `bin/corral`.
- `make test` runs `go vet` and the unit and integration tests.
- `make test-functional` runs the binary-level tests (builds first).

## Running

corral needs an upstream: a replica set (set `upstream.replica_set`) or a
single `mongos`. A standalone `mongod` is not supported. With the default
SCRAM authentication, give the service credentials as files, never on the
command line:

```
make build
./bin/corral --upstream.hosts=db1.example:27017,db2.example:27017,db3.example:27017 \
             --upstream.replica_set=rs0 \
             --upstream.auth.username_file=/run/secrets/<name> \
             --upstream.auth.password_file=/run/secrets/<name>
```

For a single `mongos`, give exactly one address in `upstream.hosts` and leave
`upstream.replica_set` unset. Use `--upstream.auth.mechanism=none` for an
upstream without authentication.

Applications then connect to `proxy.listen` (default `127.0.0.1:27017`) with a
plain connection string such as `mongodb://corral-host:27017/`. The admin
listener (`admin.listen`, default `127.0.0.1:9187`) serves `/readyz`, which
answers 200 once an upstream target is reachable, and `/metrics`.

`SIGINT` or `SIGTERM` triggers a graceful shutdown; a second signal forces it.

The admin listener has no authentication or TLS; read "Security and
operational warnings" in the observability guide before binding a
non-loopback address.

## Documentation

- [Proxy operations](docs/operations/proxy.md): what corral supports,
  connecting clients, the upstream, pooling, TLS, shutdown, the shared
  upstream identity and its security implications.
- [Configuration](docs/operations/configuration.md): configuration sources and
  precedence, secrets as files, and the reference for every setting.
- [Observability](docs/operations/observability.md): logging, metrics,
  tracing, the admin endpoints and security notes.
- Architecture decisions: `docs/adr/`.
