package loader_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
	"github.com/sorinpanduru/corral/internal/infra/config/source"
)

// AC-8 (secrets and file references), AC-10 loader part (read once, no
// re-read), FR-14, FR-15, FR-28 rows for secrets. t.secret is an optional
// secret of type text. Markers sit in values, paths and file contents.

const (
	envLit  = "CORRAL_T_SECRET"
	envFile = "CORRAL_T_SECRET_FILE"
)

func TestAC8_SecretLiteralOnCommandLine(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	res := load(t, baseReg(t), []string{"--t.secret=" + m}, nil, fs)
	requireRecords(t, res, rec{"t.secret", loader.ErrSecretOnCommandLine})
	if !res.Failed("t.secret") {
		t.Error("t.secret not failed")
	}
	requireNoMarker(t, res, m)
}

func TestAC8_LiteralWithFileFlag(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "p")
	fs.Put(p, []byte("fromfile"))
	res := load(t, baseReg(t), []string{"--t.secret=" + m, "--t.secret_file=" + p}, nil, fs)
	requireRecords(t, res, rec{"t.secret", loader.ErrSecretOnCommandLine})
	if n := fs.Calls(p); n != 0 {
		t.Errorf("reader called %d times for the reference next to a secret literal", n)
	}
	requireNoMarker(t, res, m)
}

// Amendment 1: two records, in this order, and the reader is never called.
func TestAC8_LiteralWithTwoFileFlags(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p1, p2 := ep(m, "p1"), ep(m, "p2")
	fs.Put(p1, []byte("a"))
	fs.Put(p2, []byte("b"))
	res := load(t, baseReg(t), []string{"--t.secret=" + m, "--t.secret_file=" + p1, "--t.secret_file=" + p2}, nil, fs)
	requireRecords(t, res, rec{"t.secret", loader.ErrSecretOnCommandLine}, rec{"t.secret_file", loader.ErrRepeated})
	if fs.Calls(p1)+fs.Calls(p2) != 0 {
		t.Error("the reader was called")
	}
	requireNoMarker(t, res, m)
}

func TestAC8_RepeatedFileFlagNotRead(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p1, p2 := ep(m, "p1"), ep(m, "p2")
	fs.Put(p1, []byte("a"))
	fs.Put(p2, []byte("b"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p1, "--t.secret_file=" + p2}, nil, fs)
	requireRecords(t, res, rec{"t.secret_file", loader.ErrRepeated})
	if fs.Calls(p1)+fs.Calls(p2) != 0 {
		t.Error("the reader was called for a repeated reference")
	}
	if !res.Failed("t.secret") {
		t.Error("t.secret not failed")
	}
}

func TestAC8_EnvLiteralAccepted(t *testing.T) {
	m := newMarker(t)
	res := load(t, baseReg(t), nil, []string{envLit + "=" + m}, nil)
	requireNoErrors(t, res)
	requireValue(t, res, "t.secret", m, loader.SourceEnv, false)
}

func TestAC8_EnvLiteralAndFileBothSet(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "p")
	fs.Put(p, []byte("v"))
	res := load(t, baseReg(t), nil, []string{envLit + "=" + m, envFile + "=" + p}, fs)
	requireRecords(t, res, rec{envLit, loader.ErrLiteralAndFile})
	if fs.Calls(p) != 0 {
		t.Error("the reader was called for a conflicting reference")
	}
	if !res.Failed("t.secret") {
		t.Error("t.secret not failed")
	}
	requireNoMarker(t, res, m)
}

// The same pair together with a valid flag file reference: still that one
// record; the flag wins and its file is read.
func TestAC8_ConflictWithValidFlagFile(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	fp, ep1 := ep(m, "flag"), ep(m, "env")
	fs.Put(fp, []byte("flagvalue\n"))
	fs.Put(ep1, []byte("envvalue\n"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + fp}, []string{envLit + "=" + m, envFile + "=" + ep1}, fs)
	requireRecords(t, res, rec{envLit, loader.ErrLiteralAndFile})
	if res.Failed("t.secret") {
		t.Error("t.secret failed although the valid flag source wins")
	}
	requireValue(t, res, "t.secret", "flagvalue", loader.SourceFlag, true)
	if fs.Calls(fp) != 1 || fs.Calls(ep1) != 0 {
		t.Errorf("reads: flag file %d, env file %d, want 1 and 0", fs.Calls(fp), fs.Calls(ep1))
	}
}

