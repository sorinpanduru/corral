// Package importcheck enforces the SPEC-001 FR-1 domain import boundary.
// It is test infrastructure and must not be imported by production code.
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

// Violation is one forbidden import in a domain package.
type Violation struct {
	Package string // import path of the domain package
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
	if strings.HasPrefix(pattern, "-") {
		return nil, fmt.Errorf("invalid pattern %q: must not start with \"-\"", pattern)
	}
	cmd := exec.Command("go", "list", "-deps", "-json", pattern)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "GOTOOLCHAIN=local", "GOPROXY=off", "GOWORK=off",
		"GOENV=off", "GOPACKAGESDRIVER=off")
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

// inDomain reports whether path is under prefix. The root package
// (path == prefix, e.g. internal/domain) counts as domain on purpose.
func inDomain(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Import < out[j].Import
	})
	return out, nil
}

// hasPrefix reports whether path equals prefix or lies under it on a path
// boundary, so "a/testkit" does not match "a/testkitx".
func hasPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

func matchesAny(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if hasPrefix(path, p) {
			return true
		}
	}
	return false
}

// Forbidden reports every non-test import, by a package matched by pattern and
// not under an exclude prefix, of a path under a forbidden prefix. Prefixes
// match on path boundaries. Dependency-only packages are not examined. It
// errors if no matched package remains after exclusion, so callers cannot pass
// vacuously. Results are sorted by Package then Import.
func Forbidden(dir, pattern string, exclude, forbidden []string) ([]Violation, error) {
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Package != out[j].Package {
			return out[i].Package < out[j].Package
		}
		return out[i].Import < out[j].Import
	})
	return out, nil
}
