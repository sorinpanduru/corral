//go:build functional

package functional

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const earlyslogPkg = "github.com/sorinpanduru/corral/internal/infra/telemetry/earlyslog"

// logOnlyInitBeforeEarlyslog lists non-stdlib packages that reach only the
// std `log` package (transitively, not log/slog) and are known to initialise
// before earlyslog (the runtime may order them ahead of it). Each entry was
// reviewed: it must not log from a package-level initialiser. A new entry
// needs the same review (FR-28, ADR-0008). The stale-allowlist check requires
// every entry to remain a transitive dependent of log or log/slog and to
// still initialise before earlyslog in the real trace.
var logOnlyInitBeforeEarlyslog = map[string]string{
	// log.Print is called only from functions (ErrDelegator.Handle and
	// friends), never during init.
	"go.opentelemetry.io/otel/internal/errorhandler": "log used only at run time",
	// Reviewed in T24.2 (grpc v1.83.2): grpclog's init only calls
	// SetLoggerV2(newLoggerV2()), which builds loggers over os.Stderr or
	// io.Discard from GRPC_GO_LOG_* and emits nothing. Its writes bypass std
	// log. Init-time and bootstrap output (grpc client bootstrap after
	// logbridge.Install) is covered by TestT24_GRPCLogNeverReachesStderr; export
	// is not, as the binary creates no spans yet.
	"google.golang.org/grpc/grpclog": "init builds the default LoggerV2 only; no output at init",
	// Reviewed in T24.2: its only package-level state is
	// `var logger = grpclog.Component("core")` (a cache lookup, no output);
	// logger.Errorf is called only from State.String on an unknown state.
	"google.golang.org/grpc/connectivity": "logger var is a lookup; logs only at run time",
}

var initTraceLine = regexp.MustCompile(`^init (\S+) @`)

// hermeticGo runs the go tool with the user's go env file and any package
// driver switched off (as in T21), from the module root.
func hermeticGo(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = moduleRoot(t)
	cmd.Env = hermeticEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go %s: %v", strings.Join(args, " "), err)
	}
	return out
}

type listedPkg struct {
	ImportPath string
	Standard   bool
	Imports    []string
	Deps       []string
}

func containsStr(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// initOrder returns the position of every "init <pkg>" line of the built
// binary's GODEBUG=inittrace=1 output, with no tracing configured. Packages
// without init work do not appear (they cannot log at init).
func initOrder(t *testing.T) map[string]int {
	t.Helper()
	// Run the binary only for its init trace; it exits on the bad flag.
	cmd := exec.Command(binPath, "-no-such-flag")
	cmd.Env = baseEnv("GODEBUG=inittrace=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	_ = cmd.Run() // exit status is irrelevant here
	order := map[string]int{}
	sc := bufio.NewScanner(&stderr)
	for i := 0; sc.Scan(); i++ {
		if m := initTraceLine.FindStringSubmatch(sc.Text()); m != nil {
			order[strings.ReplaceAll(m[1], "%2e", ".")] = i
		}
	}
	return order
}

// T24.2 init-order guard on transitive dependents (FR-24, FR-28, AC-3,
// ADR-0008). earlyslog neutralises slog.Default() and the log output from its
// own init, which only protects against packages that initialise after it.
// Using `go list -deps -json` of ./cmd/corral (Deps = transitive closure) and
// the real init order from GODEBUG=inittrace=1 of the built binary:
//
//   - every non-stdlib package whose transitive closure contains log/slog and
//     that has init work must initialise after earlyslog (strict);
//   - every non-stdlib package whose closure contains only log and that
//     initialises before earlyslog must be on the reviewed allowlist. This is
//     a "does not log at init" review gate, not a strict ordering.
//
// A package that reaches slog/log through an intermediate package is covered.
func TestT22_EarlyslogInitOrderGuard(t *testing.T) {
	out := hermeticGo(t, "list", "-deps", "-json=ImportPath,Standard,Imports,Deps", "./cmd/corral")
	var slogUsers, logOnlyUsers []string
	indirectSlog := map[string]bool{}
	haveEarly := false
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p listedPkg
		if err := dec.Decode(&p); err != nil {
			t.Fatalf("decode go list: %v", err)
		}
		if p.ImportPath == earlyslogPkg {
			haveEarly = true
			continue
		}
		if p.Standard {
			continue
		}
		hasSlog := containsStr(p.Deps, "log/slog")
		hasLog := containsStr(p.Deps, "log")
		switch {
		case hasSlog:
			slogUsers = append(slogUsers, p.ImportPath)
			if !containsStr(p.Imports, "log/slog") {
				indirectSlog[p.ImportPath] = true
			}
		case hasLog:
			logOnlyUsers = append(logOnlyUsers, p.ImportPath)
		}
	}
	if !haveEarly {
		t.Fatalf("%s is not a dependency of ./cmd/corral", earlyslogPkg)
	}
	if len(slogUsers) == 0 {
		t.Fatal("found no third-party transitive log/slog dependents: guard would be vacuous; check go list output")
	}
	// Self-check: pin a named third-party package that reaches log/slog only
	// transitively (no direct import), so the selection is provably stronger
	// than the old direct-importer guard. First-party packages would not do.
	const pinnedIndirect = "go.opentelemetry.io/otel"
	if !indirectSlog[pinnedIndirect] {
		t.Fatalf("%s is not selected as an indirect log/slog dependent: transitive selection is broken "+
			"(or the pin needs replacing with another third-party indirect dependent)", pinnedIndirect)
	}

	order := initOrder(t)
	early, ok := order[earlyslogPkg]
	if !ok {
		t.Fatalf("no inittrace line for earlyslog (got %d init lines)", len(order))
	}

	sort.Strings(slogUsers)
	for _, p := range slogUsers {
		if i, ok := order[p]; ok && i < early {
			t.Errorf("%s depends (transitively) on log/slog and initialises before earlyslog (FR-28): "+
				"a bump broke the init-order assumption; fix the import graph (ADR-0008)", p)
		}
	}
	sort.Strings(logOnlyUsers)
	for _, p := range logOnlyUsers {
		i, ok := order[p]
		if !ok || i > early {
			continue
		}
		if _, allowed := logOnlyInitBeforeEarlyslog[p]; !allowed {
			t.Errorf("%s depends only on log (transitively) and initialises before earlyslog; "+
				"review its source for init-time logging, then add it to logOnlyInitBeforeEarlyslog", p)
		}
	}
	// Keep the allowlist honest: each entry must still be a transitive
	// log-only dependent AND still init before earlyslog.
	for p := range logOnlyInitBeforeEarlyslog {
		if !containsStr(logOnlyUsers, p) {
			t.Errorf("allowlist entry %s is no longer a transitive dependent of log (or now reaches log/slog); remove it", p)
		}
		if i, ok := order[p]; !ok || i > early {
			t.Errorf("allowlist entry %s no longer initialises before earlyslog in the inittrace; remove it", p)
		}
	}
}
