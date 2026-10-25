# SPEC-003: Configuration loading

Status: approved (Owner, 2026-10-24); Amendment 1 approved (Owner, 2026-10-24); Amendment 2 approved (Owner, 2026-10-24); Amendment 3 approved (Owner, 2026-10-25)
Owner: Sorin Panduru
Last updated: 2026-10-25 (Amendment 3)

## Goal

Define how operators give corral its settings: which sources exist, their
precedence, how logical setting names map to each source, and how values are
validated, reported, kept secret and documented. Feature specs (SPEC-001,
SPEC-002 and later ones) declare their own logical settings under this
mechanism without amending this spec. This spec supersedes ADR-0003 and its
interim flags-only loader. The settings validation layer in
`internal/infra/config` that ADR-0003 introduced may remain as the basis for
type and rule checks, but where ADR-0003 and this spec differ, this spec
governs.

## Non-goals

- Defining the settings of any feature. SPEC-001 owns `log.*`, `admin.*` and
  `tracing.*`, and SPEC-002 will own its own settings. This spec defines only
  the mechanism and the rules those settings follow.
- Reloading or changing configuration at runtime. A change needs a restart
  (consistent with SPEC-001 Non-goals).
- A configuration file in v1. The precedence model leaves room for a file
  source to be added later without reordering the other sources (FR-4).
- Remote or dynamic configuration sources (Consul, etcd, Kubernetes API, HTTP),
  templating, and variable interpolation inside values.
- Changing which `OTEL_*` variables are honoured or what values they accept.
  Those stay as SPEC-001 defines them. This spec changes only how their errors
  are collected and reported (FR-19).
- Printing, logging or exporting effective setting *values*, in any mode.
- Secret-manager integrations (Vault and similar) beyond reading a file that
  such tools mount or render.
- Binary (non-UTF-8) secret values.
- Per-client or per-database configuration.
- Signal behaviour before `main` starts running, while the Go runtime and
  package initialisation are still running (FR-18).

## Users and context

- **Operators / SREs** deploy corral on VMs under systemd (often next to
  PHP-FPM) and in Kubernetes. They set configuration through command-line
  flags in unit files or container `args`, and through environment variables
  from systemd `EnvironmentFile`, Kubernetes `env` / `envFrom`, ConfigMaps and
  Secrets. They need typos to fail loudly, errors that name the bad setting,
  and secrets kept out of process listings and logs.
- **Contributors / factory agents** writing feature specs need one way to
  declare a setting (name, type, default, secrecy, validation) that loads from
  every source with no per-feature loader code.
- **Test authors** need loading to be deterministic and injectable: arguments,
  the environment and file reads are passed in rather than read from process
  globals.

## Functional requirements

### Setting registry and declaration

- **FR-1:** A *logical setting* is declared by exactly one approved spec, in
  that spec's Interfaces section, with these properties:
  - name (FR-2);
  - type, taken from the FR-12 type set;
  - presence, which is one of:
    - *default*: a default value, written as text in the type's input syntax
      (FR-12). The default must itself pass the setting's type and rule;
    - *required*: no default, and a source must provide it (FR-9);
    - *optional*: no default, and the setting may stay unset (FR-9);
  - secret: yes or no (FR-14);
  - a one-line description, which is constant text containing no secret;
  - its validation rule, beyond what the type already checks;
  - zero or more cross-setting rules (FR-30). Each rule names the settings it
    involves and one *primary* setting, which is one of them.

  At runtime, corral holds a single *settings registry* containing every
  declared setting. Adding a setting that follows this spec's rules needs no
  amendment of SPEC-003. A new type requires amending this spec.
- **FR-2:** Logical setting names:
  - consist of two or more segments joined by `.`;
  - each segment matches `[a-z][a-z0-9_]*`;
  - the last segment must not be `file` and must not end in `_file`. Both
    forms are reserved for FR-15 file references, because `a.b.file` and
    `a.b_file` would map to the same environment variable as the file
    reference of a secret `a.b`;
  - *derived-name uniqueness:* the flag name and the logical name are the
    same string, so the check works on two name sets:
    - **flag names:** every setting's logical name, plus, for each secret
      setting, its FR-15 flag reference name `<name>_file`. They must all be
      unique across the registry;
    - **environment names:** every setting's FR-3 environment variable name,
      plus, for each secret setting, its FR-15 reference name
      `CORRAL_<NAME>_FILE`. They must all be unique across the registry.
      For example, `a.b_c` and `a_b.c` both map to `CORRAL_A_B_C`, so they
      cannot both be registered;
  - the first segment is a namespace owned by exactly one spec (see
    Interfaces, "Namespace ownership"). A spec must not declare settings in
    another spec's namespace.

  The registry refuses to build when any rule is broken. This is a
  programming error, caught by tests (AC-1).
- **FR-3:** Name mapping. For a logical setting `<name>`:
  - **flag:** `--<name>`, exactly as written (`--log.level`,
    `--tracing.sample_ratio`). A single leading dash (`-log.level`) is also
    accepted;
  - **environment variable:** `CORRAL_` followed by `<name>` uppercased, with
    every `.` replaced by `_` (`CORRAL_LOG_LEVEL`,
    `CORRAL_TRACING_SAMPLE_RATIO`, `CORRAL_ADMIN_LISTEN`).

  Both mappings are mechanical. No setting has a custom alias. Environment
  variable names are matched case-sensitively.

### Sources and precedence

- **FR-4:** corral reads settings from these sources, from highest to lowest
  precedence:
  1. command-line flags;
  2. environment variables;
  3. registry defaults.

  If a configuration file source is added later, it ranks between environment
  variables and defaults.
- **FR-5:** Each setting's *effective value* comes whole from the
  highest-precedence source that provides it (a literal or an FR-15 file
  reference). Values are never merged across sources, so a list value from a
  flag replaces the environment value rather than being appended to it.
  Lower-precedence inputs that are overridden are still processed and
  validated (FR-10). An overridden environment file reference
  (`CORRAL_<NAME>_FILE`) is still read, and a missing, unreadable or invalid
  file is still a config error.
- **FR-6:** Flag syntax for settings and file references:
  - only `--<name>=<value>` (or `-<name>=<value>`) is accepted. The
    space-separated form `--<name> <value>` and bare boolean flags
    (`--tracing.enabled` with no `=`) are rejected as missing a value. In the
    space-separated form, the following word is then also reported as a
    positional argument (FR-28);
  - an empty value (`--log.level=`) is passed to validation as an empty
    string. It fails unless the setting's type and rule allow empty values
    (FR-12);
  - giving the same setting twice on the command line is an error. Every
    occurrence of a flag name counts, with or without `=` and whichever dash
    form is used, so `--x=1 -x=2`, `--x=1 --x` and `--x --x=1` are all
    repeats. The exact records are pinned in FR-28's occurrence rule;
  - positional arguments are config errors with `setting=argument`, and the
    argument is never echoed. `--` is not an end-of-flags marker, and on its
    own it is a malformed flag (FR-8);
  - control flags follow FR-27.
- **FR-7:** Environment variables:
  - the environment is an injected snapshot (FR-25) of `KEY=VALUE` entries.
    When a key appears more than once, the first occurrence is used, which
    matches Go's `os.LookupEnv`. Entries without `=` are ignored;
  - a `CORRAL_*` variable whose value is empty or whitespace-only counts as
    unset (the same "set" rule as SPEC-001 FR-41). This applies to setting
    variables, `_FILE` reference variables and unknown variables (FR-8);
  - *whitespace* means exactly the characters Go's `strings.TrimSpace`
    removes, which are the characters for which `unicode.IsSpace` is true:
    - U+0009 to U+000D (tab, line feed, vertical tab, form feed, carriage
      return);
    - U+0020 (space);
    - U+0085 (next line) and U+00A0 (no-break space);
    - every other Unicode `White_Space` character, such as U+1680, U+2000
      to U+200A, U+2028, U+2029, U+202F, U+205F and U+3000.

    Characters outside this set, such as U+200B (zero-width space) and
    U+FEFF, are not whitespace. A value that is not valid UTF-8 is never
    whitespace-only. The OpenTelemetry Go SDK uses the same `TrimSpace`
    semantics, so the SPEC-001 FR-41 "set" rule and the FR-19 `OTEL_*`
    trimming use the same set;
  - other values are used as given, with no trimming. A value with leading
    or trailing whitespace is validated as is, so for most types it fails;
  - *asymmetry, by design:* a flag can supply an empty value (`--x=`), which
    is then validated (FR-6), but an environment variable cannot, because
    empty means unset. Container templating often renders unset values as
    empty strings, and treating those as "use the lower-precedence or default
    value" avoids spurious failures. FR-10 applies to every value that is
    *provided*; an empty environment variable is not provided.
