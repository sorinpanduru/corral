package loader_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config"
	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/registry"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
	"github.com/sorinpanduru/corral/internal/infra/config/types"
	"github.com/sorinpanduru/corral/internal/testkit/memfs"
)

// ADR-0020 / SPEC-002 T13: file-content settings. t.pem is an optional string
// with a File rule whose Check wants the synthetic header "-----BEGIN
// SYNTHETIC-----". Paths and contents carry random markers. The sentinel for
// a failed Check is assumed to be loader.ErrInvalidValue.

const goodHead = "-----BEGIN SYNTHETIC-----\n"

var errBadContent = errors.New("synthetic: not a synthetic PEM")

func pemCheck(content []byte) error {
	if !bytes.HasPrefix(content, []byte(goodHead)) {
		return errBadContent
	}
	return nil
}

func pemDecl(name string) registry.Decl {
	d := d(name, types.Spec{Kind: types.KindString}, registry.PresenceOptional, "")
	d.File = &registry.FileRule{MaxBytes: 4 << 20, Check: pemCheck}
	return d
}

func goodPEM(m string) []byte { return []byte(goodHead + "body-" + m + "\n-----END SYNTHETIC-----\n") }

func fileReg(tb testing.TB, extra ...registry.CrossRule) *registry.Registry {
	return mkRegistry(tb, regOpts{extraDecls: []registry.Decl{pemDecl("t.pem"), pemDecl("t.pem2")}, extraRules: extra})
}

func fileValue(t *testing.T, res loader.Result, name string) registry.FileValue {
	t.Helper()
	v, ok := res.Get(name)
	if !ok {
		t.Fatalf("%s has no value", name)
	}
	fv, ok := v.(registry.FileValue)
	if !ok {
		t.Fatalf("%s value is %T, want registry.FileValue", name, v)
	}
	return fv
}

// AC-24 (unit): a flag winner is read exactly once and yields a FileValue.
func TestFileRuleFlagReadOnce_AC24(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	res := load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	requireNoErrors(t, res)
	fv := fileValue(t, res, "t.pem")
	if fv.Path != p || !bytes.Equal(fv.Content, goodPEM(m)) {
		t.Fatalf("FileValue path/content wrong (len %d)", len(fv.Content))
	}
	if n := fs.Calls(p); n != 1 {
		t.Fatalf("reader called %d times, want 1", n)
	}
	if v := res.Values["t.pem"]; v.Source != loader.SourceFlag || v.FileRef {
		t.Fatalf("source %v fileRef %v", v.Source, v.FileRef)
	}
}

func TestFileRuleEnvReadOnce_AC24(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	res := load(t, fileReg(t), nil, []string{"CORRAL_T_PEM=" + p}, fs)
	requireNoErrors(t, res)
	if fv := fileValue(t, res, "t.pem"); fv.Path != p || !bytes.Equal(fv.Content, goodPEM(m)) {
		t.Fatal("FileValue wrong")
	}
	if n := fs.Calls(p); n != 1 {
		t.Fatalf("reader called %d times, want 1", n)
	}
	if res.Values["t.pem"].Source != loader.SourceEnv {
		t.Fatal("source is not env")
	}
}

// Only the winner is read: with a flag and an environment value, the losing
// environment path is validated by type only and never opened.
func TestFileRuleLosingSourceNotRead_AC24(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	win, lose := ep(m, "win"), ep(m, "lose")
	fs.Put(win, goodPEM(m))
	fs.Put(lose, goodPEM(m+"x"))
	res := load(t, fileReg(t), []string{"--t.pem=" + win}, []string{"CORRAL_T_PEM=" + lose}, fs)
	requireNoErrors(t, res)
	if fs.Calls(win) != 1 || fs.Calls(lose) != 0 {
		t.Fatalf("reads: winner %d, loser %d", fs.Calls(win), fs.Calls(lose))
	}
	if fileValue(t, res, "t.pem").Path != win {
		t.Fatal("wrong winner")
	}
	// A loser that does not exist is not an error either.
	fs2 := newFS(t, m)
	fs2.Put(win, goodPEM(m))
	res = load(t, fileReg(t), []string{"--t.pem=" + win}, []string{"CORRAL_T_PEM=" + ep(m, "missing")}, fs2)
	requireNoErrors(t, res)
}

// Unset: nothing is read.
func TestFileRuleUnsetNoRead_AC24(t *testing.T) {
	fs := memfs.New()
	res := load(t, fileReg(t), nil, nil, fs)
	requireNoErrors(t, res)
	requireUnset(t, res, "t.pem")
}

