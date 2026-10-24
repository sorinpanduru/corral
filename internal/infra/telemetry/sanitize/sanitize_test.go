package sanitize_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// expectedSanitised is an independent oracle: replace each invalid byte with
// its own U+FFFD (policy: one U+FFFD per invalid byte), then keep the longest prefix of at most limit bytes that
// ends on a rune boundary.
func expectedSanitised(in string, limit int) string {
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
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

type sanitiser struct {
	name  string
	limit int
	fn    func(string) string
}

// checkInvariants asserts, for one input: valid UTF-8, within the limit,
// equal to the oracle, a prefix of the fully replaced input, maximal
// truncation (len > limit-UTFMax when truncated), and idempotence.
func checkInvariants(t *testing.T, s sanitiser, in string) string {
	t.Helper()
	got := s.fn(in)
	replaced := expectedSanitised(in, 1<<30)
	if len(got) > s.limit {
		t.Fatalf("len=%d exceeds %d", len(got), s.limit)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("result not valid UTF-8: %q", got)
	}
	if want := expectedSanitised(in, s.limit); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !strings.HasPrefix(replaced, got) {
		t.Fatalf("result is not a prefix of the replaced input")
	}
	if len(replaced) > s.limit && len(got) <= s.limit-utf8.UTFMax {
		t.Fatalf("truncated too much: len=%d limit=%d", len(got), s.limit)
	}
	if len(replaced) <= s.limit && got != replaced {
		t.Fatalf("value within limit must only be replaced, not cut")
	}
	if again := s.fn(got); again != got {
		t.Fatalf("not idempotent: %q -> %q", got, again)
	}
	return got
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
					in := strings.Repeat("x", offset) + strings.Repeat(u, s.limit)
					checkInvariants(t, s, in)
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
			if want := expectedSanitised(in, s.limit); got != want {
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
			for i := 0; i < s.limit; i++ {
				b.WriteString("\xfe€")
			}
			in := b.String()
			checkInvariants(t, s, in)
		})
	}
}

