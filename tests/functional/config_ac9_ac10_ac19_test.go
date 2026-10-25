//go:build functional

package functional

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/markerscan"
)

// ---- AC-9: a secret never appears anywhere ----

func TestFnAC9_SecretNeverAppears(t *testing.T) {
	for _, via := range []string{"env literal", "file reference", "flag file reference"} {
		t.Run(via, func(t *testing.T) {
			secret, pathMarker := fmark(t), fmark(t)
			dir := filepath.Join(t.TempDir(), pathMarker)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "secret")
			if err := os.WriteFile(file, []byte(secret+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			addr := freeAddr(t)
			env := []string{"CORRALTEST_REGISTRY=secret"}
			args := []string{"--log.level=debug", "--admin.listen=" + addr}
			switch via {
			case "env literal":
				env = append(env, "CORRAL_T_SECRET="+secret)
			case "file reference":
				env = append(env, "CORRAL_T_SECRET_FILE="+file)
			default:
				args = append(args, "--t.secret_file="+file)
			}
			sc := markerscan.New()
			sc.AddMarker(t, secret)
			sc.AddMarker(t, pathMarker)

			// Full startup and shutdown.
			p := startTest(t, baseEnv(env...), args...)
			p.waitReady(addr)
			if r, err := httpDo("GET", addr, "/metrics"); err == nil {
				sc.Add(markerscan.SourceMetrics, []byte(r.body))
			} else {
				t.Fatalf("GET /metrics: %v", err)
			}
			if r, err := httpDo("GET", addr, "/readyz"); err == nil {
				sc.Add(markerscan.SourceReadyz, []byte(r.body))
			}
			if code := p.sigterm(); code != 0 {
				t.Fatalf("exit %d", code)
			}
			sc.Add(markerscan.SourceStdout, p.stdout.Bytes())
			sc.Add(markerscan.SourceStderr, p.stderr.Bytes())
			// The FR-20 record lists the secret's name.
			found := false
			for _, r := range p.records() {
				if str(r, "msg") == msgLoadedFn {
					found = true
					if !strings.Contains(","+str(r, "setting")+",", ",t.secret,") {
						t.Errorf("FR-20 setting = %q, want it to list t.secret", str(r, "setting"))
					}
				}
			}
			if !found {
				t.Error("no FR-20 record")
			}

			// Help mode and check-config mode with the same inputs.
			h := startTest(t, baseEnv(env...), "--help")
			h.waitExit()
			sc.Add(markerscan.SourceStdout, h.stdout.Bytes())
			sc.Add(markerscan.SourceStderr, h.stderr.Bytes())
			cc := startTest(t, baseEnv(env...), append([]string{"--check-config"}, args...)...)
			if code := cc.waitExit(); code != 0 {
				t.Fatalf("check-config exit %d", code)
			}
			sc.Add(markerscan.SourceStdout, cc.stdout.Bytes())
			sc.Add(markerscan.SourceStderr, cc.stderr.Bytes())
			sc.AssertClean(t)
		})
	}
}

// A secret literal on the command line is rejected and not echoed (FR-14), and
// the value is never shown even with debug logging.
func TestFnAC9_SecretLiteralOnCommandLine(t *testing.T) {
	m := fmark(t)
	p := startTest(t, baseEnv("CORRALTEST_REGISTRY=secret"), "--log.level=debug", "--t.secret="+m)
	if code := p.waitExit(); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if got := settingsOfRecs(p.records()); !reflect.DeepEqual(got, []string{"t.secret"}) {
		t.Fatalf("settings = %v", got)
	}
	requireStderrEmpty(t, p)
	scanClean(t, p, m)
}

// ---- AC-10 (functional) ----

func TestFnAC10_SIGHUPRunningProcess(t *testing.T) {
	addr := freeAddr(t)
	p := startProc(t, baseEnv(), "--admin.listen="+addr)
	p.waitReady(addr)
	p.hup()
	eventuallyFn(t, "the reload record", func() bool { return reloadCount(p.records()) == 1 })
	if !p.alive() {
		t.Fatal("corral exited on SIGHUP")
	}
	if r, err := httpDo("GET", addr, "/healthz"); err != nil || r.status != 200 {
		t.Fatalf("/healthz after SIGHUP: %v", err)
	}
	for _, r := range p.records() {
		if str(r, "msg") == msgReloadFn && (str(r, "level") != "WARN" || str(r, "component") != "config") {
			t.Errorf("reload record level/component = %v/%v", r["level"], r["component"])
		}
	}
	if code := p.sigterm(); code != 0 {
		t.Fatalf("SIGTERM exit %d, want 0", code)
	}
	requireStderrEmpty(t, p)
}

// SIGHUP while startup is held before the configured logger exists: the
// process stays alive, the signal produces no record, and startup continues
// after the release. The signal is sent only after the hold point confirms
// that main is running.
func TestFnAC10_SIGHUPWhileHeldBeforeLogger(t *testing.T) {
	dir := t.TempDir()
	addr := freeAddr(t)
	p := startTest(t, holdEnv(dir, "logger"), "--admin.listen="+addr)
	waitForFile(t, filepath.Join(dir, "held"))
	for i := 0; i < 3; i++ {
		p.hup()
	}
	if !p.alive() {
		t.Fatal("corral exited on SIGHUP while held")
	}
	if n := len(p.stdout.Lines()); n != 0 {
		t.Fatalf("%d records while held before the logger", n)
	}
	release(t, dir)
	p.waitReady(addr)
	if n := reloadCount(p.records()); n != 0 {
		t.Fatalf("%d reload records for SIGHUPs sent before the logger existed", n)
	}
	if code := p.sigterm(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

// ---- AC-19 (functional) ----

func TestFnAC19_CheckConfigHeldSIGHUP(t *testing.T) {
	dir := t.TempDir()
	p := startTest(t, holdEnv(dir, "checkconfig", "CORRAL_ZZ_TYPO=1"), "--check-config", "--admin.listen="+freeAddr(t))
	waitForFile(t, filepath.Join(dir, "held"))
	for i := 0; i < 5; i++ {
		p.hup()
	}
	if !p.alive() {
		t.Fatal("corral exited on SIGHUP in check-config mode")
	}
	release(t, dir)
	if code := p.waitExit(); code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	want := []string{msgUnknownEnvFn, msgLoadedFn}
	if got := msgsOfRecs(p.records()); !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %q, want exactly the FR-23 records %q", got, want)
	}
	requireStderrEmpty(t, p)
}