func TestAC8_FileReferenceAcceptedFlagAndEnv(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "p")
	fs.Put(p, []byte("filevalue"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
	requireNoErrors(t, res)
	requireValue(t, res, "t.secret", "filevalue", loader.SourceFlag, true)
	res = load(t, baseReg(t), nil, []string{envFile + "=" + p}, fs)
	requireNoErrors(t, res)
	requireValue(t, res, "t.secret", "filevalue", loader.SourceEnv, true)
	res = load(t, baseReg(t), []string{"-t.secret_file=" + p}, nil, fs)
	requireNoErrors(t, res)
}

// FR-15: exactly one trailing \n or \r\n is removed; an inner newline is kept.
func TestAC8_TrailingNewlineStripping(t *testing.T) {
	cases := []struct{ name, content, want string }{
		{"no newline", "v", "v"},
		{"LF", "v\n", "v"},
		{"CRLF", "v\r\n", "v"},
		{"two LF: only one removed", "v\n\n", "v\n"},
		{"two CRLF: only one removed", "v\r\n\r\n", "v\r\n"},
		{"inner newline kept", "a\nb\n", "a\nb"},
		{"inner CRLF kept", "a\r\nb\r\n", "a\r\nb"},
		{"lone CR kept", "v\r", "v\r"},
		{"leading newline kept", "\nv", "\nv"},
		{"spaces kept", " v \n", " v "},
		{"multi-line PEM-like", "-----BEGIN X-----\nQUJD\n-----END X-----\n", "-----BEGIN X-----\nQUJD\n-----END X-----"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newMarker(t)
			fs := newFS(t, m)
			fs.Put(ep(m, "f"), []byte(c.content))
			for _, kind := range []string{"flag", "env"} {
				var res loader.Result
				if kind == "flag" {
					res = load(t, baseReg(t), []string{"--t.secret_file=" + ep(m, "f")}, nil, fs)
				} else {
					res = load(t, baseReg(t), nil, []string{envFile + "=" + ep(m, "f")}, fs)
				}
				requireNoErrors(t, res)
				v, _ := res.Get("t.secret")
				if v != c.want {
					t.Errorf("%s: value length %d, want length %d", kind, len(v.(string)), len(c.want))
				}
			}
		})
	}
}

// Each failure: one record naming the reference (not the setting), and the
// path marker does not appear.
func TestAC8_FileFailuresNameTheReference(t *testing.T) {
	m := newMarker(t)
	big := bytes.Repeat([]byte("a"), 65537)
	cases := []struct {
		name  string
		setup func(fs interface{ Put(string, []byte) }, fail func(string, error), p string)
		want  error
	}{
		{"missing file", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {}, loader.ErrFileUnreadable},
		{"reader says unreadable", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fail(p, source.ErrUnreadable)
		}, loader.ErrFileUnreadable},
		{"directory, FIFO, device", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fail(p, source.ErrNotRegular)
		}, loader.ErrFileNotRegular},
		{"65,537 bytes", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) { fs.Put(p, big) }, loader.ErrFileTooLarge},
		{"reader says too large", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fail(p, source.ErrTooLarge)
		}, loader.ErrFileTooLarge},
		{"only a newline", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) { fs.Put(p, []byte("\n")) }, loader.ErrInvalidValue},
		{"only CRLF", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fs.Put(p, []byte("\r\n"))
		}, loader.ErrInvalidValue},
		{"empty file", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) { fs.Put(p, nil) }, loader.ErrInvalidValue},
		{"content with a control character", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fs.Put(p, []byte("x"+m+"\x00y"))
		}, loader.ErrInvalidValue},
		{"content that is not UTF-8", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fs.Put(p, append([]byte(m), 0xff, 0xfe))
		}, loader.ErrInvalidValue},
		{"unknown reader error", func(fs interface{ Put(string, []byte) }, fail func(string, error), p string) {
			fail(p, errors.New("reader failed for "+p+" "+m))
		}, loader.ErrFileUnreadable},
	}
	for _, c := range cases {
		for _, kind := range []string{"flag", "env"} {
			t.Run(c.name+"/"+kind, func(t *testing.T) {
				fs := newFS(t, m)
				p := ep(m, "f")
				c.setup(fs, fs.Fail, p)
				var res loader.Result
				want := "t.secret_file"
				if kind == "flag" {
					res = load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
				} else {
					want = envFile
					res = load(t, baseReg(t), nil, []string{envFile + "=" + p}, fs)
				}
				requireRecords(t, res, rec{want, c.want})
				if !res.Failed("t.secret") {
					t.Error("t.secret not failed")
				}
				if fs.Calls(p) != 1 {
					t.Errorf("reader called %d times, want 1", fs.Calls(p))
				}
				requireNoMarker(t, res, m)
			})
		}
	}
}

