// Package phpfpm runs PHP-FPM with the MongoDB extension in Docker, driven
// over FastCGI, for the functional tests (ADR-0022). The base image is pinned
// by digest and the extension by exact version. Test infrastructure only.
package phpfpm

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/testkit/fcgi"
)

const (
	// BaseImage is the PHP 8.3 FPM image on Debian bookworm, by digest.
	BaseImage = "docker.io/library/php@sha256:6b4641a875dd3e0f7551b2751e1f5909084856d18166af7fbb522ac230feb702"
	// PeclMongoDBVersion is the exact PECL mongodb release installed.
	PeclMongoDBVersion = "2.5.5"
)

//go:embed Dockerfile
var dockerfile []byte

//go:embed php-fpm.conf
var fpmConf []byte

// Opts configures Start.
type Opts struct {
	Workers    int    // pm.max_children (default 10)
	ScriptsDir string // PHP scripts, copied into the container
}

// Fixture is a running PHP-FPM container.
type Fixture struct {
	tb   testing.TB
	name string
	addr string
}

func docker(args ...string) (string, error) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	return string(out), err
}

// Start builds the image (cached by Docker) and starts the container.
func Start(tb testing.TB, o Opts) *Fixture {
	tb.Helper()
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("CORRALTEST_REQUIRE_DOCKER") == "1" {
			tb.Fatal("phpfpm: Docker daemon not available and CORRALTEST_REQUIRE_DOCKER=1")
		}
		tb.Skip("phpfpm: Docker daemon not available")
	}
	if o.Workers == 0 {
		o.Workers = 10
	}
	dir := tb.TempDir()
	for name, b := range map[string][]byte{"Dockerfile": dockerfile, "php-fpm.conf": fpmConf} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	sum := sha256.Sum256(append(append([]byte(BaseImage+PeclMongoDBVersion), dockerfile...), fpmConf...))
	tag := "corralfx-php:" + hex.EncodeToString(sum[:6])
	if out, err := docker("build", "-q", "-t", tag, "--build-arg", "BASE_IMAGE="+BaseImage,
		"--build-arg", "PECL_MONGODB_VERSION="+PeclMongoDBVersion, dir); err != nil {
		tb.Fatalf("phpfpm: image build failed: %s", tail(out))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	// The name must start with "corralfx-": the mongofx reaper only removes
	// containers with both that prefix and the corralfx=1 label.
	name := fmt.Sprintf("corralfx-php-%s-%d", hex.EncodeToString(sum[:3]), port)
	if out, err := docker("run", "-d", "--name", name, "--network", "host", "--label", "corralfx=1",
		"-e", "FPM_LISTEN="+addr, "-e", "FPM_WORKERS="+strconv.Itoa(o.Workers), tag); err != nil {
		tb.Fatalf("phpfpm: docker run failed: %s", tail(out))
	}
	tb.Cleanup(func() { _, _ = docker("rm", "-f", "-v", name) }) // -v: never leak anonymous volumes
	if o.ScriptsDir != "" {
		if out, err := docker("cp", o.ScriptsDir+"/.", name+":/srv/scripts"); err != nil {
			tb.Fatalf("phpfpm: copying scripts failed: %s", tail(out))
		}
		if out, err := docker("exec", name, "chmod", "-R", "a+rX", "/srv/scripts"); err != nil {
			tb.Fatalf("phpfpm: chmod failed: %s", tail(out))
		}
	}
	f := &Fixture{tb: tb, name: name, addr: addr}
	end := time.Now().Add(60 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(end) {
			logs, _ := docker("logs", name)
			tb.Fatalf("phpfpm: FPM did not start: %s", tail(logs))
		}
		time.Sleep(200 * time.Millisecond)
	}
	return f
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

// Addr returns the FastCGI address.
func (f *Fixture) Addr() string { return f.addr }

// Run executes script (a path relative to ScriptsDir) in a worker and returns
// the CGI stdout, headers included. params are extra FastCGI parameters.
func (f *Fixture) Run(script string, params map[string]string) ([]byte, error) {
	p := map[string]string{
		"REQUEST_METHOD":    "GET",
		"SCRIPT_FILENAME":   "/srv/scripts/" + script,
		"SCRIPT_NAME":       "/" + script,
		"REQUEST_URI":       "/" + script,
		"SERVER_PROTOCOL":   "HTTP/1.1",
		"GATEWAY_INTERFACE": "CGI/1.1",
		"SERVER_NAME":       "localhost",
		"SERVER_PORT":       "80",
	}
	for k, v := range params {
		p[k] = v
	}
	resp, err := fcgi.Do(f.addr, fcgi.Request{Params: p}, 60*time.Second)
	if err != nil {
		return nil, err
	}
	return resp.Stdout, nil
}
