package metrics_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
)

// Expected API (plans/SPEC-001.md T7, ADR-0004). Beyond the plan's named
// interfaces these tests rely on: Catalog.Metrics []MetricDef; MetricDef.Name
// and MetricDef.Labels []LabelDef; LabelDef.Name and LabelDef.Values
// []string. Type, Help and Problem are only inspected through fmt (%v), so
// their concrete Go types are free.

type labelSpec struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

type metricSpec struct {
	Name    string      `json:"name"`
	Type    string      `json:"type"`
	Help    string      `json:"help"`
	Labels  []labelSpec `json:"labels"`
	Buckets []float64   `json:"buckets,omitempty"`
}

// catalogJSON renders a synthetic catalog document.
func catalogJSON(t testing.TB, ms ...metricSpec) []byte {
	t.Helper()
	for i := range ms {
		if ms[i].Labels == nil {
			ms[i].Labels = []labelSpec{}
		}
	}
	b, err := json.Marshal(map[string]any{"metrics": ms})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parse(t testing.TB, ms ...metricSpec) *metrics.Catalog {
	t.Helper()
	c, err := metrics.ParseCatalog(catalogJSON(t, ms...))
	if err != nil {
		t.Fatalf("ParseCatalog: %v", err)
	}
	return c
}

func parseFile(t testing.TB, name string) *metrics.Catalog {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	c, err := metrics.ParseCatalog(b)
	if err != nil {
		t.Fatalf("ParseCatalog(%s): %v", name, err)
	}
	return c
}

func problemText(ps []metrics.Problem) string {
	var sb strings.Builder
	for _, p := range ps {
		fmt.Fprintf(&sb, "%v\n", p)
	}
	return sb.String()
}

func seq(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%d", prefix, i)
	}
	return out
}
