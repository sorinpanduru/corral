# Configuration

This guide describes how corral is configured: the sources, their
precedence, naming, value syntax, secrets, error handling and the control
flags. Logging, metrics, tracing and the admin listener are described in
[observability.md](observability.md). The behaviour here is specified by
SPEC-003.

Examples use placeholders only (`<YOUR_DB_PASSWORD>`, `/run/secrets/<name>`).
Never put a real credential in a manifest, a script or a command line.

## Sources and precedence

Settings come from these sources, from highest to lowest precedence:

1. command-line flags;
2. environment variables;
3. defaults declared by the setting.

A setting's effective value comes whole from the highest-precedence source
that provides it. Values are never merged across sources: a list given by a
flag replaces the environment list, it is not appended to it.

A value that is overridden by a higher-precedence source is still processed
and validated. An invalid environment value is a configuration error even when
a flag overrides it, and an overridden `CORRAL_<NAME>_FILE` is still read, so a
missing or invalid file is still an error.

Settings are read once at startup (see "Restart and SIGHUP").

## Names

Every setting has a logical name made of at least two dot-separated segments,
for example `log.level`.

| Logical name | Flag | Environment variable | File reference (secret settings only) |
|---|---|---|---|
| `log.level` | `--log.level=<value>` | `CORRAL_LOG_LEVEL` | not applicable |
| `tracing.sample_ratio` | `--tracing.sample_ratio=<value>` | `CORRAL_TRACING_SAMPLE_RATIO` | not applicable |
| `<ns>.<secret>` (example) | rejected as a literal | `CORRAL_<NS>_<SECRET>` | `--<ns>.<secret>_file=<path>`, `CORRAL_<NS>_<SECRET>_FILE` |

The environment variable is `CORRAL_` followed by the logical name in upper
case with each `.` replaced by `_`.

## Command-line syntax

- Only `--<name>=<value>` and `-<name>=<value>` are accepted.
- The space-separated form (`--log.level debug`) and bare boolean flags
  (`--tracing.enabled`) are errors: the flag has no value. In the
  space-separated form the next word is also reported as an unexpected
  argument.
- An empty value (`--log.level=`) is validated as an empty string and fails
  unless the setting allows empty values.
- Giving the same flag twice is an error, in any dash form and with or without
  a value.
- Positional arguments are errors (`setting=argument`). `--` is not an
  end-of-flags marker.
- Unknown or malformed flags are errors with `setting=unknown`. The flag text
  is never echoed, because a mistyped name may carry a secret.

## Environment variables

- Only variables whose names start with the exact, upper-case prefix `CORRAL_`
  are corral variables. Names such as `corral_x` are ignored.
- **Empty means unset.** A `CORRAL_*` variable whose value is empty or only
  whitespace is treated as not set. This applies to setting variables,
  `_FILE` variables and unknown variables.
- Whitespace means the characters Go's `strings.TrimSpace` removes: tab, line
  feed, vertical tab, form feed, carriage return, space, U+0085 and the
  Unicode `White_Space` characters such as the no-break space (U+00A0),
  U+2003 and U+3000. The zero-width space (U+200B) and U+FEFF are not
  whitespace. A value that is not valid UTF-8 is never whitespace-only.
- Other values are used as given, with no trimming. A value with leading or
  trailing whitespace is validated as is and usually fails.
- If a variable appears more than once in the environment, the first
  occurrence is used.
- **Asymmetry with flags, by design.** A flag can give an empty value, which is
  then validated and usually rejected. An environment variable cannot, because
  empty means unset. Container templates often render unset values as empty
  strings, and this rule makes them fall back to the lower-precedence value or
  the default.

### Unknown `CORRAL_` variables

