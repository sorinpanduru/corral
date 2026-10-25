package loader_test

import (
	"errors"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
)

// AC-18 (FR-30, FR-28): cross-setting rules over t.a and t.b with primary
// t.b; the other registry rules cover optional, secret and env-file spelling.
// Rule failures never carry values or the rule's own error text.

func TestAC18_ViolationNamesThePrimaryAsSpelledForItsSource(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	long := ep(m, "long")
	fs.Put(long, []byte("toolong-secret\n"))
	cases := []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{"flag", []string{"--t.b=0"}, nil, "t.b"},
		{"flag beats a valid env", []string{"--t.b=0"}, []string{"CORRAL_T_B=50"}, "t.b"},
		{"default", []string{"--t.a=20"}, nil, "t.b"}, // default b=10 < a=20
		{"env literal", nil, []string{"CORRAL_T_B=0"}, "CORRAL_T_B"},
		{"env literal, default a raised by flag", []string{"--t.a=50"}, []string{"CORRAL_T_B=20"}, "CORRAL_T_B"},
		{"secret from env literal", nil, []string{"CORRAL_T_SECRET=toolong-secret"}, "CORRAL_T_SECRET"},
		{"secret from env file reference: logical name", nil, []string{"CORRAL_T_SECRET_FILE=" + long}, "t.secret"},
		{"secret from flag file reference: logical name", []string{"--t.secret_file=" + long}, nil, "t.secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), c.args, c.env, fs)
			requireRecords(t, res, rec{c.want, loader.ErrCrossRule})
			requireNoMarker(t, res, m, "toolong-secret", "below a", "above")
		})
	}
}

func TestAC18_NoViolationNoRecord(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.a=5", "--t.b=5"}, nil, nil)
	requireNoErrors(t, res)
	res = load(t, baseReg(t), nil, nil, nil) // defaults satisfy every rule
	requireNoErrors(t, res)
}

// FR-30: a rule is skipped, with no group 3 record, when a setting it involves failed.
func TestAC18_SkippedWhenAnInvolvedSettingFailed(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  []string
		want []rec
	}{
		{"t.a invalid flag", []string{"--t.a=x", "--t.b=0"}, nil, []rec{{"t.a", loader.ErrInvalidValue}}},
		{"t.a invalid env", []string{"--t.b=0"}, []string{"CORRAL_T_A=x"}, []rec{{"CORRAL_T_A", loader.ErrInvalidValue}}},
		{"t.b repeated", []string{"--t.b=1", "--t.b=2"}, nil, []rec{{"t.b", loader.ErrRepeated}}},
		{"t.b repeated with bare", []string{"--t.b=1", "--t.b"}, nil, []rec{{"t.b", loader.ErrRepeated}}},
		{"t.b missing value", []string{"--t.b"}, nil, []rec{{"t.b", loader.ErrMissingValue}}},
		{"t.b invalid", []string{"--t.b=abc"}, nil, []rec{{"t.b", loader.ErrInvalidValue}}},
		{"t.b env invalid, no flag", nil, []string{"CORRAL_T_B=abc", "CORRAL_T_A=5"}, []rec{{"CORRAL_T_B", loader.ErrInvalidValue}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := load(t, baseReg(t), c.args, c.env, nil)
			requireRecords(t, res, c.want...)
		})
	}
}

// An invalid but overridden lower source does not fail the setting, so its
// rules still run.
func TestAC18_OverriddenInvalidEnvDoesNotSkipRules(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.b=0"}, []string{"CORRAL_T_B=abc"}, nil)
	requireRecords(t, res, rec{"CORRAL_T_B", loader.ErrInvalidValue}, rec{"t.b", loader.ErrCrossRule})
}

// An involved optional setting that is unset skips the rule (unless the rule
// says it applies to unset).
func TestFR30_OptionalUnsetSkipsRule(t *testing.T) {
	// opt-le-b involves the optional t.opt; unset: skipped.
	res := load(t, baseReg(t), []string{"--t.b=1"}, nil, nil)
	requireNoErrors(t, res)
	// Set and violating: one record for the primary t.opt.
	res = load(t, baseReg(t), []string{"--t.b=1", "--t.opt=5"}, nil, nil)
	requireRecords(t, res, rec{"t.opt", loader.ErrCrossRule})
	res = load(t, baseReg(t), []string{"--t.b=10"}, []string{"CORRAL_T_OPT=50"}, nil)
	requireRecords(t, res, rec{"CORRAL_T_OPT", loader.ErrCrossRule})
}