- **FR-8:** Unknown inputs:
  - any flag argument that is not a registered setting's flag, a secret's
    FR-15 flag reference or an FR-27 control-flag spelling is a config error
    with `setting=unknown`. Malformed flags (`-`, `--`, `-=x`, an empty name,
    or any control character as defined in FR-12) are reported the same way.
    The supplied text is never echoed, because a mistyped name may carry a
    secret;
  - an environment variable is *unknown* when its name starts with the exact,
    case-sensitive prefix `CORRAL_`, it is set per FR-7, and it is neither a
    registered setting's FR-3 name nor a secret's FR-15 environment reference.
    A variable named exactly `CORRAL_` is unknown. Names starting with
    `corral_` or another casing are not CORRAL variables and are ignored;
  - unknown environment variables are **not** fatal. corral emits exactly one
    warn record listing them (FR-29), with `component=config`, the fixed
    message `unknown CORRAL_ environment variables ignored`, and a `setting`
    attribute holding their names, deduplicated, sorted in ascending byte
    order and joined with `,`. A name containing any byte outside
    `[A-Za-z0-9_]` is not rendered; it is replaced by the fixed token
    `CORRAL_<invalid-name>`, which appears at most once. The replacement
    happens before deduplication and sorting, so the token is sorted like any
    other name. `<` is byte 0x3C, so the token sorts after names whose next
    byte is a digit and before names whose next byte is an uppercase letter.
    The record never contains the variables' values. Rationale: Kubernetes
    service links inject `CORRAL_SERVICE_HOST`, `CORRAL_PORT` and similar into
    pods when a Service is named `corral`;
  - `OTEL_*` variables are never unknown (FR-19).
- **FR-9:** Presence:
  - a *required* setting that has no effective value from any source, and no
    errored input either (see the FR-28 skip rules), is a config error with
    `setting` set to its logical name;
  - an *optional* setting with no effective value is *unset*. The loader
    output marks it as unset, which is distinct from an empty string, and the
    application layer decides the behaviour of unset according to the owning
    spec. Domain code receives either a typed value or an explicit "not set"
    (FR-16);
  - a *default* setting with no source value takes its default.

### Validation and error reporting

- **FR-10:** Every provided value from every source is validated against its
  setting's type (FR-12) and rule (FR-1). This happens before any listener
  binds, any outbound connection opens, or the `lifecycle` starting record is
  emitted. The same value gets the same result whichever source supplied it.
  Overridden lower-precedence values are included: an invalid
  `CORRAL_LOG_LEVEL` is fatal even when `--log.level` is valid. Cross-setting
  rules (FR-30) are checked after the individual values.
- **FR-11:** Config error records follow SPEC-001 FR-7, FR-27 and FR-29. Each
  record:
  - is written to stdout as JSON through the bootstrap logger (FR-29);
  - has level error, `component=config`, the constant message `invalid
    configuration`, and the SPEC-001 FR-29 `error` and `corral.error.detail`
    attributes (class `config`);
  - has a `setting` attribute holding the name *as it appears in the source
    of the failing input*:
    - the logical name for a flag (`log.level`);
    - the environment variable name for an environment value
      (`CORRAL_LOG_LEVEL`);
    - the reference's own name for an FR-15 file reference
      (`upstream.password_file` or `CORRAL_UPSTREAM_PASSWORD_FILE`);
    - the logical name for a missing required setting;
    - `unknown` or `argument` per FR-8 and FR-6;
    - for a cross-setting error, the name of the rule's primary setting,
      spelled for the source of that setting's effective value as FR-30
      defines (Amendment 2);
    - the variable name for `OTEL_*` errors (FR-19);
    - the exact values pinned in FR-28 for conflicts and repeats;
  - never contains the supplied value, part of it, a file path or file
    contents.
- **FR-12:** Type set. Each type's accepted syntax is fixed here. Feature
  specs may narrow it with a rule (for example a range). In this spec, a
  *control character* is any of U+0000 to U+001F, U+007F (DEL) and U+0080 to
  U+009F (C1). Every type rejects invalid UTF-8.

  | Type | Accepted syntax |
  |---|---|
  | `string` | UTF-8 text without control characters. Empty only if the rule allows it |
  | `text` | UTF-8 text whose only allowed control characters are `\t` (U+0009), `\n` (U+000A) and `\r` (U+000D). Intended for multi-line secrets such as PEM blocks. Empty only if the rule allows it |
  | `bool` | `true` or `false`, case-insensitive. Nothing else (`1`, `yes`, `on`) is accepted |
  | `int` | matches `-?[0-9]+`: base 10, optional leading `-`, no `+`, no `_`, no hex; fits in a signed 64-bit integer; range per rule |
  | `float` | matches `-?([0-9]+(\.[0-9]*)?\|\.[0-9]+)([eE][+-]?[0-9]+)?`, finite as a 64-bit float; no `+` sign, hex, `_`, `Inf` or `NaN`; range per rule |
  | `duration` | one or more `<decimal><unit>` groups as in Go `time.ParseDuration` (`250ms`, `5s`, `1m30s`, `1.5h`), with ASCII units `ns`, `us`, `ms`, `s`, `m`, `h` only (`µs` / `μs` are rejected); no sign; a unit is required, so bare `5` and bare `0` are rejected; must be > 0 unless the rule allows zero, in which case `0s` (or `0` with any unit) is accepted |
  | `enum` | one of a declared value set; case-insensitive unless the rule says otherwise |
  | `hostport` | `host:port`. The host is non-empty (SPEC-001 FR-16) and is either an IP literal (IPv6 in brackets) or a valid DNS hostname: letters, digits, `-` and `.`; labels 1 to 63 characters, not starting or ending with `-`; at most 253 characters; and a last label that is neither all-numeric nor `0x`-hex-like, so `0`, `0x0` and `127.1` are rejected. A trailing dot (`localhost.`) is rejected, because it leaves an empty last label. IPv6 zone identifiers (`[fe80::1%eth0]`) are rejected. The port is decimal digits only, 0 to 65535, with no sign and no leading zeros (`09187` is rejected; `0` is accepted) |
  | `list` of one of the scalar types above except `text` | comma-separated; leading and trailing ASCII spaces and tabs around each element are trimmed; empty elements are rejected; an empty list only if the rule allows it |

  List trimming (ASCII space and tab only) is deliberately narrower than the
  FR-7 whitespace set. Any other whitespace character in an element is kept
  and validated as part of the element.

  The SPEC-001 settings use these types as declared in Interfaces. *Note:*
  the following are tightenings relative to the interim loader, which used
  `strconv` and accepted a trailing dot on host names:
  - the stricter port syntax (no leading zeros or `+`);
  - the rejection of a trailing dot and of IPv6 zone identifiers in
    `hostport` hosts;
  - the rejection of a leading `+` on `int` and `float` values. For example,
    `tracing.sample_ratio=+0.5` was accepted before and is now rejected.
- **FR-13:** Collection and ordering. corral collects every error from
  loading and validation before exiting, emits one FR-11 record per error,
  and then exits with exit code 2 (FR-24). Records are emitted in these
  groups, in this order:
  1. **Command-line errors** (FR-6, FR-8, FR-27, FR-28: unknown or malformed
     flags, positional arguments, missing values, repeats, secret literals,
     control-flag misuse), in command-line order. When one argument produces
     a record, its position decides the order, so several `setting=unknown`
     or `setting=argument` records keep their argument order.
  2. **Per-input errors**: invalid values from flags and environment
     variables, FR-15 file-reference errors, environment literal-and-reference
     conflicts (FR-28) and missing required settings (FR-9). These are sorted
     by `setting` in ascending byte order. Ties cannot happen, because each
     input yields at most one record.
  3. **Cross-setting errors** (FR-30), sorted by `setting` and then by the
     rule's declaration order.
  4. **`OTEL_*` errors** (FR-19), sorted by `setting` (the variable name).

  Each input produces at most one record. When an input has several
  problems, the first applicable check in the type's validation reports it.

### Secrets

- **FR-14:** A setting declared *secret* (for example the upstream
  credentials in SPEC-002):
  - has presence *required* or *optional*, never *default*;
  - is never emitted in any signal or output: logs, error records, help or
    usage output, metrics, spans and HTTP responses. This extends SPEC-001
    FR-24's ban on credentials to every secret setting, including values that
    are not credentials in the narrow sense;
  - cannot be given as a literal on the command line, because command lines
    are visible to other local users through the process list and
    `/proc/<pid>/cmdline`. `--<name>=<value>` for a secret setting is a config
    error (FR-28). The value is not echoed;
  - may be given as a literal through its FR-3 environment variable, or
    through an FR-15 file reference.

  Any setting whose value can contain a credential, such as a connection
  string with userinfo, must be declared secret by its owning spec. Secret
  settings that may hold multi-line material use the `text` type (FR-12).
- **FR-15:** File references:
  - for each secret setting `<name>`, two reference names exist: the flag
    `--<name>_file=<path>` and the environment variable
    `CORRAL_<NAME>_FILE`. Non-secret settings have no file reference, and
    their `_file` forms are unknown inputs (FR-8);
  - an empty path in a flag (`--<name>_file=`) is a config error naming the
    reference. An empty or whitespace-only `CORRAL_<NAME>_FILE` counts as
    unset (FR-7);
  - files are read through the injected file reader (FR-25), once each,
    during loading (FR-10);
  - it is a config error (FR-11, naming the reference) when the path does not
    exist or cannot be read, does not resolve to a regular file after
    following symlinks (directories, FIFOs, sockets and devices are rejected
    without blocking), or the file is larger than 64 KiB (65,536 bytes,
    measured on the raw file before any newline is removed);
  - the value is the file's contents, with exactly one trailing `\n` or
    `\r\n` removed if present. The value is then validated by the setting's
    type and rule as usual. An empty value after removal is an error unless
    the rule allows empty values. Validation errors name the reference;
  - file ownership and permission bits are not checked, because Kubernetes
    Secret mounts are commonly group- or world-readable through symlinks;
  - literal and reference conflicts and repeats are handled as pinned in
    FR-28. Across sources, FR-4 precedence applies to the setting as a whole,
    so a flag file reference beats an environment literal;
  - paths and contents are never emitted (SPEC-001 FR-24).

