package metrics_test

import (
	"fmt"
	"os"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

const droppedHelp = "Spans dropped because the trace export queue was full."

// FR-31, FR-37: the embedded catalog holds the spans-dropped counter unchanged
// (SPEC-002 adds 23 more metrics; see catalog_spec002_test.go).
func TestEmbeddedCatalogContainsSpansDropped(t *testing.T) {
	c, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(c.Metrics) != 24 {
		t.Fatalf("embedded catalog has %d entries, want 24 (1 + 23 from SPEC-002)", len(c.Metrics))
	}
	var m *metrics.MetricDef
	for i := range c.Metrics {
		if c.Metrics[i].Name == "corral_tracing_spans_dropped_total" {
			m = &c.Metrics[i]
		}
	}
	if m == nil {
		t.Fatal("spans-dropped counter missing")
	}
	if metrics.NameTracingSpansDropped != "corral_tracing_spans_dropped_total" {
		t.Errorf("NameTracingSpansDropped = %q", metrics.NameTracingSpansDropped)
	}
	if got := fmt.Sprintf("%v", m.Type); got != "counter" {
		t.Errorf("type = %q, want counter", got)
	}
	if got := fmt.Sprintf("%v", m.Help); got != droppedHelp {
		t.Errorf("help = %q, want %q", got, droppedHelp)
	}
	if len(m.Labels) != 0 {
		t.Errorf("labels = %v, want none (FR-37)", m.Labels)
	}
}

// AC-8: the shipped catalog itself passes lint (a bad catalog cannot ship).
func TestEmbeddedCatalogPassesLint(t *testing.T) {
	c, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if ps := metrics.Lint(c); len(ps) != 0 {
		t.Fatalf("embedded catalog has lint problems:\n%s", problemText(ps))
	}
}

// FR-31: the embedded copy is the on-disk catalog.json (ADR-0004 file path).
func TestEmbeddedCatalogMatchesFile(t *testing.T) {
	b, err := os.ReadFile("catalog.json")
	if err != nil {
		t.Fatalf("catalog.json must exist next to the package: %v", err)
	}
	fromFile, err := metrics.ParseCatalog(b)
	if err != nil {
		t.Fatalf("catalog.json does not parse: %v", err)
	}
	emb, err := metrics.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(fromFile.Metrics) != len(emb.Metrics) || fromFile.Metrics[0].Name != emb.Metrics[0].Name {
		t.Fatalf("embedded catalog differs from catalog.json")
	}
}

// ADR-0004: unknown fields and malformed JSON are parse errors.
func TestParseCatalogRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"malformed":            `{"metrics": [`,
		"empty":                ``,
		"unknown top field":    `{"metrics":[],"extra":1}`,
		"unknown metric field": `{"metrics":[{"name":"corral_x_total","type":"counter","help":"h","labels":[],"unit":"s"}]}`,
		"unknown label field":  `{"metrics":[{"name":"corral_x_total","type":"counter","help":"h","labels":[{"name":"a","values":["x"],"extra":true}]}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := metrics.ParseCatalog([]byte(doc)); err == nil {
				t.Fatalf("ParseCatalog(%q) succeeded, want error", doc)
			}
		})
	}
}

// ADR-0004: type is counter, gauge or histogram. Anything else is rejected,
// at parse time or by lint.
func TestUnknownTypeIsRejected(t *testing.T) {
	doc := []byte(`{"metrics":[{"name":"corral_x_seconds","type":"summary","help":"h","labels":[]}]}`)
	c, err := metrics.ParseCatalog(doc)
	if err != nil {
		return
	}
	if len(metrics.Lint(c)) == 0 {
		t.Fatal("type summary accepted by both parser and lint")
	}
}

// ADR-0004: all three types and histogram buckets parse.
func TestParseCatalogAcceptsAllTypes(t *testing.T) {
	c := parse(t,
		metricSpec{Name: "corral_a_total", Type: "counter", Help: "h"},
		metricSpec{Name: "corral_b", Type: "gauge", Help: "h"},
		metricSpec{Name: "corral_c_seconds", Type: "histogram", Help: "h", Buckets: []float64{0.1, 1, 10}},
	)
	if len(c.Metrics) != 3 {
		t.Fatalf("got %d metrics", len(c.Metrics))
	}
}
