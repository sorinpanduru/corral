// Package constmsg is a static check that every log/slog call passes a
// compile-time constant message (SPEC-001 FR-39, AC-5). It is test
// infrastructure and must not be imported by production code.
//
// Checked: log/slog package-level functions, *slog.Logger methods taking a
// message, method expressions, and slog.NewRecord. A reference to one of these
// that is not a direct call (method value, function variable) and a call whose
// arguments are a multi-value spread are reported, so the check fails closed.
// Files behind the functional build tag are loaded.
//
// Accepted limitations: calls through user-defined interfaces that wrap a
// logger (for example an interface with an Info(msg string) method) are not
// tracked, and neither are messages passed to the *log.Logger returned by
// slog.NewLogLogger (its Printf and friends). The latter is to be revisited
// at T8, where the bridges that use it are built.
package constmsg

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Finding is one slog call whose message is not a compile-time constant.
type Finding struct {
	File    string
	Line    int
	Column  int
	Package string
	Func    string // slog function or method name, for example "Info"
	Message string // what is wrong with the call
}

// String renders the finding as file:line:col.
func (f Finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.File, f.Line, f.Column, f.Message)
}

// msgArg is the index of the message argument for each checked slog function.
var msgArg = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2, "NewRecord": 2,
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
		// Files behind the functional build tag are checked too.
		BuildFlags: []string{"-tags=functional"},
		// Hermetic go command: no ambient flags, toolchain download, network,
		// workspace or user env file.
		// Later entries override the caller's values. No external packages
		// driver and no C toolchain is ever run.
		Env: append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off", "GOENV=off",
			"GOPACKAGESDRIVER=off", "CGO_ENABLED=0"),
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
		info := p.TypesInfo
		for _, file := range p.Syntax {
			// Pre-pass: map each callee identifier to its call, so that every
			// other reference to a slog function can be flagged as a value use.
			callOf := map[*ast.Ident]*ast.CallExpr{}
			ast.Inspect(file, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					switch fun := ast.Unparen(call.Fun).(type) {
					case *ast.Ident:
						callOf[fun] = call
					case *ast.SelectorExpr:
						callOf[fun.Sel] = call
					}
				}
				return true
			})
			report := func(pos token.Pos, name, msg string) {
				ps := p.Fset.Position(pos)
				key := fmt.Sprintf("%s:%d", ps.Filename, ps.Line)
				if seen[key] {
					return
				}
				seen[key] = true
				out = append(out, Finding{File: ps.Filename, Line: ps.Line, Column: ps.Column, Package: p.PkgPath, Func: name, Message: msg})
			}
			ast.Inspect(file, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				fn, idx, ok := slogFunc(info, id)
				if !ok {
					return true
				}
				call, isCall := callOf[id]
				if !isCall {
					report(id.Pos(), fn.Name(), "slog."+fn.Name()+" used as a value; its message cannot be tracked")
					return true
				}
				if methodExpr(info, call) {
					idx++
				}
				if len(call.Args) == 1 {
					if tup, ok := info.TypeOf(call.Args[0]).(*types.Tuple); ok && tup.Len() > 1 {
						report(call.Pos(), fn.Name(), "slog."+fn.Name()+" called with a multi-value spread; message cannot be proven constant")
						return true
					}
				}
				if idx >= len(call.Args) {
					return true
				}
				if tv, ok := info.Types[call.Args[idx]]; ok && tv.Value != nil {
					return true
				}
				report(call.Pos(), fn.Name(), "slog."+fn.Name()+" message is not a compile-time constant")
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

// methodExpr reports whether call invokes a method expression such as
// (*slog.Logger).Info(l, msg), where the receiver is the first argument.
func methodExpr(info *types.Info, call *ast.CallExpr) bool {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return false
	}
	s, ok := info.Selections[sel]
	return ok && s.Kind() == types.MethodExpr
}

// slogFunc reports whether id refers to a log/slog package-level function or
// *slog.Logger method that takes a message (or slog.NewRecord), and the index
// of the message argument for a plain call.
func slogFunc(info *types.Info, id *ast.Ident) (*types.Func, int, bool) {
	fn, ok := info.Uses[id].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return nil, 0, false
	}
	idx, ok := msgArg[fn.Name()]
	if !ok {
		return nil, 0, false
	}
	if recv := fn.Type().(*types.Signature).Recv(); recv != nil {
		t := recv.Type()
		if p, ok := t.(*types.Pointer); ok {
			t = p.Elem()
		}
		named, ok := t.(*types.Named)
		if !ok || named.Obj().Name() != "Logger" {
			return nil, 0, false
		}
	} else if fn.Name() == "NewRecord" {
		return fn, 2, true
	}
	return fn, idx, true
}
