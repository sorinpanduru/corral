package loader_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

// AC-2 (on test settings), FR-4, FR-5, FR-9, FR-10, FR-16, FR-28 skip rules.

type acCase struct {
	name, envName, flagName string
	def                     any
	flagText, envText       string
	flagWant, envWant       any
	badText                 string
}

var acCases = []acCase{
	{"string", "CORRAL_T_NAME", "t.name", "dflt", "fv", "ev", "fv", "ev", "x\x00y"},
	{"int", "CORRAL_T_N", "t.n", int64(5), "7", "9", int64(7), int64(9), "+5"},
	{"duration", "CORRAL_T_DUR", "t.dur", 5 * time.Second, "250ms", "1m", 250 * time.Millisecond, time.Minute, "5"},
	{"float", "CORRAL_T_RATIO", "t.ratio", 1.0, ".5", "0.25", 0.5, 0.25, "NaN"},
	{"enum", "CORRAL_LOG_LEVEL", "log.level", "info", "DEBUG", "warn", "debug", "warn", "trace"},
	{"bool", "CORRAL_TRACING_ENABLED", "tracing.enabled", false, "TRUE", "false", true, false, "yes"},
	{"list", "CORRAL_T_LIST", "t.list", []string{"d1", "d2"}, "a, b", "x,y,z", []string{"a", "b"}, []string{"x", "y", "z"}, "a,,b"},
	{"rule-checked int", "CORRAL_T_PORT", "t.port", int64(8080), "9000", "9100", int64(9000), int64(9100), "0"},
}

func TestAC2_Default(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), nil, nil, nil)
			requireNoErrors(t, res)
			requireValue(t, res, c.flagName, c.def, loader.SourceDefault, false)
		})
	}
}

func TestAC2_FlagAlone(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), []string{"--" + c.flagName + "=" + c.flagText}, nil, nil)
			requireNoErrors(t, res)
			requireValue(t, res, c.flagName, c.flagWant, loader.SourceFlag, false)
		})
	}
}

func TestAC2_EnvAlone(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), nil, []string{c.envName + "=" + c.envText}, nil)
			requireNoErrors(t, res)
			requireValue(t, res, c.flagName, c.envWant, loader.SourceEnv, false)
		})
	}
}

// Both sources with different valid values: the flag wins, and a list is the
// flag's list only (never merged).
func TestAC2_FlagBeatsEnv(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), []string{"-" + c.flagName + "=" + c.flagText}, []string{c.envName + "=" + c.envText}, nil)
			requireNoErrors(t, res)
			requireValue(t, res, c.flagName, c.flagWant, loader.SourceFlag, false)
		})
	}
}

// An invalid environment value is rejected under CORRAL_<NAME> even when a
// valid flag is also given (FR-10); the flag still supplies the value.
func TestAC2_InvalidEnvRejectedEvenWithValidFlag(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			res := load(t, baseReg(t), []string{"--" + c.flagName + "=" + c.flagText}, []string{c.envName + "=" + c.badText + m}, nil)
			requireRecords(t, res, rec{c.envName, loader.ErrInvalidValue})
			requireNoMarker(t, res, m)
			if res.Failed(c.flagName) {
				t.Error("the setting is failed although its winning source (the flag) is valid")
			}
			requireValue(t, res, c.flagName, c.flagWant, loader.SourceFlag, false)
		})
	}
}

// An invalid flag value is rejected under the logical name; the setting is failed.
func TestAC2_InvalidFlagRejected(t *testing.T) {
	for _, c := range acCases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			res := load(t, baseReg(t), []string{"--" + c.flagName + "=" + c.badText + m}, nil, nil)
			requireRecords(t, res, rec{c.flagName, loader.ErrInvalidValue})
			requireNoMarker(t, res, m)
			if !res.Failed(c.flagName) {
				t.Error("setting with an invalid winning flag is not failed")
			}
			requireUnset(t, res, c.flagName)
		})
	}
}

