package metrics_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/sorinpanduru/corral/internal/infra/telemetry/errclass"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/metrics"
	"github.com/sorinpanduru/corral/internal/infra/telemetry/sanitize"
)

// Each testdata catalog breaks exactly one rule. Lint must report it and the
// report must name the offender (metric name, or label name for label rules).
// AC-8 unless noted.
func TestLintBadCatalogs(t *testing.T) {
	cases := []struct {
		file    string
		mention string // must appear in the rendered problem list
		rule    string
	}{
		{"bad_prefix.json", "pool_requests_total", "FR-10 corral_ prefix"},
		{"bad_not_snake.json", "corral_PoolRequests_total", "FR-10 snake_case"},
		{"bad_unit_ms.json", "corral_op_duration_ms", "FR-10 non-base unit"},
		{"bad_unit_mb.json", "corral_buffer_mb", "FR-10 non-base unit"},
		{"bad_counter_no_total.json", "corral_things", "FR-10 counter needs _total"},
		{"bad_gauge_total.json", "corral_things_total", "FR-10 _total only on counters"},
		{"bad_histogram_total.json", "corral_op_seconds_total", "FR-10 _total only on counters"},
		{"bad_empty_help.json", "corral_things_total", "FR-10 help required"},
		{"bad_label_not_snake.json", "PoolState", "FR-10 label snake_case"},
		{"bad_label_forbidden_address.json", "client_address", "FR-11 forbidden label"},
		{"bad_label_forbidden_id.json", "session_id", "FR-11 forbidden label"},
		{"bad_label_forbidden_appname.json", "app_name", "FR-11 forbidden label"},
		{"bad_label_forbidden_collection.json", "collection", "FR-11 forbidden label"},
		{"bad_label_forbidden_code.json", "code_name", "FR-11 forbidden label"},
		{"bad_label_empty_values.json", "command", "FR-11/FR-31 label needs enumerated values"},
		{"bad_label_value_empty.json", "command", "D: label value must be non-empty"},
		{"bad_label_value_uppercase.json", "Find", "D: label value must match ^[a-z0-9_]+$"},
		{"bad_label_value_chars.json", "find-one", "D: label value must match ^[a-z0-9_]+$"},
		{"bad_label_not_allowlisted.json", "db_name", "D: label name allowlist"},
		{"bad_label_le.json", "le", "FR-13 le not declarable"},
		{"bad_label_quantile.json", "quantile", "FR-13 quantile not declarable"},
		{"bad_error_class_values.json", "bogus_class", "FR-29 class enum"},
		{"bad_error_detail_values.json", "ECONNFOO", "FR-29 detail enum"},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			ps := metrics.Lint(parseFile(t, tc.file))
			if len(ps) == 0 {
				t.Fatalf("Lint reported nothing for %s (%s)", tc.file, tc.rule)
			}
			if txt := problemText(ps); !strings.Contains(txt, tc.mention) {
				t.Fatalf("problems do not mention %q (%s):\n%s", tc.mention, tc.rule, txt)
			}
		})
	}
}

// FR-10: a well-formed catalog of every type, with base-unit suffixes, passes.
func TestLintAcceptsValidCatalog(t *testing.T) {
	c := parse(t,
		metricSpec{Name: "corral_a_total", Type: "counter", Help: "A counter.",
			Labels: []labelSpec{{Name: "command", Values: []string{"find", "hello", "other"}}}},
		metricSpec{Name: "corral_b", Type: "gauge", Help: "A gauge."},
		metricSpec{Name: "corral_c_seconds", Type: "histogram", Help: "A histogram."},
		metricSpec{Name: "corral_d_bytes", Type: "gauge", Help: "Bytes."},
		// Class and detail are declared on separate metrics: independent label
		// value lists multiply (7 x 28 = 196 series would exceed the FR-13 cap
		// of 100), and every class and every detail must still be accepted.
		metricSpec{Name: "corral_e_total", Type: "counter", Help: "Errors by class.", Labels: []labelSpec{
			{Name: "error_class", Values: classValues()},
		}},
		metricSpec{Name: "corral_f_total", Type: "counter", Help: "Errors by detail.", Labels: []labelSpec{
			{Name: "error_detail", Values: detailValues()},
		}},
	)
	if ps := metrics.Lint(c); len(ps) != 0 {
		t.Fatalf("unexpected problems:\n%s", problemText(ps))
	}
}