// A reader failure is one record under the input's name, with the sentinel for
// the failure, and nothing from the path or content.
func TestFileRuleReaderFailures_AC31(t *testing.T) {
	cases := []struct {
		name  string
		setup func(fs *memfs.FS, p, m string)
		want  error
	}{
		{"not found", func(fs *memfs.FS, p, m string) {}, loader.ErrFileUnreadable},
		{"unreadable", func(fs *memfs.FS, p, m string) { fs.Fail(p, source.ErrUnreadable) }, loader.ErrFileUnreadable},
		{"not a regular file", func(fs *memfs.FS, p, m string) { fs.Fail(p, source.ErrNotRegular) }, loader.ErrFileNotRegular},
		{"too large", func(fs *memfs.FS, p, m string) {
			fs.Put(p, append([]byte(goodHead), bytes.Repeat([]byte("x"), 4<<20)...))
		}, loader.ErrFileTooLarge},
	}
	for _, tc := range cases {
		for _, via := range []string{"flag", "env"} {
			t.Run(tc.name+"/"+via, func(t *testing.T) {
				m := newMarker(t)
				fs := newFS(t, m)
				p := ep(m, "pem")
				tc.setup(fs, p, m)
				var args, env []string
				name := "t.pem"
				if via == "flag" {
					args = []string{"--t.pem=" + p}
				} else {
					env = []string{"CORRAL_T_PEM=" + p}
					name = "CORRAL_T_PEM"
				}
				res := load(t, fileReg(t), args, env, fs)
				requireRecords(t, res, rec{name, tc.want})
				if !res.Failed("t.pem") {
					t.Fatal("t.pem not marked failed")
				}
				requireUnset(t, res, "t.pem")
				requireNoMarker(t, res, m, p)
				if n := fs.Calls(p); n != 1 {
					t.Fatalf("reader called %d times", n)
				}
			})
		}
	}
}

// The limit is 4 MiB: exactly 4 MiB is read, one byte more is too large.
func TestFileRuleLimit_AC31(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	content := append([]byte(goodHead), bytes.Repeat([]byte("x"), 4<<20-len(goodHead))...)
	if len(content) != 4<<20 {
		t.Fatalf("setup: %d", len(content))
	}
	fs.Put(p, content)
	res := load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	requireNoErrors(t, res)
	if fv := fileValue(t, res, "t.pem"); len(fv.Content) != 4<<20 {
		t.Fatalf("content length %d", len(fv.Content))
	}
	fs.Put(p, append(content, 'x'))
	res = load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	requireRecords(t, res, rec{"t.pem", loader.ErrFileTooLarge})
}

// AC-31: a Check failure is one record under the input's name, with no content
// or path.
func TestFileRuleCheckFailure_AC31(t *testing.T) {
	m := newMarker(t)
	for _, via := range []string{"flag", "env"} {
		t.Run(via, func(t *testing.T) {
			fs := newFS(t, m)
			p := ep(m, "bad")
			fs.Put(p, []byte("not a pem "+m))
			var args, env []string
			name := "t.pem"
			if via == "flag" {
				args = []string{"--t.pem=" + p}
			} else {
				env = []string{"CORRAL_T_PEM=" + p}
				name = "CORRAL_T_PEM"
			}
			res := load(t, fileReg(t), args, env, fs)
			requireRecords(t, res, rec{name, loader.ErrInvalidValue})
			if !res.Failed("t.pem") {
				t.Fatal("not failed")
			}
			requireNoMarker(t, res, m, p)
			if strings.Contains(renderRecords(res), errBadContent.Error()) {
				t.Error("the Check's own error text reached the records")
			}
			if fs.Calls(p) != 1 {
				t.Fatalf("reads: %d", fs.Calls(p))
			}
		})
	}
}

// An empty file fails the Check, with no content.
func TestFileRuleEmptyFile_AC31(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "empty")
	fs.Put(p, nil)
	res := load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	requireRecords(t, res, rec{"t.pem", loader.ErrInvalidValue})
}