### Domain boundary

- **FR-16:** Domain packages (SPEC-001 FR-1) never read flags, environment
  variables or files, and never import the settings registry or loader. The
  application layer passes them typed, validated values, or an explicit
  "not set" for unset optional settings. `OTEL_*` handling stays in the
  tracing infrastructure (SPEC-001).

### Reload stance

- **FR-17:** Settings are read once at startup. There is no reload in any
  form. Changes to the environment or to referenced files after startup have
  no effect until restart.
- **FR-18:** SIGHUP does not terminate corral and does not reload anything.
  - SIGHUP handling is installed as the first action of `main`, before
    arguments are parsed or any configuration is loaded. A SIGHUP that
    arrives after that point, during loading, help mode, version mode or
    check-config mode, does not terminate the process.
  - The window before `main` runs, while the Go runtime and package
    initialisation are still running, is out of scope (Non-goals). A SIGHUP
    in that window may terminate the process with the default signal action.
  - SIGHUP never produces a record in these cases:
    - before the configured logger exists (FR-29);
    - in help mode and version mode;
    - at any point in check-config mode, including after the configured
      logger has been built. Check-config output is exactly the records FR-23
      lists.
  - In a normal run, once the configured logger exists, each SIGHUP leads to
    a warn record with `component=config` and the fixed message
    `configuration reload not supported; restart required`. These records are
    rate-limited like SPEC-001 FR-33: at most one per 30 seconds, and a
    record that follows dropped ones carries `suppressed_count`, the number
    dropped since the previous record.
  - SIGINT and SIGTERM keep their existing graceful-shutdown behaviour.

### OTEL variables

- **FR-19:** `OTEL_*` variables:
  - are outside the registry. They are read from the same injected
    environment snapshot (FR-25), have no flag form, are not prefixed
    `CORRAL_`, and are never reported as unknown (FR-8);
  - are honoured, trimmed and validated as SPEC-001 FR-19 to FR-21, FR-27,
    FR-30, FR-37 and FR-41 require. SPEC-001's whitespace trimming of
    `OTEL_*` values is kept, using the FR-7 whitespace set; FR-7's no-trim
    rule applies to `CORRAL_*` only;
  - are validated in the same pass as the CORRAL settings (FR-13 group 4),
    even when CORRAL errors exist, provided the effective `tracing.enabled`
    is known. It is *not known* when its winning input failed validation, or
    when the command-line `--tracing.enabled` flag has a group 1 error. In
    that case `OTEL_*` validation is skipped and no `OTEL_*` errors are
    reported. An invalid overridden `CORRAL_TRACING_ENABLED` does not make it
    unknown when a valid flag wins. `OTEL_SDK_DISABLED=true` still turns
    tracing off and skips validation (SPEC-001 FR-19);
  - every invalid variable is reported, one record per variable, not just
    the first;
  - an invalid `OTEL_TRACES_EXPORTER` does not stop the other checks. Every
    other honoured or validated variable is still validated and reported,
    including the protocol, endpoint, headers, timeout, compression,
    insecure, TLS and `OTEL_BSP_*` variables and both `OTEL_TRACES_SAMPLER`
    variables. This is the same validation as when the exporter value is
    valid, whether that value is `otlp` or `none`;
  - cross-variable checks report under these fixed names:
    - batch size larger than queue size: `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`;
    - a client certificate without a key: the key variable of the same form
      (general or `_TRACES_`);
    - a client key without a certificate: the certificate variable of the
      same form;
    - a certificate and key that do not pair: the key variable of the same
      form;
    - the sampler argument, checked against the effective sampler (SPEC-001
      FR-21, because whether the argument is valid depends on the sampler):
      `OTEL_TRACES_SAMPLER_ARG`. When `OTEL_TRACES_SAMPLER` itself has an
      error, this check is skipped and produces no record (Amendment 2);
  - *skip rule for cross-variable checks* (consistent with FR-30): if any
    variable a check involves already has its own error, the check is
    skipped entirely and produces no record. It is not re-evaluated with
    that variable's default. For example:
    - an invalid `OTEL_BSP_MAX_QUEUE_SIZE` skips the batch-size check;
    - a non-PEM client certificate skips the pairing check for that form;
    - an invalid `OTEL_TRACES_SAMPLER` skips the `OTEL_TRACES_SAMPLER_ARG`
      check;
  - for validation, PEM files named by `OTEL_*` variables are read through
    the same injected file reader (FR-25), with a maximum size of 4 MiB
    (4,194,304 bytes, measured on the raw file). This is the limit the
    current implementation already applies;
  - injection covers validation only. The OTLP exporter that corral starts
    keeps standard SDK semantics (SPEC-001 FR-20, FR-41) and reads the real
    process environment and files itself;
  - *behaviour change:* `OTEL_*` errors were previously emitted through the
    configured logger, after it was built. They are now emitted through the
    bootstrap JSON logger along with the other config errors (FR-29), so
    they are JSON even when `log.format=text`, as SPEC-001 FR-27 and AC-21
    expect.

### Startup visibility

- **FR-20:** After successful validation, corral emits exactly one info record
  (FR-29) with `component=config`, the fixed message `configuration loaded`,
  and a `setting` attribute.
  - The attribute lists the logical names of the registered settings whose
    effective value came from a flag or an environment variable (literal or
    file reference), rather than from a default or left unset. Names are
    sorted in ascending byte order and joined with `,`.
  - Secret settings are included by name only. Values are never included.
  - The list reports *sources*, not effects: `tracing.enabled` is listed
    when supplied, even if `OTEL_SDK_DISABLED` turns tracing off, and
    `tracing.sample_ratio` is listed when supplied, even if
    `OTEL_TRACES_SAMPLER_ARG` replaces it. `OTEL_*` variables are never
    listed.
  - When no setting was supplied, the attribute is the empty string.
  - Like any record, it is subject to the configured level (SPEC-001 FR-6),
    so it is not emitted at `log.level=warn` or `error`.

### Help, version and validation-only mode (control flags)

- **FR-21:** Help mode (FR-27) prints usage text to stdout and exits 0
  without loading any configuration or starting anything.
  - The output is plain text, not a log record.
  - For each registered setting, sorted by logical name, it lists:
    - the logical name, flag form and environment variable;
    - the type, and for enums the allowed values;
    - the presence:
      - the declared default text, printed verbatim as declared (for example
        `1.0`, `5s`, `a,b`);
      - or `(required)`;
      - or `(optional, unset by default)`;
    - for secret settings, additionally the fixed text `(secret: set
      CORRAL_<NAME> or CORRAL_<NAME>_FILE, or pass --<name>_file; never pass
      the value on the command line)`, with the names filled in;
    - the one-line description.
  - It ends with a fixed line saying that `OTEL_*` variables are honoured as
    described in the operator documentation.
  - Help output never includes values taken from the environment, flags or
    files.
- **FR-22:** Version mode (FR-27) prints one line, `corral <version>`, to
  stdout and exits 0 without loading any configuration or starting anything.
  - `<version>` is the value stamped into the binary at build time (a
    build-time variable set through the Go linker).
  - `make build` stamps it from the `VERSION` make variable, defaulting to
    `devel`.
  - An unstamped build (for example `go build` or `go run`) prints
    `corral devel`.
- **FR-23:** Check-config mode (FR-27) loads and validates all settings,
  including FR-15 file reads and `OTEL_*` validation (FR-19). It then exits
  without binding any listener, connecting to any upstream or contacting any
  collector, and emits no `lifecycle` records.
  - On success, it emits the same records a normal startup emits up to this
    point, in the same order (FR-29), through the configured logger with the
    configured format and level:
    - the FR-8 warn record, if any;
    - the SPEC-001 FR-41 warn record, if a normal startup with these
      settings would emit it;
    - the FR-20 info record.

    It then exits 0. The configured format applies here intentionally, so
    that operators see the same output they will get in production.
  - On failure, it emits the failure-path records (FR-29) as JSON and exits
    2.
  - It never emits the FR-18 SIGHUP record.

### Exit codes

- **FR-24:** Exit codes:
  - `0`: graceful shutdown, help mode, version mode, or a successful
    check-config;
  - `2`: config error found during loading or validation (FR-6 to FR-15,
    FR-19, FR-27, FR-28, FR-30), including control-flag misuse;
  - `1`: any other startup or runtime failure. This includes admin listener
    bind failure (SPEC-001 FR-35), which is logged with the distinct
    *admin-bind record* (Amendment 3), never with `invalid configuration`.
    The admin-bind record:
    - has level error, `component=config`, `setting=admin.listen` and the
      constant message `admin listener bind failed`;
    - carries the SPEC-001 FR-29 `error` and `corral.error.detail`
      attributes. The error class is `config`, and the detail comes from the
      FR-29 enum, for example `EADDRINUSE`;
    - never contains the address, host or port;
    - is written through the configured logger, because binding happens
      after configuration has loaded successfully (FR-29).

  The Go runtime also exits with code 2 on an unrecovered panic or fatal
  runtime error. Exit code 2 therefore does not by itself mean a config error;
  config errors are identified by their `invalid configuration` records
  (FR-26). *Identification is by message, not by class:* the admin-bind
  record keeps error class `config` (SPEC-001 FR-29 and FR-35, so `error`
  and `component` are unchanged), so the error class alone does not tell a
  loading or validation error from a bind failure. Operators and tests tell
  them apart by message: `invalid configuration` (exit 2) versus `admin
  listener bind failed` (exit 1).

