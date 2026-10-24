package lint_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SPEC-003 T12 (FR-18, FR-22, ADR-0013): structural checks of the entry point.
// The first statement of main registers SIGHUP before arguments are looked at;
// the version is a package variable defaulting to "devel", stamped by the
// linker through the Makefile.

func parseMain(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	dir := filepath.Join("..", "..", "cmd", "corral")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		t.Fatal("no Go files in cmd/corral")
	}
	return fset, files
}

// FR-18: signal.Notify(<chan>, syscall.SIGHUP) is the first statement of main.
func TestMainRegistersSIGHUPFirst(t *testing.T) {
	_, files := parseMain(t)
	var mainFn *ast.FuncDecl
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Name.Name == "main" {
				mainFn = fd
			}
		}
	}
	if mainFn == nil || mainFn.Body == nil || len(mainFn.Body.List) == 0 {
		t.Fatal("no main function with a body")
	}
	es, ok := mainFn.Body.List[0].(*ast.ExprStmt)
	if !ok {
		t.Fatalf("the first statement of main is %T, want a call to signal.Notify", mainFn.Body.List[0])
	}
	call, ok := es.X.(*ast.CallExpr)
	if !ok {
		t.Fatal("the first statement of main is not a call")
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Notify" {
		t.Fatal("the first statement of main is not signal.Notify")
	}
	if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "signal" {
		t.Fatal("the first statement of main is not os/signal.Notify")
	}
	hasHUP := false
	for _, a := range call.Args[1:] {
		if s, ok := a.(*ast.SelectorExpr); ok && s.Sel.Name == "SIGHUP" {
			hasHUP = true
		}
	}
	if len(call.Args) < 2 || !hasHUP {
		t.Fatal("the first statement of main does not register syscall.SIGHUP")
	}
}

// FR-22 / ADR-0013: `var version = "devel"` in package main (linker target
// main.version).
func TestMainVersionVariable(t *testing.T) {
	_, files := parseMain(t)
	found := false
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, sp := range gd.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, n := range vs.Names {
					if n.Name != "version" || i >= len(vs.Values) {
						continue
					}
					if bl, ok := vs.Values[i].(*ast.BasicLit); ok && bl.Value == `"devel"` {
						found = true
					}
				}
			}
		}
	}
	if !found {
		t.Fatal(`cmd/corral has no package variable version initialised to "devel"`)
	}
}

// FR-22: the Makefile defaults VERSION to devel and stamps main.version.
func TestMakefileStampsVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	mk := string(b)
	if !strings.Contains(mk, "VERSION ?= devel") {
		t.Error("Makefile lacks `VERSION ?= devel`")
	}
	var buildLine string
	for _, l := range strings.Split(mk, "\n") {
		if strings.Contains(l, "go build") && strings.Contains(l, "./cmd/corral") {
			buildLine = l
		}
	}
	if !strings.Contains(buildLine, "-ldflags") || !(strings.Contains(buildLine, "-X main.version=$(VERSION)") || strings.Contains(buildLine, "-X 'main.version=$(VERSION)'")) {
		t.Errorf("the build recipe does not stamp main.version from $(VERSION)")
	}
}
