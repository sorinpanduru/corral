package types_test

import (
	"math"
	"regexp"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// Fuzz targets run their seed corpus under `make test`. They assert that
// Parse never panics and that accepted inputs satisfy the FR-12 grammar.

var (
	reInt   = regexp.MustCompile(`^-?[0-9]+$`)
	reFloat = regexp.MustCompile(`^-?([0-9]+(\.[0-9]*)?|\.[0-9]+)([eE][+-]?[0-9]+)?$`)
	reDur   = regexp.MustCompile(`^([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|ms|s|m|h)(([0-9]+(\.[0-9]*)?|\.[0-9]+)(ns|us|ms|s|m|h))*$`)
)

func hasControl(s string) bool {
	for _, r := range s {
		if r <= 0x1f || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func FuzzParseInt(f *testing.F) {
	for _, s := range []string{"0", "-5", "+5", "0x10", "1_0", "9223372036854775808", "", "١٢"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		v, err := spec(types.KindInt).Parse(in)
		if err == nil {
			if !reInt.MatchString(in) {
				t.Fatalf("accepted an int outside the FR-12 grammar (length %d)", len(in))
			}
			if _, ok := v.(int64); !ok {
				t.Fatalf("dynamic type %T", v)
			}
		}
	})
}

func FuzzParseFloat(f *testing.F) {
	for _, s := range []string{"0", ".5", "1.", "1e-3", "NaN", "Inf", "+0.5", "0x1p-2", "1_0", "1e999", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		v, err := spec(types.KindFloat).Parse(in)
		if err == nil {
			if !reFloat.MatchString(in) {
				t.Fatalf("accepted a float outside the FR-12 grammar (length %d)", len(in))
			}
			x, ok := v.(float64)
			if !ok || math.IsInf(x, 0) || math.IsNaN(x) {
				t.Fatalf("not a finite float64: %T", v)
			}
		}
	})
}

func FuzzParseDuration(f *testing.F) {
	for _, s := range []string{"250ms", "1m30s", "0s", "5", "-1s", "1µs", "1.5h", "", "9999999999h"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, az := range []bool{false, true} {
			v, err := types.Spec{Kind: types.KindDuration, AllowZero: az}.Parse(in)
			if err == nil {
				if !reDur.MatchString(in) {
					t.Fatalf("accepted a duration outside the FR-12 grammar (length %d)", len(in))
				}
				d, ok := v.(time.Duration)
				if !ok || d < 0 || (d == 0 && !az) {
					t.Fatalf("bad duration value %T", v)
				}
			}
		}
	})
}

func FuzzParseHostPort(f *testing.F) {
	for _, s := range []string{"127.0.0.1:9187", "[::1]:0", "0x0:80", "127.1:80", "a:09", ":80", "h:+1", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		v, err := hp.Parse(in)
		if err == nil {
			if s, ok := v.(string); !ok || s != in || !utf8.ValidString(in) || hasControl(in) {
				t.Fatalf("accepted an invalid hostport (length %d)", len(in))
			}
		}
	})
}

// FuzzParse covers every kind: never panics; accepted string-like results are
// valid UTF-8 without control characters (text may keep \t \n \r).
func FuzzParse(f *testing.F) {
	for _, s := range []string{"", "a", "a,b", "true", "1", "\x00", "\xff", "json", "1s", "[::1]:1", "a\nb"} {
		f.Add(s)
	}
	specs := allScalarSpecs()
	f.Fuzz(func(t *testing.T, in string) {
		for name, s := range specs {
			v, err := s.Parse(in)
			if err != nil {
				continue
			}
			if !utf8.ValidString(in) {
				t.Fatalf("%s accepted invalid UTF-8", name)
			}
			if str, ok := v.(string); ok && name == "string" && hasControl(str) {
				t.Fatalf("string accepted a control character")
			}
		}
	})
}