// FR-10: the non-base unit suffix ban covers the plan's list.
func TestLintNonBaseUnitSuffixes(t *testing.T) {
	for _, s := range []string{"ms", "milliseconds", "us", "microseconds", "ns", "nanoseconds",
		"minutes", "hours", "kb", "mb", "gb", "kib", "mib", "gib", "kilobytes", "megabytes", "percent",
		"millis", "sec", "secs", "minute", "hour", "day", "days", "bits", "bit",
		"millisecond", "microsecond", "nanosecond", "kilobyte", "megabyte", "gigabyte"} {
		t.Run(s, func(t *testing.T) {
			name := "corral_thing_" + s
			ps := metrics.Lint(parse(t, metricSpec{Name: name, Type: "gauge", Help: "h"}))
			if !strings.Contains(problemText(ps), name) {
				t.Fatalf("suffix _%s not flagged:\n%s", s, problemText(ps))
			}
		})
	}
}

// FR-11, AC-8: forbidden label names, including the pattern families.
func TestLintForbiddenLabels(t *testing.T) {
	forbidden := []string{
		"address", "client_address", "remote_address", "ip", "client_ip", "host", "upstream_host",
		"port", "upstream_port", "id", "connection_id", "session_id", "request_id", "cursor_id",
		"lsid", "session", "cursor", "request", "connection",
		"app_name", "appname", "driver", "driver_name", "driver_version",
		"user", "username", "user_name", "database", "db", "namespace", "collection",
		"code", "code_name", "codename", "trace", "trace_id", "span", "span_id",
	}
	for _, l := range forbidden {
		t.Run(l, func(t *testing.T) {
			c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
				Labels: []labelSpec{{Name: l, Values: []string{"v"}}}})
			if txt := problemText(metrics.Lint(c)); !strings.Contains(txt, l) {
				t.Fatalf("label %q not flagged:\n%s", l, txt)
			}
		})
	}
}

// FR-11: allowlisted labels with valid values are accepted, and none of the
// allowlisted names is on the deny-list (second line of defence, ADR-0004).
func TestLintAllowlistedNamesAreNotForbidden(t *testing.T) {
	valid := map[string]string{"command": "find", "error_class": "timeout", "error_detail": "eof"}
	for _, l := range metrics.AllowedLabelNames() {
		t.Run(l, func(t *testing.T) {
			v, ok := valid[l]
			if !ok {
				v = "v"
			}
			c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
				Labels: []labelSpec{{Name: l, Values: []string{v}}}})
			if ps := metrics.Lint(c); len(ps) != 0 {
				t.Fatalf("allowlisted label %q flagged (forbidden or invalid):\n%s", l, problemText(ps))
			}
		})
	}
}

// FR-29, AC-8: every class value is accepted, one by one; a value outside is not.
func TestLintErrorClassEnum(t *testing.T) {
	for _, v := range classValues() {
		c := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "error_class", Values: []string{v}}}})
		if ps := metrics.Lint(c); len(ps) != 0 {
			t.Errorf("class %q rejected:\n%s", v, problemText(ps))
		}
	}
	for _, v := range []string{"Timeout", "TIMEOUT", "other", "ECONNRESET", "", " timeout"} {
		c := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "error_class", Values: []string{"timeout", v}}}})
		if ps := metrics.Lint(c); len(ps) == 0 {
			t.Errorf("class value %q accepted", v)
		}
	}
}

// FR-29, FR-11, AC-8: every detail value is accepted; a value outside is not.
// Class names are not details and vice versa.
func TestLintErrorDetailEnum(t *testing.T) {
	for _, v := range detailValues() {
		c := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "error_detail", Values: []string{v}}}})
		if ps := metrics.Lint(c); len(ps) != 0 {
			t.Errorf("detail %q rejected:\n%s", v, problemText(ps))
		}
	}
	for _, v := range []string{"econnreset", "ECONNFOO", "timeout", "EOF", "", "Other"} {
		c := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "error_detail", Values: []string{"eof", v}}}})
		if ps := metrics.Lint(c); len(ps) == 0 {
			t.Errorf("detail value %q accepted", v)
		}
	}
}

// D4/ADR-0004: the enum checks bind to the exact label names error_class and
// error_detail, and the old span/log key is not a valid snake_case label name.
func TestLintDottedErrorDetailKeyIsNotALabel(t *testing.T) {
	c := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
		Labels: []labelSpec{{Name: "corral.error.detail", Values: []string{"eof"}}}})
	if ps := metrics.Lint(c); len(ps) == 0 {
		t.Fatal("label name corral.error.detail accepted")
	}
}