// Two file settings are each read once, and one failing does not stop the
// other from loading.
func TestFileRuleTwoSettings_AC24(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	a, b := ep(m, "a"), ep(m, "b")
	fs.Put(a, goodPEM(m))
	fs.Put(b, []byte("bad"))
	res := load(t, fileReg(t), []string{"--t.pem=" + a, "--t.pem2=" + b}, nil, fs)
	requireRecords(t, res, rec{"t.pem2", loader.ErrInvalidValue})
	if _, ok := res.Get("t.pem"); !ok {
		t.Fatal("the good setting did not load")
	}
	if fs.Calls(a) != 1 || fs.Calls(b) != 1 {
		t.Fatalf("reads: %d %d", fs.Calls(a), fs.Calls(b))
	}
}

// A cross rule gets the FileValue (the bytes already read), not the path
// alone, and the reader is not called again for it.
func TestFileRuleCrossRuleReceivesContent_AC24(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	var seen registry.FileValue
	var gotType bool
	r := rule("pem-seen", []string{"t.pem"}, "t.pem", false, func(get func(string) (any, bool)) error {
		v, set := get("t.pem")
		if !set {
			return nil
		}
		seen, gotType = v.(registry.FileValue)
		return nil
	})
	res := load(t, fileReg(t, r), []string{"--t.pem=" + p}, nil, fs)
	requireNoErrors(t, res)
	if !gotType || seen.Path != p || !bytes.Equal(seen.Content, goodPEM(m)) {
		t.Fatalf("the cross rule saw type-ok=%v path-ok=%v content-len=%d", gotType, seen.Path == p, len(seen.Content))
	}
	if fs.Calls(p) != 1 {
		t.Fatalf("reads: %d", fs.Calls(p))
	}
}

// A cross rule can refuse on the content, reported under its primary setting,
// without leaking the content.
func TestFileRuleCrossRuleFailureOnContent_FR17(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	r := rule("pem-no-marker", []string{"t.pem"}, "t.pem", false, func(get func(string) (any, bool)) error {
		v, _ := get("t.pem")
		if fv, ok := v.(registry.FileValue); ok && bytes.Contains(fv.Content, []byte("body-")) {
			return errors.New("synthetic rule failure " + string(fv.Content))
		}
		return nil
	})
	res := load(t, fileReg(t, r), []string{"--t.pem=" + p}, nil, fs)
	requireRecords(t, res, rec{"t.pem", loader.ErrCrossRule})
	requireNoMarker(t, res, m, p)
}

// SPEC-003 FR-13: group order holds with mixed errors: argument problems
// first, then per-input errors in registry order, then cross rules.
func TestFileRuleRecordOrder_FR13(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "missing")
	res := load(t, fileReg(t), []string{
		"--bogus=" + m, // group 1
		"--t.pem=" + p, // group 2: unreadable file (declared after t.n)
		"--t.n=notanumber",
		"--t.a=10", "--t.b=1", // group 3: b-ge-a
	}, nil, fs)
	var order []string
	for _, e := range res.Errors {
		order = append(order, errSetting(e))
	}
	idx := func(name string) int {
		for i, s := range order {
			if s == name {
				return i
			}
		}
		return -1
	}
	iN, iP, iB := idx("t.n"), idx("t.pem"), idx("t.b")
	if iN < 0 || iP < 0 || iB < 0 {
		t.Fatalf("records = %v", order)
	}
	if !(iN < iP && iP < iB) {
		t.Fatalf("order = %v, want t.n < t.pem < t.b", order)
	}
	if len(order) < 4 {
		t.Fatalf("records = %v, want the argument problem first", order)
	}
	requireNoMarker(t, res, m, p)
}

// Supplied lists only the logical name.
func TestFileRuleSuppliedLogicalName_FR20(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	res := load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	requireNoErrors(t, res)
	found := false
	for _, s := range res.Supplied {
		if s == "t.pem" {
			found = true
		}
		if strings.Contains(s, p) || strings.Contains(s, m) {
			t.Fatalf("Supplied carries a path or marker: %q", s)
		}
	}
	if !found {
		t.Fatalf("Supplied = %v", res.Supplied)
	}
}

// Content bytes are copied: the FileValue is the loader's own copy.
func TestFileRuleContentIsACopy_FR25(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "pem")
	fs.Put(p, goodPEM(m))
	res := load(t, fileReg(t), []string{"--t.pem=" + p}, nil, fs)
	fv := fileValue(t, res, "t.pem")
	fs.Put(p, []byte("changed"))
	if !bytes.Equal(fv.Content, goodPEM(m)) {
		t.Fatal("the loaded content changed with the file")
	}
}

func errSetting(e error) string {
	var ce *config.Error
	if errors.As(e, &ce) {
		return ce.Setting
	}
	return ""
}
