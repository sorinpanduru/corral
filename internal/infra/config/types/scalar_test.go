package types_test

import (
	"math"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// ---- bool ----

func TestBool(t *testing.T) {
	requireAccepted(t, spec(types.KindBool), []accept{
		{"true", true}, {"false", false}, {"TRUE", true}, {"False", false},
		{"tRuE", true}, {"FALSE", false},
	})
	requireRejected(t, spec(types.KindBool), []string{
		"1", "0", "yes", "no", "on", "off", "t", "f", "T", "y", "",
		" true", "true ", "true\n", "truee", "tru", "Kalse", // Kelvin sign must not fold to k/K
		"ｔｒｕｅ", // fullwidth
	})
}

// ---- int ----

func TestInt_Accepted(t *testing.T) {
	requireAccepted(t, spec(types.KindInt), []accept{
		{"0", int64(0)},
		{"-0", int64(0)},
		{"5", int64(5)},
		{"-5", int64(-5)},
		{"007", int64(7)}, // regex -?[0-9]+ allows leading zeros
		{"9223372036854775807", int64(math.MaxInt64)},
		{"-9223372036854775808", int64(math.MinInt64)},
	})
}

func TestInt_Rejected(t *testing.T) {
	requireRejected(t, spec(types.KindInt), []string{
		"+5", "+0", "0x10", "0X10", "1_000", "", "-", "--1", "-+1", "1.5", "1.", ".5",
		" 1", "1 ", "1\n", "1e3", "١٢", "１２", "0b1", "0o7", "NaN", "abc", "5s",
	}, types.ErrSyntax)
}

func TestInt_Overflow(t *testing.T) {
	for _, in := range []string{
		"9223372036854775808", "-9223372036854775809",
		"99999999999999999999999999", "-99999999999999999999999999",
		"0009223372036854775808",
	} {
		requireIs(t, spec(types.KindInt), in, types.ErrRange)
	}
}

// ---- float ----

func TestFloat_Accepted(t *testing.T) {
	requireAccepted(t, spec(types.KindFloat), []accept{
		{"0", 0.0}, {"1", 1.0}, {".5", 0.5}, {"1.", 1.0}, {"1.5", 1.5},
		{"-0.5", -0.5}, {"-.5", -0.5}, {"-1.", -1.0}, {"1e-3", 0.001},
		{"1E3", 1000.0}, {"1.5e+2", 150.0}, {"2e0", 2.0}, {"0.0", 0.0},
		{"007", 7.0}, {"1e308", 1e308}, {"1.7976931348623157e308", math.MaxFloat64},
	})
}

func TestFloat_Rejected(t *testing.T) {
	requireRejected(t, spec(types.KindFloat), []string{
		"NaN", "nan", "Inf", "inf", "-Inf", "+Inf", "Infinity", "+0.5", "+1", "0x1p-2", "0x1.8p1",
		"1_0", "1_0.5", "", ".", "-", "-.", "e5", ".e5", "1e", "1e+", "1e-", "1.5f", " 1", "1 ",
		"1,5", "--1", "1e1.5", "1.5.5", "1e+-3", "１.５", "٣.٥",
	}, types.ErrSyntax)
}

func TestFloat_OverflowToInfIsRejected(t *testing.T) {
	for _, in := range []string{"1e999", "-1e999", "1.8e308", "1e400"} {
		requireIs(t, spec(types.KindFloat), in, types.ErrRange)
	}
}

func TestFloat_AcceptedIsFinite(t *testing.T) {
	got, err := spec(types.KindFloat).Parse("1e308")
	if err != nil {
		t.Fatal(err)
	}
	f, ok := got.(float64)
	if !ok || math.IsInf(f, 0) || math.IsNaN(f) {
		t.Fatalf("got %T %v", got, got)
	}
}

// ---- duration ----

func TestDuration_Accepted(t *testing.T) {
	requireAccepted(t, spec(types.KindDuration), []accept{
		{"250ms", 250 * time.Millisecond},
		{"5s", 5 * time.Second},
		{"1m30s", 90 * time.Second},
		{"1.5h", 90 * time.Minute},
		{"1ns", time.Nanosecond},
		{"1us", time.Microsecond},
		{"2h45m", 2*time.Hour + 45*time.Minute},
		{"1h1m1s1ms1us1ns", time.Hour + time.Minute + time.Second + time.Millisecond + time.Microsecond + time.Nanosecond},
		{"0.5s", 500 * time.Millisecond},
	})
}

func TestDuration_Rejected(t *testing.T) {
	requireRejected(t, spec(types.KindDuration), []string{
		"5", "0", "-1s", "+1s", "-0s", "+0s", "1µs", "1μs", // U+00B5 and U+03BC
		"1S", "1MS", "1H", "1M", "1d", "1w", "1y", "", "s", "ms", "1 s", " 1s", "1s ", "1s\n",
		"1s1", "1.5.5s", "1m 30s", "１s", "1h-1m", "1h+1m", "5sec", "1e3s", "0x10s", "1_0s",
		"NaN", "Inf", "1s,2s", "..s", ".s", "1.s.",
	}, types.ErrSyntax, types.ErrRange)
}

func TestDuration_ZeroOnlyWithAllowZero(t *testing.T) {
	// Without AllowZero, every zero is rejected.
	requireRejected(t, spec(types.KindDuration), []string{"0s", "0ms", "0h", "0.0s", "0m0s", "0ns"},
		types.ErrRange, types.ErrSyntax)

	az := types.Spec{Kind: types.KindDuration, AllowZero: true}
	requireAccepted(t, az, []accept{
		{"0s", time.Duration(0)}, {"0ms", time.Duration(0)}, {"0h", time.Duration(0)},
		{"0ns", time.Duration(0)}, {"5s", 5 * time.Second},
	})
	// A unit is still required, and the sign and unit rules still apply.
	requireRejected(t, az, []string{"0", "-0s", "+0s", "0µs", "0S", ""}, types.ErrSyntax, types.ErrRange)
}

func TestDuration_OverflowRejected(t *testing.T) {
	requireRejected(t, spec(types.KindDuration), []string{"9999999999h", "9223372036854775808ns", "2562048h"},
		types.ErrRange, types.ErrSyntax)
}

// ---- enum ----

func TestEnum_CaseInsensitiveReturnsDeclaredSpelling(t *testing.T) {
	s := types.Spec{Kind: types.KindEnum, EnumValues: []string{"Debug", "info", "WARN"}}
	requireAccepted(t, s, []accept{
		{"Debug", "Debug"}, {"debug", "Debug"}, {"DEBUG", "Debug"},
		{"info", "info"}, {"INFO", "info"}, {"warn", "WARN"}, {"Warn", "WARN"},
	})
	requireRejected(t, s, []string{"", " info", "info ", "infos", "inf", "error", "info\n", "ınfo"}, // dotless i must not fold
		types.ErrNotInSet, types.ErrEmpty, types.ErrSyntax)
	requireIs(t, s, "error", types.ErrNotInSet)
}

func TestEnum_CaseSensitive(t *testing.T) {
	s := types.Spec{Kind: types.KindEnum, EnumValues: []string{"Debug", "info"}, EnumCaseSensitive: true}
	requireAccepted(t, s, []accept{{"Debug", "Debug"}, {"info", "info"}})
	for _, in := range []string{"debug", "DEBUG", "Info", "INFO"} {
		requireIs(t, s, in, types.ErrNotInSet)
	}
}

func TestEnum_JSONText(t *testing.T) {
	s := types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}
	requireAccepted(t, s, []accept{{"JSON", "json"}, {"Text", "text"}})
}

// ---- Describe ----

func TestDescribe(t *testing.T) {
	cases := []struct {
		s    types.Spec
		want string
	}{
		{spec(types.KindString), "string"},
		{spec(types.KindText), "text"},
		{spec(types.KindBool), "bool"},
		{spec(types.KindInt), "int"},
		{spec(types.KindFloat), "float"},
		{spec(types.KindDuration), "duration"},
		{spec(types.KindHostPort), "hostport"},
		{types.Spec{Kind: types.KindEnum, EnumValues: []string{"json", "text"}}, "enum (json, text)"},
		{listOf(types.KindDuration), "list of duration"},
		{listOf(types.KindString), "list of string"},
		{listOf(types.KindInt), "list of int"},
	}
	for _, c := range cases {
		if got := c.s.Describe(); got != c.want {
			t.Errorf("Describe() = %q, want %q", got, c.want)
		}
	}
}
