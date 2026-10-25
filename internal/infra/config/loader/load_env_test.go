package loader_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

// FR-7, FR-8 (environment), AC-4, AC-5 (unit), AC-22 (loader part).

func TestUnknownEnv_AC5List(t *testing.T) {
	m := newMarker(t)
	res := load(t, baseReg(t), nil, []string{
		"CORRAL_SERVICE_HOST=" + m, "CORRAL_ZZ_TYPO=" + m, "CORRAL_=" + m, "corral_lower=" + m,
		"OTEL_TRACES_EXPORTER=" + m, "OTEL_SDK_DISABLED=" + m, "PATH=" + m, "CORRAL_EMPTY=",
	}, nil)
	requireNoErrors(t, res) // unknown variables are not fatal
	want := []string{"CORRAL_", "CORRAL_SERVICE_HOST", "CORRAL_ZZ_TYPO"}
	if !reflect.DeepEqual(res.UnknownEnv, want) {
		t.Fatalf("UnknownEnv = %v, want %v", res.UnknownEnv, want)
	}
	requireNoMarker(t, res, m)
}

func TestUnknownEnv_EmptyAndWhitespaceNotListed(t *testing.T) {
	var env []string
	for i, v := range []string{"", " ", "\t", "\n", "\v", "\f", "\r", "\u0085", "\u00a0", "\u2003", "\u3000", "\u2028", "\u1680", " \t\u00a0 "} {
		env = append(env, "CORRAL_BLANK"+string(rune('A'+i))+"="+v)
	}
	res := load(t, baseReg(t), nil, env, nil)
	if len(res.UnknownEnv) != 0 {
		t.Fatalf("blank unknown variables listed: %v", res.UnknownEnv)
	}
	// U+200B and invalid UTF-8 are not whitespace, so those are set.
	res = load(t, baseReg(t), nil, []string{"CORRAL_ZW=\u200b", "CORRAL_BOM=\ufeff", "CORRAL_BAD=\xff"}, nil)
	if !reflect.DeepEqual(res.UnknownEnv, []string{"CORRAL_BAD", "CORRAL_BOM", "CORRAL_ZW"}) {
		t.Fatalf("UnknownEnv = %v", res.UnknownEnv)
	}
}

func TestUnknownEnv_FirstOccurrenceDecidesSet(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"CORRAL_Z=", "CORRAL_Z=x", "CORRAL_Y=x", "CORRAL_Y="}, nil)
	if !reflect.DeepEqual(res.UnknownEnv, []string{"CORRAL_Y"}) {
		t.Fatalf("UnknownEnv = %v, want [CORRAL_Y]", res.UnknownEnv)
	}
}

// FR-8: bytes outside [A-Za-z0-9_] give the fixed token, once, sorted like any name.
func TestUnknownEnv_InvalidNameToken(t *testing.T) {
	m := newMarker(t)
	const tok = "CORRAL_<invalid-name>"
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"hyphen", []string{"CORRAL_A-B=1"}, []string{tok}},
		{"non-ASCII", []string{"CORRAL_\u00e9=1"}, []string{tok}},
		{"dot", []string{"CORRAL_A.B=1"}, []string{tok}},
		{"space", []string{"CORRAL_A B=1"}, []string{tok}},
		{"invalid UTF-8", []string{"CORRAL_X\xff=1"}, []string{tok}},
		{"control character", []string{"CORRAL_X\x01=1"}, []string{tok}},
		{"several invalid names give one token", []string{"CORRAL_A-B=1", "CORRAL_C.D=1", "CORRAL_\u00e9=1"}, []string{tok}},
		{"token sorts after digits, before uppercase",
			[]string{"CORRAL_A=1", "CORRAL_9=1", "CORRAL_B-C=1"}, []string{"CORRAL_9", tok, "CORRAL_A"}},
		{"underscore sorts after the token", []string{"CORRAL__X=1", "CORRAL_B-C=1", "CORRAL_Z=1"}, []string{tok, "CORRAL_Z", "CORRAL__X"}},
		{"raw name carries a marker", []string{"CORRAL_" + m + "-x=1", "CORRAL_ok=1"}, []string{"CORRAL_ok", tok}},
	}
	// the last case: lowercase 'o' (0x6F) sorts after '<' (0x3C), so the token is first.
	cases[len(cases)-1].want = []string{tok, "CORRAL_ok"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), nil, c.env, nil)
			if !reflect.DeepEqual(res.UnknownEnv, c.want) {
				t.Fatalf("UnknownEnv = %q, want %q", res.UnknownEnv, c.want)
			}
			requireNoMarker(t, res, m)
		})
	}
}

