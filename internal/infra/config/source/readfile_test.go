package source_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// FR-15, FR-25: the file-reader contract. Exactly three failure sentinels with
// constant text; the sizes are the ones FR-15 and FR-19 pin.

func TestMaxSizes(t *testing.T) {
	if source.MaxSecretFileBytes != 65536 {
		t.Errorf("MaxSecretFileBytes = %d, want 65536 (FR-15: 64 KiB)", int64(source.MaxSecretFileBytes))
	}
	if source.MaxPEMFileBytes != 4194304 {
		t.Errorf("MaxPEMFileBytes = %d, want 4194304 (FR-19: 4 MiB)", int64(source.MaxPEMFileBytes))
	}
}

func TestSentinels(t *testing.T) {
	all := []error{source.ErrUnreadable, source.ErrNotRegular, source.ErrTooLarge}
	seen := map[string]bool{}
	for i, e := range all {
		if e == nil || e.Error() == "" {
			t.Fatalf("sentinel %d is nil or has empty text", i)
		}
		if seen[e.Error()] {
			t.Errorf("sentinel %d shares its text with another", i)
		}
		seen[e.Error()] = true
		for j, o := range all {
			if i != j && errors.Is(e, o) {
				t.Errorf("sentinels %d and %d are not distinct", i, j)
			}
		}
		// Constant text: no path separator or format verb leaking in.
		for _, bad := range []string{"/", "%", "\\"} {
			if containsStr(e.Error(), bad) {
				t.Errorf("sentinel %d text contains %q", i, bad)
			}
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ReadFile is a function type usable as an injected dependency.
func TestReadFileTypeShape(t *testing.T) {
	var rf source.ReadFile = func(path string, maxBytes int64) ([]byte, error) {
		return nil, fmt.Errorf("%w", source.ErrUnreadable)
	}
	if _, err := rf("x", 1); !errors.Is(err, source.ErrUnreadable) {
		t.Fatal("wrapped sentinel not matched by errors.Is")
	}
}
