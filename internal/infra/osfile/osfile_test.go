package osfile_test

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/osfile"
)

// Compliance (SPEC-003 FR-15, FR-25, SPEC-001 FR-24): files hold synthetic
// text only. Paths carry a random marker; no returned error may carry the path.

// osfile.Read is assignable to source.ReadFile (interface contract).
var _ source.ReadFile = osfile.Read

const secretMax = source.MaxSecretFileBytes

func newMarker(tb testing.TB) string {
	tb.Helper()
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		tb.Fatalf("rand: %v", err)
	}
	return "MARKER" + hex.EncodeToString(b)
}

// markerDir returns a temp directory whose path contains a fresh marker.
func markerDir(t *testing.T) (dir, marker string) {
	t.Helper()
	marker = newMarker(t)
	dir = filepath.Join(t.TempDir(), marker)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir, marker
}

func write(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireErr asserts err wraps want, matches no other sentinel, is not an
// *fs.PathError (which holds the path) and renders no marker.
func requireErr(t *testing.T, err error, want error, marker string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got nil", want)
	}
	for _, s := range []error{source.ErrUnreadable, source.ErrNotRegular, source.ErrTooLarge} {
		if got := errors.Is(err, s); got != (s == want) {
			t.Errorf("errors.Is(err, %v) = %v, want %v (error text %q)", s, got, s == want, err.Error())
		}
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		t.Errorf("error wraps a *fs.PathError, which carries the path")
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		t.Errorf("error wraps a *os.LinkError")
	}
	if marker != "" {
		for _, txt := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
			if strings.Contains(txt, marker) {
				t.Errorf("error rendering carries the path marker")
			}
		}
	}
	// Constant text: identical for the same sentinel.
	if err.Error() != want.Error() {
		// Wrapping with context is allowed only if it keeps the sentinel text.
		if !strings.Contains(err.Error(), want.Error()) {
			t.Errorf("error text %q does not contain the sentinel text %q", err.Error(), want.Error())
		}
	}
}

func TestRead_RegularFile(t *testing.T) {
	dir, _ := markerDir(t)
	p := filepath.Join(dir, "f")
	content := []byte("synthetic-secret\n")
	write(t, p, content)
	got, err := osfile.Read(p, secretMax)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// Raw bytes: trailing newline stripping is the loader's job (FR-15).
	if !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes, want %d exact bytes", len(got), len(content))
	}
}

func TestRead_BinaryContentExact(t *testing.T) {
	dir, _ := markerDir(t)
	p := filepath.Join(dir, "f")
	content := []byte{0, 1, 2, 0xff, 0xfe, '\r', '\n', 0, '\n'}
	write(t, p, content)
	got, err := osfile.Read(p, secretMax)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("binary content not returned verbatim (err %v)", err)
	}
}

func TestRead_EmptyFile(t *testing.T) {
	dir, _ := markerDir(t)
	p := filepath.Join(dir, "empty")
	write(t, p, nil)
	got, err := osfile.Read(p, secretMax)
	if err != nil || len(got) != 0 {
		t.Fatalf("empty file: len %d, err %v", len(got), err)
	}
}

func TestRead_MissingPath(t *testing.T) {
	dir, m := markerDir(t)
	_, err := osfile.Read(filepath.Join(dir, "nope"), secretMax)
	requireErr(t, err, source.ErrUnreadable, m)
}

func TestRead_EmptyAndMalformedPaths(t *testing.T) {
	for i, p := range []string{"", "a\x00b", strings.Repeat("x", 5000), "/" + strings.Repeat("d/", 3000)} {
		_, err := osfile.Read(p, secretMax)
		if err == nil {
			t.Errorf("case %d: accepted", i)
			continue
		}
		requireErr(t, err, source.ErrUnreadable, "")
	}
}

func TestRead_Directory(t *testing.T) {
	dir, m := markerDir(t)
	_, err := osfile.Read(dir, secretMax)
	requireErr(t, err, source.ErrNotRegular, m)
}

func TestRead_SymlinkToRegularFileAccepted(t *testing.T) {
	dir, _ := markerDir(t)
	target := filepath.Join(dir, "real")
	write(t, target, []byte("via-link"))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	got, err := osfile.Read(link, secretMax)
	if err != nil || string(got) != "via-link" {
		t.Fatalf("symlink to regular file: err %v, len %d", err, len(got))
	}
	// Kubernetes Secret mounts chain relative symlinks through a directory.
	if err := os.Mkdir(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "data", "v"), []byte("chained"))
	if err := os.Symlink("data/v", filepath.Join(dir, "l1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("l1", filepath.Join(dir, "l2")); err != nil {
		t.Fatal(err)
	}
	got, err = osfile.Read(filepath.Join(dir, "l2"), secretMax)
	if err != nil || string(got) != "chained" {
		t.Fatalf("symlink chain: err %v, len %d", err, len(got))
	}
}

