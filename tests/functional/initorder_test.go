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

// logOnlyInitBeforeEarlyslog lists non-stdlib packages that import only the
// std `log` package and are known to initialise before earlyslog (they import
// no log/slog, so the runtime may order them ahead of it). Each entry was
// reviewed: it must not log from a package-level initialiser. A new entry
// needs the same review (FR-28, ADR-0008).
var logOnlyInitBeforeEarlyslog = map[string]string{
	// log.Print is called only from functions (ErrDelegator.Handle and
	// friends), never during init.
	"go.opentelemetry.io/otel/internal/errorhandler": "log used only at run time",
}

var initTraceLine = regexp.MustCompile(`^init (\S+) @`)

// T22 init-order guard (FR-24, FR-28, ADR-0008). earlyslog neutralises
// slog.Default() and the log output from its own init, which only protects
// against packages that initialise after it. Using the dependency graph of
// ./cmd/corral and GODEBUG=inittrace=1 of the built binary:
//
//   - every non-stdlib package that imports log/slog and has init work must
//     initialise after earlyslog (strict);
//   - every non-stdlib package that imports only log and initialises before
//     earlyslog must be on the reviewed allowlist above. This is a
//     "does not log at init" review gate, not a strict ordering, because the
//     Go runtime may legitimately order such a package first.
//
// A dependency bump that changes either assumption fails here.
func TestT22_EarlyslogInitOrderGuard(t *testing.T) {
	root, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	ls := exec.Command("go", "list", "-deps", "-json=ImportPath,Standard,Imports", "./cmd/corral")
	ls.Dir = string(trimNL(root))
	out, err := ls.Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}
	type pkg struct {
		ImportPath string
		Standard   bool
		Imports    []string
	}
	var slogUsers, logOnlyUsers []string
	haveEarly := false
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var p pkg
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
		var hasLog, hasSlog bool
		for _, im := range p.Imports {
			hasLog = hasLog || im == "log"
			hasSlog = hasSlog || im == "log/slog"
		}
		switch {
		case hasSlog:
			slogUsers = append(slogUsers, p.ImportPath)
		case hasLog:
			logOnlyUsers = append(logOnlyUsers, p.ImportPath)
		}
	}
	if !haveEarly {
		t.Fatalf("%s is not a dependency of ./cmd/corral", earlyslogPkg)
	}
	if len(slogUsers) == 0 {
		t.Fatal("found no third-party log/slog importers: guard would be vacuous; check go list output")
	}

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
	early, ok := order[earlyslogPkg]
	if !ok {
		t.Fatalf("no inittrace line for earlyslog (got %d init lines)", len(order))
	}

	sort.Strings(slogUsers)
	for _, p := range slogUsers {
		if i, ok := order[p]; ok && i < early {
			t.Errorf("%s imports log/slog and initialises before earlyslog (FR-28): "+
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
			t.Errorf("%s imports only log and initialises before earlyslog; "+
				"review that it does not log at init, then add it to logOnlyInitBeforeEarlyslog", p)
		}
	}
	// Keep the allowlist honest: stale entries are noise.
	for p := range logOnlyInitBeforeEarlyslog {
		found := false
		for _, q := range logOnlyUsers {
			found = found || q == p
		}
		if !found {
			t.Errorf("allowlist entry %s is no longer a log-only importer; remove it", p)
		}
	}
}