### Testability and documentation

- **FR-25:** Loading is a deterministic function of three injected inputs:
  - the argument list;
  - an *environment snapshot*: the full list of `KEY=VALUE` entries, as
    returned by `os.Environ`. Lookup by name is derived from it (FR-7), and
    so is the enumeration needed for FR-8. The tracing environment
    validation (FR-19) uses the same snapshot;
  - a *file reader*: given a path and a maximum size, it returns either the
    file's bytes or one failure kind out of: not found or unreadable, not a
    regular file, too large. It checks that the path resolves to a regular
    file and opens it without blocking on FIFOs or devices before reading
    any content. It reads at most the maximum size plus one byte. FR-15
    secret files and the FR-19 `OTEL_*` PEM files both go through it.

  The loader and the tracing environment-validation code read no process
  globals directly. Only `cmd/corral` binds the real arguments, environment
  and filesystem. The OTLP exporter is outside this rule: it keeps SDK
  semantics over the real process environment and files (FR-19).
- **FR-26:** The operator documentation file
  `docs/operations/configuration.md` contains:
  - a reference entry for every registered setting, with the FR-21 fields;
  - the FR-3 mapping, the FR-4 precedence, the FR-7 empty-value rule and its
    asymmetry with flags;
  - the FR-14 / FR-15 secret rules, including advice to rotate any secret
    that was passed on a command line, even when rejected, because it was
    visible in the process list;
  - the FR-17 / FR-18 restart requirement and SIGHUP behaviour;
  - the FR-8 Kubernetes service-link note, and the option of setting
    `enableServiceLinks: false`;
  - the FR-21 to FR-23 and FR-27 control flags;
  - the FR-24 exit codes, including the note that the Go runtime also uses
    exit code 2 for unrecovered panics.

### Added in revision 2

- **FR-27:** Control-flag grammar. Control flags are not settings and take no
  value.
  - **Spellings:**
    - help: `--help`, `-help`, `-h`;
    - version: `--version`, `-version`;
    - check-config: `--check-config`, `-check-config`.

    `--h` and every other variant are unknown flags (FR-8).
  - **Mode selection.** corral scans all arguments before parsing anything
    else:
    1. if any argument is exactly a help spelling, help mode runs (FR-21)
       and exits 0. Every other argument is ignored, valid or not, including
       version and check-config flags and malformed flags;
    2. otherwise, if any argument is exactly a version spelling, version
       mode runs (FR-22) and exits 0. Every other argument is ignored;
    3. otherwise, if any argument is exactly a check-config spelling,
       check-config mode runs (FR-23). All other arguments are parsed as
       usual;
    4. otherwise, a normal run.
  - **Repeats:** repeating a control flag, in the same or another spelling,
    is allowed and has no extra effect.
  - **Values:** a control flag written with `=` (`--check-config=true`,
    `--help=x`, `--version=`) is not a control-flag spelling. It is a
    command-line error with `setting=unknown` (exit 2), unless an exact help
    or version spelling elsewhere on the command line selects that mode
    first.
  - **Combinations:**

    | Command line | Mode | Exit | Records |
    |---|---|---|---|
    | `--help --version` | help | 0 | none |
    | `--version --check-config` | version | 0 | none |
    | `--check-config --help` | help | 0 | none |
    | `--help --bogus=1` | help | 0 | none |
    | `--version --log.level=bad` | version | 0 | none |
    | `--check-config --check-config` | check-config | 0 or 2 | per FR-23 |
    | `--check-config=true` | normal (fails) | 2 | one, `setting=unknown` |
    | `--check-config --help=x` | check-config (fails) | 2 | one, `setting=unknown`, plus any others |

- **FR-28:** Conflicts and repeats. Each case yields exactly the records
  listed, and nothing else for the inputs involved. "Not read" means the file
  reader is not called for that reference. "Failed" means the setting's
  effective value is unavailable, which drives the skip rules below.

  **Occurrence rule.** This general rule applies to every flag name
  separately: each setting flag and each `<name>_file` reference flag. An
  *occurrence* is any argument naming that flag, with or without `=`, in
  either dash form. All records named below are group 1, use the flag name
  as `setting`, and leave the setting failed.
  - **Non-secret setting flags and `_file` reference flags:**
    - two or more occurrences give exactly one repeat record, at the
      position of the second occurrence. No missing-value record is
      emitted, no occurrence is validated, and no file is read;
    - a single occurrence without `=` gives one missing-value record at its
      position.
  - **Secret setting flags (`--a.b`):**
    - if any occurrence has `=` (a literal), exactly one secret-literal
      record is emitted, at the position of the first occurrence with `=`.
      No repeat or missing-value record is emitted for `a.b`;
    - otherwise, a single bare occurrence (`--a.b`) gives one missing-value
      record, `setting=a.b`, at its position. It is not a secret-literal
      record, because no value was supplied;
    - otherwise (two or more bare occurrences), exactly one repeat record is
      emitted, at the position of the second occurrence.
  - The setting flag and its `_file` reference flag are evaluated
    separately, so each can produce its own record (see the table).

  The table below gives the main cases.

  | Input | Records (group, `setting`) | Effect |
  |---|---|---|
  | secret literal flag `--a.b=v` (alone, or together with a single `--a.b_file=p`) | exactly one: group 1, `a.b` | the flag file reference is not read; the flag source counts as failed for `a.b`; environment inputs for `a.b` are still processed and validated (FR-5) |
  | `--a.b=v --a.b_file=p1 --a.b_file=p2` | two, in this order: group 1, `a.b`, at the literal's position; then group 1, `a.b_file`, at the position of the second `_file` occurrence | no file is read; `a.b` failed |
  | bare secret flag `--a.b` (single occurrence, no `=`) | exactly one: group 1, `a.b`, at its position (missing value) | `a.b` failed |
  | `--x=1 --x` or `--x --x=1` | exactly one: group 1, `x`, at the position of the second occurrence | no missing-value record; nothing validated; `x` failed |
  | `--x=1 --x=2` (or `--x=1 -x=2`), or any number of repeats such as `--x=1 --x=2 --x=3` | exactly one: group 1, `x`, placed at the position of the second occurrence; later occurrences add no record | no occurrence is validated; `x` failed |
  | repeated secret literal `--a.b=v1 --a.b=v2` (any count) | exactly one: group 1, `a.b`, placed at the position of the first occurrence (the secret-literal error), with no separate repeat record | as for a single secret literal flag |
  | `--a.b_file=p1 --a.b_file=p2` (any count ≥ 2) | exactly one: group 1, `a.b_file`, placed at the position of the second occurrence | no file is read; `a.b` failed |
  | `--log.level` (no `=`) | exactly one: group 1, `log.level` | `log.level` failed |
  | `--log.level debug` | two: group 1, `log.level` (missing value), then group 1, `argument` (`debug`) | `log.level` failed |
  | `CORRAL_A_B` and `CORRAL_A_B_FILE` both set | exactly one: group 2, `CORRAL_A_B` | the file is not read and the literal is not validated; the environment source for `a.b` failed. If a flag input for `a.b` wins, the flag value is still used |
  | `CORRAL_A_B_FILE` set and overridden by a flag | file read and validated; any error is reported under `CORRAL_A_B_FILE` | the flag value wins if valid |

  **Skip rules:**
  - a setting whose winning source failed gets no FR-9 missing-required
    error;
  - the FR-30 cross-setting rules that involve such a setting are skipped;
  - when `tracing.enabled` is affected, FR-19 applies.
- **FR-29:** Output path and order of configuration records.
  - **Failure path** (any config error): no configured logger is built. All
    records go through the bootstrap logger, which writes JSON to stdout,
    filters nothing by level, and adds `component=config`. The order is:
    1. the FR-8 warn record, if there are unknown environment variables (so
       that a typo can explain a missing-required error);
    2. the FR-11 error records, in FR-13 order.

    No SPEC-001 FR-41 record and no FR-20 record are emitted. Exit 2.
  - **Success path:** the configured logger is built (with the configured
    format and level), and these records are emitted through it, each subject
    to the level, in this order:
    1. the FR-8 warn record (if any);
    2. the SPEC-001 FR-41 warn record (if applicable);
    3. the FR-20 info record;
    4. then, in a normal run, the `lifecycle` starting record.

    In check-config mode, the process exits 0 after step 3.
  - Help and version modes emit no log records.