func TestRead_SymlinkToDirectoryRejected(t *testing.T) {
	dir, m := markerDir(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := osfile.Read(link, secretMax)
	requireErr(t, err, source.ErrNotRegular, m)
}

func TestRead_DanglingSymlink(t *testing.T) {
	dir, m := markerDir(t)
	link := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err := osfile.Read(link, secretMax)
	requireErr(t, err, source.ErrUnreadable, m)
}

func TestRead_SymlinkLoop(t *testing.T) {
	dir, m := markerDir(t)
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	_, err := osfile.Read(a, secretMax)
	requireErr(t, err, source.ErrUnreadable, m)
}

// FR-15: 65,536 bytes (measured raw, before newline removal) is accepted,
// 65,537 is too large.
func TestRead_SizeBoundary(t *testing.T) {
	dir, m := markerDir(t)
	cases := []struct {
		name    string
		content []byte
		ok      bool
	}{
		{"65535", bytes.Repeat([]byte("a"), 65535), true},
		{"65536", bytes.Repeat([]byte("a"), 65536), true},
		{"65537", bytes.Repeat([]byte("a"), 65537), false},
		// The size is the raw size: the trailing newline counts.
		{"65535 plus newline", append(bytes.Repeat([]byte("a"), 65535), '\n'), true},
		{"65536 plus newline", append(bytes.Repeat([]byte("a"), 65536), '\n'), false},
		{"65535 plus CRLF", append(bytes.Repeat([]byte("a"), 65535), '\r', '\n'), false},
	}
	for _, c := range cases {
		p := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_"))
		write(t, p, c.content)
		got, err := osfile.Read(p, secretMax)
		if c.ok {
			if err != nil || !bytes.Equal(got, c.content) {
				t.Errorf("%s: err %v, got %d bytes, want accepted verbatim", c.name, err, len(got))
			}
			continue
		}
		requireErr(t, err, source.ErrTooLarge, m)
		if len(got) > 0 {
			t.Errorf("%s: returned %d bytes alongside the error", c.name, len(got))
		}
	}
}

func TestRead_MaxBytesParameterIsHonoured(t *testing.T) {
	dir, m := markerDir(t)
	p := filepath.Join(dir, "f")
	write(t, p, []byte("12345"))
	if got, err := osfile.Read(p, 5); err != nil || string(got) != "12345" {
		t.Fatalf("maxBytes == size: err %v", err)
	}
	_, err := osfile.Read(p, 4)
	requireErr(t, err, source.ErrTooLarge, m)
	// A zero cap accepts only an empty file.
	_, err = osfile.Read(p, 0)
	requireErr(t, err, source.ErrTooLarge, m)
	e := filepath.Join(dir, "e")
	write(t, e, nil)
	if got, err := osfile.Read(e, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty file with maxBytes 0: err %v", err)
	}
	// The 4 MiB PEM cap (FR-19).
	big := filepath.Join(dir, "pem")
	write(t, big, bytes.Repeat([]byte("p"), int(source.MaxPEMFileBytes)))
	if _, err := osfile.Read(big, source.MaxPEMFileBytes); err != nil {
		t.Fatalf("exactly 4 MiB: %v", err)
	}
	write(t, big, bytes.Repeat([]byte("p"), int(source.MaxPEMFileBytes)+1))
	_, err = osfile.Read(big, source.MaxPEMFileBytes)
	requireErr(t, err, source.ErrTooLarge, m)
}

// Plan: a large sparse file returns ErrTooLarge quickly because at most
// maxBytes+1 bytes are read.
func TestRead_ReadBoundIsMaxBytesPlusOne(t *testing.T) {
	dir, m := markerDir(t)
	p := filepath.Join(dir, "sparse")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 34); err != nil { // 16 GiB, sparse
		f.Close()
		t.Skipf("sparse files unavailable: %v", err)
	}
	f.Close()
	done := make(chan error, 1)
	go func() {
		_, err := osfile.Read(p, secretMax)
		done <- err
	}()
	select {
	case err := <-done:
		requireErr(t, err, source.ErrTooLarge, m)
	case <-time.After(10 * time.Second):
		t.Fatal("Read of a large sparse file did not return promptly: it reads more than maxBytes+1 bytes")
	}
}

func TestRead_UnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	dir, m := markerDir(t)
	p := filepath.Join(dir, "locked")
	write(t, p, []byte("x"))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if f, err := os.Open(p); err == nil {
		f.Close()
		t.Skip("file still readable")
	}
	_, err := osfile.Read(p, secretMax)
	requireErr(t, err, source.ErrUnreadable, m)
}

func TestRead_UnreadableDirectoryComponent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse any directory")
	}
	dir, m := markerDir(t)
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(sub, "f")
	write(t, p, []byte("x"))
	if err := os.Chmod(sub, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })
	_, err := osfile.Read(p, secretMax)
	requireErr(t, err, source.ErrUnreadable, m)
}

// FR-15: permission bits are not checked (Kubernetes mounts are often
// group- or world-readable).
func TestRead_WorldReadableAccepted(t *testing.T) {
	dir, _ := markerDir(t)
	p := filepath.Join(dir, "f")
	write(t, p, []byte("x"))
	if err := os.Chmod(p, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := osfile.Read(p, secretMax); err != nil {
		t.Fatalf("world-readable file rejected: %v", err)
	}
}

// Read does not modify or remove the file.
func TestRead_DoesNotMutate(t *testing.T) {
	dir, _ := markerDir(t)
	p := filepath.Join(dir, "f")
	write(t, p, []byte("keep\n"))
	for i := 0; i < 3; i++ {
		if _, err := osfile.Read(p, secretMax); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "keep\n" {
		t.Fatalf("file changed after Read (err %v)", err)
	}
}
