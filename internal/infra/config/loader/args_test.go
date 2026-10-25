package loader_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

const (
	unknown  = config.SettingUnknown
	argument = config.SettingArgument
)

// ---- valid input ----

// AC-3: --x=v and -x=v are both accepted.
func TestParseArgs_BothDashForms(t *testing.T) {
	in, pr, failed := parse(t, "--t.x=1", "-t.y=2", "--log.level=debug", "-tracing.enabled=true")
	requireProblems(t, pr, nil)
	requireFailed(t, failed)
	requireInputs(t, in, []inp{
		{0, "t.x", false, "1"}, {1, "t.y", false, "2"}, {2, "log.level", false, "debug"}, {3, "tracing.enabled", false, "true"},
	})
}

func TestParseArgs_NoArguments(t *testing.T) {
	for _, args := range [][]string{nil, {}} {
		in, pr, failed := parse(t, args...)
		requireInputs(t, in, nil)
		requireProblems(t, pr, nil)
		requireFailed(t, failed)
	}
}

// FR-6: an empty value is passed through (validation decides).
func TestParseArgs_EmptyValueIsAnInput(t *testing.T) {
	in, pr, _ := parse(t, "--log.level=", "--t.x=")
	requireProblems(t, pr, nil)
	requireInputs(t, in, []inp{{0, "log.level", false, ""}, {1, "t.x", false, ""}})
}

// Only the first "=" splits; the value is kept whole, with any bytes.
func TestParseArgs_ValueKeptVerbatim(t *testing.T) {
	for _, v := range []string{"a=b=c", " padded ", "x\x01y", " ", "日本語", "--t.y=1", "-", "--", "a,b", "\xff"} {
		in, pr, _ := parse(t, "--t.x="+v)
		requireProblems(t, pr, nil)
		if len(in) != 1 || in[0].Value != v {
			t.Errorf("value of length %d not kept verbatim (inputs %d)", len(v), len(in))
		}
	}
}

// FR-27: exact control spellings are skipped by the parser.
func TestParseArgs_ControlSpellingsAreSkipped(t *testing.T) {
	for _, a := range []string{"--help", "-help", "-h", "--version", "-version", "--check-config", "-check-config"} {
		in, pr, failed := parse(t, a, "--t.x=1")
		requireProblems(t, pr, nil)
		requireFailed(t, failed)
		requireInputs(t, in, []inp{{1, "t.x", false, "1"}})
	}
}

// ---- FR-6 / AC-3 single-record cases ----

func TestAC3_SpaceSeparatedForm(t *testing.T) {
	in, pr, failed := parse(t, "--log.level", "debug")
	requireProblems(t, pr, []prob{
		{0, "log.level", loader.ErrMissingValue},
		{1, argument, loader.ErrPositional},
	})
	requireFailed(t, failed, "log.level")
	requireInputs(t, in, nil)
}

func TestAC3_BareFlags(t *testing.T) {
	_, pr, failed := parse(t, "--tracing.enabled")
	requireProblems(t, pr, []prob{{0, "tracing.enabled", loader.ErrMissingValue}})
	requireFailed(t, failed, "tracing.enabled")

	_, pr, failed = parse(t, "-t.x")
	requireProblems(t, pr, []prob{{0, "t.x", loader.ErrMissingValue}})
	requireFailed(t, failed, "t.x")
}

type tc struct {
	name   string
	args   []string
	probs  []prob
	failed []string
	inputs []inp
}

func runCases(t *testing.T, cases []tc) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in, pr, failed := parse(t, c.args...)
			requireProblems(t, pr, c.probs)
			requireFailed(t, failed, c.failed...)
			requireInputs(t, in, c.inputs)
		})
	}
}