// A file whose size is exactly 65,536 bytes is accepted; the reader is given
// the 64 KiB limit.
func TestAC8_SizeLimit(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "f")
	fs.Put(p, bytes.Repeat([]byte("a"), 65536))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
	requireNoErrors(t, res)
	if v, _ := res.Get("t.secret"); len(v.(string)) != 65536 {
		t.Errorf("value length %d, want 65536", len(v.(string)))
	}
	// 65,535 + "\n" is 65,536 raw bytes: accepted, newline removed.
	fs.Put(p, append(bytes.Repeat([]byte("a"), 65535), '\n'))
	res = load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
	requireNoErrors(t, res)
	if v, _ := res.Get("t.secret"); len(v.(string)) != 65535 {
		t.Errorf("value length %d, want 65535", len(v.(string)))
	}
	// 65,536 + "\n" is 65,537 raw bytes: too large, measured before stripping.
	fs.Put(p, append(bytes.Repeat([]byte("a"), 65536), '\n'))
	res = load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
	requireRecords(t, res, rec{"t.secret_file", loader.ErrFileTooLarge})
	// The loader asks the reader for the secret limit.
	var seen []int64
	rd := func(path string, max int64) ([]byte, error) { seen = append(seen, max); return fs.Read(path, max) }
	loader.Load(baseReg(t), loader.Inputs{Args: []string{"--t.secret_file=" + p}, ReadFile: rd})
	if len(seen) != 1 || seen[0] != source.MaxSecretFileBytes {
		t.Errorf("reader limits = %v, want [%d]", seen, int64(source.MaxSecretFileBytes))
	}
}

func TestAC8_EmptyFlagPath(t *testing.T) {
	fs := newFS(t, "m")
	res := load(t, baseReg(t), []string{"--t.secret_file="}, nil, fs)
	requireRecords(t, res, rec{"t.secret_file", loader.ErrEmptyPath})
	if fs.Calls("") != 0 {
		t.Error("reader called for an empty path")
	}
	if !res.Failed("t.secret") {
		t.Error("t.secret not failed")
	}
}

func TestAC8_FlagFileBeatsEnvLiteral(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "f")
	fs.Put(p, []byte("filevalue"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p}, []string{envLit + "=envliteral"}, fs)
	requireNoErrors(t, res)
	requireValue(t, res, "t.secret", "filevalue", loader.SourceFlag, true)
	// The overridden env literal is still validated (FR-10).
	res = load(t, baseReg(t), []string{"--t.secret_file=" + p}, []string{envLit + "=bad\x00"}, fs)
	requireRecords(t, res, rec{envLit, loader.ErrInvalidValue})
	requireValue(t, res, "t.secret", "filevalue", loader.SourceFlag, true)
}

