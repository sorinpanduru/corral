package types_test

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// FR-12 `string`: UTF-8 without control characters; empty only if allowed.
func TestString_Accepted(t *testing.T) {
	requireAccepted(t, spec(types.KindString), []accept{
		{"abc", "abc"},
		{"a b", "a b"},
		{" padded ", " padded "}, // no trimming inside the type
		{" ", " "},               // NBSP is not a control character
		{"¡", "¡"},
		{" ", " "},
		{"日本語", "日本語"},
		{"\U0001F600", "\U0001F600"},
		{"-----BEGIN-----", "-----BEGIN-----"},
	})
}

// FR-12: control characters are U+0000-U+001F, U+007F and U+0080-U+009F.
func TestString_RejectsEveryControlChar(t *testing.T) {
	s := spec(types.KindString)
	var cs []rune
	for r := rune(0); r <= 0x1f; r++ {
		cs = append(cs, r)
	}
	cs = append(cs, 0x7f)
	for r := rune(0x80); r <= 0x9f; r++ {
		cs = append(cs, r)
	}
	if len(cs) != 65 {
		t.Fatalf("test bug: %d control characters", len(cs))
	}
	for _, r := range cs {
		for _, in := range []string{string(r), "a" + string(r), string(r) + "a", "a" + string(r) + "b"} {
			_, err := s.Parse(in)
			if !errors.Is(err, types.ErrControlChar) {
				t.Errorf("U+%04X in %d-byte input: got class %s, want ErrControlChar", r, len(in), classOf(err))
			}
		}
	}
}

func TestString_DELAndC1Examples(t *testing.T) {
	requireIs(t, spec(types.KindString), "a\u007fb", types.ErrControlChar)
	requireIs(t, spec(types.KindString), "a\u0085b", types.ErrControlChar)
}

func TestString_InvalidUTF8(t *testing.T) {
	for i, in := range []string{"\xff", "a\xc3", "\xc3\x28", "\xed\xa0\x80", "\xf8\x88\x80\x80\x80", "abc\xe2\x82"} {
		if utf8.ValidString(in) {
			t.Fatalf("test bug: case %d is valid UTF-8", i)
		}
		requireIs(t, spec(types.KindString), in, types.ErrInvalidUTF8)
	}
}

// FR-13: exactly one error, the first failing check. Invalid UTF-8 comes
// before the control-character check.
func TestString_InvalidUTF8BeforeControlChar(t *testing.T) {
	_, err := spec(types.KindString).Parse("\xff\x00")
	if !errors.Is(err, types.ErrInvalidUTF8) || errors.Is(err, types.ErrControlChar) {
		t.Fatalf("class %s, want only ErrInvalidUTF8", classOf(err))
	}
}

func TestString_Empty(t *testing.T) {
	requireIs(t, spec(types.KindString), "", types.ErrEmpty)
	got, err := types.Spec{Kind: types.KindString, AllowEmpty: true}.Parse("")
	if err != nil || got != "" {
		t.Fatalf("AllowEmpty: got %v, %v", got, err)
	}
	// AllowEmpty does not relax the other checks.
	requireIs(t, types.Spec{Kind: types.KindString, AllowEmpty: true}, "\x00", types.ErrControlChar)
}

// FR-12 `text`: only \t, \n and \r among the control characters.
func TestText_Accepted(t *testing.T) {
	pem := "-----BEGIN TEST-----\nQUJD\r\nREVG\tGH\n-----END TEST-----\n"
	requireAccepted(t, spec(types.KindText), []accept{
		{"a\nb", "a\nb"},
		{"a\r\nb", "a\r\nb"},
		{"a\tb", "a\tb"},
		{"\n", "\n"},
		{pem, pem}, // returned verbatim, no newline stripping or trimming
		{"日本語\n", "日本語\n"},
		{" ", " "},
	})
}

func TestText_RejectsOtherControlChars(t *testing.T) {
	s := spec(types.KindText)
	var cs []rune
	for r := rune(0); r <= 0x1f; r++ {
		if r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		cs = append(cs, r)
	}
	cs = append(cs, 0x7f)
	for r := rune(0x80); r <= 0x9f; r++ {
		cs = append(cs, r)
	}
	if len(cs) != 62 {
		t.Fatalf("test bug: %d characters", len(cs))
	}
	for _, r := range cs {
		for _, in := range []string{string(r), "a\n" + string(r) + "b"} {
			_, err := s.Parse(in)
			if !errors.Is(err, types.ErrControlChar) {
				t.Errorf("U+%04X: got class %s, want ErrControlChar", r, classOf(err))
			}
		}
	}
}

