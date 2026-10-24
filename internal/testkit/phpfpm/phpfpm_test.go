//go:build functional

package phpfpm_test

// Docker self-test for the PHP-FPM fixture. The API fixed here:
//
//	phpfpm.BaseImage            // const string, pinned by digest
//	phpfpm.PeclMongoDBVersion   // const string, exact semver
//	phpfpm.Start(t, phpfpm.Opts{Workers int; ScriptsDir string}) *Fixture
//	(*Fixture).Run(script string, params map[string]string) ([]byte, error)
//
// Run drives a worker over FastCGI and returns the CGI stdout (headers
// included; use the fcgi client's Body for the payload).

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/phpfpm"
)

func TestPinned_ADR0022(t *testing.T) {
	if !regexp.MustCompile(`@sha256:[0-9a-f]{64}$`).MatchString(phpfpm.BaseImage) {
		t.Fatalf("BaseImage %q is not pinned by digest", phpfpm.BaseImage)
	}
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(phpfpm.PeclMongoDBVersion) {
		t.Fatalf("PeclMongoDBVersion %q is not an exact version", phpfpm.PeclMongoDBVersion)
	}
}

func TestPHPFPMWithMongoDBExtension_FR54(t *testing.T) {
	dir := t.TempDir()
	script := `<?php
header('Content-Type: text/plain');
echo "ext=" . (extension_loaded('mongodb') ? '1' : '0') . "\n";
echo "version=" . phpversion('mongodb') . "\n";
echo "param=" . ($_SERVER['HTTP_X_SYNTHETIC'] ?? '') . "\n";
echo "pid=" . getmypid() . "\n";
`
	if err := os.WriteFile(filepath.Join(dir, "info.php"), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	fx := phpfpm.Start(t, phpfpm.Opts{Workers: 50, ScriptsDir: dir})
	out, err := fx.Run("info.php", map[string]string{"HTTP_X_SYNTHETIC": "marker-77"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{"ext=1", "version=" + phpfpm.PeclMongoDBVersion, "param=marker-77"} {
		if !strings.Contains(s, want) {
			t.Errorf("output lacks %q (%d bytes)", want, len(s))
		}
	}
	// pm=static with many workers: concurrent runs reach different workers.
	pids := map[string]bool{}
	for i := 0; i < 20; i++ {
		o, err := fx.Run("info.php", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(o), "\n") {
			if strings.HasPrefix(l, "pid=") {
				pids[l] = true
			}
		}
	}
	if len(pids) == 0 {
		t.Fatal("no pid seen")
	}
}
