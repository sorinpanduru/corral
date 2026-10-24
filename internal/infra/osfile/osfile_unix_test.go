//go:build unix

package osfile_test

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/osfile"
)

// readBounded runs Read in a goroutine and fails the test when it does not
// return within the bound (a read that blocks on a FIFO or device). On a
// timeout it unblocks the goroutine through unblock before failing.
func readBounded(t *testing.T, path string, unblock func()) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := osfile.Read(path, source.MaxSecretFileBytes)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(3 * time.Second):
		if unblock != nil {
			unblock()
		}
		<-done
		t.Fatal("Read blocked: it must reject non-regular files without blocking (FR-15, FR-25)")
		return nil
	}
}

// openRDWR returns a function that opens the FIFO read-write, which releases
// any goroutine blocked in open(2).
func openRDWR(path string) func() {
	return func() {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err == nil {
			_ = f.Close()
		}
	}
}

func mkfifo(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	return p
}

// AC-8: a FIFO with no writer must not block the loader.
func TestRead_FIFOWithoutWriter(t *testing.T) {
	dir, m := markerDir(t)
	p := mkfifo(t, dir, "fifo")
	err := readBounded(t, p, openRDWR(p))
	requireErr(t, err, source.ErrNotRegular, m)
}

// A FIFO that has a writer holding it open (and writing) is still rejected
// without reading content.
func TestRead_FIFOWithWriter(t *testing.T) {
	dir, m := markerDir(t)
	p := mkfifo(t, dir, "fifo")
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Skipf("cannot open fifo: %v", err)
	}
	defer w.Close()
	_, _ = w.Write([]byte("synthetic"))
	requireErr(t, readBounded(t, p, nil), source.ErrNotRegular, m)
}

func TestRead_SymlinkToFIFO(t *testing.T) {
	dir, m := markerDir(t)
	fifo := mkfifo(t, dir, "fifo")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(fifo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	requireErr(t, readBounded(t, link, openRDWR(fifo)), source.ErrNotRegular, m)
}

func TestRead_DeviceFiles(t *testing.T) {
	for _, dev := range []string{"/dev/null", "/dev/zero", "/dev/urandom"} {
		if _, err := os.Stat(dev); err != nil {
			continue
		}
		// /dev/zero and /dev/urandom would never end if read.
		err := readBounded(t, dev, nil)
		requireErr(t, err, source.ErrNotRegular, "")
	}
}

func TestRead_UnixSocket(t *testing.T) {
	// Socket paths are length-limited; use a short directory.
	dir, err := os.MkdirTemp("", "s")
	if err != nil {
		t.Skipf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p := filepath.Join(dir, "s")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer l.Close()
	requireErr(t, readBounded(t, p, nil), source.ErrNotRegular, "")
}

// ENAMETOOLONG, ELOOP and ENOTDIR map to the unreadable sentinel and never
// leak the path.
func TestRead_NotADirectoryComponent(t *testing.T) {
	dir, m := markerDir(t)
	f := filepath.Join(dir, "file")
	write(t, f, []byte("x"))
	_, err := osfile.Read(filepath.Join(f, "child"), source.MaxSecretFileBytes)
	requireErr(t, err, source.ErrUnreadable, m)
}