// FR-5: an overridden CORRAL_T_SECRET_FILE is still read; a missing file is
// still an error under that name.
func TestAC8_OverriddenEnvFileStillRead(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	fp, gone := ep(m, "flag"), ep(m, "gone")
	fs.Put(fp, []byte("flagvalue"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + fp}, []string{envFile + "=" + gone}, fs)
	requireRecords(t, res, rec{envFile, loader.ErrFileUnreadable})
	if fs.Calls(gone) != 1 {
		t.Errorf("overridden env file read %d times, want 1", fs.Calls(gone))
	}
	if res.Failed("t.secret") {
		t.Error("t.secret failed although its winning flag source is valid")
	}
	requireValue(t, res, "t.secret", "flagvalue", loader.SourceFlag, true)
	requireNoMarker(t, res, m)
}

// FR-5: environment inputs are still processed when the flag source failed at
// group 1 (a secret literal).
func TestAC8_EnvFileReadEvenWhenFlagLiteralFailed(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p, gone := ep(m, "ok"), ep(m, "gone")
	fs.Put(p, []byte("v"))
	res := load(t, baseReg(t), []string{"--t.secret=" + m}, []string{envFile + "=" + p}, fs)
	requireRecords(t, res, rec{"t.secret", loader.ErrSecretOnCommandLine})
	if fs.Calls(p) != 1 {
		t.Errorf("env file read %d times, want 1", fs.Calls(p))
	}
	res = load(t, baseReg(t), []string{"--t.secret=" + m}, []string{envFile + "=" + gone}, fs)
	requireRecords(t, res, rec{"t.secret", loader.ErrSecretOnCommandLine}, rec{envFile, loader.ErrFileUnreadable})
}

func TestAC8_OptionalSecretUnset(t *testing.T) {
	res := load(t, baseReg(t), nil, nil, nil)
	requireNoErrors(t, res)
	requireUnset(t, res, "t.secret")
	if res.Failed("t.secret") {
		t.Error("an unset optional secret is failed")
	}
}

// The non-secret _file flag/variable is unknown (flag: T4; env: here).
func TestAC8_NonSecretFileFormIsUnknown(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	res := load(t, baseReg(t), []string{"--t.name_file=" + ep(m, "p")}, []string{"CORRAL_T_NAME_FILE=" + ep(m, "q")}, fs)
	requireRecords(t, res, rec{"unknown", loader.ErrUnknownFlag})
	if !reflect.DeepEqual(res.UnknownEnv, []string{"CORRAL_T_NAME_FILE"}) {
		t.Fatalf("UnknownEnv = %v", res.UnknownEnv)
	}
	if fs.Calls(ep(m, "p"))+fs.Calls(ep(m, "q")) != 0 {
		t.Error("a non-secret file reference was read")
	}
	requireNoMarker(t, res, m)
}

// AC-10 (unit): one Load reads each reference once, and later changes to the
// file do not change the result.
func TestAC10_ReadOnceAndNoReRead(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	p := ep(m, "f")
	fs.Put(p, []byte("first\n"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + p}, nil, fs)
	requireNoErrors(t, res)
	if fs.Calls(p) != 1 {
		t.Fatalf("reference read %d times, want 1", fs.Calls(p))
	}
	fs.Put(p, []byte("second\n"))
	if v, _ := res.Get("t.secret"); v != "first" {
		t.Fatalf("result changed after the file changed")
	}
	if fs.Calls(p) != 1 {
		t.Fatalf("a later access re-read the file (%d reads)", fs.Calls(p))
	}
	// Values hold their own copy of the content.
	v, _ := res.Get("t.secret")
	if strings.Contains(v.(string), "second") {
		t.Fatal("value follows the file")
	}
}

// Both an env and a flag reference to different files: each read exactly once.
func TestAC10_EachReferenceReadOnce(t *testing.T) {
	m := newMarker(t)
	fs := newFS(t, m)
	a, b := ep(m, "a"), ep(m, "b")
	fs.Put(a, []byte("A"))
	fs.Put(b, []byte("B"))
	res := load(t, baseReg(t), []string{"--t.secret_file=" + a}, []string{envFile + "=" + b}, fs)
	requireNoErrors(t, res)
	if fs.Calls(a) != 1 || fs.Calls(b) != 1 {
		t.Fatalf("reads: %d and %d, want 1 and 1", fs.Calls(a), fs.Calls(b))
	}
	requireValue(t, res, "t.secret", "A", loader.SourceFlag, true)
}
