// Package promtext escapes label values for, and validates, Prometheus text exposition output (format
// version 0.0.4) the way a strict scraper reads it. The metrics in this
// repository are written by hand, without a client library, so nothing
// else stops a duplicate series or a badly escaped label from shipping;
// the tests of every package that writes metrics run their output
// through Validate.
package promtext

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	metricNameRE = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRE  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Label escapes a label value for the text format: backslash, double
// quote and newline, and nothing else. (Go's %q is not it: it also
// writes \t and \u00e9, which a scraper rejects.)
func Label(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(v)
}

// Sample is one parsed sample line.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Validate parses text and returns its samples, or the first violation of
// the exposition format:
//
//   - a line that is not a comment, a HELP, a TYPE or a well-formed sample;
//   - an invalid metric or label name, a label value with an escape other
//     than \\, \" and \n, a repeated label name, a value that is not a
//     float;
//   - HELP or TYPE given twice for a family, TYPE after the family's first
//     sample, an unknown type, a family whose lines are not together;
//   - two samples with the same name and label set (a duplicate series);
//   - a histogram without a +Inf bucket, with buckets that decrease, or
//     whose _count differs from its +Inf bucket.
func Validate(text string) ([]Sample, error) {
	type family struct {
		typ      string
		help     bool
		typed    bool
		sampled  bool
		closed   bool // another family's lines came after this one's
		lastLine int
	}
	families := map[string]*family{}
	get := func(name string) *family {
		f := families[name]
		if f == nil {
			f = &family{}
			families[name] = f
		}
		return f
	}
	seen := map[string]int{}
	var (
		samples []Sample
		current string
	)
	enter := func(name string, lineNo int) error {
		f := get(name)
		if f.closed {
			return fmt.Errorf("line %d: family %s continues after lines of another family (first part ended at line %d)", lineNo, name, f.lastLine)
		}
		if current != "" && current != name {
			families[current].closed = true
		}
		current = name
		f.lastLine = lineNo
		return nil
	}
	if text != "" && !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("the output does not end with a newline")
	}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		lineNo := i + 1
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			fields := strings.SplitN(line, " ", 4)
			if len(fields) < 2 || (fields[1] != "HELP" && fields[1] != "TYPE") {
				continue // a plain comment
			}
			if len(fields) < 3 || !metricNameRE.MatchString(fields[2]) {
				return nil, fmt.Errorf("line %d: malformed %s line %q", lineNo, fields[1], line)
			}
			name := fields[2]
			if err := enter(name, lineNo); err != nil {
				return nil, err
			}
			f := get(name)
			switch fields[1] {
			case "HELP":
				if f.help {
					return nil, fmt.Errorf("line %d: second HELP for %s", lineNo, name)
				}
				f.help = true
				if len(fields) == 4 {
					if err := checkEscapes(fields[3], false); err != nil {
						return nil, fmt.Errorf("line %d: HELP of %s: %w", lineNo, name, err)
					}
				}
			case "TYPE":
				if f.typed {
					return nil, fmt.Errorf("line %d: second TYPE for %s", lineNo, name)
				}
				if f.sampled {
					return nil, fmt.Errorf("line %d: TYPE for %s after its first sample", lineNo, name)
				}
				if len(fields) != 4 {
					return nil, fmt.Errorf("line %d: TYPE line without a type: %q", lineNo, line)
				}
				switch fields[3] {
				case "counter", "gauge", "histogram", "summary", "untyped":
				default:
					return nil, fmt.Errorf("line %d: unknown type %q for %s", lineNo, fields[3], name)
				}
				f.typed, f.typ = true, fields[3]
			}
			continue
		}
		s, err := parseSample(line)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w: %q", lineNo, err, line)
		}
		// The family of a sample: histograms and summaries own suffixed names.
		fam := s.Name
		for _, suffix := range []string{"_bucket", "_sum", "_count"} {
			base := strings.TrimSuffix(s.Name, suffix)
			if base == s.Name {
				continue
			}
			if f := families[base]; f != nil && (f.typ == "histogram" || (f.typ == "summary" && suffix != "_bucket")) {
				fam = base
			}
		}
		if err := enter(fam, lineNo); err != nil {
			return nil, err
		}
		f := get(fam)
		f.sampled = true
		if f.typ == "histogram" && s.Name == fam {
			return nil, fmt.Errorf("line %d: histogram %s has a sample without _bucket, _sum or _count", lineNo, fam)
		}
		if _, isBucket := s.Labels["le"]; f.typ == "histogram" && strings.HasSuffix(s.Name, "_bucket") && !isBucket {
			return nil, fmt.Errorf("line %d: histogram bucket without an le label", lineNo)
		}
		key := seriesKey(s)
		if first, dup := seen[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate series %s (first at line %d)", lineNo, key, first)
		}
		seen[key] = lineNo
		samples = append(samples, s)
	}
	if err := checkHistograms(samples, func(name string) bool {
		f := families[name]
		return f != nil && f.typ == "histogram"
	}); err != nil {
		return nil, err
	}
	return samples, nil
}

func seriesKey(s Sample) string {
	names := make([]string, 0, len(s.Labels))
	for n := range s.Labels {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(s.Name)
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%s=%q", n, s.Labels[n])
	}
	b.WriteByte('}')
	return b.String()
}

