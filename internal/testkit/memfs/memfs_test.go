package memfs_test

import (
	"bytes"
	"errors"
	"sync"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// memfs is the in-memory source.ReadFile used by loader tests. Synthetic
// content only.

// (*FS).Read is assignable to source.ReadFile.
func TestReadIsASourceReadFile(t *testing.T) {
	var rf source.ReadFile = memfs.New().Read
	if rf == nil {
		t.Fatal("nil")
	}
}

func TestPutAndRead(t *testing.T) {
	fs := memfs.New()
	fs.Put("/a", []byte("alpha"))
	fs.Put("/b", []byte("beta\n"))
	for path, want := range map[string]string{"/a": "alpha", "/b": "beta\n"} {
		got, err := fs.Read(path, source.MaxSecretFileBytes)
		if err != nil || string(got) != want {
			t.Errorf("Read(%s): err %v, len %d, want len %d", path, err, len(got), len(want))
		}
	}
	fs.Put("/empty", nil)
	if got, err := fs.Read("/empty", 10); err != nil || len(got) != 0 {
		t.Errorf("empty file: err %v len %d", err, len(got))
	}
}

// Assumption (minimal shape): an unknown path behaves like a missing file.
func TestUnknownPathIsUnreadable(t *testing.T) {
	_, err := memfs.New().Read("/missing", source.MaxSecretFileBytes)
	if !errors.Is(err, source.ErrUnreadable) {
		t.Fatalf("got %v, want ErrUnreadable", err)
	}
}

// The reader neither aliases the stored content nor the Put argument.
func TestContentIsCopied(t *testing.T) {
	fs := memfs.New()
	in := []byte("content")
	fs.Put("/f", in)
	in[0] = 'X'
	got, _ := fs.Read("/f", 100)
	if string(got) != "content" {
		t.Fatal("Put aliases its argument")
	}
	got[0] = 'Y'
	again, _ := fs.Read("/f", 100)
	if string(again) != "content" {
		t.Fatal("Read exposes internal storage")
	}
}

func TestFailReturnsTheGivenKind(t *testing.T) {
	fs := memfs.New()
	for _, e := range []error{source.ErrUnreadable, source.ErrNotRegular, source.ErrTooLarge} {
		p := "/" + e.Error()
		fs.Fail(p, e)
		got, err := fs.Read(p, source.MaxSecretFileBytes)
		if !errors.Is(err, e) || got != nil && len(got) != 0 {
			t.Errorf("Fail(%v): got err %v", e, err)
		}
	}
}

// Assumption: the most recent Put or Fail for a path wins.
func TestLatestSetupWins(t *testing.T) {
	fs := memfs.New()
	fs.Put("/f", []byte("x"))
	fs.Fail("/f", source.ErrNotRegular)
	if _, err := fs.Read("/f", 10); !errors.Is(err, source.ErrNotRegular) {
		t.Fatalf("Fail after Put: %v", err)
	}
	fs.Put("/f", []byte("y"))
	if got, err := fs.Read("/f", 10); err != nil || string(got) != "y" {
		t.Fatalf("Put after Fail: err %v", err)
	}
}

// Assumption: like the real reader, content longer than maxBytes is
// ErrTooLarge and exactly maxBytes is accepted, so loader tests can exercise
// the 64 KiB rule against either reader.
func TestMaxBytesHonoured(t *testing.T) {
	fs := memfs.New()
	fs.Put("/f", bytes.Repeat([]byte("a"), 65536))
	if _, err := fs.Read("/f", source.MaxSecretFileBytes); err != nil {
		t.Fatalf("exactly the cap: %v", err)
	}
	fs.Put("/g", bytes.Repeat([]byte("a"), 65537))
	if _, err := fs.Read("/g", source.MaxSecretFileBytes); !errors.Is(err, source.ErrTooLarge) {
		t.Fatalf("over the cap: %v", err)
	}
	if _, err := fs.Read("/f", 10); !errors.Is(err, source.ErrTooLarge) {
		t.Fatalf("small cap: %v", err)
	}
}

// AC-10: every Read increments the per-path counter, whatever the outcome.
func TestCalls(t *testing.T) {
	fs := memfs.New()
	if n := fs.Calls("/a"); n != 0 {
		t.Fatalf("Calls before any read = %d", n)
	}
	fs.Put("/a", []byte("x"))
	fs.Fail("/bad", source.ErrNotRegular)
	fs.Put("/big", bytes.Repeat([]byte("a"), 20))
	_, _ = fs.Read("/a", 10)
	_, _ = fs.Read("/a", 10)
	_, _ = fs.Read("/a", 10)
	_, _ = fs.Read("/bad", 10)
	_, _ = fs.Read("/unknown", 10)
	_, _ = fs.Read("/unknown", 10)
	_, _ = fs.Read("/big", 10)
	want := map[string]int{"/a": 3, "/bad": 1, "/unknown": 2, "/big": 1, "/never": 0}
	for p, n := range want {
		if got := fs.Calls(p); got != n {
			t.Errorf("Calls(%s) = %d, want %d", p, got, n)
		}
	}
	// Put and Fail setup do not count as reads.
	fs.Put("/a", []byte("z"))
	if got := fs.Calls("/a"); got != 3 {
		t.Errorf("Put changed the call counter: %d", got)
	}
}

func TestIndependentInstances(t *testing.T) {
	a, b := memfs.New(), memfs.New()
	a.Put("/f", []byte("x"))
	if _, err := b.Read("/f", 10); !errors.Is(err, source.ErrUnreadable) {
		t.Fatalf("instances share state: %v", err)
	}
	if b.Calls("/f") != 1 || a.Calls("/f") != 0 {
		t.Fatal("instances share counters")
	}
}

// Safe for concurrent use (run with -race).
func TestConcurrentUse(t *testing.T) {
	fs := memfs.New()
	fs.Put("/f", []byte("x"))
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = fs.Read("/f", 10)
			_ = fs.Calls("/f")
		}()
	}
	wg.Wait()
	if got := fs.Calls("/f"); got != n {
		t.Fatalf("Calls = %d, want %d", got, n)
	}
}

// Error text is constant: it never carries the path (SPEC-003 FR-11).
func TestErrorsCarryNoPath(t *testing.T) {
	fs := memfs.New()
	const path = "/MARKERpathcarrier"
	fs.Fail(path, source.ErrNotRegular)
	_, e1 := fs.Read(path, 10)
	_, e2 := fs.Read("/MARKERmissing", 10)
	for _, e := range []error{e1, e2} {
		if e == nil {
			t.Fatal("want errors")
		}
		if s := e.Error(); bytes.Contains([]byte(s), []byte("MARKER")) {
			t.Errorf("error text carries the path")
		}
	}
}
