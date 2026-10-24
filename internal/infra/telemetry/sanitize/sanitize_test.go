package sanitize_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// expectedSanitised is an independent oracle: replace each invalid byte with
// U+FFFD (tests only use isolated invalid bytes, so per-byte and per-run
// replacement agree), then keep the longest prefix of at most max bytes that
// ends on a rune boundary.
func expectedSanitised(in string, max int) string {
	var b strings.Builder
	for i := 0; i < len(in); {
		r, n := utf8.DecodeRuneInString(in[i:])
		if r == utf8.RuneError && n == 1 {
			b.WriteString("�")
		} else {
			b.WriteString(in[i : i+n])
		}
		i += n
	}
	s := b.String()
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

type sanitiser struct {
	name string
	max  int
	fn   func(string) string
}

func sanitisers() []sanitiser {
	return []sanitiser{
		{"CommandForTelemetry", 64, sanitize.CommandForTelemetry},
		{"AppName", 128, sanitize.AppName},
		{"DriverName", 64, sanitize.DriverName},
		{"DriverVersion", 64, sanitize.DriverVersion},
	}
}

// FR-12, FR-25 (AC-37): shared truncation and replacement behaviour.
func TestSanitisersTruncateAndReplace(t *testing.T) {
	for _, s := range sanitisers() {
		t.Run(s.name, func(t *testing.T) {
			// Multi-byte runes of 2, 3 and 4 bytes, long enough to exceed
			// the limit at every possible alignment.
			unit := []string{"é", "€", "😀", "a"}
			for offset := 0; offset < 5; offset++ {
				for _, u := range unit {
					in := strings.Repeat("x", offset) + strings.Repeat(u, s.max)
					got := s.fn(in)
					if len(got) > s.max {
						t.Fatalf("len=%d exceeds %d for unit %q offset %d", len(got), s.max, u, offset)
					}
					if !utf8.ValidString(got) {
						t.Fatalf("result not valid UTF-8 for unit %q offset %d", u, offset)
					}
					if want := expectedSanitised(in, s.max); got != want {
						t.Fatalf("unit %q offset %d: got %q want %q", u, offset, got, want)
					}
				}
			}
		})
	}
}

func TestSanitisersInvalidUTF8Replaced(t *testing.T) {
	for _, s := range sanitisers() {
		t.Run(s.name, func(t *testing.T) {
			in := "ab\xffcd\xc3(ef"
			got := s.fn(in)
			if !utf8.ValidString(got) {
				t.Fatalf("not valid UTF-8: %q", got)
			}
			if want := expectedSanitised(in, s.max); got != want {
				t.Fatalf("got %q want %q", got, want)
			}
			if !strings.Contains(got, "�") {
				t.Fatalf("expected U+FFFD in %q", got)
			}
		})
	}
}

// Replacement happens before truncation: the 3-byte U+FFFD counts toward the
// limit, and the result is still valid and within bounds.
func TestSanitisersReplaceThenTruncate(t *testing.T) {
	for _, s := range sanitisers() {
		t.Run(s.name, func(t *testing.T) {
			var b strings.Builder
			for i := 0; i < s.max; i++ {
				b.WriteString("\xfe€")
			}
			in := b.String()
			got := s.fn(in)
			if len(got) > s.max || !utf8.ValidString(got) {
				t.Fatalf("len=%d valid=%v", len(got), utf8.ValidString(got))
			}
			if want := expectedSanitised(in, s.max); got != want {
				t.Fatalf("got %q want %q", got, want)
			}
		})
	}
}

func TestSanitisersBoundaryAndEmpty(t *testing.T) {
	for _, s := range sanitisers() {
		t.Run(s.name, func(t *testing.T) {
			exact := strings.Repeat("a", s.max)
			if got := s.fn(exact); got != exact {
				t.Fatalf("value of exactly %d bytes must be unchanged, got len %d", s.max, len(got))
			}
			over := exact + "a"
			if got := s.fn(over); got != exact {
				t.Fatalf("value of %d bytes must truncate to %d, got len %d", s.max+1, s.max, len(got))
			}
			if got := s.fn(""); got != "" {
				t.Fatalf("empty must stay empty, got %q", got)
			}
			if got := s.fn("plain-value_1.2.3"); got != "plain-value_1.2.3" {
				t.Fatalf("short valid value must be unchanged, got %q", got)
			}
			once := s.fn(strings.Repeat("€", s.max))
			if twice := s.fn(once); twice != once {
				t.Fatalf("sanitiser must be idempotent")
			}
		})
	}
}

// FR-25 (AC-37): the three limits differ and apply independently.
func TestLimitsPerField(t *testing.T) {
	in := strings.Repeat("a", 200)
	if got := len(sanitize.AppName(in)); got != 128 {
		t.Fatalf("AppName len=%d want 128", got)
	}
	if got := len(sanitize.DriverName(in)); got != 64 {
		t.Fatalf("DriverName len=%d want 64", got)
	}
	if got := len(sanitize.DriverVersion(in)); got != 64 {
		t.Fatalf("DriverVersion len=%d want 64", got)
	}
	if got := len(sanitize.CommandForTelemetry(in)); got != 64 {
		t.Fatalf("CommandForTelemetry len=%d want 64", got)
	}
}

// FR-12 (AC-37): log/span value keeps the name as received, no alias, no
// allowlist filtering.
func TestCommandForTelemetryKeepsAsReceived(t *testing.T) {
	for _, in := range []string{"isMaster", "ismaster", "hello", "find", "Find", "someUnknownCommand", "FIND"} {
		if got := sanitize.CommandForTelemetry(in); got != in {
			t.Errorf("CommandForTelemetry(%q)=%q, want unchanged", in, got)
		}
	}
	long := strings.Repeat("€", 100) + "tail"
	got := sanitize.CommandForTelemetry(long)
	if want := expectedSanitised(long, 64); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if len(got) != 63 {
		t.Fatalf("21 three-byte runes expected (63 bytes), got %d", len(got))
	}
}

// FR-12 (AC-37): metric label mapping.
func TestMetricLabel(t *testing.T) {
	cases := map[string]string{
		"find":                   "find",
		"hello":                  "hello",
		"isMaster":               "hello",
		"ismaster":               "hello",
		"Find":                   "other", // case-sensitive
		"FIND":                   "other",
		"HELLO":                  "other",
		"IsMaster":               "other", // only the two listed spellings alias
		"ISMASTER":               "other",
		"insert":                 "other", // allowlist is exactly find and hello (D2)
		"":                       "other",
		" find":                  "other",
		"find ":                  "other",
		"find\x00":               "other",
		"fin\xffd":               "other",
		strings.Repeat("a", 500): "other",
	}
	for in, want := range cases {
		if got := sanitize.MetricLabel(in); got != want {
			t.Errorf("MetricLabel(%q)=%q want %q", in, got, want)
		}
	}
}

// A command that only matches after truncation or replacement must not match.
func TestMetricLabelNoMatchAfterTruncation(t *testing.T) {
	in := "find" + strings.Repeat(" ", 100)
	if got := sanitize.MetricLabel(in); got != sanitize.OtherCommand {
		t.Fatalf("got %q want other", got)
	}
}

// FR-12 (D2): the allowlist is exactly find and hello; "other" is the literal.
func TestAllowlistContents(t *testing.T) {
	if sanitize.OtherCommand != "other" {
		t.Fatalf("OtherCommand=%q", sanitize.OtherCommand)
	}
	got := slices.Clone(sanitize.CommandAllowlist())
	slices.Sort(got)
	if want := []string{"find", "hello"}; !slices.Equal(got, want) {
		t.Fatalf("allowlist=%v want %v", got, want)
	}
	for _, name := range sanitize.CommandAllowlist() {
		if sanitize.MetricLabel(name) != name {
			t.Errorf("allowlisted %q must map to itself", name)
		}
	}
}

// Callers must not be able to alter the allowlist through the returned slice.
func TestAllowlistIsCopy(t *testing.T) {
	a := sanitize.CommandAllowlist()
	for i := range a {
		a[i] = "tampered"
	}
	if got := sanitize.MetricLabel("find"); got != "find" {
		t.Fatalf("allowlist mutated via returned slice: MetricLabel(find)=%q", got)
	}
	if slices.Contains(sanitize.CommandAllowlist(), "tampered") {
		t.Fatalf("allowlist mutated via returned slice")
	}
}

// FR-12: the allowlist lives in one file (plan T6).
func TestAllowlistFileExists(t *testing.T) {
	if _, err := os.Stat("commands.go"); err != nil {
		t.Fatalf("central allowlist file commands.go missing: %v", err)
	}
}