- **FR-30:** Cross-setting rules.
  - **Declaration:** a feature spec declares each rule in its settings table
    (Interfaces), with the settings it involves and its primary setting.
  - **Evaluation:** rules are evaluated on effective values, including
    defaults, after every per-input check (FR-13 group 3). A rule is skipped
    when any setting it involves failed (FR-28), or when an optional setting
    it involves is unset, unless the rule says it applies to unset.
  - **`setting` value:** a failing rule produces one record, whose `setting`
    is the primary setting's name spelled for the source of its effective
    value: the logical name for a flag or a default, the `CORRAL_*` name for
    an environment literal, and the logical name for an environment file
    reference. The reference name is not used here, because the file itself
    was valid.

## Interfaces

### Namespace ownership

| Namespace (first segment) | Owning spec |
|---|---|
| `log` | SPEC-001 |
| `admin` | SPEC-001 |
| `tracing` | SPEC-001 |

Other specs claim namespaces in their own Interfaces section, and the claim
takes effect on approval. This table lists SPEC-001's namespaces for
reference only. A new spec does not need to amend it. The registry's
derived-name uniqueness check (FR-2) catches collisions, and review catches a
namespace claimed twice.

### Setting declaration (what a feature spec writes)

Each feature spec that adds settings includes a table in its Interfaces
section with these columns:

| Setting | Type (FR-12) | Presence (default text / required / optional) | Secret | Rule | Cross-setting rules (involved settings; primary) | Description |
|---|---|---|---|---|---|---|

The SPEC-001 settings declared under this contract:

| Setting | Type | Presence | Secret | Rule | Cross-setting rules | Description |
|---|---|---|---|---|---|---|
| `log.format` | enum `json`, `text` | default `json` | no | SPEC-001 FR-5 | none | Log output format. |
| `log.level` | enum `debug`, `info`, `warn`, `error` | default `info` | no | case-insensitive, no offsets (SPEC-001 FR-6) | none | Minimum log level. |
| `admin.listen` | hostport | default `127.0.0.1:9187` | no | non-empty host (SPEC-001 FR-16) | none | Admin HTTP listener address. |
| `tracing.enabled` | bool | default `false` | no | forced off by `OTEL_SDK_DISABLED` (SPEC-001 FR-19) | none | Enable OTLP tracing. |
| `tracing.sample_ratio` | float | default `1.0` | no | 0.0 to 1.0 inclusive (SPEC-001 FR-21) | none | Trace sampling ratio. |

**Normative status:** SPEC-001 stays normative for these settings' names,
meanings, value sets, defaults and rules. This table adds only what SPEC-001
left to the configuration spec: the FR-12 type, the presence and the
description. Where the table adds detail, it is normative. If it ever
conflicts with SPEC-001, SPEC-001 governs, and the conflict is a defect in
this spec. No SPEC-001 amendment is needed.

### Name mapping

| Logical | Flag | Environment | File reference (secret only) |
|---|---|---|---|
| `log.level` | `--log.level=<v>` | `CORRAL_LOG_LEVEL` | n/a |
| `tracing.sample_ratio` | `--tracing.sample_ratio=<v>` | `CORRAL_TRACING_SAMPLE_RATIO` | n/a |
| `<ns>.<secret>` (example) | rejected as a literal | `CORRAL_<NS>_<SECRET>` | `--<ns>.<secret>_file=<path>`, `CORRAL_<NS>_<SECRET>_FILE` |

### Control flags

See FR-27 for the grammar and combinations.

| Flag | Effect | Exit |
|---|---|---|
| `--help`, `-help`, `-h` | usage to stdout (FR-21) | 0 |
| `--version`, `-version` | `corral <version>` to stdout (FR-22) | 0 |
| `--check-config`, `-check-config` | validate only (FR-23) | 0 or 2 |

### Loader contract

- **Inputs** (FR-25):
  - the argument list;
  - the environment snapshot (`KEY=VALUE` entries);
  - the file reader `(path, maxBytes) -> bytes | {not_found_or_unreadable,
    not_regular, too_large}`.
- **Output:** one of these.
  - Success:
    - for every registered setting, a typed value or "unset" (optional
      settings only);
    - the set of setting names supplied by a source (FR-20);
    - the sorted list of unknown environment-variable names (FR-8);
    - the resolved tracing configuration (SPEC-001).
  - Failure:
    - the unknown environment-variable names;
    - the complete, ordered list of config errors (FR-13). Each error
      carries only a `setting` name and a SPEC-001 FR-29 class and detail.

### Log records added or changed by this spec (component `config`, no new attribute keys)

| Message | Level | Attributes | When | Logger |
|---|---|---|---|---|
| `invalid configuration` | error | `setting`, `error`, `corral.error.detail` | each config error (FR-11), including `OTEL_*` | bootstrap JSON |
| `unknown CORRAL_ environment variables ignored` | warn | `setting` (sorted, comma-joined names) | once, if any (FR-8) | bootstrap JSON on failure, configured on success (FR-29) |
| `configuration loaded` | info | `setting` (sorted, comma-joined names) | once, on success (FR-20) | configured |
| `configuration reload not supported; restart required` | warn | `suppressed_count` when some were dropped | SIGHUP in a normal run, rate-limited (FR-18); never in check-config mode | configured |
| `admin listener bind failed` | error | `setting=admin.listen`, `error` (class `config`), `corral.error.detail` (FR-29 enum, for example `EADDRINUSE`); never the address, host or port | the admin listener cannot bind at startup (SPEC-001 FR-35); the process exits 1 (FR-24, Amendment 3) | configured |

`invalid configuration` already exists. The other four messages are new.
`admin listener bind failed` replaces the earlier use of `invalid
configuration` for bind failures (Amendment 3). Its error class stays
`config`, so it is identified by its message, not by its class (FR-24).

## Behavior on failure

- **Invalid value, missing required setting, unknown or malformed flag,
  positional argument, repeated flag, control flag with a value, a secret
  given as a literal flag, or a literal and file reference both set:** every
  error is reported in FR-13 order, as JSON (FR-29), and corral exits 2 before
  binding anything (FR-10, FR-24, FR-28).
- **Unreadable, oversized, non-regular or invalid secret file:** a config
  error naming the reference, never the path. Exit 2 (FR-15).
- **Unknown `CORRAL_*` environment variables:** one warn record naming them.
  Startup continues unless there are other errors (FR-8, FR-29).
- **Invalid `OTEL_*` values with tracing on:** every invalid variable is
  reported in the same pass (FR-19), as JSON, including when
  `OTEL_TRACES_EXPORTER` itself is invalid. Exit 2.
- **`tracing.enabled` itself invalid:** `OTEL_*` validation is skipped
  (FR-19).
- **Admin listener bind failure:** SPEC-001 FR-35. One `admin listener bind
  failed` record is emitted, with `component=config`,
  `setting=admin.listen`, error class `config` and an FR-29 detail such as
  `EADDRINUSE`. It has no address or port, and there is no `invalid
  configuration` record. Exit 1 (FR-24, Amendment 3).
- **SIGHUP:** never terminates corral once `main` has started; in a normal
  run, once logging is configured, a rate-limited warn record (FR-18).
- **Unrecovered panic:** the Go runtime exits 2 (FR-24); this is not a
  config error.
- **Registry declaration conflict (duplicate or colliding derived name,
  reserved `file` / `_file` last segment, bad segment, secret with a default,
  default that fails its own type):** a programming error caught by AC-1. It
  is not an operator-facing failure.

## Non-functional requirements

- **Security:**
  - secrets are never on the command line, never emitted, and never shown in
    help output (FR-14, FR-21);
  - config errors never echo values or paths (FR-11);
  - secret file reads are bounded at 64 KiB and never block on FIFOs or
    devices (FR-15, FR-25);
  - unknown environment-variable names with unusual bytes are not rendered
    (FR-8);
  - check-config mode opens no network connections (FR-23).
- **Dependencies:** standard library only, per Owner preference. No
  third-party flag, environment or config library is added.
- **Performance:** loading runs once at startup. There are no hot-path
  requirements.
- **Observability:** the records in the Interfaces table use only existing
  SPEC-001 FR-39 keys (`setting`, `error`, `corral.error.detail`,
  `suppressed_count`). No SPEC-001 amendment is needed.

## Data and compliance

- **Data touched and its classification:**
  - operational settings: internal;
  - secret settings, such as upstream MongoDB service credentials:
    confidential, never emitted;
  - file paths of secret references: internal, never emitted;
  - names of unknown `CORRAL_*` environment variables: internal, names only.
- **PII:** none. Settings describe infrastructure. Upstream credentials
  belong to service accounts, not end users. Under SPEC-001 FR-24, they and
  any usernames are never emitted.
- **Retention:** corral persists nothing. Values live in process memory for
  the life of the process.

## Acceptance criteria

- **AC-1** (FR-1, FR-2, FR-3, FR-14): Unit.
  - The registry refuses to build for each of:
    - a duplicate name;
    - a single-segment name;
    - an uppercase or hyphenated segment;
    - a last segment of `file` (`a.b.file`);
    - a last segment ending in `_file` (`a.b_file`);
    - two names that collide on the environment variable (`a.b_c` /
      `a_b.c`);
    - `a_b.file` registered alongside a secret `a.b`. Every collision with a
      file reference needs a last segment of `file` or ending in `_file`, so
      the reserved-segment rule catches it. A separate unit test of the
      derived-name check, which bypasses the segment rule, confirms that the
      check includes the `<name>_file` and `CORRAL_<NAME>_FILE` names;
    - a secret with a default;
    - a default that fails its own type or rule;
    - a cross-setting rule whose primary setting is not among its involved
      settings.
  - A test over the real registry asserts that every setting has a type from
    FR-12, a valid presence, a non-empty description and unique derived
    names that follow FR-3.
  - The SPEC-001 settings are all present with the types, presences and
    defaults in Interfaces.
