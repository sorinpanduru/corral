package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests are in package metrics because they need to extend the label
// name allowlist temporarily. They expect a package-level variable
//
//	var allowedLabelNames []string
//
// that AllowedLabelNames() copies and Lint consults. Not parallel: they
// mutate package state and restore it with t.Cleanup.

func withExtraLabelNames(t *testing.T, names ...string) {
	t.Helper()
	saved := allowedLabelNames
	allowedLabelNames = append(append([]string(nil), saved...), names...)
	t.Cleanup(func() { allowedLabelNames = saved })
}

func oneLabelCatalog(t *testing.T, typ, name string, label string, values []string) *Catalog {
	t.Helper()
	var vs []string
	for _, v := range values {
		vs = append(vs, fmt.Sprintf("%q", v))
	}
	doc := fmt.Sprintf(`{"metrics":[{"name":%q,"type":%q,"help":"h","labels":[{"name":%q,"values":[%s]}]}]}`,
		name, typ, label, strings.Join(vs, ","))
	c, err := ParseCatalog([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func probs(ps []Problem) string {
	var sb strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&sb, "%v\n", p)
	}
	return sb.String()
}

// ADR-0004 second line of defence: if the allowlist is ever extended with a
// forbidden name, the deny-list still reports it.
func TestDenyListStillAppliesWhenAllowlistIsExtended(t *testing.T) {
	for _, name := range []string{"user_kind", "db", "client_ip", "collection", "trace_id", "app_name", "code"} {
		t.Run(name, func(t *testing.T) {
			withExtraLabelNames(t, name)
			ps := Lint(oneLabelCatalog(t, "counter", "corral_x_total", name, []string{"v"}))
			if len(ps) == 0 {
				t.Fatalf("forbidden name %q accepted once allowlisted", name)
			}
			if !strings.Contains(probs(ps), name) {
				t.Fatalf("problems do not mention %q:\n%s", name, probs(ps))
			}
		})
	}
}

// Control: the extension mechanism itself works for a harmless name, so the
// previous test is not passing merely because the allowlist rejects.
func TestExtendedAllowlistAcceptsHarmlessName(t *testing.T) {
	withExtraLabelNames(t, "pool_kind")
	if ps := Lint(oneLabelCatalog(t, "counter", "corral_x_total", "pool_kind", []string{"a", "b_1", "9"})); len(ps) != 0 {
		t.Fatalf("harmless extended name rejected:\n%s", probs(ps))
	}
}

// Other allowlisted labels (none today) use ^[a-z0-9_]+$ for values.
func TestOtherAllowlistedLabelValueCharset(t *testing.T) {
	withExtraLabelNames(t, "pool_kind")
	for _, v := range []string{"", "Upper", "a-b", "a b", "a.b", "a/b", "caf\u00e9", "a\n"} {
		if ps := Lint(oneLabelCatalog(t, "counter", "corral_x_total", "pool_kind", []string{"ok", v})); len(ps) == 0 {
			t.Errorf("value %q accepted", v)
		}
	}
}

func twoLabelCatalog(t *testing.T, typ string, na, nb int) *Catalog {
	t.Helper()
	gen := func(p string, n int) string {
		var vs []string
		for i := 0; i < n; i++ {
			vs = append(vs, fmt.Sprintf("%q", fmt.Sprintf("%s%d", p, i)))
		}
		return strings.Join(vs, ",")
	}
	labels := fmt.Sprintf(`{"name":"pool_kind","values":[%s]}`, gen("a", na))
	if nb > 0 {
		labels += fmt.Sprintf(`,{"name":"pool_mode","values":[%s]}`, gen("b", nb))
	}
	name := "corral_big_total"
	if typ != "counter" {
		name = "corral_big_seconds"
	}
	c, err := ParseCatalog([]byte(fmt.Sprintf(`{"metrics":[{"name":%q,"type":%q,"help":"h","labels":[%s]}]}`, name, typ, labels)))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// FR-13: series cap. Needs extra allowlisted labels, since the real
// allowlist cannot reach 100 series with valid values.
func TestSeriesCap(t *testing.T) {
	withExtraLabelNames(t, "pool_kind", "pool_mode")
	if ps := Lint(twoLabelCatalog(t, "counter", 10, 10)); len(ps) != 0 {
		t.Fatalf("100 series rejected:\n%s", probs(ps))
	}
	if ps := Lint(twoLabelCatalog(t, "counter", 100, 0)); len(ps) != 0 {
		t.Fatalf("100 series (single label) rejected:\n%s", probs(ps))
	}
	if ps := Lint(twoLabelCatalog(t, "counter", 101, 0)); len(ps) == 0 {
		t.Fatal("101 series accepted")
	}
	if ps := Lint(twoLabelCatalog(t, "counter", 10, 11)); len(ps) == 0 {
		t.Fatal("110 series (cartesian product) accepted")
	}
	// le is excluded from the count: a histogram at 100 label combinations passes.
	if ps := Lint(twoLabelCatalog(t, "histogram", 10, 10)); len(ps) != 0 {
		t.Fatalf("histogram at 100 series rejected:\n%s", probs(ps))
	}
}

// Fixtures in testdata for the cap (kept for review readability); they use
// labels "a" and "b", which are not allowlisted, so here they are extended.
func TestSeriesCapFixtures(t *testing.T) {
	withExtraLabelNames(t, "a", "b")
	load := func(f string) *Catalog {
		b, err := readTestdata(f)
		if err != nil {
			t.Fatal(err)
		}
		c, err := ParseCatalog(b)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	if ps := Lint(load("good_max_series.json")); len(ps) != 0 {
		t.Fatalf("good_max_series.json:\n%s", probs(ps))
	}
	for _, f := range []string{"bad_series_cap_single.json", "bad_series_cap_product.json"} {
		ps := Lint(load(f))
		if len(ps) == 0 || !strings.Contains(probs(ps), "corral_big_total") {
			t.Errorf("%s: want a problem naming corral_big_total, got:\n%s", f, probs(ps))
		}
	}
}

func readTestdata(name string) ([]byte, error) { return os.ReadFile(filepath.Join("testdata", name)) }
