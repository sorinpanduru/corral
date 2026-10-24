package loader_test

import (
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

// FR-13 (groups 1 to 3), FR-11 (setting naming), FR-28 env rows, and the
// marker scan over everything the loader reports.

// One load that produces records in every group, plus an unknown variable.
func TestFR13_MixedOrderAcrossGroups(t *testing.T) {
	r := mkRegistry(t, regOpts{req: true})
	m := newMarker(t)
	fs := newFS(t, m)
	pa, pb := ep(m, "a"), ep(m, "b")
	fs.Put(pa, []byte("x"))
	fs.Put(pb, []byte("y"))
	args := []string{
		"--t.n=" + m,               // g2 (flag): t.n invalid
		"--bogus=" + m,             // g1 @1
		"--t.name=a", "--t.name=b", // g1 repeat @3
		"stray-" + m,        // g1 @4
		"--t.b=0",           // g3 (b < a)
		"--tracing.enabled", // g1 @6 missing value
	}
	env := []string{
		"CORRAL_LOG_LEVEL=" + m,                              // g2
		"CORRAL_T_DUR=" + m,                                  // g2
		"CORRAL_T_SECRET=" + m, "CORRAL_T_SECRET_FILE=" + pa, // g2 conflict
		"CORRAL_UNKNOWN_" + m + "=1", // unknown env (not an error)
		"CORRAL_A-B=1",
	}
	// t.req is missing: g2 "t.req".
	res := load(t, r, args, env, fs)
	requireRecords(t, res,
		// group 1, in argument order
		rec{config.SettingUnknown, loader.ErrUnknownFlag},
		rec{"t.name", loader.ErrRepeated},
		rec{config.SettingArgument, loader.ErrPositional},
		rec{"tracing.enabled", loader.ErrMissingValue},
		// group 2, byte order of setting
		rec{"CORRAL_LOG_LEVEL", loader.ErrInvalidValue},
		rec{"CORRAL_T_DUR", loader.ErrInvalidValue},
		rec{"CORRAL_T_SECRET", loader.ErrLiteralAndFile},
		rec{"t.n", loader.ErrInvalidValue},
		rec{"t.req", loader.ErrMissingRequired},
		// group 3
		rec{"t.b", loader.ErrCrossRule},
	)
	wantUnknown := []string{"CORRAL_<invalid-name>", "CORRAL_UNKNOWN_" + m}
	if len(res.UnknownEnv) != 2 || res.UnknownEnv[0] != wantUnknown[0] || res.UnknownEnv[1] != wantUnknown[1] {
		t.Errorf("UnknownEnv has %d entries, want the token and the unknown name", len(res.UnknownEnv))
	}
	// The unknown NAME (not value) legitimately appears in UnknownEnv; the
	// error records carry none of the markers.
	res.UnknownEnv = nil
	requireNoMarker(t, res, m)
	if fs.Calls(pa) != 0 {
		t.Error("the conflicting reference was read")
	}
}

func TestFR13_Group1KeepsArgumentOrderEvenWhenSettingsSortDifferently(t *testing.T) {
	res := load(t, baseReg(t), []string{"--t.x", "pos", "--log.level", "--bogus", "--t.n=1", "--t.n=2"}, nil, nil)
	requireRecords(t, res,
		rec{"t.x", loader.ErrMissingValue},
		rec{config.SettingArgument, loader.ErrPositional},
		rec{"log.level", loader.ErrMissingValue},
		rec{config.SettingUnknown, loader.ErrUnknownFlag},
		rec{"t.n", loader.ErrRepeated},
	)
}

func TestFR13_Group2SortedByByteOrder(t *testing.T) {
	r := mkRegistry(t, regOpts{req: true})
	res := load(t, r,
		[]string{"--t.x=\x01", "--log.level=bad", "--t.name=\x01"},
		[]string{"CORRAL_T_RATIO=x", "CORRAL_TRACING_ENABLED=x", "CORRAL_T_A=x"}, nil)
	requireRecords(t, res,
		rec{"CORRAL_TRACING_ENABLED", loader.ErrInvalidValue}, // 'R' (0x52) sorts before '_' (0x5F)
		rec{"CORRAL_T_A", loader.ErrInvalidValue},
		rec{"CORRAL_T_RATIO", loader.ErrInvalidValue},
		rec{"log.level", loader.ErrInvalidValue},
		rec{"t.name", loader.ErrInvalidValue},
		rec{"t.req", loader.ErrMissingRequired},
		rec{"t.x", loader.ErrInvalidValue},
	)
}

// Every error is a *config.Error with a constant text naming the setting.
func TestErrors_AreConstantConfigErrors(t *testing.T) {
	m1, m2 := newMarker(t), newMarker(t)
	mk := func(m string) loader.Result {
		return load(t, mkRegistry(t, regOpts{req: true}),
			[]string{"--t.n=" + m, "--bogus=" + m, "--t.secret=" + m, "stray" + m},
			[]string{"CORRAL_LOG_LEVEL=" + m, "CORRAL_T_SECRET_FILE=/" + m}, nil)
	}
	a, b := mk(m1), mk(m2)
	if len(a.Errors) == 0 || len(a.Errors) != len(b.Errors) {
		t.Fatalf("error counts %d and %d", len(a.Errors), len(b.Errors))
	}
	for i := range a.Errors {
		if a.Errors[i].Error() != b.Errors[i].Error() {
			t.Errorf("record %d: error text depends on the supplied values", i)
		}
	}
	requireNoMarker(t, a, m1)
	requireNoMarker(t, b, m2)
}

// FR-28 environment rows.
func TestFR28_EnvRows(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "p")
	fs.Put(p, []byte("v"))

	// CORRAL_A_B and CORRAL_A_B_FILE both set, with a valid flag input: one
	// record, the flag value is still used.
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p}, []string{envLit + "=" + m, envFile + "=" + p}, fs)
	requireRecords(t, res, rec{envLit, loader.ErrLiteralAndFile})
	requireValue(t, res, "t.secret", "v", loader.SourceFlag, true)

	// CORRAL_T_SECRET_FILE set and overridden by a flag: file read and
	// validated; the flag wins.
	fs2 := newFS(t, m)
	fs2.Put(p, []byte("envfile"))
	other := ep(m, "q")
	fs2.Put(other, []byte("flagfile"))
	res = load(t, baseReg(t), []string{"--t.secret_file=" + other}, []string{envFile + "=" + p}, fs2)
	requireNoErrors(t, res)
	if fs2.Calls(p) != 1 {
		t.Errorf("overridden env file read %d times, want 1", fs2.Calls(p))
	}
	requireValue(t, res, "t.secret", "flagfile", loader.SourceFlag, true)

	// An invalid overridden env file content is reported under the env reference.
	fs2.Put(p, []byte("bad\x00"))
	res = load(t, baseReg(t), []string{"--t.secret_file=" + other}, []string{envFile + "=" + p}, fs2)
	requireRecords(t, res, rec{envFile, loader.ErrInvalidValue})
}

// Everything reported by one load is free of every marker: errors, unknown
// names (values never listed), supplied names.
func TestMarkerScan_WholeResult(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	pf := ep(m, "f")
	fs.Put(pf, []byte("content-"+m+"\x00"))
	res := load(t, mkRegistry(t, regOpts{req: true}),
		[]string{"--t.secret=" + m, "--t.secret_file=" + pf, "--bogus=" + m, "pos-" + m, "--t.n=" + m},
		[]string{"CORRAL_LOG_LEVEL=" + m, "CORRAL_ZZ=" + m, "CORRAL_T_SECRET_FILE=" + ep(m, "gone"), "CORRAL_T_NAME=" + m + "\x00"}, fs)
	if len(res.Errors) == 0 {
		t.Fatal("no errors")
	}
	requireNoMarker(t, res, m)
	for _, e := range res.UnknownEnv {
		if len(e) > 0 && contains(e, m) {
			t.Errorf("unknown-env entry carries a value marker")
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
