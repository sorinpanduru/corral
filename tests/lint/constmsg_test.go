package lint_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/testkit/constmsg"
)

// AC-5 (static check), FR-39: every slog call in the repository passes a
// compile-time constant message. Runtime values go in allowed attributes.
// Fixtures under testdata/ are excluded by the go tool's "./..." pattern.
func TestAC5_FR39_SlogMessagesAreConstants(t *testing.T) {
	fs, err := constmsg.Check(repoRoot(t), "./...")
	if err != nil {
		t.Fatalf("constmsg.Check returned error: %v", err)
	}
	if len(fs) != 0 {
		var b strings.Builder
		for _, f := range fs {
			fmt.Fprintf(&b, "%s:%d\n", f.File, f.Line)
		}
		t.Fatalf("slog calls with a non-constant message:\n%s", b.String())
	}
}
