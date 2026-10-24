# ADR-0020: File-content settings (the SPEC-002 PEM rule) in the SPEC-003 registry and loader

- Status: Accepted (planner, 2026-10-24, for SPEC-002)
- Related: SPEC-002 FR-44, FR-45, FR-55, Interfaces ("PEM rule",
  `proxy-tls-keypair`), AC-24, AC-31; SPEC-003 FR-1, FR-11, FR-13, FR-15,
  FR-17, FR-25, FR-30, AC-15; ADR-0011, ADR-0012

## Context

SPEC-002 declares three path settings (`proxy.tls.cert_path`,
`proxy.tls.client_ca_path`, `upstream.tls.ca_path`) with the PEM rule:

- the file is read once during loading, through the SPEC-003 FR-25 file
  reader, with a 4 MiB limit;
- it must contain at least one PEM `CERTIFICATE` block that parses;
- the bytes read are the bytes used for TLS, and are never read again.

The cross rule `proxy-tls-keypair` needs the certificate bytes. A SPEC-003
`registry.Decl.Rule` is `func(v any) error` and has no access to the
injected reader. Reading the files after loading, in the app layer, would
break the FR-13 record order and the read-once guarantee, and would need
`os` access outside `cmd/corral` and `osfile` (SPEC-003 AC-15).

## Decision

- `registry.Decl` gains an optional field
  `File *registry.FileRule{MaxBytes int64; Check func(content []byte) error}`.
  It is allowed only on `string` settings that are not secret (registry
  build check).
- For such a setting, the loader:
  - validates the path value as usual (type, `Rule`);
  - then reads it once through `Inputs.ReadFile` with `MaxBytes`;
  - maps the three reader failures to the existing constant sentinels;
  - runs `Check` on the content.

  Any failure is one FR-11 record for that input, with the same `setting`
  naming as any other per-input error (flag name or `CORRAL_*` name). It
  never contains the path or content. These are group 2 records (FR-13),
  like any other per-input validation.
- Only the winning input's path is read. A losing source is validated by
  type and `Rule` only, and is not read. This matches the read-once rule and
  avoids touching files that will not be used.
- The effective value of such a setting is `registry.FileValue{Path string;
  Content []byte}`. Cross-rule `get` functions and the typed views receive
  this value. `loader.Result.Supplied` and the FR-20 record still list only
  the logical name.
- `settings.Proxy` carries the PEM `Content` bytes into the TLS builders.
  Nothing re-reads the path (FR-44 "not read again"; AC-24 unit: reader
  called once per file).
- The PEM rule itself (`settings.pemCertificates(content) error`: at least
  one `CERTIFICATE` block that `x509.ParseCertificate` accepts) and the key
  rule for `proxy.tls.key` (PKCS#1, PKCS#8 or SEC 1 EC) live in
  `internal/app/settings/proxy.go`. They return constant sentinels.

## Consequences

- This is a small extension to the SPEC-003 registry and loader (SPEC-003
  plan tasks T3 and T5). It is planned as SPEC-002 task T13, after those
  tasks land. It adds no new loader inputs and no `os` access, so SPEC-003
  AC-15 still holds.
- SPEC-003's own tests are unaffected. New loader tests cover file-rule
  settings with `memfs`: read count, the sentinel mapping, losing sources
  not read, and the no-path-in-records check.