func TestSanitisersBoundaryAndEmpty(t *testing.T) {
	for _, s := range sanitisers() {
		t.Run(s.name, func(t *testing.T) {
			exact := strings.Repeat("a", s.limit)
			if got := s.fn(exact); got != exact {
				t.Fatalf("value of exactly %d bytes must be unchanged, got len %d", s.limit, len(got))
			}
			over := exact + "a"
			if got := s.fn(over); got != exact {
				t.Fatalf("value of %d bytes must truncate to %d, got len %d", s.limit+1, s.limit, len(got))
			}
			if got := s.fn(""); got != "" {
				t.Fatalf("empty must stay empty, got %q", got)
			}
			if got := s.fn("plain-value_1.2.3"); got != "plain-value_1.2.3" {
				t.Fatalf("short valid value must be unchanged, got %q", got)
			}
			once := s.fn(strings.Repeat("€", s.limit))
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
		"insert":                 "insert", // Amendment 3 adds 25 names (FR-12)
		"getMore":                "getMore",
		"GetMore":                "other", // case-sensitive
		"getmore":                "other",
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

// FR-12 (Amendment 3, AC-41): the allowlist is exactly the 27 names; "other"
// is the literal.
func TestAllowlistContents(t *testing.T) {
	if sanitize.OtherCommand != "other" {
		t.Fatalf("OtherCommand=%q", sanitize.OtherCommand)
	}
	got := slices.Clone(sanitize.CommandAllowlist())
	slices.Sort(got)
	want := []string{
		"find", "hello", "getMore", "killCursors", "insert", "update", "delete",
		"findAndModify", "aggregate", "count", "distinct", "bulkWrite",
		"createIndexes", "dropIndexes", "listIndexes", "listCollections",
		"listDatabases", "create", "drop", "ping", "buildInfo", "endSessions",
		"commitTransaction", "abortTransaction", "saslStart", "saslContinue",
		"authenticate",
	}
	slices.Sort(want)
	if len(want) != 27 {
		t.Fatalf("test oracle has %d names, want 27", len(want))
	}
	if !slices.Equal(got, want) {
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

// FR-12: the allowlist lives in one file (plan T6): commandAllowlist must be
// declared in commands.go.
func TestAllowlistDeclaredInCommandsGo(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "commands.go", nil, 0)
	if err != nil {
		t.Fatalf("parse commands.go: %v", err)
	}
	found := false
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if n.Name == "commandAllowlist" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("commandAllowlist is not declared in commands.go")
	}
}

// Hand-written literal expectations, independent of the oracle. Policy: one
// U+FFFD per invalid byte.
func TestLiteralExpectations(t *testing.T) {
	if got, want := sanitize.CommandForTelemetry("ab\xffcd\xc3(ef"), "ab\uFFFDcd\uFFFD(ef"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	for _, s := range sanitisers() {
		if got, want := s.fn("\xff\xfe"), "\uFFFD\uFFFD"; got != want {
			t.Errorf("%s: invalid run: got %q want %q", s.name, got, want)
		}
		if got, want := s.fn("\xe2\x82"), "\uFFFD\uFFFD"; got != want {
			t.Errorf("%s: truncated sequence: got %q want %q", s.name, got, want)
		}
		if got, want := s.fn("a\xe2\x82b"), "a\uFFFD\uFFFDb"; got != want {
			t.Errorf("%s: truncated sequence in text: got %q want %q", s.name, got, want)
		}
	}
	// 64-byte cap over "\xfe€" pairs: each pair is 6 bytes after replacement,
	// so 10 pairs = 60 bytes, then U+FFFD = 63 bytes (21 runes); the next
	// 3-byte rune would reach 66.
	got := sanitize.CommandForTelemetry(strings.Repeat("\xfe\u20ac", 50))
	if utf8.RuneCountInString(got) != 21 || len(got) != 63 {
		t.Errorf("pairs: runes=%d bytes=%d, want 21/63", utf8.RuneCountInString(got), len(got))
	}
	if want := strings.Repeat("\uFFFD\u20ac", 10) + "\uFFFD"; got != want {
		t.Errorf("pairs: got %q want %q", got, want)
	}
	// "x" + 4-byte runes at 64: 1 + 15*4 = 61 bytes.
	got = sanitize.CommandForTelemetry("x" + strings.Repeat("\U0001F600", 40))
	if len(got) != 61 || utf8.RuneCountInString(got) != 16 {
		t.Errorf("emoji: bytes=%d runes=%d, want 61/16", len(got), utf8.RuneCountInString(got))
	}
	// AppName at 128: 42 three-byte runes = 126 bytes.
	if got := sanitize.AppName(strings.Repeat("\u20ac", 100)); len(got) != 126 {
		t.Errorf("AppName euro: bytes=%d want 126", len(got))
	}
}

// Work and allocation must be proportional to the limit, not the input size
// (DoS guard). Uses a minimum over several runs of the TotalAlloc delta so a
// stray background allocation cannot make it flaky; not run in parallel.
func TestBoundedAllocation(t *testing.T) {
	const size = 16 << 20
	inputs := map[string]string{
		"invalid": strings.Repeat("\xff", size),
		"euro":    strings.Repeat("\u20ac", size/3),
		"emoji":   strings.Repeat("\U0001F600", size/4),
		"mixed":   strings.Repeat("\xfe\u20ac", size/4),
	}
	const bytesBound = 64 << 10 // 64 KiB, vs 16 MiB input
	for _, s := range sanitisers() {
		for name, in := range inputs {
			t.Run(s.name+"/"+name, func(t *testing.T) {
				var sink string
				if allocs := testing.AllocsPerRun(3, func() { sink = s.fn(in) }); allocs > 8 {
					t.Errorf("allocs per run = %v, want <= 8", allocs)
				}
				minDelta := ^uint64(0)
				for i := 0; i < 3; i++ {
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					sink = s.fn(in)
					runtime.ReadMemStats(&after)
					if d := after.TotalAlloc - before.TotalAlloc; d < minDelta {
						minDelta = d
					}
				}
				if minDelta > bytesBound {
					t.Errorf("allocated %d bytes for a %d byte input, want <= %d", minDelta, len(in), bytesBound)
				}
				if len(sink) > s.limit || !utf8.ValidString(sink) {
					t.Errorf("bad result: len=%d valid=%v", len(sink), utf8.ValidString(sink))
				}
			})
		}
	}
}
