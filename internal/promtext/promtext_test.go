package promtext

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	good := `# HELP a_total Things. With \\ and \n.
# TYPE a_total counter
a_total{x="1",y="q\"uo\\te\n"} 3
a_total{x="2"} 4 1700000000000
# just a comment
# HELP h_seconds A histogram.
# TYPE h_seconds histogram
h_seconds_bucket{r="a",le="0.1"} 1
h_seconds_bucket{r="a",le="+Inf"} 2
h_seconds_sum{r="a"} 0.5
h_seconds_count{r="a"} 2
# TYPE g gauge
g NaN
`
	samples, err := Validate(good)
	if err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	if len(samples) != 7 || samples[0].Labels["y"] != "q\"uo\\te\n" || samples[1].Value != 4 {
		t.Errorf("samples: %+v", samples)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"duplicate series":       {"# TYPE a gauge\na{s=\"running\"} 1\na{s=\"running\"} 0\n", "duplicate series"},
		"duplicate, other order": {"a{x=\"1\",y=\"2\"} 1\na{y=\"2\",x=\"1\"} 1\n", "duplicate series"},
		"second TYPE":            {"# TYPE a gauge\na 1\n# TYPE a gauge\n", "second TYPE"},
		"second HELP":            {"# HELP a x\n# HELP a y\na 1\n", "second HELP"},
		"TYPE after sample":      {"a 1\n# TYPE a gauge\n", "after its first sample"},
		"split family":           {"# TYPE a gauge\na{x=\"1\"} 1\n# TYPE b gauge\nb 1\na{x=\"2\"} 1\n", "continues after"},
		"bad escape":             {"a{x=\"\\t\"} 1\n", "invalid escape"},
		"go-quoted unicode":      {"a{x=\"\\u00e9\"} 1\n", "invalid escape"},
		"unquoted value":         {"a{x=1} 1\n", "not quoted"},
		"bad label name":         {"a{0x=\"1\"} 1\n", "invalid label name"},
		"repeated label":         {"a{x=\"1\",x=\"2\"} 1\n", "given twice"},
		"bad metric name":        {"0a 1\n", "invalid metric name"},
		"no value":               {"a{x=\"1\"}\n", "expected one value"},
		"text value":             {"a one\n", "not a float"},
		"unknown type":           {"# TYPE a meter\na 1\n", "unknown type"},
		"no newline at the end":  {"a 1", "newline"},
		"histogram without +Inf": {"# TYPE h histogram\nh_bucket{le=\"1\"} 1\nh_sum 1\nh_count 1\n", "no +Inf bucket"},
		"histogram count":        {"# TYPE h histogram\nh_bucket{le=\"+Inf\"} 2\nh_sum 1\nh_count 1\n", "differs from the +Inf bucket"},
		"decreasing buckets":     {"# TYPE h histogram\nh_bucket{le=\"1\"} 2\nh_bucket{le=\"+Inf\"} 1\nh_count 1\n", "out of order"},
		"bad HELP escape":        {"# HELP a tab\\t\na 1\n", "invalid escape"},
	} {
		if _, err := Validate(tc.text); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
}

func TestLabelRoundTrips(t *testing.T) {
	for _, v := range []string{"plain", `back\slash`, `quo"te`, "new\nline", "tab\there", "caf\u00e9", `\"`, ""} {
		samples, err := Validate("m{v=\"" + Label(v) + "\"} 1\n")
		if err != nil || len(samples) != 1 || samples[0].Labels["v"] != v {
			t.Errorf("Label(%q) = %q does not round-trip: %+v, %v", v, Label(v), samples, err)
		}
	}
}