// AC-3 and the FR-28 occurrence rule for non-secret setting flags.
func TestAC3_RepeatedSettingFlags(t *testing.T) {
	rep := func(pos int) []prob { return []prob{{pos, "t.x", loader.ErrRepeated}} }
	runCases(t, []tc{
		{"--x=1 --x=2", []string{"--t.x=1", "--t.x=2"}, rep(1), []string{"t.x"}, nil},
		{"three repeats: second position", []string{"--t.x=1", "--t.x=2", "--t.x=3"}, rep(1), []string{"t.x"}, nil},
		{"five repeats", []string{"--t.x=1", "-t.x=2", "--t.x=3", "-t.x=4", "--t.x=5"}, rep(1), []string{"t.x"}, nil},
		{"--x=1 -x=2", []string{"--t.x=1", "-t.x=2"}, rep(1), []string{"t.x"}, nil},
		{"--x=1 --x: no missing-value record", []string{"--t.x=1", "--t.x"}, rep(1), []string{"t.x"}, nil},
		{"--x --x=1: no missing-value record", []string{"--t.x", "--t.x=1"}, rep(1), []string{"t.x"}, nil},
		{"two bare", []string{"--t.x", "-t.x"}, rep(1), []string{"t.x"}, nil},
		{"bare, literal, literal", []string{"--t.x", "--t.x=1", "--t.x=2"}, rep(1), []string{"t.x"}, nil},
		{"repeat among other flags",
			[]string{"--t.y=9", "--t.x=1", "--t.y=8x", "--t.x=2"},
			[]prob{{2, "t.y", loader.ErrRepeated}, {3, "t.x", loader.ErrRepeated}}, []string{"t.x", "t.y"}, nil},
		{"a repeat does not fail other settings",
			[]string{"--t.x=1", "--t.y=ok", "--t.x=2"}, rep(2), []string{"t.x"}, []inp{{1, "t.y", false, "ok"}}},
		{"empty value occurrences still repeat", []string{"--t.x=", "--t.x="}, rep(1), []string{"t.x"}, nil},
		{"repeat of enum flag", []string{"--log.level=debug", "--log.level=info"},
			[]prob{{1, "log.level", loader.ErrRepeated}}, []string{"log.level"}, nil},
	})
}