// checkEscapes checks the backslash escapes of a HELP text or (quoted
// true) of the inside of a label value.
func checkEscapes(v string, quoted bool) error {
	for i := 0; i < len(v); i++ {
		if v[i] != '\\' {
			continue
		}
		i++
		if i == len(v) {
			return fmt.Errorf("trailing backslash")
		}
		switch {
		case v[i] == '\\' || v[i] == 'n':
		case v[i] == '"' && quoted:
		default:
			return fmt.Errorf("invalid escape \\%c", v[i])
		}
	}
	return nil
}

func parseSample(line string) (Sample, error) {
	s := Sample{Labels: map[string]string{}}
	rest := line
	end := strings.IndexAny(rest, "{ ")
	if end < 0 {
		return s, fmt.Errorf("sample without a value")
	}
	s.Name = rest[:end]
	if !metricNameRE.MatchString(s.Name) {
		return s, fmt.Errorf("invalid metric name %q", s.Name)
	}
	rest = rest[end:]
	if rest[0] == '{' {
		rest = rest[1:]
		for {
			if rest == "" {
				return s, fmt.Errorf("unterminated label set")
			}
			if rest[0] == '}' {
				rest = rest[1:]
				break
			}
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return s, fmt.Errorf("label without a value")
			}
			name := rest[:eq]
			if !labelNameRE.MatchString(name) || strings.HasPrefix(name, "__") {
				return s, fmt.Errorf("invalid label name %q", name)
			}
			rest = rest[eq+1:]
			if rest == "" || rest[0] != '"' {
				return s, fmt.Errorf("label %s: value is not quoted", name)
			}
			rest = rest[1:]
			var val strings.Builder
			closed := false
			for i := 0; i < len(rest); i++ {
				c := rest[i]
				if c == '\\' {
					if i+1 == len(rest) {
						return s, fmt.Errorf("label %s: trailing backslash", name)
					}
					i++
					switch rest[i] {
					case '\\':
						val.WriteByte('\\')
					case '"':
						val.WriteByte('"')
					case 'n':
						val.WriteByte('\n')
					default:
						return s, fmt.Errorf("label %s: invalid escape \\%c", name, rest[i])
					}
					continue
				}
				if c == '"' {
					rest = rest[i+1:]
					closed = true
					break
				}
				val.WriteByte(c)
			}
			if !closed {
				return s, fmt.Errorf("label %s: unterminated value", name)
			}
			if _, dup := s.Labels[name]; dup {
				return s, fmt.Errorf("label %s given twice", name)
			}
			s.Labels[name] = val.String()
			switch {
			case strings.HasPrefix(rest, ","):
				rest = rest[1:]
			case strings.HasPrefix(rest, "}"):
			default:
				return s, fmt.Errorf("label %s: expected , or } after the value", name)
			}
		}
	}
	fields := strings.Fields(rest)
	if !strings.HasPrefix(rest, " ") || len(fields) < 1 || len(fields) > 2 {
		return s, fmt.Errorf("expected one value (and at most a timestamp) after the series")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return s, fmt.Errorf("value %q is not a float", fields[0])
	}
	s.Value = v
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return s, fmt.Errorf("timestamp %q is not an integer", fields[1])
		}
	}
	return s, nil
}

func checkHistograms(samples []Sample, isHistogram func(string) bool) error {
	type series struct {
		last    float64
		inf     *float64
		count   *float64
		buckets int
		lastLe  float64
	}
	all := map[string]*series{}
	order := []string{}
	for _, s := range samples {
		var base, kind string
		for _, suffix := range []string{"_bucket", "_count"} {
			if b := strings.TrimSuffix(s.Name, suffix); b != s.Name && isHistogram(b) {
				base, kind = b, suffix
			}
		}
		if base == "" {
			continue
		}
		rest := Sample{Name: base, Labels: map[string]string{}}
		for k, v := range s.Labels {
			if k != "le" {
				rest.Labels[k] = v
			}
		}
		key := seriesKey(rest)
		h := all[key]
		if h == nil {
			h = &series{}
			all[key] = h
			order = append(order, key)
		}
		if kind == "_count" {
			v := s.Value
			h.count = &v
			continue
		}
		le, err := strconv.ParseFloat(s.Labels["le"], 64)
		if err != nil {
			return fmt.Errorf("histogram %s: le=%q is not a float", key, s.Labels["le"])
		}
		if h.buckets > 0 && (le <= h.lastLe || s.Value < h.last) {
			return fmt.Errorf("histogram %s: bucket le=%q is out of order or smaller than the one before", key, s.Labels["le"])
		}
		h.buckets++
		h.lastLe, h.last = le, s.Value
		if s.Labels["le"] == "+Inf" {
			v := s.Value
			h.inf = &v
		}
	}
	for _, key := range order {
		h := all[key]
		switch {
		case h.inf == nil:
			return fmt.Errorf("histogram %s has no +Inf bucket", key)
		case h.count == nil:
			return fmt.Errorf("histogram %s has no _count", key)
		case *h.count != *h.inf:
			return fmt.Errorf("histogram %s: _count %v differs from the +Inf bucket %v", key, *h.count, *h.inf)
		}
	}
	return nil
}