- **AC-2** (FR-3, FR-4, FR-5, FR-10): Unit, table-driven over every
  registered non-secret setting:
  - with no input, the default applies;
  - a valid value via the flag alone is used, and likewise via the
    environment variable alone;
  - when both are given with different valid values, the flag's value wins;
  - a list given in both sources takes the flag's list only;
  - an invalid value via the environment variable is rejected, with
    `setting=CORRAL_<NAME>`, even when a valid flag is also given.
- **AC-3** (FR-6, FR-8, FR-13, FR-28): Unit.
  - `--x=v` and `-x=v` are both accepted.
  - `--log.level debug` yields exactly two records, in this order:
    `setting=log.level`, then `setting=argument`.
  - Each of these yields exactly one record:
    - bare `--tracing.enabled`: `setting=tracing.enabled`;
    - `--x=1 --x=2`: `setting=x`;
    - `--x=1 --x=2 --x=3`: `setting=x`, at the position of the second
      occurrence;
    - `--t.secret=v1 --t.secret=v2` (secret): `setting=t.secret`, at the
      position of the first occurrence;
    - `--x=1 -x=2`: `setting=x`;
    - `--x=1 --x`: `setting=x`, at the position of the second occurrence,
      with no missing-value record;
    - `--x --x=1`: `setting=x`, at the position of the second occurrence,
      with no missing-value record;
    - bare secret `--t.secret`: `setting=t.secret`, at its position (missing
      value, not a secret-literal record);
    - `--t.secret --t.secret=v`: `setting=t.secret`, at the position of the
      second argument (the first occurrence with `=`);
    - a positional argument: `setting=argument`;
    - `--`: `setting=unknown`;
    - `-`: `setting=unknown`.
  - Two unknown flags and a positional argument, interleaved, produce
    records in argument order.
- **AC-4** (FR-7, FR-12): Unit.
  - For each type, accepted and rejected examples, including:
    - `bool` rejects `1` and `yes`;
    - `int` rejects `+5`, `0x10` and `1_000`;
    - `float` rejects `NaN`, `Inf`, `+0.5`, `0x1p-2` and `1_0`, and accepts
      `.5`, `1.` and `1e-3`;
    - `duration` rejects `5`, `0`, `-1s`, `+1s` and `1µs`, and accepts
      `250ms` and `1m30s`; `0s` is accepted only with a rule that allows
      zero;
    - `hostport` rejects `127.0.0.1:09187`, `127.0.0.1:+80`, `:9187`,
      `localhost.:9187` (trailing dot) and `[fe80::1%eth0]:9187` (IPv6
      zone), and accepts `localhost:9187` and `[::1]:9187`;
    - `list` keeps a U+00A0 at the edge of an element (it is not trimmed),
      so an `int` list consisting of `1`, a comma, U+00A0 and `2` is
      rejected (Amendment 3: the example is now spelled out, because the
      literal U+00A0 had been lost and the text showed an ordinary space,
      which is trimmed);
    - `string` rejects a value containing U+007F and a value containing
      U+0085;
    - `text` accepts an inner `\n` and rejects U+0000;
    - `list` rejects `a,,b` and trims `a, b`;
    - invalid UTF-8 is rejected by `string` and `text`.
  - An environment variable set to `""` or `"  "` counts as unset (the
    default applies), and the same holds for `CORRAL_<NAME>_FILE`.
  - A flag with an empty value is rejected for `log.level`.
  - In an environment snapshot with a duplicate key, the first occurrence
    is used.
- **AC-5** (FR-8, FR-11, FR-29): Unit and functional.
  - An unknown flag carrying a marker yields `setting=unknown`, and the
    marker appears nowhere in stdout or stderr (SPEC-001 AC-33 harness).
  - With `CORRAL_SERVICE_HOST`, `CORRAL_ZZ_TYPO`, `CORRAL_` and
    `corral_lower` set (each value a marker), startup continues and exactly
    one warn record is emitted, with
    `setting=CORRAL_,CORRAL_SERVICE_HOST,CORRAL_ZZ_TYPO`. No marker appears.
  - An unknown variable set to an empty string is not listed.
  - An unknown variable whose name contains a byte outside `[A-Za-z0-9_]`
    is listed as `CORRAL_<invalid-name>`, and the raw name does not appear.
  - `OTEL_*` variables never appear in the record.
  - With an unknown variable plus an invalid setting, the warn record comes
    first, then the error record, both as JSON, even with
    `--log.format=text`.
- **AC-6** (FR-9, FR-11, FR-13, FR-19, FR-24, FR-29): Functional (binary).
  - Inputs, all carrying markers:
    - an unknown flag;
    - two invalid settings from flags;
    - one invalid setting from the environment;
    - `--tracing.enabled=true`;
    - two invalid `OTEL_*` variables;
    - `--log.format=text`.
  - The process exits 2.
  - stdout holds only JSON records, in this order: the unknown-flag record
    (group 1); the three per-input records, sorted by `setting` (logical
    names for the flags, the `CORRAL_*` name for the environment variable);
    the two `OTEL_*` records, sorted.
  - No marker appears in any encoding the AC-33 harness scans, and nothing
    is written to stderr.
  - With a test registry containing a required setting that is not
    supplied, the error names that setting by its logical name.
  - With `CORRAL_TRACING_ENABLED=maybe` and no flag, and an invalid `OTEL_*`
    variable, only the `CORRAL_TRACING_ENABLED` record appears.
- **AC-7** (FR-10): Functional: with an invalid setting and the admin
  listener's port pre-bound by the test, the process exits 2 (not 1), and no
  `lifecycle` starting record is emitted.
- **AC-8** (FR-14, FR-15, FR-28, FR-25): Unit, with a test registry holding a
  secret setting `t.secret` of type `text`, an in-memory file reader, and
  marker values and paths throughout.
  - `--t.secret=<marker>` yields exactly one record, `setting=t.secret`
    (group 1), and exit code 2. The marker is not echoed.
  - `--t.secret=<marker> --t.secret_file=<path>` yields exactly one record,
    `setting=t.secret`, and the reader is not called for that path.
  - `--t.secret=<marker> --t.secret_file=p1 --t.secret_file=p2` yields
    exactly two records, in this order: `setting=t.secret` (at the literal's
    position), then `setting=t.secret_file` (at the position of the second
    `_file` occurrence). The reader is never called.
  - A literal via the environment is accepted.
  - `CORRAL_T_SECRET` and `CORRAL_T_SECRET_FILE` both set yields exactly one
    record, `setting=CORRAL_T_SECRET`, and the reader is not called.
  - The same pair together with a valid `--t.secret_file` yields that one
    record, and the run still fails with exit code 2.
  - `--t.secret_file=p1 --t.secret_file=p2` yields exactly one record,
    `setting=t.secret_file`, and the reader is not called.
  - A file reference via the flag and via the environment is accepted.
    Exactly one trailing `\n`, and separately `\r\n`, is removed, and an
    inner newline is kept.
  - Each of these is rejected with one record naming the reference, and the
    path marker does not appear:
    - a missing file;
    - a directory;
    - a FIFO (rejected without blocking, unit test with the real reader on
      Unix);
    - a 65,537-byte file;
    - a file containing only `\n`;
    - `--t.secret_file=` (empty path).
  - A 65,536-byte file is accepted.
  - A flag file reference beats an environment literal.
  - An overridden `CORRAL_T_SECRET_FILE` pointing at a missing file is still
    a config error, under `CORRAL_T_SECRET_FILE`.
  - A non-secret setting's `_file` form is an unknown flag, or an unknown
    environment variable.
  - An optional secret that no source provides loads as unset, with no
    error.
- **AC-9** (FR-14, FR-20, FR-21): Functional, using the AC-33 harness, with a
  secret setting supplied through an environment literal and through a file
  reference (markers in the value and the path), plus debug logging:
  - across a full startup and shutdown, help mode and check-config mode, no
    marker appears in stdout, stderr, `/metrics` or `/readyz` bodies;
  - the FR-20 record lists the secret's name.
- **AC-10** (FR-17, FR-18): Functional and unit.
  - Functional: sending SIGHUP to a running corral leaves it running and
    serving `/healthz`, one warn record with the FR-18 message is emitted,
    and a later SIGTERM exits 0. SIGHUP sent while startup is held by a test
    hook before the configured logger exists does not terminate the process,
    and it emits no record. The test sends signals only after `main` has
    started (for example after a test hook confirms the SIGHUP handler is
    installed), never in the pre-`main` window.
  - Unit, with a fake clock: 6 SIGHUPs within 30 seconds produce one record.
    The next SIGHUP after 30 seconds produces a record with
    `suppressed_count=5`.
  - Unit: the loader's injected reader is called once per reference, so a
    referenced file changed after loading has no effect.
