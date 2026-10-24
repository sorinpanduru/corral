// Package symcheck reports references to forbidden package-level symbols
// (SPEC-003 AC-15, FR-25), found by resolved object (types.Info.Uses), so
// import aliases, dot-imports and method values are caught and look-alike
// names from other packages are not. Only the default build is analysed (host
// GOOS/GOARCH, no extra build tags, cgo off): files excluded by build
// constraints, such as other-OS or corraltest-tagged files, are not. It is
// test infrastructure and must not be imported by production code.
package symcheck

import (
	"errors"
	"fmt"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Finding is one reference to a forbidden symbol.
type Finding struct {
	Package string // import path of the referencing package
	File    string // source file path
	Line    int
	Symbol  string // "<import path>.<Name>", for example "os.Getenv"
}

type symKey struct{ pkg, name string }

type found struct {
	Finding
	offset int
}

// Forbidden loads the packages matched by patterns (relative to dir) and
// returns every non-test reference to one of symbols in a file accepted by
// fileFilter (nil accepts every file), sorted by Package, File, Line, Symbol.
//
// It never passes vacuously: it errors on empty patterns or symbols, a
// pattern starting with "-", a pattern that matches no package, a package that
// fails to load or type-check, and a missing dir. The analysis is hermetic:
// the caller's GOFLAGS, GOPACKAGESDRIVER, GOWORK and GOENV have no effect.
func Forbidden(dir string, patterns []string, fileFilter func(path string) bool, symbols []string) ([]Finding, error) {
	if len(patterns) == 0 {
		return nil, errors.New("symcheck: no patterns")
	}
	if len(symbols) == 0 {
		return nil, errors.New("symcheck: empty symbol list")
	}
	for _, p := range patterns {
		if p == "" || strings.HasPrefix(p, "-") {
			return nil, errors.New("symcheck: invalid pattern: must be non-empty and not start with \"-\"")
		}
	}
	want := make(map[symKey]string, len(symbols))
	for _, s := range symbols {
		i := strings.LastIndexByte(s, '.')
		if i <= 0 || i == len(s)-1 {
			return nil, fmt.Errorf("symcheck: invalid symbol %q: want <import path>.<Name>", s)
		}
		want[symKey{s[:i], s[i+1:]}] = s
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("symcheck: invalid directory %q", dir)
	}

	seen := map[found]bool{}
	var all []found
	for _, pattern := range patterns {
		// One load per pattern, so each pattern must match something.
		pkgs, err := load(dir, pattern)
		if err != nil {
			return nil, err
		}
		for _, f := range scan(pkgs, want, fileFilter) {
			if !seen[f] {
				seen[f] = true
				all = append(all, f)
			}
		}
	}
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i], all[j]
		switch {
		case a.Package != b.Package:
			return a.Package < b.Package
		case a.File != b.File:
			return a.File < b.File
		case a.Line != b.Line:
			return a.Line < b.Line
		case a.Symbol != b.Symbol:
			return a.Symbol < b.Symbol
		}
		return a.offset < b.offset
	})
	out := make([]Finding, len(all))
	for i, f := range all {
		out[i] = f.Finding
	}
	return out, nil
}

func load(dir, pattern string) ([]*packages.Package, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedTypes | packages.NeedTypesSizes |
			packages.NeedSyntax | packages.NeedTypesInfo,
		Dir:   dir,
		Tests: false,
		// Later entries win: the caller's go environment cannot change
		// what is analysed.
		Env: append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off",
			"GOWORK=off", "GOENV=off", "GOPACKAGESDRIVER=off", "CGO_ENABLED=0"),
	}
	pkgs, err := packages.Load(cfg, pattern)
	if err != nil {
		return nil, fmt.Errorf("symcheck: load: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("symcheck: pattern %q matched no packages", pattern)
	}
	for _, p := range pkgs {
		if len(p.Errors) > 0 {
			return nil, fmt.Errorf("symcheck: package %s does not load or type-check: %v", p.PkgPath, p.Errors[0])
		}
		if p.TypesInfo == nil || p.Fset == nil {
			return nil, fmt.Errorf("symcheck: package %s has no type information", p.PkgPath)
		}
	}
	return pkgs, nil
}

func scan(pkgs []*packages.Package, want map[symKey]string, filter func(string) bool) []found {
	var out []found
	for _, p := range pkgs {
		inScope := map[string]bool{}
		for _, f := range p.Syntax {
			name := p.Fset.Position(f.Pos()).Filename
			inScope[name] = filter == nil || filter(name)
		}
		for id, obj := range p.TypesInfo.Uses {
			if obj == nil || obj.Pkg() == nil || obj.Parent() != obj.Pkg().Scope() {
				continue // not a package-level object
			}
			sym, ok := want[symKey{obj.Pkg().Path(), obj.Name()}]
			if !ok || !isValueOrFunc(obj) {
				continue
			}
			pos := p.Fset.Position(id.Pos())
			if !inScope[pos.Filename] {
				continue
			}
			out = append(out, found{Finding{p.PkgPath, pos.Filename, pos.Line, sym}, pos.Offset})
		}
	}
	return out
}

func isValueOrFunc(obj types.Object) bool {
	switch obj.(type) {
	case *types.Func, *types.Var, *types.Const:
		return true
	}
	return false
}
