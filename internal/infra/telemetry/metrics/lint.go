package metrics

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
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
	"millis", "sec", "secs", "minute", "hour", "day", "bits", "bit",
	"millisecond", "microsecond", "nanosecond", "kilobyte", "megabyte", "gigabyte",
}

// allowedLabelNames is the closed allowlist of label names (FR-11): every
// other name is rejected, which subsumes the FR-11 deny patterns.
var allowedLabelNames = []string{
	"command", LabelErrorClass, LabelErrorDetail,
	"outcome", "reason", "side", "state", "opcode", "kind",
}

// AllowedLabelNames returns a copy of the label name allowlist.
func AllowedLabelNames() []string { return slices.Clone(allowedLabelNames) }

// forbiddenTokens are whole underscore-separated tokens (or token prefixes,
// see forbiddenLabel) that may never appear in a label name (FR-11). This is
// a second line of defence that applies even to allowlisted names.
var forbiddenTokens = []string{
	"address", "ip", "host", "port", "id", "lsid", "session", "cursor", "request",
	"connection", "appname", "database", "db", "namespace", "collection",
	"code", "codename",
}

var forbiddenTokenPrefixes = []string{"driver", "user", "trace", "span"}

func forbiddenLabel(name string) bool {
	if name == "app_name" {
		return true
	}
	for _, tok := range strings.Split(name, "_") {
		if slices.Contains(forbiddenTokens, tok) {
			return true
		}
		for _, p := range forbiddenTokenPrefixes {
			if strings.HasPrefix(tok, p) {
				return true
			}
		}
	}
	return false
}

var valueRE = regexp.MustCompile(`^[a-z0-9_]+$`)

// Lint returns every rule violation in the catalog (empty when clean).
func Lint(c *Catalog) []Problem {
	if c == nil {
		return []Problem{{Msg: "catalog is nil"}}
	}
	var ps []Problem
	seen := map[string]bool{}
	for _, m := range c.Metrics {
		if seen[m.Name] {
			ps = append(ps, Problem{Metric: m.Name, Msg: "duplicate metric name"})
		}
		seen[m.Name] = true
		ps = append(ps, lintName(m)...)
		ps = append(ps, lintLabels(m)...)
	}
	return ps
}

// lintName checks the metric-level rules: name, type, help, buckets.
func lintName(m MetricDef) []Problem {
	var ps []Problem
	add := func(format string, args ...any) {
		ps = append(ps, Problem{Metric: m.Name, Msg: fmt.Sprintf(format, args...)})
	}
	if !strings.HasPrefix(m.Name, "corral_") {
		add("name must start with corral_ (FR-10)")
	}
	if !snakeRE.MatchString(m.Name) {
		add("name must be snake_case (FR-10)")
	}
	base := strings.TrimSuffix(m.Name, "_total")
	for _, u := range nonBaseUnits {
		if strings.HasSuffix(base, "_"+u) {
			add("non-base unit suffix _%s, use base units (FR-10)", u)
			break
		}
	}
	switch m.Type {
	case TypeCounter:
		if !strings.HasSuffix(m.Name, "_total") {
			add("counter name must end in _total (FR-10)")
		}
	case TypeGauge, TypeHistogram:
		if strings.HasSuffix(m.Name, "_total") {
			add("_total is only allowed on counters (FR-10)")
		}
	default:
		add("unknown type %q, want counter, gauge or histogram", string(m.Type))
	}
	if strings.TrimSpace(m.Help) == "" {
		add("help must not be empty (FR-10)")
	}
	if len(m.Buckets) > 0 {
		if m.Type != TypeHistogram {
			add("buckets are only allowed on histograms")
		}
		for i := 1; i < len(m.Buckets); i++ {
			if m.Buckets[i] <= m.Buckets[i-1] {
				add("buckets must be strictly increasing")
				break
			}
		}
	}
	return ps
}

// lintLabels checks label names, values and the FR-13 series cap.
func lintLabels(m MetricDef) []Problem {
	var ps []Problem
	emptySet := false
	series := 1
	labelSeen := map[string]bool{}
	for _, l := range m.Labels {
		ps = append(ps, lintLabel(m.Name, l, labelSeen[l.Name])...)
		labelSeen[l.Name] = true
		if len(l.Values) == 0 {
			emptySet = true
			continue
		}
		// Saturate at maxSeries+1 so the product cannot overflow; only
		// "above the cap" matters.
		series = min(series*len(l.Values), maxSeries+1)
	}
	if !emptySet && series > maxSeries {
		ps = append(ps, Problem{Metric: m.Name, Msg: fmt.Sprintf("series cap exceeded: label value product exceeds %d (FR-13)", maxSeries)})
	}
	return ps
}

// lintLabel checks one label: name allowlist and value rules. Values are
// rendered with %q so hostile characters stay escaped.
func lintLabel(metric string, l LabelDef, duplicate bool) []Problem {
	var ps []Problem
	add := func(format string, args ...any) {
		ps = append(ps, Problem{Metric: metric, Label: l.Name, Msg: fmt.Sprintf(format, args...)})
	}
	if duplicate {
		add("duplicate label name")
	}
	if l.Name == "le" || l.Name == "quantile" {
		add("label name is reserved and must not be declared (FR-13)")
	}
	if forbiddenLabel(l.Name) {
		add("forbidden label name (FR-11)")
	}
	if !slices.Contains(allowedLabelNames, l.Name) {
		add("label name is not in the allowlist (FR-11)")
	}
	if len(l.Values) == 0 {
		add("label must declare a non-empty value set (FR-11, FR-31)")
	}
	dup := map[string]bool{}
	for _, v := range l.Values {
		if dup[v] {
			add("duplicate value %q", v)
		}
		dup[v] = true
		switch l.Name {
		case LabelErrorClass:
			if !errclassHas(v) {
				add("value %q is outside the FR-29 error class enum", v)
			}
		case LabelErrorDetail:
			if !detailHas(v) {
				add("value %q is outside the FR-29 error detail enum", v)
			}
		case "command":
			if v != sanitize.OtherCommand && !slices.Contains(sanitize.CommandAllowlist(), v) {
				add("value %q is not in the command allowlist (FR-12)", v)
			}
		default:
			if !valueRE.MatchString(v) {
				add("value %q must be non-empty and match ^[a-z0-9_]+$", v)
			} else if errclassHas(v) || detailHas(v) {
				// Amendment 3 (FR-11): outcome, reason, state, side, opcode
				// and kind hold non-error categories, never FR-29 values.
				add("value %q equals an FR-29 class or detail; use error_class or error_detail", v)
			}
		}
	}
	return ps
}

func errclassHas(v string) bool {
	for _, c := range errclass.Classes() {
		if string(c) == v {
			return true
		}
	}
	return false
}

func detailHas(v string) bool {
	for _, d := range errclass.Details() {
		if string(d) == v {
			return true
		}
	}
	return false
}
