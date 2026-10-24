package attr_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/logging"
)

// requiredKeys is the single-source allowlist: the FR-39 fixed keys, the
// FR-23 span attribute keys (required and optional) and suppressed_count
// (plan T3: attr holds a string constant for each of them).
var requiredKeys = []string{
	// FR-39 fixed keys
	"time", "level", "msg", "component", "error", "trace_id", "span_id",
	"stack", "setting",
	// plan T3 named extras
	"corral.error.detail", "suppressed_count", "reason",
	// FR-23 required attributes
	"db.system.name", "db.operation.name", "db.namespace", "db.collection.name",
	"client.address", "client.port", "error.type",
	"db.response.status_code", "corral.db.response.code_name",
	// FR-23 optional attributes
	"server.address", "server.port", "corral.connection.id",
	"corral.upstream.connection.id", "corral.request.id", "corral.session.id",
	"corral.cursor.id", "corral.client.app_name",
	"corral.client.driver.name", "corral.client.driver.version",
}

// stringConstValues returns the values of all string-literal constants declared
// in the non-test files of this package. It reads the package source so the
// test does not depend on the (unspecified) Go identifier names.
func stringConstValues(t *testing.T) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	vals := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", n, err)
		}
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				for _, v := range vs.Values {
					if bl, ok := v.(*ast.BasicLit); ok && bl.Kind == token.STRING {
						if u, err := strconv.Unquote(bl.Value); err == nil {
							vals[u] = true
						}
					}
				}
			}
		}
	}
	return vals
}

// FR-39, FR-23: every allowed log key and FR-23 span attribute key has a
// string constant in attr.
func TestAttrDefinesConstantForEveryKey(t *testing.T) {
	vals := stringConstValues(t)
	var missing []string
	for _, k := range requiredKeys {
		if !vals[k] {
			missing = append(missing, k)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("attr has no string constant for keys: %v", missing)
	}
}

// FR-39: the allowlist exists once; every key attr defines for logs must be
// accepted by logging.AllowedKey.
func TestAttrKeysAreAllowedByLogging(t *testing.T) {
	for _, k := range requiredKeys {
		if !logging.AllowedKey(k) {
			t.Errorf("logging.AllowedKey(%q) = false, want true", k)
		}
	}
}

// FR-23: query text is never an allowed attribute.
func TestAttrHasNoQueryTextConstant(t *testing.T) {
	vals := stringConstValues(t)
	for _, bad := range []string{"db.query.text", "db.query.summary"} {
		if vals[bad] {
			t.Errorf("attr defines forbidden key %q", bad)
		}
	}
}