A set variable with the `CORRAL_` prefix that is not a registered setting (or a
secret's `_FILE` variable) is ignored with one warn record: `component=config`,
message `unknown CORRAL_ environment variables ignored`, and a `setting`
attribute listing the names, deduplicated, sorted and joined with `,`. Values
are never listed. A name with a byte outside `[A-Za-z0-9_]` is shown as
`CORRAL_<invalid-name>`. `OTEL_*` variables are never unknown.

**Kubernetes service links.** If a Service is named `corral`, Kubernetes
injects variables such as `CORRAL_SERVICE_HOST` and `CORRAL_PORT` into pods.
They are harmless but produce this warning. To avoid it, set
`enableServiceLinks: false` in the pod spec.

## Value syntax

| Type | Accepted syntax |
|---|---|
| `string` | valid UTF-8 without control characters (U+0000 to U+001F, U+007F, U+0080 to U+009F); non-empty unless the setting allows empty |
| `text` | like `string`, but tab, line feed and carriage return are allowed (multi-line secrets) |
| `bool` | `true` or `false`, case-insensitive; nothing else (`1`, `yes` are rejected) |
| `int` | optional `-` and digits; no `+`, hex or `_`; must fit 64 bits |
| `float` | `-?([0-9]+(\.[0-9]*)?\|\.[0-9]+)([eE][+-]?[0-9]+)?`, finite; no `NaN`, `Inf`, `+`, hex or `_` |
| `duration` | one or more `<number><unit>` groups with units `ns`, `us`, `ms`, `s`, `m`, `h` (ASCII only), no sign, unit required, greater than zero unless the setting allows zero (`250ms`, `1m30s`) |
| `enum` | one of the declared values, case-insensitive; the declared spelling is used |
| `hostport` | `host:port`; port in decimal digits 0 to 65535 with no sign or leading zeros; host is an IP literal (IPv6 in brackets, no zone) or a DNS name (labels 1 to 63 letters, digits and `-`, no leading or trailing `-`, at most 253 characters in total, no trailing dot, last label not numeric or hex-like) |
| `list` | elements separated by `,`; ASCII space and tab around an element are trimmed; empty elements are rejected; each element uses the element type |

Invalid UTF-8 is rejected by every type. Each rejected value produces exactly
one error naming the setting, never the value.

## Secrets

A setting declared secret (for example upstream credentials added by later
specs):

- never has a default;
- is never written to any log record, error, help text, metric, span or HTTP
  response;
- **cannot be given as a literal on the command line.** `--<name>=<value>` is a
  configuration error, because command lines are visible to other local users
  through the process list and `/proc/<pid>/cmdline`. **If a secret was ever
  passed on a command line, rotate it, even though corral rejected it: it was
  visible in the process list.**
- may be given through its environment variable (`CORRAL_<NAME>`) or through a
  file reference.

### File references (`_FILE`)

For each secret setting `<name>` there are two reference names:
`--<name>_file=<path>` and `CORRAL_<NAME>_FILE`. Non-secret settings have none
(their `_file` forms are unknown inputs).

```
# Kubernetes Secret mounted as a file
CORRAL_<NAME>_FILE=/run/secrets/<name>
corral --<name>_file=/run/secrets/<name>
```

- An empty path in the flag is an error. An empty or whitespace-only
  `CORRAL_<NAME>_FILE` counts as unset.
- Each file is read once, at startup, through a reader that never blocks on a
  FIFO or device.
- It is an error when the path does not exist or cannot be read, does not
  resolve (after following symlinks) to a regular file, or the file is larger
  than 64 KiB (65,536 bytes, measured before any newline is removed).
- The value is the file content with exactly one trailing newline (`\n` or
  `\r\n`) removed. It is then validated like any other value. An empty value is an
  error unless the setting allows it.
- File ownership and permission bits are not checked, because Kubernetes
  Secret mounts are often group- or world-readable.
- Paths and contents are never logged. Errors name the reference
  (`<name>_file` or `CORRAL_<NAME>_FILE`).
- A literal and a reference for the same secret in the same source are a
  conflict. `CORRAL_<NAME>` together with `CORRAL_<NAME>_FILE` is one error
  naming `CORRAL_<NAME>`, and neither is used. Across sources the normal
  precedence applies to the setting as a whole, so a flag file reference beats
  an environment literal.

## Errors and exit codes

All configuration problems are found in one pass and reported together, each as
an error record with `component=config`, message `invalid configuration` and a
`setting` attribute naming the offending flag, environment variable or
setting. Values, paths and file contents are never echoed.

Configuration errors are always written as JSON to stdout, even when
`log.format=text`, because the configured logger does not exist yet. Records
appear in this order: the unknown-variable warning (if any), command-line
errors in argument order, per-input errors sorted by name, cross-setting errors,
then `OTEL_*` errors.

| Exit code | Meaning |
|---|---|
| `0` | graceful shutdown, `--help`, `--version`, or a successful `--check-config` |
| `2` | configuration error (including misuse of control flags) |
| `1` | any other startup or runtime failure, including failure to bind the admin listener (record `admin listener bind failed`, `component=config`, `setting=admin.listen`; it is not an `invalid configuration` record) |

The Go runtime also exits with code 2 on an unrecovered panic or fatal runtime
error. Exit code 2 therefore does not by itself mean a configuration error:
look for `invalid configuration` records. A failed admin bind is not a
configuration error: it exits 1 with an `admin listener bind failed` record.

## Control flags

Control flags take no value and are not settings.

| Flag | Effect | Exit |
|---|---|---|
| `--help`, `-help`, `-h` | print usage to stdout | 0 |
| `--version`, `-version` | print `corral <version>` to stdout | 0 |
| `--check-config`, `-check-config` | validate the configuration and exit | 0 or 2 |

Every other spelling (`--h`, `--HELP`, `--help=x`) is an unknown flag. Mode
selection scans all arguments first: help wins over version, version wins over
check-config. In help and version mode every other argument is ignored. Repeating
a control flag has no extra effect. A control flag written with `=` is an error
(`setting=unknown`) unless a help or version spelling elsewhere selects that
mode.

| Command line | Mode | Exit | Records |
|---|---|---|---|
| `--help --version` | help | 0 | none |
| `--version --check-config` | version | 0 | none |
| `--check-config --help` | help | 0 | none |
| `--help --bogus=1` | help | 0 | none |
| `--version --log.level=bad` | version | 0 | none |
| `--check-config --check-config` | check-config | 0 or 2 | as below |
| `--check-config=true` | normal run (fails) | 2 | one, `setting=unknown` |
| `--check-config --help=x` | check-config (fails) | 2 | one, `setting=unknown`, plus any others |

**Help** lists every setting sorted by name: logical name, flag form,
environment variable, type (with the allowed enum values), presence (the default
text as declared, `(required)` or `(optional, unset by default)`), a note for
secrets and the description. It never prints values from the environment, flags
or files, and ends with a line pointing to the `OTEL_*` documentation.

**Version** prints one line, `corral <version>`. `make build` stamps the
version from the `VERSION` make variable (default `devel`):

```
make build VERSION=1.2.3
```

`VERSION` may contain only letters, digits and `. _ + -`. A build made without
`make` (`go build`, `go run`) prints `corral devel`.

**Check-config** validates everything a normal start would, including secret
file reads and `OTEL_*` validation, then exits without binding a listener,
connecting to anything or contacting a collector, and without `lifecycle`
records. On success it writes the same configuration records a start would, in
the configured format and level, and exits 0. On failure it writes the JSON error
records and exits 2. It never writes a SIGHUP record.

## Startup records

On a successful start, before the `lifecycle` record `corral starting`, corral
writes through the configured logger (subject to `log.level`):

1. the unknown-variable warning, if any (see above);
2. the warning that OTLP TLS settings are ignored because the traces endpoint
   uses `http://`, if it applies (names of variables only);
3. an info record `configuration loaded` with `component=config` and a
   `setting` attribute listing the names of the settings that were supplied by
   a flag or an environment variable, sorted and comma-separated (empty when
   none). Values are never listed. It is not written at `log.level=warn` or
   `error`.

## Restart and SIGHUP

Settings are read once at startup. There is no reload: changes to the
environment or to referenced files have no effect until corral is restarted.

SIGHUP does not terminate corral and does not reload anything. The handler is
installed as the first action of `main`. A SIGHUP that arrives during the Go
runtime start-up, before `main` runs, may still terminate the process.

- Before the configured logger exists, and in help, version and check-config
  mode, a SIGHUP produces no record.
- In a normal run, each SIGHUP produces a warn record `configuration reload not
  supported; restart required` (`component=config`), at most one per 30
  seconds. A record that follows dropped ones carries `suppressed_count`.

SIGINT and SIGTERM keep their graceful-shutdown behaviour.

## `OTEL_*` variables

`OTEL_*` variables are honoured and validated as described in
[observability.md](observability.md). They are not corral settings: they are never
listed as unknown or supplied. When tracing is on, every invalid variable is
reported in the same pass as the other configuration errors, as JSON, sorted by
variable name and after the corral errors. Validation of `OTEL_*` is skipped
when `tracing.enabled` could not be determined, or when tracing is off.
Whitespace-only `OTEL_*` values count as unset, with the same whitespace set as
above.

## Setting reference

All settings are listed with their flag, environment variable, type, presence,
secret status and description. None of the current settings is secret.

### `log.format`

- Flag: `--log.format=<value>`; environment variable: `CORRAL_LOG_FORMAT`
- Type: enum `json`, `text` (case-insensitive)
- Presence: default `json`
- Secret: no
- Log output format.

### `log.level`

- Flag: `--log.level=<value>`; environment variable: `CORRAL_LOG_LEVEL`
- Type: enum `debug`, `info`, `warn`, `error` (case-insensitive; offsets such
  as `info+2` are rejected)
- Presence: default `info`
- Secret: no
- Minimum log level.

### `admin.listen`

- Flag: `--admin.listen=<value>`; environment variable: `CORRAL_ADMIN_LISTEN`
- Type: hostport
- Presence: default `127.0.0.1:9187`
- Secret: no
- Admin HTTP listener address. The host must not be empty (`:9187` is
  rejected). Binding a non-loopback address exposes the unauthenticated admin
  listener; see the warnings in [observability.md](observability.md).

### `tracing.enabled`

- Flag: `--tracing.enabled=<value>`; environment variable:
  `CORRAL_TRACING_ENABLED`
- Type: bool
- Presence: default `false`
- Secret: no
- Enable OTLP tracing. `OTEL_SDK_DISABLED=true` forces it off.

### `tracing.sample_ratio`

- Flag: `--tracing.sample_ratio=<value>`; environment variable:
  `CORRAL_TRACING_SAMPLE_RATIO`
- Type: float, 0.0 to 1.0 inclusive
- Presence: default `1.0`
- Secret: no
- Trace sampling ratio. `OTEL_TRACES_SAMPLER` and `OTEL_TRACES_SAMPLER_ARG`
  take precedence when set.

Summary:

| Setting | Flag | Environment variable | Type | Presence | Secret |
|---|---|---|---|---|---|
| `admin.listen` | `--admin.listen=<value>` | `CORRAL_ADMIN_LISTEN` | hostport | default `127.0.0.1:9187` | no |
| `log.format` | `--log.format=<value>` | `CORRAL_LOG_FORMAT` | enum (json, text) | default `json` | no |
| `log.level` | `--log.level=<value>` | `CORRAL_LOG_LEVEL` | enum (debug, info, warn, error) | default `info` | no |
| `tracing.enabled` | `--tracing.enabled=<value>` | `CORRAL_TRACING_ENABLED` | bool | default `false` | no |
| `tracing.sample_ratio` | `--tracing.sample_ratio=<value>` | `CORRAL_TRACING_SAMPLE_RATIO` | float | default `1.0` | no |
