package settings_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// SPEC-003 AC-16 and FR-26 (T15): the operator documentation. AC-16 is exact:
// every registered setting's logical name and environment variable name
// appear in docs/operations/configuration.md. The other FR-26 topics are
// verified by review; the keyword checks below only guard that each topic is
// present at all (they cannot judge its content). Test code may read files;
// the AC-15 restriction applies to the loader, not to tests.

func docPath(name string) string {
	return filepath.Join("..", "..", "..", "docs", "operations", name)
}

func readDoc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(docPath(name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// entries splits configuration.md into per-setting entries: a heading
// "#### `<name>`" and the text up to the next heading.
func entries(doc string) map[string]string {
	out := map[string]string{}
	heading := regexp.MustCompile("(?m)^#{2,6} +`([^`]+)`[ \t]*$")
	idx := heading.FindAllStringSubmatchIndex(doc, -1)
	for i, m := range idx {
		end := len(doc)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		// an entry also ends at the next heading of any kind
		rest := doc[m[1]:end]
		if j := regexp.MustCompile("(?m)^#{1,6} ").FindStringIndex(rest); j != nil {
			rest = rest[:j[0]]
		}
		out[doc[m[2]:m[3]]] = rest
	}
	return out
}

// AC-16 (strengthened): every registered setting, in every group (SPEC-001,
// SPEC-002, SPEC-003), has its own entry in configuration.md that shows:
//   - for an ordinary setting, its flag form --<name>= and its environment
//     variable;
//   - for a secret, that a literal flag is not accepted, its file-reference
//     flag --<name>_file= and its CORRAL_..._FILE environment variable.
//
// A mention in a table alone does not satisfy it.
func TestAC16_EverySettingIsDocumented(t *testing.T) {
	doc := readDoc(t, "configuration.md")
	r := mustRegistry(t)
	if len(r.Settings()) == 0 {
		t.Fatal("empty registry")
	}
	ents := entries(doc)
	for _, s := range r.Settings() {
		body, ok := ents[s.Name]
		if !ok {
			t.Errorf("configuration.md has no entry (a heading with the setting name) for %s", s.Name)
			continue
		}
		if !strings.Contains(body, s.Env) {
			t.Errorf("the entry for %s does not show its environment variable %s", s.Name, s.Env)
		}
		if !s.Secret {
			if want := "--" + s.Name + "="; !strings.Contains(body, want) {
				t.Errorf("the entry for %s does not show its flag form %s", s.Name, want)
			}
			continue
		}
		if !strings.Contains(strings.ToLower(body), "not accepted") {
			t.Errorf("the entry for secret %s does not say that a literal flag is not accepted", s.Name)
		}
		if want := "--" + s.Name + "_file="; !strings.Contains(body, want) {
			t.Errorf("the entry for secret %s does not show %s", s.Name, want)
		}
		if !strings.Contains(body, s.FileEnv) {
			t.Errorf("the entry for secret %s does not show %s", s.Name, s.FileEnv)
		}
		if !strings.Contains(s.FileEnv, "_FILE") || !strings.Contains(s.FileFlag, "_file") {
			t.Errorf("unexpected file reference names for %s: %s, %s", s.Name, s.FileEnv, s.FileFlag)
		}
	}
	// Non-vacuity: the five SPEC-001 settings are among those checked, and
	// there are entries for settings of the SPEC-002 groups too.
	for _, n := range spec001Names {
		if _, ok := r.Lookup(n); !ok {
			t.Errorf("%s is not registered", n)
		}
	}
	if _, ok := r.Lookup("proxy.listen"); !ok || len(r.Settings()) < 30 {
		t.Errorf("the registry does not hold the SPEC-002 settings (%d settings)", len(r.Settings()))
	}
}

// FR-26: each required topic is present (keyword presence, review judges the
// content). Matching is case-insensitive.
func TestFR26_TopicsPresent(t *testing.T) {
	doc := strings.ToLower(readDoc(t, "configuration.md"))
	topics := []struct {
		topic string
		any   []string // at least one must appear
	}{
		{"FR-3 name mapping", []string{"corral_log_level"}},
		{"FR-4 precedence", []string{"precedence"}},
		{"FR-7 empty value rule", []string{"whitespace-only", "whitespace only"}},
		{"FR-7 flag asymmetry", []string{"--log.level="}},
		{"FR-7 whitespace set", []string{"u+00a0", "no-break space", "non-breaking space"}},
		{"FR-7 zero-width space", []string{"u+200b", "zero-width"}},
		{"FR-12 list trimming", []string{"comma-separated", "comma separated"}},
		{"FR-14 secret on command line", []string{"process list"}},
		{"FR-14 rotate", []string{"rotate"}},
		{"FR-15 file reference flag", []string{"_file"}},
		{"FR-15 size limit", []string{"64 kib", "65,536", "65536"}},
		{"FR-15 trailing newline", []string{"trailing newline", "trailing \\n"}},
		{"FR-17 restart", []string{"restart"}},
		{"FR-18 SIGHUP", []string{"sighup"}},
		{"FR-8 service links", []string{"enableservicelinks"}},
		{"FR-8 service link note", []string{"service link", "service-link", "corral_service_host"}},
		{"FR-21 help", []string{"--help"}},
		{"FR-22 version", []string{"--version"}},
		{"FR-23 check-config", []string{"--check-config"}},
		{"FR-22 build version", []string{"version="}},
		{"FR-24 exit codes", []string{"exit code"}},
		{"FR-24 Go runtime panic", []string{"panic"}},
		{"FR-27 combinations", []string{"--check-config=true"}},
		{"FR-19 OTEL pointer", []string{"observability.md"}},
	}
	for _, c := range topics {
		found := false
		for _, k := range c.any {
			if strings.Contains(doc, strings.ToLower(k)) {
				found = true
			}
		}
		if !found {
			t.Errorf("configuration.md lacks the topic %q (looked for any of %v)", c.topic, c.any)
		}
	}
}

// FR-24: all three exit codes are documented.
func TestFR26_ExitCodesDocumented(t *testing.T) {
	doc := readDoc(t, "configuration.md")
	for _, code := range []string{"`0`", "`1`", "`2`"} {
		if !strings.Contains(doc, code) {
			t.Errorf("configuration.md does not document exit code %s", code)
		}
	}
}

// Documentation compliance: examples use placeholders, never realistic
// credentials (org rule; SPEC-003 Compliance).
func TestFR26_NoRealisticCredentials(t *testing.T) {
	doc := readDoc(t, "configuration.md")
	for _, re := range []string{
		`AKIA[0-9A-Z]{16}`, `sk-[A-Za-z0-9]{20,}`, `sk-ant-`, `AIza[0-9A-Za-z_-]{35}`, `ya29\.`,
		`-----BEGIN [A-Z ]*PRIVATE KEY-----`, `(?i)bearer\s+[A-Za-z0-9._-]{20,}`, `AccountKey=`,
		`mongodb(\+srv)?://[^\s<]+:[^\s<@]+@`,
	} {
		if regexp.MustCompile(re).MatchString(doc) {
			t.Errorf("configuration.md matches the credential pattern %s", re)
		}
	}
}

// T15: observability.md no longer documents the interim flag syntax; it points
// to configuration.md and keeps the OTEL_* reference.
func TestObservabilityDocPointsToConfiguration(t *testing.T) {
	doc := readDoc(t, "observability.md")
	if strings.Contains(doc, "Interim flag syntax") {
		t.Error("observability.md still has the interim flag syntax section")
	}
	if !strings.Contains(doc, "configuration.md") {
		t.Error("observability.md does not point to configuration.md")
	}
	if !strings.Contains(doc, "OTEL_") {
		t.Error("observability.md lost the OTEL_* reference")
	}
}