- **AC-11** (FR-20, FR-29): Unit.
  - With `--log.level=debug` and `CORRAL_ADMIN_LISTEN` set, the record's
    `setting` is `admin.listen,log.level`.
  - With `--tracing.enabled=true` and `OTEL_SDK_DISABLED=true`,
    `tracing.enabled` is listed.
  - With no input, the record's `setting` is empty.
  - The record holds no values (marker check).
  - At `log.level=warn`, the record is not emitted.
  - On the success path, the records appear in FR-29 order: FR-8 warn, then
    FR-41 warn, then FR-20 info, then `lifecycle` starting.
- **AC-12** (FR-21, FR-22, FR-27): Functional.
  - `--help`, `-help` and `-h` exit 0 and print, for every registered
    setting, its logical name, flag, environment variable, type, presence
    text, the secret text for secret settings, and its description.
  - With `CORRAL_LOG_LEVEL=<marker>` set, the marker is absent from the help
    output.
  - Every row of the FR-27 combinations table gives the stated mode, exit
    code and records.
  - `--version` prints exactly one line matching `^corral \S+$`. An
    unstamped test build prints `corral devel`.
  - Neither help nor version mode starts the admin listener (its port stays
    free), and neither emits a log record.
- **AC-13** (FR-23, FR-24, FR-29): Functional.
  - `--check-config --log.level=info` with valid settings exits 0 and emits
    the FR-20 record. It opens no listener (ports stay free) and no outbound
    connection (with tracing on, a test collector configured through
    `OTEL_EXPORTER_OTLP_ENDPOINT` accepts zero connections). It emits no
    `lifecycle` record.
  - With `--log.format=text`, the success output is in slog text format.
  - With an invalid `OTEL_TRACES_SAMPLER` and tracing on, it exits 2 with the
    SPEC-001 FR-27 record as JSON.
  - With an `http://` endpoint and a valid CA certificate variable, it emits
    the SPEC-001 FR-41 record before the FR-20 record.
- **AC-14** (FR-16): The SPEC-001 AC-1 import check additionally fails if a
  domain package imports the settings registry or loader package.
- **AC-15** (FR-25, FR-19): Unit and static.
  - Unit: the loader runs with a fake environment snapshot and an in-memory
    file reader, and two runs with identical inputs give identical results.
  - Static check: the loader package and the tracing environment-validation
    code do not reference any of: `os.Args`, `os.Getenv`, `os.LookupEnv`,
    `os.Environ`, `syscall.Getenv`, `syscall.Environ`, `os.ReadFile`,
    `os.Open`, `os.OpenFile`, `os.Stat` and `os.Lstat`.
- **AC-16** (FR-26): Unit: every registered setting's logical name and
  environment variable name appear in `docs/operations/configuration.md`. The
  other FR-26 topics are verified by review.
- **AC-17** (FR-12, FR-1): Unit: the SPEC-001 AC-21 cases pass when supplied
  through the environment as well as through flags, including
  `log.level=debug-4` and `admin.listen=:9187` (both rejected) and
  `log.level=DEBUG` (accepted).
- **AC-18** (FR-30, FR-28): Unit, with a test registry declaring a rule over
  `t.a` and `t.b` with primary `t.b`:
  - a violation reports one record with `setting=t.b` when `t.b` comes from
    a flag or a default, and `setting=CORRAL_T_B` when it comes from an
    environment literal;
  - the rule is skipped, with no group 3 record, when `t.a` has an invalid
    value or `t.b` was repeated on the command line;
  - group 3 records follow every group 2 record.

### Added in Amendment 1

- **AC-19** (FR-18, FR-23): Unit or functional: in check-config mode, with
  a test hook that holds the process after the configured logger is built and
  before exit, a SIGHUP neither terminates the process nor produces any
  record. The output is exactly the FR-23 success records, and the exit code
  is 0.
- **AC-20** (FR-19): Unit, with tracing on:
  - with `OTEL_BSP_MAX_QUEUE_SIZE=abc` and `OTEL_BSP_MAX_EXPORT_BATCH_SIZE`
    set to a value larger than the default queue size (2048), exactly one
    record appears, with `setting=OTEL_BSP_MAX_QUEUE_SIZE`; there is no
    batch-size record;
  - with a non-PEM `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` and a valid
    `OTEL_EXPORTER_OTLP_CLIENT_KEY`, exactly one record appears, with
    `setting=OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE`; there is no pairing
    record;
  - with a valid queue size of 100 and a batch size of 200, exactly one
    record appears, with `setting=OTEL_BSP_MAX_EXPORT_BATCH_SIZE`;
  - (Amendment 2) with `OTEL_TRACES_SAMPLER=x` and
    `OTEL_TRACES_SAMPLER_ARG=9`, exactly one record appears, with
    `setting=OTEL_TRACES_SAMPLER`; there is no `OTEL_TRACES_SAMPLER_ARG`
    record.
- **AC-21** (FR-19): Unit, with tracing on: `OTEL_TRACES_EXPORTER=console`
  together with an invalid `OTEL_EXPORTER_OTLP_PROTOCOL` and a malformed
  `OTEL_EXPORTER_OTLP_HEADERS` yields exactly three records, sorted by
  `setting`: `OTEL_EXPORTER_OTLP_HEADERS`, `OTEL_EXPORTER_OTLP_PROTOCOL`,
  `OTEL_TRACES_EXPORTER`.
- **AC-22** (FR-7, FR-19): Unit.
  - A `CORRAL_LOG_LEVEL` whose value consists only of each of these counts as
    unset (the default applies): U+0009, U+000B, U+000C, U+0085, U+00A0,
    U+2003 and U+3000.
  - A value consisting only of U+200B is set, and is rejected as an invalid
    `log.level`.
  - A value consisting of invalid UTF-8 bytes only is set and rejected.
  - With tracing on and a CA certificate set, an `OTEL_EXPORTER_OTLP_ENDPOINT`
    consisting only of U+00A0 counts as not set, so no FR-41 record is
    emitted. This matches the SDK's `strings.TrimSpace` behaviour.

### Added in Amendment 3

- **AC-23** (FR-24): Functional (binary). The test pre-binds the port of a
  valid `admin.listen` address whose host and port carry a distinctive
  marker (for example a marker port number), with every other setting
  valid. Then:
  - the process exits 1;
  - stdout holds exactly one record with message `admin listener bind
    failed`, level error, `component=config`, `setting=admin.listen`,
    `error=config` and `corral.error.detail=EADDRINUSE`;
  - no record has the message `invalid configuration`;
  - the address marker appears nowhere in stdout or stderr, in any encoding
    the SPEC-001 AC-33 harness scans.

  This also covers SPEC-001 AC-30.

## Open questions

Must be empty before approval.

None.

## Decisions log

The Owner answered "accept all" on 2026-10-24, adopting the spec-writer's
recommended default for every question (relayed by the Orchestrator). Q-10
was merged into Q-9 before the questions were asked.