func TestUnknownEnv_SortedAndDeduplicated(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"CORRAL_B=1", "CORRAL_A=1", "CORRAL_B=2", "CORRAL_a=1", "CORRAL_AA=1", "CORRAL_A1=1"}, nil)
	want := []string{"CORRAL_A", "CORRAL_A1", "CORRAL_AA", "CORRAL_B", "CORRAL_a"}
	if !reflect.DeepEqual(res.UnknownEnv, want) {
		t.Fatalf("UnknownEnv = %v, want %v", res.UnknownEnv, want)
	}
}

// Registered names (and a secret's _FILE reference) are never unknown; a
// non-secret's _FILE form is.
func TestUnknownEnv_RegisteredNamesAreKnown(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	fs.Put(ep(m, "s"), []byte("sv"))
	res := load(t, baseReg(t), nil, []string{
		"CORRAL_T_NAME=v", "CORRAL_LOG_LEVEL=debug", "CORRAL_T_SECRET_FILE=" + ep(m, "s"),
		"CORRAL_T_NAME_FILE=" + ep(m, "x"), "CORRAL_LOG_LEVEL_FILE=" + ep(m, "x"), "CORRAL_T_SECRET_FILE_FILE=x",
	}, fs)
	requireNoErrors(t, res)
	want := []string{"CORRAL_LOG_LEVEL_FILE", "CORRAL_T_NAME_FILE", "CORRAL_T_SECRET_FILE_FILE"}
	if !reflect.DeepEqual(res.UnknownEnv, want) {
		t.Fatalf("UnknownEnv = %v, want %v", res.UnknownEnv, want)
	}
	if fs.Calls(ep(m, "x")) != 0 {
		t.Error("a non-secret _FILE variable was read")
	}
}

func TestUnknownEnv_CaseSensitivePrefix(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"Corral_X=1", "corral_t_name=zzz", "CORRAl_X=1", "XCORRAL_X=1"}, nil)
	if len(res.UnknownEnv) != 0 {
		t.Fatalf("UnknownEnv = %v", res.UnknownEnv)
	}
	requireValue(t, res, "t.name", "dflt", loader.SourceDefault, false)
}

// ---- AC-4: empty and whitespace-only values count as unset ----

var blank = []string{"", " ", "  ", "\t", "\n", "\r\n", "\v", "\f", "\u0085", "\u00a0", "\u2003", "\u3000", "\u2028", "\u1680", " \t\u00a0\u3000 "}

func TestAC4_EmptyOrWhitespaceEnvIsUnset(t *testing.T) {
	for _, v := range blank {
		res := load(t, baseReg(t), nil, []string{"CORRAL_LOG_LEVEL=" + v, "CORRAL_T_NAME=" + v, "CORRAL_T_N=" + v, "CORRAL_T_OPT=" + v}, nil)
		requireNoErrors(t, res)
		requireValue(t, res, "log.level", "info", loader.SourceDefault, false)
		requireValue(t, res, "t.name", "dflt", loader.SourceDefault, false)
		requireValue(t, res, "t.n", int64(5), loader.SourceDefault, false)
		requireUnset(t, res, "t.opt")
		if len(res.Supplied) != 0 {
			t.Errorf("blank value of length %d counted as supplied: %v", len(v), res.Supplied)
		}
	}
}

// A blank environment variable does not override a lower source either way:
// with a valid flag the flag wins, and it is not validated.
func TestAC4_BlankEnvDoesNotBlockAFlag(t *testing.T) {
	res := load(t, baseReg(t), []string{"--log.level=debug"}, []string{"CORRAL_LOG_LEVEL=  "}, nil)
	requireNoErrors(t, res)
	requireValue(t, res, "log.level", "debug", loader.SourceFlag, false)
}

func TestAC4_EmptyFileEnvIsUnset(t *testing.T) {
	for _, v := range blank {
		fs := newFS(t, "m")
		res := load(t, baseReg(t), nil, []string{"CORRAL_T_SECRET_FILE=" + v}, fs)
		requireNoErrors(t, res)
		requireUnset(t, res, "t.secret")
		if fs.Calls(v) != 0 {
			t.Errorf("reader called for a blank CORRAL_T_SECRET_FILE (length %d)", len(v))
		}
	}
}

// A blank CORRAL_T_SECRET together with a real file is not a conflict.
func TestAC4_BlankLiteralWithFileIsNotAConflict(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	fs.Put(ep(m, "s"), []byte("value\n"))
	res := load(t, baseReg(t), nil, []string{"CORRAL_T_SECRET=  ", "CORRAL_T_SECRET_FILE=" + ep(m, "s")}, fs)
	requireNoErrors(t, res)
	requireValue(t, res, "t.secret", "value", loader.SourceEnv, true)
}