func TestFR30_AppliesToUnset(t *testing.T) {
	var calls int
	var sawUnset bool
	r := mkRegistry(t, regOpts{
		extraDecls: []registry.Decl{d("t.u", types.Spec{Kind: types.KindInt}, registry.PresenceOptional, "")},
		extraRules: []registry.CrossRule{
			rule("u-required", []string{"t.u"}, "t.u", true, func(get func(string) (any, bool)) error {
				calls++
				if _, set := get("t.u"); !set {
					sawUnset = true
					return errors.New("t.u must be given")
				}
				return nil
			}),
		},
	})
	res := load(t, r, nil, nil, nil)
	requireRecords(t, res, rec{"t.u", loader.ErrCrossRule})
	if calls != 1 || !sawUnset {
		t.Fatalf("rule ran %d times, saw unset=%v", calls, sawUnset)
	}
	res = load(t, r, []string{"--t.u=1"}, nil, nil)
	requireNoErrors(t, res)
	// Still skipped when the involved setting failed.
	calls = 0
	res = load(t, r, []string{"--t.u=x"}, nil, nil)
	requireRecords(t, res, rec{"t.u", loader.ErrInvalidValue})
	if calls != 0 {
		t.Fatalf("rule ran %d times for a failed setting", calls)
	}
}

// The rule sees effective values including defaults, typed, and unset flags.
func TestFR30_RuleSeesEffectiveValues(t *testing.T) {
	var gotA, gotB, gotOpt any
	var optSet bool
	r := mkRegistry(t, regOpts{extraRules: []registry.CrossRule{
		rule("probe", []string{"t.a", "t.b", "t.opt"}, "t.b", true, func(get func(string) (any, bool)) error {
			gotA, _ = get("t.a")
			gotB, _ = get("t.b")
			gotOpt, optSet = get("t.opt")
			return nil
		}),
	}})
	res := load(t, r, []string{"--t.b=77"}, nil, nil)
	requireNoErrors(t, res)
	if gotA != int64(1) || gotB != int64(77) || optSet || gotOpt != nil {
		t.Fatalf("rule saw a=%#v b=%#v opt=%#v set=%v", gotA, gotB, gotOpt, optSet)
	}
}

// A skipped rule's check is not called at all.
func TestFR30_SkippedRuleIsNotCalled(t *testing.T) {
	calls := 0
	r := mkRegistry(t, regOpts{extraRules: []registry.CrossRule{
		rule("count", []string{"t.a", "t.b"}, "t.b", false, func(get func(string) (any, bool)) error { calls++; return nil }),
	}})
	load(t, r, []string{"--t.a=x"}, nil, nil)
	if calls != 0 {
		t.Fatalf("check called %d times although t.a failed", calls)
	}
	load(t, r, nil, nil, nil)
	if calls != 1 {
		t.Fatalf("check called %d times, want 1", calls)
	}
}

// FR-13: group 3 follows every group 2 record, whatever the byte order of the
// settings; group 3 is sorted by setting.
func TestFR13_Group3AfterGroup2AndSorted(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.b=0", "--t.n=x"}, nil, nil)
	requireRecords(t, res, rec{"t.n", loader.ErrInvalidValue}, rec{"t.b", loader.ErrCrossRule})

	// Two failing rules with different primaries, spelled differently:
	// CORRAL_T_B (env literal) sorts before t.secret (byte order).
	long := "toolong-secret"
	res = load(t, baseReg(t), nil, []string{"CORRAL_T_B=0", "CORRAL_T_SECRET=" + long}, nil)
	requireRecords(t, res, rec{"CORRAL_T_B", loader.ErrCrossRule}, rec{"CORRAL_T_SECRET", loader.ErrCrossRule})
	res = load(t, baseReg(t), []string{"--t.b=0"}, []string{"CORRAL_T_SECRET=" + long}, nil)
	requireRecords(t, res, rec{"CORRAL_T_SECRET", loader.ErrCrossRule}, rec{"t.b", loader.ErrCrossRule})
	res = load(t, baseReg(t), []string{"--t.b=0", "--t.opt=99"}, nil, nil)
	requireRecords(t, res, rec{"t.b", loader.ErrCrossRule}, rec{"t.opt", loader.ErrCrossRule})
}

// Two rules failing for the same primary give two records (the same setting).
func TestFR13_TwoRulesSamePrimary(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.a=1000", "--t.b=500"}, nil, nil) // b<a and b>100
	requireRecords(t, res, rec{"t.b", loader.ErrCrossRule}, rec{"t.b", loader.ErrCrossRule})
}

// The record never carries values or the check's own error text.
func TestFR30_NoValuesInRecords(t *testing.T) {
	m := newMarker(t)
	r := mkRegistry(t, regOpts{
		extraDecls: []registry.Decl{d("t.s2", types.Spec{Kind: types.KindString}, registry.PresenceOptional, "")},
		extraRules: []registry.CrossRule{
			rule("echo", []string{"t.s2"}, "t.s2", false, func(get func(string) (any, bool)) error {
				v, _ := get("t.s2")
				return errors.New("bad value " + v.(string))
			}),
		},
	})
	res := load(t, r, []string{"--t.s2=" + m}, nil, nil)
	requireRecords(t, res, rec{"t.s2", loader.ErrCrossRule})
	requireNoMarker(t, res, m, "bad value")
	if ce := res.Errors[0].(*config.Error); ce.Setting != "t.s2" {
		t.Fatal("setting")
	}
}