// AC-3 / FR-28: secret setting flags.
func TestAC3_SecretFlags(t *testing.T) {
	lit := func(pos int) []prob { return []prob{{pos, "t.secret", loader.ErrSecretOnCommandLine}} }
	runCases(t, []tc{
		{"literal", []string{"--t.secret=v"}, lit(0), []string{"t.secret"}, nil},
		{"literal, single dash", []string{"-t.secret=v"}, lit(0), []string{"t.secret"}, nil},
		{"empty literal is still a literal", []string{"--t.secret="}, lit(0), []string{"t.secret"}, nil},
		{"repeated literal: first position", []string{"--t.secret=v1", "--t.secret=v2"}, lit(0), []string{"t.secret"}, nil},
		{"repeated literal, three", []string{"--t.secret=v1", "-t.secret=v2", "--t.secret=v3"}, lit(0), []string{"t.secret"}, nil},
		{"bare secret: missing value, not a literal", []string{"--t.secret"},
			[]prob{{0, "t.secret", loader.ErrMissingValue}}, []string{"t.secret"}, nil},
		{"bare then literal: first occurrence with =", []string{"--t.secret", "--t.secret=v"}, lit(1), []string{"t.secret"}, nil},
		{"literal then bare: no extra record", []string{"--t.secret=v", "--t.secret"}, lit(0), []string{"t.secret"}, nil},
		{"two bare then literal", []string{"--t.secret", "--t.secret", "--t.secret=v"}, lit(2), []string{"t.secret"}, nil},
		{"two bare: repeat at the second", []string{"--t.secret", "-t.secret"},
			[]prob{{1, "t.secret", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"three bare: one repeat at the second", []string{"--t.secret", "--t.secret", "--t.secret"},
			[]prob{{1, "t.secret", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"two secrets, literals", []string{"--t.secret=v", "--a.b=w"},
			[]prob{{0, "t.secret", loader.ErrSecretOnCommandLine}, {1, "a.b", loader.ErrSecretOnCommandLine}},
			[]string{"t.secret", "a.b"}, nil},
	})
}

// FR-15 / FR-28: _file reference flags and their interaction with the setting flag.
func TestFR28_FileReferenceFlags(t *testing.T) {
	runCases(t, []tc{
		{"single reference is an input", []string{"--t.secret_file=/p"}, nil, nil, []inp{{0, "t.secret", true, "/p"}}},
		{"single dash reference", []string{"-t.secret_file=/p"}, nil, nil, []inp{{0, "t.secret", true, "/p"}}},
		{"empty path is an input (T5 reports it)", []string{"--a.b_file="}, nil, nil, []inp{{0, "a.b", true, ""}}},
		{"bare reference: missing value", []string{"--t.secret_file"},
			[]prob{{0, "t.secret_file", loader.ErrMissingValue}}, []string{"t.secret"}, nil},
		{"two references: one record at the second", []string{"--t.secret_file=p1", "--t.secret_file=p2"},
			[]prob{{1, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"mixed dash forms", []string{"--t.secret_file=p1", "-t.secret_file=p2"},
			[]prob{{1, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"three references", []string{"--t.secret_file=p1", "--t.secret_file=p2", "--t.secret_file=p3"},
			[]prob{{1, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"reference with bare repeat", []string{"--t.secret_file=p1", "--t.secret_file"},
			[]prob{{1, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		{"bare then valued reference", []string{"--t.secret_file", "--t.secret_file=p"},
			[]prob{{1, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, nil},
		// The setting flag and its reference are evaluated separately.
		{"literal with a single reference: one record, reference not kept",
			[]string{"--t.secret=v", "--t.secret_file=p"},
			[]prob{{0, "t.secret", loader.ErrSecretOnCommandLine}}, []string{"t.secret"}, nil},
		{"reference before the literal",
			[]string{"--t.secret_file=p", "--t.secret=v"},
			[]prob{{1, "t.secret", loader.ErrSecretOnCommandLine}}, []string{"t.secret"}, nil},
		{"literal with two references: two records in position order",
			[]string{"--t.secret=v", "--t.secret_file=p1", "--t.secret_file=p2"},
			[]prob{{0, "t.secret", loader.ErrSecretOnCommandLine}, {2, "t.secret_file", loader.ErrRepeated}},
			[]string{"t.secret"}, nil},
		{"two references around the literal",
			[]string{"--t.secret_file=p1", "--t.secret=v", "--t.secret_file=p2"},
			[]prob{{1, "t.secret", loader.ErrSecretOnCommandLine}, {2, "t.secret_file", loader.ErrRepeated}},
			[]string{"t.secret"}, nil},
		{"bare secret with a single reference: reference not kept",
			[]string{"--t.secret", "--t.secret_file=p"},
			[]prob{{0, "t.secret", loader.ErrMissingValue}}, []string{"t.secret"}, nil},
		{"repeated reference does not touch other secrets",
			[]string{"--t.secret_file=p1", "--a.b_file=q", "--t.secret_file=p2"},
			[]prob{{2, "t.secret_file", loader.ErrRepeated}}, []string{"t.secret"}, []inp{{1, "a.b", true, "q"}}},
		{"two secrets, both references valid",
			[]string{"--t.secret_file=p", "--a.b_file=q"}, nil, nil,
			[]inp{{0, "t.secret", true, "p"}, {1, "a.b", true, "q"}}},
	})
}

// FR-15: a non-secret has no file reference flag; its _file form is unknown.
func TestFR15_NonSecretFileFlagIsUnknown(t *testing.T) {
	runCases(t, []tc{
		{"non-secret _file", []string{"--t.x_file=p"}, []prob{{0, unknown, loader.ErrUnknownFlag}}, nil, nil},
		{"enum _file", []string{"--log.level_file=p"}, []prob{{0, unknown, loader.ErrUnknownFlag}}, nil, nil},
		{"bare non-secret _file", []string{"-t.x_file"}, []prob{{0, unknown, loader.ErrUnknownFlag}}, nil, nil},
		{"secret double _file", []string{"--t.secret_file_file=p"}, []prob{{0, unknown, loader.ErrUnknownFlag}}, nil, nil},
		{"secret .file", []string{"--t.secret.file=p"}, []prob{{0, unknown, loader.ErrUnknownFlag}}, nil, nil},
	})
}

// ---- malformed and unknown flags, positionals ----

func TestAC3_UnknownMalformedAndPositional(t *testing.T) {
	u := func(pos int) []prob { return []prob{{pos, unknown, loader.ErrUnknownFlag}} }
	runCases(t, []tc{
		{"--", []string{"--"}, u(0), nil, nil},
		{"-", []string{"-"}, u(0), nil, nil},
		{"-=x", []string{"-=x"}, u(0), nil, nil},
		{"--=x", []string{"--=x"}, u(0), nil, nil},
		{"--=", []string{"--="}, u(0), nil, nil},
		{"three dashes", []string{"---t.x=1"}, u(0), nil, nil},
		{"unknown name", []string{"--bogus=1"}, u(0), nil, nil},
		{"unknown bare", []string{"--bogus"}, u(0), nil, nil},
		{"unknown single dash", []string{"-bogus=1"}, u(0), nil, nil},
		{"wrong case", []string{"--T.X=1"}, u(0), nil, nil},
		{"wrong case enum", []string{"--Log.Level=debug"}, u(0), nil, nil},
		{"trailing space in name", []string{"--t.x =1"}, u(0), nil, nil},
		{"leading space in name", []string{"-- t.x=1"}, u(0), nil, nil},
		{"prefix of a setting", []string{"--t=1"}, u(0), nil, nil},
		{"setting with a suffix", []string{"--t.xx=1"}, u(0), nil, nil},
		{"--h is unknown", []string{"--h"}, u(0), nil, nil},
		{"--v is unknown", []string{"--v"}, u(0), nil, nil},
		{"--HELP is unknown", []string{"--HELP"}, u(0), nil, nil},
		{"-help with a trailing space is unknown", []string{"-help "}, u(0), nil, nil},
		{"--check_config is unknown", []string{"--check_config"}, u(0), nil, nil},
		{"--no-help is unknown", []string{"--no-help"}, u(0), nil, nil},
		{"env-style name is not a flag", []string{"--CORRAL_T_X=1"}, u(0), nil, nil},
		{"positional", []string{"debug"}, []prob{{0, argument, loader.ErrPositional}}, nil, nil},
		{"positional that looks like a setting", []string{"t.x=1"}, []prob{{0, argument, loader.ErrPositional}}, nil, nil},
		{"empty argument is a positional", []string{""}, []prob{{0, argument, loader.ErrPositional}}, nil, nil},
		{"space argument is a positional", []string{" "}, []prob{{0, argument, loader.ErrPositional}}, nil, nil},
		{"positional with dash later", []string{"a-b"}, []prob{{0, argument, loader.ErrPositional}}, nil, nil},
		{"-- is not an end-of-flags marker", []string{"--", "--t.x=1"}, u(0), nil, []inp{{1, "t.x", false, "1"}}},
		{"flags after a positional are still parsed", []string{"pos", "--t.x=1"},
			[]prob{{0, argument, loader.ErrPositional}}, nil, []inp{{1, "t.x", false, "1"}}},
	})
}

func TestAC3_InterleavedInArgumentOrder(t *testing.T) {
	runCases(t, []tc{
		{"unknown, positional, unknown", []string{"--bogus=1", "pos", "--other"},
			[]prob{{0, unknown, loader.ErrUnknownFlag}, {1, argument, loader.ErrPositional}, {2, unknown, loader.ErrUnknownFlag}}, nil, nil},
		{"mixed with valid and missing",
			[]string{"pos1", "--t.x=1", "--bogus", "--tracing.enabled", "pos2", "--", "-"},
			[]prob{{0, argument, loader.ErrPositional}, {2, unknown, loader.ErrUnknownFlag}, {3, "tracing.enabled", loader.ErrMissingValue},
				{4, argument, loader.ErrPositional}, {5, unknown, loader.ErrUnknownFlag}, {6, unknown, loader.ErrUnknownFlag}},
			[]string{"tracing.enabled"}, []inp{{1, "t.x", false, "1"}}},
		{"space form interleaved", []string{"--log.level", "debug", "--bogus"},
			[]prob{{0, "log.level", loader.ErrMissingValue}, {1, argument, loader.ErrPositional}, {2, unknown, loader.ErrUnknownFlag}},
			[]string{"log.level"}, nil},
	})
}

// Control characters anywhere in the name part make the flag malformed.
func TestParseArgs_ControlCharactersInNameAreUnknown(t *testing.T) {
	for _, c := range []string{"\x00", "\x01", "\t", "\n", "\r", "\x1f", "\x7f", "\u0085", "\u009f"} {
		for _, a := range []string{"--t.x" + c + "=1", "--" + c + "t.x=1", "--t.x" + c, "-t." + c + "x=1", "--" + c} {
			_, pr, _ := parse(t, a)
			requireProblems(t, pr, []prob{{0, unknown, loader.ErrUnknownFlag}})
		}
	}
	// ...but not in the value part.
	in, pr, _ := parse(t, "--t.x=a\x00b\x7f\u0085")
	requireProblems(t, pr, nil)
	if len(in) != 1 || in[0].Setting != "t.x" {
		t.Fatalf("value with control characters was not kept as an input for validation")
	}
}

func TestParseArgs_InvalidUTF8NameIsUnknown(t *testing.T) {
	_, pr, _ := parse(t, "--t.x\xff=1", "--\xc3(", "-\xff")
	requireProblems(t, pr, []prob{{0, unknown, loader.ErrUnknownFlag}, {1, unknown, loader.ErrUnknownFlag}, {2, unknown, loader.ErrUnknownFlag}})
}

// ---- control flags in the parser (FR-27) ----

func TestParseArgs_ControlSpellingWithValueIsUnknown(t *testing.T) {
	for _, a := range []string{"--help=x", "--help=", "-help=1", "-h=1", "--version=", "--version=1", "-version=", "--check-config=true", "-check-config=", "--check-config=false"} {
		in, pr, _ := parse(t, a)
		requireProblems(t, pr, []prob{{0, unknown, loader.ErrControlFlagValue}})
		requireInputs(t, in, nil)
	}
}

// ---- compliance: nothing supplied is echoed ----

func TestParseArgs_NoSuppliedTextInProblems(t *testing.T) {
	m := newMarker(t)
	args := []string{
		"--" + m + "=1",                  // unknown name
		"--" + m,                         // unknown bare
		"-" + m + "=" + m,                // unknown with a value
		m,                                // positional
		"--t.secret=" + m,                // secret literal
		"--t.x=" + m, "--t.x=" + m + "2", // repeat
		"--t.secret_file=/" + m, "--t.secret_file=/" + m + "2", // repeated reference
		"--log.level", m, // space form
		"--check-config=" + m, // control flag with a value
		"--" + m + "\x00",     // control character in a name
	}
	_, pr, _ := parse(t, args...)
	if len(pr) == 0 {
		t.Fatal("no problems")
	}
	noMarker(t, pr, m)
}

func TestSentinelsAreDistinctConstants(t *testing.T) {
	all := []error{loader.ErrUnknownFlag, loader.ErrPositional, loader.ErrMissingValue, loader.ErrRepeated,
		loader.ErrSecretOnCommandLine, loader.ErrControlFlagValue}
	seen := map[string]bool{}
	for i, e := range all {
		if e == nil || strings.TrimSpace(e.Error()) == "" {
			t.Fatalf("sentinel %d is nil or empty", i)
		}
		if seen[e.Error()] {
			t.Errorf("sentinels share a text")
		}
		seen[e.Error()] = true
		for j, o := range all {
			if i != j && errorsIs(e, o) {
				t.Errorf("sentinels %d and %d are not distinct", i, j)
			}
		}
	}
}

func errorsIs(a, b error) bool { return errors.Is(a, b) }
