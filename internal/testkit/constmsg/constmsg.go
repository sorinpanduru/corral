// Package constmsg is a static check that every log/slog call passes a
// compile-time constant message (SPEC-001 FR-39, AC-5). It is test
// infrastructure and must not be imported by production code.
package constmsg

import (
	"fmt"
	"go/ast"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

// Finding is one slog call whose message is not a compile-time constant.
type Finding struct {
	File    string
	Line    int
	Column  int
	Package string
	Func    string // slog function or method name, for example "Info"
}

// String renders the finding as file:line:col.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: slog.%s message is not a constant", f.File, f.Line, f.Column, f.Func)
}

// msgArg is the index of the message argument for each checked slog function.
var msgArg = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2,
}

// Check loads the packages matching patterns (relative to dir), including
// their test files, and reports every non-constant slog message. A load or
// type-check failure is returned as an error so the check never passes
// vacuously.
func Check(dir string, patterns ...string) ([]Finding, error) {
	for _, p := range patterns {
		if strings.HasPrefix(p, "-") {
			return nil, fmt.Errorf("invalid pattern %q: must not start with \"-\"", p)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("constmsg: %w", err)
	}
	cfg := &packages.Config{
		Dir: dir,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
		Tests: true,
		// Hermetic go command: no ambient flags, toolchain download, network,
		// workspace or user env file.
		Env: append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOENV=off"),
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("constmsg: load: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("constmsg: no packages matched %v", patterns)
	}
	var errs []string
	for _, p := range pkgs {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return nil, fmt.Errorf("constmsg: package errors: %s", strings.Join(errs, "; "))
	}

	seen := map[string]bool{} // test variants share files with the base package
	var out []Finding
	for _, p := range pkgs {
		for _, file := range p.Syntax {
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name, idx, ok := slogCall(p.TypesInfo, call)
				if !ok || idx >= len(call.Args) {
					return true
				}
				if tv, ok := p.TypesInfo.Types[call.Args[idx]]; ok && tv.Value != nil {
					return true
				}
				pos := p.Fset.Position(call.Pos())
				key := fmt.Sprintf("%s:%d:%d", pos.Filename, pos.Line, pos.Column)
				if seen[key] {
					return true
				}
				seen[key] = true
				out = append(out, Finding{File: pos.Filename, Line: pos.Line, Column: pos.Column, Package: p.PkgPath, Func: name})
				return true
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		if out[i].Line != out[j].Line {
			return out[i].Line < out[j].Line
		}
		return out[i].Column < out[j].Column
	})
	return out, nil
}

// slogCall reports whether call is a log/slog package-level function or a
// *slog.Logger method that takes a message, and the message argument index.
func slogCall(info *types.Info, call *ast.CallExpr) (string, int, bool) {
	fn, ok := typeutil.Callee(info, call).(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return "", 0, false
	}
	idx, ok := msgArg[fn.Name()]
	if !ok {
		return "", 0, false
	}
	recv := fn.Type().(*types.Signature).Recv()
	if recv != nil {
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		if !ok || named.Obj().Name() != "Logger" {
			return "", 0, false
		}
	}
	return fn.Name(), idx, true
}
