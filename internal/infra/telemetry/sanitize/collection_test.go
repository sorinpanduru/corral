package sanitize_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// SPEC-001 FR-40 (Amendment 3): collection names for logs and spans have
// invalid UTF-8 replaced with U+FFFD and are not truncated.
func TestCollectionForTelemetry_FR40(t *testing.T) {
	cases := map[string]string{
		"":                         "",
		"users":                    "users",
		"with.dots_and-$":          "with.dots_and-$",
		"café":                     "café",
		"\U0001F600":               "\U0001F600",
		"a\xffb":                   "a�b",
		"\xff\xfe":                 "��",
		"\xe2\x82":                 "��",
		"ab\xc3(cd":                "ab�(cd",
		"trailing\xe2\x82\xac\xe2": "trailing€�",
	}
	for in, want := range cases {
		if got := sanitize.CollectionForTelemetry(in); got != want {
			t.Errorf("CollectionForTelemetry(%q) = %q, want %q", in, got, want)
		}
	}
}

// No truncation: 1 KiB and larger names come back whole, including a name that
// would be cut by the 64-byte command limit.
func TestCollectionForTelemetryNoTruncation_FR40(t *testing.T) {
	for _, n := range []int{63, 64, 65, 128, 1023, 1024, 1025, 4096, 120 * 1024} {
		in := strings.Repeat("c", n)
		if got := sanitize.CollectionForTelemetry(in); got != in {
			t.Errorf("length %d: got %d bytes", n, len(got))
		}
	}
	long := strings.Repeat("€", 2000)
	if got := sanitize.CollectionForTelemetry(long); got != long {
		t.Error("multi-byte name was truncated")
	}
	// Invalid bytes inside a long name are replaced and the length is kept.
	in := strings.Repeat("x", 2000) + "\xff" + strings.Repeat("y", 2000)
	got := sanitize.CollectionForTelemetry(in)
	if !utf8.ValidString(got) || len(got) != 4000+3 {
		t.Errorf("long name with an invalid byte: valid=%v len=%d", utf8.ValidString(got), len(got))
	}
}

// The result is always valid UTF-8.
func TestCollectionForTelemetryAlwaysValid_FR40(t *testing.T) {
	for _, in := range []string{"\x80", "\xc0\xaf", "\xed\xa0\x80", "\xf4\x90\x80\x80", "a\x00b"} {
		if got := sanitize.CollectionForTelemetry(in); !utf8.ValidString(got) {
			t.Errorf("CollectionForTelemetry(%q) is not valid UTF-8", in)
		}
	}
}