| Date | Decision | Reason |
|---|---|---|
| 2026-10-24 | Q-1: v1 sources are command-line flags and environment variables only. A configuration file is a non-goal for v1; if one is added later, it ranks between environment variables and defaults (FR-4, Non-goals). | Owner decision. systemd `EnvironmentFile` and Kubernetes `envFrom` cover file-based setups, and no third-party parser is needed (stdlib preference). |
| 2026-10-24 | Q-2: precedence is flags > environment > defaults. Values are not merged across sources, and an overridden lower-precedence value is still validated (FR-4, FR-5, FR-10). | Owner decision. |
| 2026-10-24 | Q-3: the flag is `--` plus the logical name verbatim (`-` also accepted); the environment variable is `CORRAL_` plus the name uppercased with `.` replaced by `_`; environment-name collisions are rejected by the registry (FR-2, FR-3). | Owner decision. Mechanical mapping with no aliases, and the interim flags keep working. |
| 2026-10-24 | Q-4: only the `--name=value` flag form is accepted; there is no space-separated form and no bare boolean flag (FR-6). | Owner decision. Unambiguous errors, and the same as the interim behaviour. |
| 2026-10-24 | Q-5: value syntax per the FR-12 type set (strict `true`/`false`, Go durations with a required unit, comma-separated lists). An environment variable that is empty or whitespace-only counts as unset (FR-7, FR-12). | Owner decision. |
| 2026-10-24 | Q-6: unknown flags are fatal with `setting=unknown`, never echoed. Unknown `CORRAL_*` environment variables are not fatal: one warn record lists their names, never their values (FR-8). | Owner decision. Kubernetes service links inject `CORRAL_*` variables when a Service is named `corral`. |
| 2026-10-24 | Q-7: all config errors are reported in one pass, ordered by `setting`, where `setting` is the name as written in its source (FR-11, FR-13). | Owner decision. |
| 2026-10-24 | Q-8: exit code 2 for config and usage errors, 1 for other startup or runtime failures (including admin bind failure), 0 for clean exits (FR-24). | Owner decision. |
| 2026-10-24 | Q-9 (merged with Q-10): secret settings have no default and are never emitted; literal secrets on the command line are rejected; an environment literal or a `_FILE` / `--<name>_file` reference is allowed (64 KiB cap, one trailing newline stripped, no permission check); file references exist for secret settings only (FR-14, FR-15). | Owner decision. Command lines are visible in the process list. |
| 2026-10-24 | Q-11: no reload. SIGHUP is caught and ignored, with a warn record rate-limited to one per 30 seconds (FR-17, FR-18). | Owner decision. Go's default action on SIGHUP would kill the process. |
| 2026-10-24 | Q-12: `--help` / `-h` print plain-text usage (defaults only, never current values or secrets), and `--version` prints the version; both exit 0 (FR-21, FR-22). | Owner decision. |
| 2026-10-24 | Q-13: a `--check-config` validation-only mode with no listeners or outbound connections; exit 0 or 2 (FR-23). | Owner decision. |
| 2026-10-24 | Q-14: one `configuration loaded` info record lists the names, never the values, of explicitly set settings (FR-20). | Owner decision. Logging values would contradict SPEC-001 FR-7. |
| 2026-10-24 | SPEC-003 formally supersedes ADR-0003. The interim flags-only loader is retired. Unknown environment-variable names are logged (FR-8), unlike unknown flag names, which are never echoed. The ADR-0003 host-name rules for `admin.listen` and the `setting=unknown` / `setting=argument` reporting are carried into this spec (FR-6, FR-8, FR-12). | Owner decision. |
| 2026-10-24 | Revision 2 resolves challenge-review findings M1 to M9 and S1 to S15 by making the existing decisions precise. No new product decision was taken; the Owner may override any of these. The resolutions are: environment snapshot injection (FR-7, FR-25); `file` / `_file` last-segment reservation and derived-name uniqueness (FR-2); a new `text` type for multi-line secrets (FR-12); an *optional* presence (FR-1, FR-9); cross-setting rules and their `setting` value (FR-30); grouped error ordering and the `OTEL_*` validation conditions, with every `OTEL_*` error reported (FR-13, FR-19); control-flag grammar with help > version > check-config precedence (FR-27); pinned conflict and repeat outcomes (FR-28); output path and order (FR-29), with all config errors (including `OTEL_*`) as bootstrap JSON and success records through the configured logger and level, including in check-config; panic exit code 2 documented (FR-24, FR-26); SIGHUP from process start (FR-18); overridden `_FILE` references still read (FR-5); unknown-variable matching and rendering rules (FR-8); exact numeric, duration, port and control-character syntax (FR-12); the empty-value asymmetry stated (FR-7); SPEC-001 table status and namespace list (Interfaces); file-reader contract (FR-25); version stamping (FR-22); documentation path (FR-26); help text for secrets and defaults (FR-21); FR-20 lists sources, not effects. | Challenge review REQUEST_CHANGES, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Re-check fixes N1 to N5:<br>N1: uniqueness is checked over two name sets, flag names (including `<name>_file`) and environment names (including `CORRAL_<NAME>_FILE`) (FR-2).<br>N2: injection covers validation only, and the OTLP exporter keeps SDK semantics over the real environment and files (FR-19, FR-25).<br>N3: the PEM validation size limit is 4 MiB (FR-19).<br>N4: three or more repeats, and repeated secret literals, each give one record at a fixed position (FR-28, AC-3).<br>N5: `CORRAL_<invalid-name>` is substituted before deduplication and sorting (FR-8).<br>Optional item applied: the tightening note now includes leading `+` on `int` and `float` (FR-12). | Re-check verdict APPROVE with small fixes, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Amendment 1 (approved by the Owner, 2026-10-24, together with the additional pins below) closes five gaps found while deriving acceptance tests:<br>(1) the SIGHUP handler is installed as the first action of `main`, before configuration loading. Signal behaviour before `main` starts running, while the Go runtime and package initialisation are still running, is out of scope. Package init code, such as the `earlyslog` package, does run in that window, but `main` has not yet installed the handler, so a SIGHUP there may still terminate the process (FR-18, Non-goals, AC-10).<br>(2) check-config mode never emits the SIGHUP record, even after the configured logger exists, so its output is deterministic (FR-18, FR-23, AC-19).<br>(3) an `OTEL_*` cross-variable check is skipped, not evaluated with a default, when any variable it involves has its own error, consistent with the FR-30 skip rule (FR-19, AC-20).<br>(4) an invalid `OTEL_TRACES_EXPORTER` does not stop validation of the other honoured or validated `OTEL_*` variables, consistent with Q-7's report-everything rule (FR-19, AC-21).<br>(5) "whitespace" in FR-7 is the `strings.TrimSpace` / `unicode.IsSpace` set that the OpenTelemetry Go SDK uses, so it matches SPEC-001 FR-41 (FR-7, FR-19, AC-22).<br>These are spec-writer resolutions that make existing decisions precise; the Owner may override any of them. | Gaps found while deriving acceptance tests, relayed by the Orchestrator, 2026-10-24. Drafted in `specs/drafts/` by the spec-writer, which may not edit approved specs. Promotion to `specs/` needs Owner approval. |
| 2026-10-24 | Amendment 1, additional pins (spec-writer resolutions; the Owner may override any of them when approving the text):<br>(P2) a `hostport` host with a trailing dot (`localhost.`) is rejected, because it leaves an empty last label; IPv6 zone identifiers (`[fe80::1%eth0]`) are rejected. Both are listed as tightenings relative to the interim loader (FR-12, AC-4).<br>(P4) FR-28 row 1 now applies to a literal together with a *single* `--a.b_file=p`. `--a.b=v --a.b_file=p1 --a.b_file=p2` gives two group 1 records, `a.b` at the literal's position and `a.b_file` at the second `_file` position, and no file is read (FR-28, AC-8).<br>(P5) one general occurrence rule: every occurrence of a flag name counts toward repeats, with or without `=`. Two or more occurrences give one repeat record at the second occurrence, with no missing-value record. A single bare occurrence gives a missing-value record. A bare secret `--a.b` gives one `a.b` record at its position, counted as a missing value, not a literal. If any occurrence of a secret flag has `=`, the single secret-literal record (at the first such occurrence) takes precedence (FR-6, FR-28, AC-3).<br>(Optional) FR-12 list trimming (ASCII space and tab) is stated to be deliberately narrower than the FR-7 whitespace set (FR-12, AC-4).<br>(Nit) the pre-`main` rationale in the previous row now uses the Non-goals wording. | Challenge review of Amendment 1, relayed by the Orchestrator, 2026-10-24. |
| 2026-10-24 | Amendment 2: when `OTEL_TRACES_SAMPLER` itself has an error, the `OTEL_TRACES_SAMPLER_ARG` check is skipped and produces no record, consistent with the FR-19 cross-variable skip rule, because whether the argument is valid depends on the sampler (SPEC-001 FR-21). This is listed as a cross-variable check in FR-19, and AC-20 gains the case `OTEL_TRACES_SAMPLER=x`, `OTEL_TRACES_SAMPLER_ARG=9`, which gives exactly one record, `OTEL_TRACES_SAMPLER` (FR-19, AC-20). | Owner decision, 2026-10-24, relayed by the Orchestrator. The Owner approved the content, and it is recorded here as Amendment 2. |
| 2026-10-24 | Amendment 2, consistency fix: FR-11 previously gave the `setting` of a cross-setting error record as "the FR-30 rule", which contradicted FR-30 (and SPEC-002 AC-31). FR-11 now matches FR-30: `setting` is the name of the rule's primary setting, spelled for the source of its effective value (FR-11, FR-30). | Spec-writer consistency fix, requested by the Orchestrator, 2026-10-24. It is not part of the Owner's sampler decision and needs Owner approval when the amendment is promoted. |
| 2026-10-24 | Amendment 2 promoted; the Owner also approved the FR-11 consistency fix (G-16). | Owner approval, 2026-10-24. |
| 2026-10-25 | Amendment 3: an admin listener bind failure (SPEC-001 FR-35) is logged with a distinct constant message, `admin listener bind failed`. The record has level error, `component=config`, `setting=admin.listen`, the SPEC-001 FR-29 `error` attribute (class `config`) and a `corral.error.detail` from the FR-29 enum (for example `EADDRINUSE`), and never the address, host or port. The process exits 1. Its error class stays `config`, so config errors (exit 2) are identified by the message `invalid configuration`, not by class. The record is added to the Interfaces log-records table and referenced from FR-24 and Behavior on failure. AC-23 is added. This keeps FR-24 true, because using `invalid configuration` for bind failures had contradicted FR-24's identification rule (FR-24, Interfaces, Behavior on failure, AC-23). | Verification of SPEC-003 found the contradiction, and the implementation was fixed. Relayed by the Orchestrator, 2026-10-25. Drafted in `specs/drafts/` by the spec-writer, which may not edit approved specs. Promotion to `specs/` needs Owner approval. |
| 2026-10-25 | Amendment 3, consistency fix: the AC-4 `list` example meant to contain U+00A0 had lost that character and showed an ordinary space. An ordinary space is trimmed under FR-12, so the example contradicted its own "is rejected". The example now spells out the characters (`1`, comma, U+00A0, `2`) (AC-4). | Spec-writer consistency fix, found while copying the approved text, 2026-10-25. It is not part of the bind-failure change and needs Owner approval when the amendment is promoted. |
| 2026-10-25 | Amendment 3 approved by the Owner and promoted, including the AC-4 U+00A0 consistency fix. | Owner approval, 2026-10-25. |
