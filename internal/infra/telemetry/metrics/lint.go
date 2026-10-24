package metrics

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
)

// Label names carrying the FR-29 error class and detail (decision D4).
const (
	LabelErrorClass  = "error_class"
	LabelErrorDetail = "error_detail"
)

// maxSeries is the FR-13 cap on the cartesian product of label values.
const maxSeries = 100

// Problem is one lint violation.
type Problem struct {
	Metric string // metric name (may be empty for catalog-level problems)
	Label  string // label name, empty for metric-level problems
	Msg    string
}

// String renders "metric <name>[ label <name>]: <msg>".
func (p Problem) String() string {
	var sb strings.Builder
	if p.Metric != "" {
		sb.WriteString("metric " + p.Metric)
	} else {
		sb.WriteString("catalog")
	}
	if p.Label != "" {
		sb.WriteString(" label " + p.Label)
	}
	sb.WriteString(": " + p.Msg)
	return sb.String()
}

var snakeRE = regexp.MustCompile(`^[a-z][a-z0-9]*(_[a-z0-9]+)*$`)

var nonBaseUnits = []string{
	"ms", "milliseconds", "us", "microseconds", "ns", "nanoseconds",
	"minutes", "hours", "days", "kb", "mb", "gb", "tb", "kib", "mib", "gib", "tib",
	"kilobytes", "megabytes", "gigabytes", "percent",
}

var (
	forbiddenExact = []string{
		"address", "ip", "host", "port", "id", "lsid", "app_name", "appname",
		"database", "db", "namespace", "collection", "code", "code_name", "codename",
	}
	forbiddenSuffix = []string{"_address", "_ip", "_host", "_port", "_id"}
	forbiddenPrefix = []string{
		"session", "cursor", "request", "connection", "driver", "user", "trace", "span",
	}
)

func forbiddenLabel(name string) bool {
	if slices.Contains(forbiddenExact, name) {
		return true
	}
	for _, s := range forbiddenSuffix {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	for _, p := range forbiddenPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Lint returns every rule violation in the catalog (empty when clean).
func Lint(c *Catalog) []Problem {
	var ps []Problem
	if c == nil {
		return []Problem{{Msg: "catalog is nil"}}
	}
	classes := map[string]bool{}
	for _, v := range errclass.Classes() {
		classes[string(v)] = true
	}
	details := map[string]bool{}
	for _, v := range errclass.Details() {
		details[string(v)] = true
	}
	seen := map[string]bool{}
	for _, m := range c.Metrics {
		add := func(label, format string, args ...any) {
			ps = append(ps, Problem{Metric: m.Name, Label: label, Msg: fmt.Sprintf(format, args...)})
		}
		if seen[m.Name] {
			add("", "duplicate metric name")
		}
		seen[m.Name] = true
		if !strings.HasPrefix(m.Name, "corral_") {
			add("", "name must start with corral_ (FR-10)")
		}
		if !snakeRE.MatchString(m.Name) {
			add("", "name must be snake_case (FR-10)")
		}
		base := strings.TrimSuffix(m.Name, "_total")
		for _, u := range nonBaseUnits {
			if strings.HasSuffix(base, "_"+u) {
				add("", "non-base unit suffix _%s, use base units (FR-10)", u)
				break
			}
		}
		switch m.Type {
		case TypeCounter:
			if !strings.HasSuffix(m.Name, "_total") {
				add("", "counter name must end in _total (FR-10)")
			}
		case TypeGauge, TypeHistogram:
			if strings.HasSuffix(m.Name, "_total") {
				add("", "_total is only allowed on counters (FR-10)")
			}
		default:
			add("", "unknown type %q, want counter, gauge or histogram", string(m.Type))
		}
		if strings.TrimSpace(m.Help) == "" {
			add("", "help must not be empty (FR-10)")
		}
		if len(m.Buckets) > 0 {
			if m.Type != TypeHistogram {
				add("", "buckets are only allowed on histograms")
			}
			for i := 1; i < len(m.Buckets); i++ {
				if m.Buckets[i] <= m.Buckets[i-1] {
					add("", "buckets must be strictly increasing")
					break
				}
			}
		}

		series := 1
		emptySet := false
		labelSeen := map[string]bool{}
		for _, l := range m.Labels {
			ladd := func(format string, args ...any) { add(l.Name, format, args...) }
			if labelSeen[l.Name] {
				ladd("duplicate label name")
			}
			labelSeen[l.Name] = true
			if !snakeRE.MatchString(l.Name) {
				ladd("label name must be snake_case (FR-10)")
			}
			if l.Name == "le" || l.Name == "quantile" {
				ladd("label name is reserved and must not be declared (FR-13)")
			}
			if forbiddenLabel(l.Name) {
				ladd("forbidden label name (FR-11)")
			}
			if len(l.Values) == 0 {
				ladd("label must declare a non-empty value set (FR-11, FR-31)")
				emptySet = true
			}
			switch l.Name {
			case LabelErrorClass:
				for _, v := range l.Values {
					if !classes[v] {
						ladd("value %q is outside the FR-29 error class enum", v)
					}
				}
			case LabelErrorDetail:
				for _, v := range l.Values {
					if !details[v] {
						ladd("value %q is outside the FR-29 error detail enum", v)
					}
				}
			}
			dup := map[string]bool{}
			for _, v := range l.Values {
				if dup[v] {
					ladd("duplicate value %q", v)
				}
				dup[v] = true
			}
			if n := len(l.Values); n > 0 && series <= maxSeries*maxSeries {
				series *= n
			}
		}
		if !emptySet && series > maxSeries {
			add("", "series cap exceeded: label value product exceeds %d (FR-13)", maxSeries)
		}
	}
	return ps
}