func TestAC4_DuplicateKeyFirstWins(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"CORRAL_T_NAME=first", "CORRAL_T_NAME=second", "CORRAL_T_N=7", "CORRAL_T_N=x"}, nil)
	requireNoErrors(t, res) // the invalid second occurrence is never seen
	requireValue(t, res, "t.name", "first", loader.SourceEnv, false)
	requireValue(t, res, "t.n", int64(7), loader.SourceEnv, false)
	// First occurrence empty: unset, even though a later one has a value.
	res = load(t, baseReg(t), nil, []string{"CORRAL_T_NAME=", "CORRAL_T_NAME=later"}, nil)
	requireValue(t, res, "t.name", "dflt", loader.SourceDefault, false)
}

// FR-7: values are used as given, no trimming; leading or trailing whitespace
// is validated as is.
func TestFR7_ValuesAreNotTrimmed(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"CORRAL_T_NAME= padded "}, nil)
	requireNoErrors(t, res)
	requireValue(t, res, "t.name", " padded ", loader.SourceEnv, false)
	for _, kv := range [][2]string{{"CORRAL_T_N", " 5"}, {"CORRAL_T_N", "5 "}, {"CORRAL_LOG_LEVEL", " debug"}, {"CORRAL_TRACING_ENABLED", "true\n"}, {"CORRAL_T_DUR", "5s "}} {
		res := load(t, baseReg(t), nil, []string{kv[0] + "=" + kv[1]}, nil)
		requireRecords(t, res, rec{kv[0], loader.ErrInvalidValue})
	}
}

// ---- AC-22 (loader part) ----

func TestAC22_WhitespaceOnlyEnvIsUnset(t *testing.T) {
	for _, v := range []string{"\t", "\v", "\f", "\u0085", "\u00a0", "\u2003", "\u3000"} {
		res := load(t, baseReg(t), nil, []string{"CORRAL_LOG_LEVEL=" + v}, nil)
		requireNoErrors(t, res)
		requireValue(t, res, "log.level", "info", loader.SourceDefault, false)
	}
}

func TestAC22_ZeroWidthSpaceAndInvalidUTF8AreSetAndRejected(t *testing.T) {
	for name, v := range map[string]string{"U+200B": "\u200b", "U+FEFF": "\ufeff", "invalid UTF-8": "\xff", "invalid UTF-8 bytes": "\xff\xfe\xc3", "bare 0x85": "\x85"} {
		t.Run(name, func(t *testing.T) {
			res := load(t, baseReg(t), nil, []string{"CORRAL_LOG_LEVEL=" + v}, nil)
			requireRecords(t, res, rec{"CORRAL_LOG_LEVEL", loader.ErrInvalidValue})
			if !res.Failed("log.level") {
				t.Error("log.level not failed")
			}
		})
	}
}

// ---- FR-4 / FR-20 supplied set ----

func TestSupplied_SourcesAndOrder(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	fs.Put(ep(m, "s"), []byte("sv"))
	res := load(t, baseReg(t), []string{"--t.n=7", "--t.name=a"},
		[]string{"CORRAL_T_NAME=b", "CORRAL_LOG_LEVEL=debug", "CORRAL_T_SECRET_FILE=" + ep(m, "s"), "CORRAL_T_X=  "}, fs)
	requireNoErrors(t, res)
	want := []string{"log.level", "t.n", "t.name", "t.secret"}
	if !reflect.DeepEqual(res.Supplied, want) {
		t.Fatalf("Supplied = %v, want %v", res.Supplied, want)
	}
}

func TestSupplied_NothingSupplied(t *testing.T) {
	res := load(t, baseReg(t), nil, nil, nil)
	requireNoErrors(t, res)
	if len(res.Supplied) != 0 {
		t.Fatalf("Supplied = %v, want none", res.Supplied)
	}
}

// Defaults and unset do not count; a setting given by flag and env counts once.
func TestSupplied_ListsEachSettingOnce(t *testing.T) {
	res := load(t, baseReg(t), []string{"--log.level=warn", "--t.opt=3"}, []string{"CORRAL_LOG_LEVEL=debug", "CORRAL_T_B=20"}, nil)
	requireNoErrors(t, res)
	if !reflect.DeepEqual(res.Supplied, []string{"log.level", "t.b", "t.opt"}) {
		t.Fatalf("Supplied = %v", res.Supplied)
	}
}

// Secrets are listed by name only.
func TestSupplied_SecretByName(t *testing.T) {
	m := newMarker(t)
	res := load(t, baseReg(t), nil, []string{"CORRAL_T_SECRET=" + m}, nil)
	requireNoErrors(t, res)
	if !reflect.DeepEqual(res.Supplied, []string{"t.secret"}) {
		t.Fatalf("Supplied = %v", res.Supplied)
	}
	requireNoMarker(t, res, m)
}

var _ = time.Second