func TestText_NULAndC1Examples(t *testing.T) {
	requireIs(t, spec(types.KindText), "a\x00b", types.ErrControlChar)
	requireIs(t, spec(types.KindText), "a\u0085b", types.ErrControlChar)
}

func TestText_InvalidUTF8AndEmpty(t *testing.T) {
	requireIs(t, spec(types.KindText), "a\n\xff", types.ErrInvalidUTF8)
	requireIs(t, spec(types.KindText), "\xff\x00", types.ErrInvalidUTF8)
	requireIs(t, spec(types.KindText), "", types.ErrEmpty)
	got, err := types.Spec{Kind: types.KindText, AllowEmpty: true}.Parse("")
	if err != nil || got != "" {
		t.Fatalf("AllowEmpty: got %v, %v", got, err)
	}
}

// Every type rejects invalid UTF-8 first (FR-12), including types whose
// grammar would also reject the bytes.
func TestEveryTypeRejectsInvalidUTF8First(t *testing.T) {
	for name, s := range allScalarSpecs() {
		for _, in := range []string{"\xff", "1\xff", "true\xc3", "\xc3\x28:80", "json\xff"} {
			_, err := s.Parse(in)
			if !errors.Is(err, types.ErrInvalidUTF8) {
				t.Errorf("%s: input of length %d: class %s, want ErrInvalidUTF8", name, len(in), classOf(err))
			}
		}
	}
}

func TestDynamicTypesOfStringAndText(t *testing.T) {
	for _, k := range []types.Kind{types.KindString, types.KindText} {
		got, err := spec(k).Parse("x")
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got.(string); !ok {
			t.Errorf("kind %v: dynamic type %T, want string", k, got)
		}
	}
}

// FR-13 / FR-11: errors carry no input text, and one input class gives one
// constant text regardless of the input.
func TestErrorsAreConstantAndCarryNoInput(t *testing.T) {
	m1, m2 := newMarker(t), newMarker(t)
	type bad struct {
		name string
		s    types.Spec
		mk   func(m string) string
	}
	cases := []bad{
		{"string-ctrl", spec(types.KindString), func(m string) string { return m + "\x00" }},
		{"string-utf8", spec(types.KindString), func(m string) string { return m + "\xff" }},
		{"text-ctrl", spec(types.KindText), func(m string) string { return m + "\x01" }},
		{"bool", spec(types.KindBool), func(m string) string { return m }},
		{"int", spec(types.KindInt), func(m string) string { return m }},
		{"int-range", spec(types.KindInt), func(m string) string { return "99999999999999999999" + m[:0] }},
		{"float", spec(types.KindFloat), func(m string) string { return m }},
		{"duration", spec(types.KindDuration), func(m string) string { return m }},
		{"enum", types.Spec{Kind: types.KindEnum, EnumValues: []string{"a"}}, func(m string) string { return m }},
		{"hostport", spec(types.KindHostPort), func(m string) string { return m + ":80" + m }},
		{"hostport-host", spec(types.KindHostPort), func(m string) string { return "-" + m + ":80" }},
		{"list", listOf(types.KindInt), func(m string) string { return "1," + m }},
	}
	for _, c := range cases {
		_, e1 := c.s.Parse(c.mk(m1))
		_, e2 := c.s.Parse(c.mk(m2))
		if e1 == nil || e2 == nil {
			t.Errorf("%s: want rejection", c.name)
			continue
		}
		for _, e := range []error{e1, e2} {
			for _, txt := range []string{e.Error(), sprintf("%v", e), sprintf("%+v", e), sprintf("%#v", e), sprintf("%s", e)} {
				if strings.Contains(txt, m1) || strings.Contains(txt, m2) {
					t.Errorf("%s: error rendering carries the marker", c.name)
				}
			}
			if strings.TrimSpace(e.Error()) == "" {
				t.Errorf("%s: empty error text", c.name)
			}
			// Exactly one error, not a joined list (FR-13).
			if _, joined := e.(interface{ Unwrap() []error }); joined {
				t.Errorf("%s: error is a joined list", c.name)
			}
		}
		if e1.Error() != e2.Error() {
			t.Errorf("%s: error text depends on the input", c.name)
		}
		if !matchesAny(e1, allSentinels) {
			t.Errorf("%s: error matches no exported sentinel", c.name)
		}
	}
}
