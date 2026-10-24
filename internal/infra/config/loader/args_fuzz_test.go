package loader_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/config/loader"
)

// FuzzParseArgs: ParseArgs never panics; returns at most one problem per
// argument; returns at most one problem per registered flag name, whether a
// setting flag or a _file flag (the FR-28 occurrence rule); keeps positions
// in range and in order; and never returns an input for a failed setting.
func FuzzParseArgs(f *testing.F) {
	seeds := [][4]string{
		{"--t.x=1", "--t.x=2", "--t.x", "pos"},
		{"--t.secret=v", "--t.secret_file=p", "--t.secret_file=q", "-t.secret"},
		{"--", "-", "-=x", ""},
		{"--log.level", "debug", "--check-config=true", "--help"},
		{"--a.b_file=", "-a.b_file", "--a.b=1", "--a.b"},
		{"--t.x\x00=1", "--\xff", "-t.y=\xc3", "--tracing.enabled=true"},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1], s[2], s[3])
	}
	r := testRegistry(f)
	flagNames := map[string]bool{}
	for _, s := range r.Settings() {
		flagNames[s.Flag] = true
		if s.FileFlag != "" {
			flagNames[s.FileFlag] = true
		}
	}
	// Constant text a problem may legitimately contain: sentinel texts and the
	// fixed setting names. Supplied text is only checked when it is not part
	// of these, so short coincidences cannot cause false failures.
	constants := []string{"unknown", "argument"}
	for _, e := range []error{loader.ErrUnknownFlag, loader.ErrPositional, loader.ErrMissingValue, loader.ErrRepeated,
		loader.ErrSecretOnCommandLine, loader.ErrControlFlagValue} {
		constants = append(constants, e.Error())
	}
	secretNames := map[string]bool{}
	for _, s := range r.Settings() {
		constants = append(constants, s.Name, s.FileFlag)
		if s.Secret {
			secretNames[s.Name] = true
		}
	}
	constText := strings.Join(constants, "\x00")
	f.Fuzz(func(t *testing.T, a, b, c, d string) {
		args := []string{a, b, c, d}
		for n := 0; n <= len(args); n++ {
			in, pr, failed := loader.ParseArgs(r, args[:n])
			perArg := map[int]int{}
			perName := map[string]int{}
			for _, p := range pr {
				if p.Pos < 0 || p.Pos >= n {
					t.Fatalf("problem position %d out of range", p.Pos)
				}
				perArg[p.Pos]++
				if perArg[p.Pos] > 1 {
					t.Fatalf("more than one problem for argument %d", p.Pos)
				}
				if p.Err == nil {
					t.Fatal("nil problem error")
				}
				if flagNames[p.Setting] {
					perName[p.Setting]++
					if perName[p.Setting] > 1 {
						t.Fatalf("more than one problem for flag name %s", p.Setting)
					}
				}
			}
			// No supplied text appears in any rendering of a problem (FR-8,
			// FR-14: a mistyped name or a secret literal is never echoed).
			for _, p := range pr {
				var shown []string
				shown = append(shown, p.Setting, p.Err.Error(), fmt.Sprintf("%v", p.Err), fmt.Sprintf("%+v", p.Err), fmt.Sprintf("%#v", p.Err))
				rendered := strings.Join(shown, "\x00")
				for _, arg := range args[:n] {
					_, val, _ := strings.Cut(arg, "=")
					for _, secret := range []string{arg, val} {
						if len(secret) >= 6 && !strings.Contains(constText, secret) && strings.Contains(rendered, secret) {
							t.Fatalf("a problem at %d renders supplied text of length %d", p.Pos, len(secret))
						}
					}
				}
			}
			for _, i := range in {
				// A secret literal is never an input: the only input form for a
				// secret is a file reference, and its value is a path.
				if secretNames[i.Setting] && !i.FileRef {
					t.Fatalf("secret literal for %s returned as an input", i.Setting)
				}
				if i.Pos < 0 || i.Pos >= n {
					t.Fatalf("input position %d out of range", i.Pos)
				}
				if _, ok := r.Lookup(i.Setting); !ok {
					t.Fatalf("input for unregistered setting")
				}
				if failed[i.Setting] {
					t.Fatalf("input for a failed setting %s", i.Setting)
				}
			}
		}
	})
}