// Invalid env with no flag: failed. Both invalid: two records sorted by
// setting (FR-13 group 2: CORRAL_* sorts before logical names).
func TestAC2_InvalidEnvAloneAndBothInvalid(t *testing.T) {
	res := load(t, baseReg(t), nil, []string{"CORRAL_T_N=x"}, nil)
	requireRecords(t, res, rec{"CORRAL_T_N", loader.ErrInvalidValue})
	if !res.Failed("t.n") {
		t.Error("t.n not failed")
	}
	res = load(t, baseReg(t), []string{"--t.n=y"}, []string{"CORRAL_T_N=x"}, nil)
	requireRecords(t, res, rec{"CORRAL_T_N", loader.ErrInvalidValue}, rec{"t.n", loader.ErrInvalidValue})
	if !res.Failed("t.n") {
		t.Error("t.n not failed although the winning flag is invalid")
	}
}

// FR-6: an empty flag value is validated, not skipped.
func TestFR6_EmptyFlagValueIsValidated(t *testing.T) {
	res := load(t, baseReg(t), []string{"--log.level=", "--t.n="}, nil, nil)
	requireRecords(t, res, rec{"log.level", loader.ErrInvalidValue}, rec{"t.n", loader.ErrInvalidValue})
	// ...and is valid where the type allows it (string with a default).
	res = load(t, baseReg(t), []string{"--t.name="}, nil, nil)
	if len(res.Errors) != 1 {
		t.Fatalf("empty string value gave %d records, want 1 (strings are non-empty unless the rule allows it)", len(res.Errors))
	}
}

// Rules run on parsed values from every source.
func TestFR10_RuleAppliesToEverySource(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.port=70000"}, []string{"CORRAL_T_PORT=0"}, nil)
	requireRecords(t, res, rec{"CORRAL_T_PORT", loader.ErrInvalidValue}, rec{"t.port", loader.ErrInvalidValue})
}

// ---- presence (FR-9) ----

func TestFR9_OptionalUnsetIsDistinctFromEmpty(t *testing.T) {
	res := load(t, baseReg(t), nil, nil, nil)
	requireNoErrors(t, res)
	requireUnset(t, res, "t.opt")
	requireUnset(t, res, "t.x")
	requireUnset(t, res, "t.secret")
	if v := res.Values["t.opt"]; v.Source != loader.SourceUnset {
		t.Errorf("t.opt source = %v, want SourceUnset", v.Source)
	}
	// Supplied with an empty flag value: for a string with no AllowEmpty, an error, not "unset".
	res = load(t, baseReg(t), []string{"--t.x="}, nil, nil)
	requireRecords(t, res, rec{"t.x", loader.ErrInvalidValue})
}

func TestFR9_MissingRequiredNamedByLogicalName(t *testing.T) {
	r := mkRegistry(t, regOpts{req: true})
	res := load(t, r, nil, nil, nil)
	requireRecords(t, res, rec{"t.req", loader.ErrMissingRequired})
	// Supplied by flag, by env, and both.
	for _, c := range []struct {
		args, env []string
		src       loader.Source
	}{
		{[]string{"--t.req=v"}, nil, loader.SourceFlag},
		{nil, []string{"CORRAL_T_REQ=v"}, loader.SourceEnv},
		{[]string{"--t.req=a"}, []string{"CORRAL_T_REQ=b"}, loader.SourceFlag},
	} {
		res := load(t, r, c.args, c.env, nil)
		requireNoErrors(t, res)
		if v := res.Values["t.req"]; !v.Set || v.Source != c.src {
			t.Errorf("t.req: set=%v source=%v, want source %v", v.Set, v.Source, c.src)
		}
	}
	// A blank environment variable does not provide it.
	res = load(t, r, nil, []string{"CORRAL_T_REQ=  "}, nil)
	requireRecords(t, res, rec{"t.req", loader.ErrMissingRequired})
}

