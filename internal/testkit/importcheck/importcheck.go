// Package importcheck enforces the SPEC-001 FR-1 domain import boundary.
// It is test infrastructure and must not be imported by production code.
package importcheck

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// Format implements fmt.Formatter so that %v, %+v and %s all include both paths.
func (v Violation) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, v.String())
}

type listedPkg struct {
	ImportPath string
	Standard   bool
	Imports    []string
}

const slogPath = "log/slog"

// Check runs `go list -deps -json pattern` in dir and reports every non-test
// import of a package under domainPrefix that is not stdlib (except log/slog),
// not under domainPrefix, and not in allow.
func Check(dir, pattern, domainPrefix string, allow []string) ([]Violation, error) {
	cmd := exec.Command("go", "list", "-deps", "-json", pattern)
	cmd.Dir = dir
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
	inDomain := func(path string) bool {
		return path == domainPrefix || strings.HasPrefix(path, domainPrefix+"/")
	}

	var out []Violation
	for _, p := range pkgs {
		if !inDomain(p.ImportPath) {
			continue
		}
		for _, imp := range p.Imports {
			switch {
			case imp == slogPath:
			case inDomain(imp), allowed[imp], std[imp]:
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
