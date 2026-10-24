// Package importcheck holds static import checks over `go list` output: the
// SPEC-001 FR-1 domain import boundary (Check, Scan), forbidden direct
// imports (Forbidden) and forbidden packages in a binary's dependency closure
// (ForbiddenDeps). It is test infrastructure and must not be imported by
// production code.
package importcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// DomainAllowlist is the FR-1(c) domain import allowlist. It is currently
// empty; adding to it requires amending the spec.
var DomainAllowlist []string

// Violation is one forbidden import of Import by the package Package.
type Violation struct {
	Package string // import path of the importing package
	Import  string // the forbidden import path
}

// String renders the violation with both paths.
func (v Violation) String() string {
	return fmt.Sprintf("package %s imports %s", v.Package, v.Import)
}

type listedPkg struct {
	ImportPath string
	Standard   bool
	DepOnly    bool
	Imports    []string
}

const slogPath = "log/slog"

// list runs go list in a hermetic environment and decodes the packages.
func list(dir, pattern string) ([]listedPkg, error) {
	return listEnv(dir, pattern)
}

// listEnv is list with extra environment entries appended last, so they win.
func listEnv(dir, pattern string, extraEnv ...string) ([]listedPkg, error) {
	if strings.HasPrefix(pattern, "-") {
		return nil, fmt.Errorf("invalid pattern %q: must not start with \"-\"", pattern)
	}
	cmd := exec.Command("go", "list", "-deps", "-json", pattern)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off",
		"GOENV=off", "GOPACKAGESDRIVER=off")
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go list in %q: %w: %s", dir, err, strings.TrimSpace(stderr.String()))
	}
	var pkgs []listedPkg
	dec := json.NewDecoder(&stdout)
	for {
		var p listedPkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("decode go list output: %w", err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs, nil
}

// inDomain reports whether path equals prefix or lies under it on a path
// boundary, so "a/testkit" does not match "a/testkitx". The root package
// (path == prefix, e.g. internal/domain) counts as domain on purpose.
func inDomain(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func sortViolations(vs []Violation) {
	sort.Slice(vs, func(i, j int) bool {
		if vs[i].Package != vs[j].Package {
			return vs[i].Package < vs[j].Package
		}
		return vs[i].Import < vs[j].Import
	})
}

// Scan returns the sorted import paths of the domain packages that pattern
// matches. It errors if none match, so callers cannot pass vacuously.
func Scan(dir, pattern, domainPrefix string) ([]string, error) {
	pkgs, err := list(dir, pattern)
	if err != nil {
		return nil, err
	}
	return domainPaths(pkgs, domainPrefix)
}

func domainPaths(pkgs []listedPkg, prefix string) ([]string, error) {
	var out []string
	for _, p := range pkgs {
		if inDomain(p.ImportPath, prefix) {
			out = append(out, p.ImportPath)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no domain packages under %q matched", prefix)
	}
	sort.Strings(out)
	return out, nil
}

// Check reports every non-test import of a package under domainPrefix that is
// not stdlib (except log/slog), not under domainPrefix, and not in allow.
func Check(dir, pattern, domainPrefix string, allow []string) ([]Violation, error) {
	pkgs, err := list(dir, pattern)
	if err != nil {
		return nil, err
	}
	if _, err := domainPaths(pkgs, domainPrefix); err != nil {
		return nil, err
	}
	std := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		if p.Standard {
			std[p.ImportPath] = true
		}
	}
	allowed := make(map[string]bool, len(allow))
	for _, a := range allow {
		allowed[a] = true
	}
	var out []Violation
	for _, p := range pkgs {
		if !inDomain(p.ImportPath, domainPrefix) {
			continue
		}
		for _, imp := range p.Imports {
			if imp == slogPath {
				out = append(out, Violation{Package: p.ImportPath, Import: imp})
				continue
			}
			if inDomain(imp, domainPrefix) || allowed[imp] || std[imp] {
				continue
			}
			out = append(out, Violation{Package: p.ImportPath, Import: imp})
		}
	}
	sortViolations(out)
	return out, nil
}

func matchesAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if inDomain(path, p) {
			return true
		}
	}
	return false
}

// Forbidden reports every non-test import, by a package matched by pattern and
// not under an exclude prefix, of a path under a forbidden prefix. Prefixes
// match on path boundaries. Dependency-only packages are not examined. It
// errors on an empty forbidden list or if no matched package remains after
// exclusion, so callers cannot pass vacuously. Results are sorted by Package
// then Import.
func Forbidden(dir, pattern string, exclude, forbidden []string) ([]Violation, error) {
	if len(forbidden) == 0 {
		return nil, fmt.Errorf("empty forbidden list")
	}
	pkgs, err := list(dir, pattern)
	if err != nil {
		return nil, err
	}
	var out []Violation
	checked := 0
	for _, p := range pkgs {
		if p.DepOnly || matchesAny(p.ImportPath, exclude) {
			continue
		}
		checked++
		for _, imp := range p.Imports {
			if matchesAny(imp, forbidden) {
				out = append(out, Violation{Package: p.ImportPath, Import: imp})
			}
		}
	}
	if checked == 0 {
		return nil, fmt.Errorf("no packages matched %q after exclusion", pattern)
	}
	sortViolations(out)
	return out, nil
}

// ForbiddenDeps returns the sorted, deduplicated paths in the dependency
// closure of pattern (including the matched packages) that fall under a
// forbidden prefix, for the given goos/goarch with cgo disabled. It errors on
// an empty forbidden list or when nothing matches.
func ForbiddenDeps(dir, pattern, goos, goarch string, forbidden []string) ([]string, error) {
	if len(forbidden) == 0 {
		return nil, fmt.Errorf("empty forbidden list")
	}
	pkgs, err := listEnv(dir, pattern, "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
	if err != nil {
		return nil, err
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %q", pattern)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range pkgs {
		if matchesAny(p.ImportPath, forbidden) && !seen[p.ImportPath] {
			seen[p.ImportPath] = true
			out = append(out, p.ImportPath)
		}
	}
	sort.Strings(out)
	return out, nil
}