// FR-9 / FR-28 skip rules: a required setting with an errored input gets no
// missing-required record.
func TestFR28_NoMissingRequiredWhenAnInputErrored(t *testing.T) {
	r := mkRegistry(t, regOpts{req: true})
	cases := []struct {
		name string
		args []string
		env  []string
		want []rec
	}{
		{"repeated flag", []string{"--t.req=1", "--t.req=2"}, nil, []rec{{"t.req", loader.ErrRepeated}}},
		{"--t.req --t.req=v", []string{"--t.req", "--t.req=v"}, nil, []rec{{"t.req", loader.ErrRepeated}}},
		{"bare flag", []string{"--t.req"}, nil, []rec{{"t.req", loader.ErrMissingValue}}},
		{"space-separated form", []string{"--t.req", "v"}, nil, []rec{{"t.req", loader.ErrMissingValue}, {config.SettingArgument, loader.ErrPositional}}},
		{"invalid env value", nil, []string{"CORRAL_T_REQ=x\x01"}, []rec{{"CORRAL_T_REQ", loader.ErrInvalidValue}}},
		{"invalid flag value", []string{"--t.req=x\x01"}, nil, []rec{{"t.req", loader.ErrInvalidValue}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, r, c.args, c.env, nil)
			requireRecords(t, res, c.want...)
		})
	}
}

// FR-28: the setting is failed whenever its winning source failed, regardless
// of a valid lower source.
func TestADR0012_FailedFollowsTheWinningSource(t *testing.T) {
	// repeated flag + valid env: flag source failed, flag wins -> failed.
	res := load(t, baseReg(t), []string{"--t.n=1", "--t.n=2"}, []string{"CORRAL_T_N=3"}, nil)
	requireRecords(t, res, rec{"t.n", loader.ErrRepeated})
	if !res.Failed("t.n") {
		t.Error("t.n not failed")
	}
	// valid flag + invalid env: not failed.
	res = load(t, baseReg(t), []string{"--t.n=1"}, []string{"CORRAL_T_N=x"}, nil)
	if res.Failed("t.n") {
		t.Error("t.n failed although the flag wins")
	}
	// Unknown names are never failed.
	if res.Failed("no.such") {
		t.Error("an unregistered setting is failed")
	}
}

// ---- control spellings and positionals in Load ----

func TestLoad_ControlSpellingsIgnoredAndGroup1Records(t *testing.T) {
	res := load(t, baseReg(t), []string{"--check-config", "-h", "--version", "--t.n=7"}, nil, nil)
	requireNoErrors(t, res)
	requireValue(t, res, "t.n", int64(7), loader.SourceFlag, false)
	res = load(t, baseReg(t), []string{"--check-config=true", "stray", "--bogus"}, nil, nil)
	requireRecords(t, res,
		rec{config.SettingUnknown, loader.ErrControlFlagValue},
		rec{config.SettingArgument, loader.ErrPositional},
		rec{config.SettingUnknown, loader.ErrUnknownFlag})
}

// FR-25 / AC-15 (loader part): process globals are not read.
func TestLoad_IgnoresProcessEnvironment(t *testing.T) {
	t.Setenv("CORRAL_T_NAME", "from-process")
	t.Setenv("CORRAL_BOGUS", "x")
	res := load(t, baseReg(t), nil, nil, nil)
	requireNoErrors(t, res)
	requireValue(t, res, "t.name", "dflt", loader.SourceDefault, false)
	if len(res.UnknownEnv) != 0 {
		t.Fatalf("UnknownEnv = %v", res.UnknownEnv)
	}
}

// AC-15 unit: identical inputs give identical results, whatever the map order
// or the order of environment entries.
func TestLoad_Deterministic(t *testing.T) {
	args := []string{"--t.n=x", "--bogus", "--t.name=a", "--t.name=b", "pos"}
	env := []string{"CORRAL_LOG_LEVEL=bad", "CORRAL_ZZ=1", "CORRAL_AA=1", "CORRAL_T_DUR=bad", "CORRAL_A-B=1", "CORRAL_T_B=0"}
	first := load(t, baseReg(t), args, env, nil)
	for i := 0; i < 30; i++ {
		again := load(t, baseReg(t), args, env, nil)
		if !reflect.DeepEqual(first, again) {
			t.Fatal("two loads of identical inputs differ")
		}
	}
	// Entry order (no duplicate keys) does not matter.
	rev := make([]string, len(env))
	for i, e := range env {
		rev[len(env)-1-i] = e
	}
	if other := load(t, baseReg(t), args, rev, nil); !reflect.DeepEqual(first, other) {
		t.Fatal("result depends on the order of environment entries")
	}
}