// AC-8: Lint returns all violations, not just the first.
func TestLintReportsAllProblems(t *testing.T) {
	c := parse(t,
		metricSpec{Name: "bad_one_total", Type: "counter", Help: "h"},
		metricSpec{Name: "corral_two_ms", Type: "gauge", Help: ""},
		metricSpec{Name: "corral_three_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "user", Values: []string{"x"}}}},
	)
	ps := metrics.Lint(c)
	if len(ps) < 4 { // prefix; unit + help; forbidden label
		t.Fatalf("got %d problems, want at least 4:\n%s", len(ps), problemText(ps))
	}
	txt := problemText(ps)
	for _, want := range []string{"bad_one_total", "corral_two_ms", "corral_three_total"} {
		if !strings.Contains(txt, want) {
			t.Errorf("problems do not mention %s:\n%s", want, txt)
		}
	}
}

func classValues() []string {
	var out []string
	for _, c := range errclass.Classes() {
		out = append(out, string(c))
	}
	return out
}

func detailValues() []string {
	var out []string
	for _, d := range errclass.Details() {
		out = append(out, string(d))
	}
	return out
}

// D (Amendment 3, AC-41): label names are an allowlist of exactly nine names.
func TestAllowedLabelNames(t *testing.T) {
	got := append([]string(nil), metrics.AllowedLabelNames()...)
	sort.Strings(got)
	want := []string{"command", "error_class", "error_detail", "kind", "opcode", "outcome", "reason", "side", "state"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AllowedLabelNames() = %v, want %v", got, want)
	}
	// The returned slice must be a copy.
	a := metrics.AllowedLabelNames()
	a[0] = "evil"
	for _, n := range metrics.AllowedLabelNames() {
		if n == "evil" {
			t.Fatal("AllowedLabelNames exposes internal state")
		}
	}
}

// D: any label name outside the allowlist is rejected, including near-misses
// of forbidden names that a deny-list would miss.
func TestLintRejectsNonAllowlistedLabelNames(t *testing.T) {
	for _, l := range []string{
		"db_name", "database_name", "collection_name", "coll", "ns", "error_code", "err_code",
		"client_addr", "peer", "remote_addr", "upstream", "server", "hostname", "app", "application",
		"client_app_name", "role", "error", "error_type", "err_class", "detail",
		"connection_state", "session_state", "result", "pool", "a",
	} {
		t.Run(l, func(t *testing.T) {
			c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
				Labels: []labelSpec{{Name: l, Values: []string{"v"}}}})
			if txt := problemText(metrics.Lint(c)); !strings.Contains(txt, l) {
				t.Fatalf("label %q not rejected:\n%s", l, txt)
			}
		})
	}
}

// D: command label values are members of sanitize.CommandAllowlist() or
// "other", case-sensitive.
func TestLintCommandValuesAreAllowlisted(t *testing.T) {
	for _, v := range append(sanitize.CommandAllowlist(), sanitize.OtherCommand, "find", "hello", "other") {
		c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "command", Values: []string{v}}}})
		if ps := metrics.Lint(c); len(ps) != 0 {
			t.Errorf("command value %q rejected:\n%s", v, problemText(ps))
		}
	}
	for _, v := range []string{"", "Find", "FIND", "Hello", "getmore", "GetMore", "Insert", "find-one", "a b", "find\n", "ismaster", "isMaster", "collMod"} {
		c := parse(t, metricSpec{Name: "corral_x_total", Type: "counter", Help: "h",
			Labels: []labelSpec{{Name: "command", Values: []string{"find", v}}}})
		if ps := metrics.Lint(c); len(ps) == 0 {
			t.Errorf("command value %q accepted", v)
		}
	}
}

// D: error_detail keeps its uppercase errno values (enum-governed), while
// non-enum values are rejected even when they match the charset.
func TestLintErrorDetailUppercaseAllowedButEnumBound(t *testing.T) {
	ok := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
		Labels: []labelSpec{{Name: "error_detail", Values: []string{"ECONNREFUSED", "EADDRINUSE"}}}})
	if ps := metrics.Lint(ok); len(ps) != 0 {
		t.Fatalf("errno detail rejected:\n%s", problemText(ps))
	}
	bad := parse(t, metricSpec{Name: "corral_e_total", Type: "counter", Help: "h",
		Labels: []labelSpec{{Name: "error_detail", Values: []string{"not_in_enum"}}}})
	if ps := metrics.Lint(bad); len(ps) == 0 {
		t.Fatal("charset-valid but non-enum detail accepted")
	}
}
