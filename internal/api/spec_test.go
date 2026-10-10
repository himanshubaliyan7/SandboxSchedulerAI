package api_test

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/himanshubaliyan7/warmbox/internal/agentapi"
)

const specPath = "../../api/openapi.yaml"

var (
	specPathRE   = regexp.MustCompile(`^  (/[^\s:]*):\s*$`)
	specMethodRE = regexp.MustCompile(`^    (get|put|post|delete|patch|head|options|trace):\s*$`)
)

// specRoutes reads the "METHOD /path" pairs out of the paths section of
// the OpenAPI document. It relies on the document's block layout (paths
// at two spaces, methods at four), which is cheaper than a YAML
// dependency and fails loudly, by finding nothing, if the layout changes.
func specRoutes(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(specPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var (
		out     []string
		inPaths bool
		path    string
	)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "paths:":
			inPaths = true
		case inPaths && line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#"):
			inPaths = false // the next top-level key
		case !inPaths:
		case specPathRE.MatchString(line):
			path = specPathRE.FindStringSubmatch(line)[1]
		case specMethodRE.MatchString(line) && path != "":
			out = append(out, strings.ToUpper(specMethodRE.FindStringSubmatch(line)[1])+" "+path)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) < 10 {
		t.Fatalf("found only %d routes in %s: has its layout changed?", len(out), specPath)
	}
	return out
}

// The server's routes and the OpenAPI document describe each other
// exactly: a route that is served is documented, and a documented route
// is served.
func TestSpecListsExactlyTheServedRoutes(t *testing.T) {
	h := newHarness(t, options{noStart: true})
	served := map[string]bool{}
	for _, r := range h.api.Routes() {
		if served[r] {
			t.Errorf("route %q is registered twice", r)
		}
		served[r] = true
	}
	documented := map[string]bool{}
	for _, r := range specRoutes(t) {
		if documented[r] {
			t.Errorf("route %q is documented twice", r)
		}
		documented[r] = true
	}
	var missing, extra []string
	for r := range served {
		if !documented[r] {
			missing = append(missing, r)
		}
	}
	for r := range documented {
		if !served[r] {
			extra = append(extra, r)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 {
		t.Errorf("served but not in %s:\n  %s", specPath, strings.Join(missing, "\n  "))
	}
	if len(extra) > 0 {
		t.Errorf("in %s but not served:\n  %s", specPath, strings.Join(extra, "\n  "))
	}
	t.Logf("%d routes served, %d documented", len(served), len(documented))
}

// The frame sizes and close codes the document quotes are the constants
// the code uses.
func TestSpecQuotesTheWireConstants(t *testing.T) {
	b, err := os.ReadFile(specPath)
	if err != nil {
		t.Fatal(err)
	}
	spec := string(b)
	for what, want := range map[string]string{
		"largest client frame":   fmt.Sprintf("at most %d\n      bytes: 1 channel byte + %d", agentapi.MaxClientFrameBytes, agentapi.MaxClientPayloadBytes),
		"largest server frame":   fmt.Sprintf("at most %d bytes (1\n      channel byte + %d)", agentapi.MaxFrameBytes+1, agentapi.MaxFrameBytes),
		"flow high watermark":    fmt.Sprintf("more than %d bytes are unacknowledged", agentapi.FlowHighWatermark),
		"flow low watermark":     fmt.Sprintf("resumes below %d", agentapi.FlowLowWatermark),
		"close sandbox gone":     fmt.Sprintf("| %d | `sandbox_terminated`", agentapi.CloseSandboxTerminated),
		"close unauthorized":     fmt.Sprintf("| %d | `unauthorized`", agentapi.CloseUnauthorized),
		"close shutting down":    fmt.Sprintf("| %d | `shutting_down`", agentapi.CloseShuttingDown),
		"close agent":            fmt.Sprintf("| %d | `agent_unavailable`", agentapi.CloseAgentUnavailable),
		"close not found":        fmt.Sprintf("| %d | `process_not_found`", agentapi.CloseNotFound),
		"close bad request":      fmt.Sprintf("| %d | `bad_request`", agentapi.CloseBadRequest),
		"close rate limited":     fmt.Sprintf("| %d | `rate_limited`", agentapi.CloseRateLimited),
		"close other":            fmt.Sprintf("| %d | any other API error code", agentapi.CloseError),
		"oversized client frame": fmt.Sprintf("| 1009 | (set by the library) | A client frame exceeded %d bytes", agentapi.MaxClientFrameBytes),
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("%s: the document does not contain %q", what, want)
		}
	}
}
