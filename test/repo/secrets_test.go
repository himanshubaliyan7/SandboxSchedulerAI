// Package repo holds checks on the repository itself rather than on its
// code.
package repo

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// secretShapes are the literals a secret scanner reports for this
// project's own credential formats. Nothing in the repository may match
// one, real or example: an example that looks like a secret makes every
// scan of the public repository fire (the Standard Webhooks test vector
// was reported as a leaked signing secret eight times), and people stop
// reading the alerts.
//
// Documentation uses placeholders that cannot match ("whsec_<base64-secret>",
// "sbx_...", "sbt_<64 hex digits>"); a test that needs a well-formed
// value builds it at run time from parts ("whsec_" + body).
var secretShapes = map[string]*regexp.Regexp{
	"webhook signing secret (whsec_ + 20 or more base64 characters)": regexp.MustCompile(`whsec_[A-Za-z0-9+/=]{20,}`),
	"API key (sbx_ + 8 hex + _ + 64 hex)":                            regexp.MustCompile(`sbx_[0-9a-f]{8}_[0-9a-f]{64}`),
	"sandbox access token (sbt_ + 64 hex)":                           regexp.MustCompile(`sbt_[0-9a-f]{64}`),
}

// allowed lists "path: literal" pairs that may stay. It is empty, and
// should remain so: prefer a placeholder or run-time construction.
var allowed = map[string]bool{}

// trackedFiles returns the files git tracks, plus the untracked ones it
// does not ignore (a new file is checked before it is first committed).
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Skipf("git ls-files: %v (the check needs a git checkout)", err)
	}
	var files []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

func TestNoSecretShapedLiterals(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	files := trackedFiles(t, root)
	if len(files) < 50 {
		t.Fatalf("only %d files found under %s; the walk is broken", len(files), root)
	}
	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			continue // deleted in the working tree, or a dangling link
		}
		if bytes.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0 {
			continue // binary
		}
		checked++
		for what, re := range secretShapes {
			for _, m := range re.FindAll(raw, -1) {
				if allowed[f+": "+string(m)] {
					continue
				}
				line := 1 + bytes.Count(raw[:bytes.Index(raw, m)], []byte("\n"))
				t.Errorf("%s:%d contains a literal shaped like a %s: %.12s...\n"+
					"use a placeholder in documentation, or build the value at run time in a test", f, line, what, m)
			}
		}
	}
	t.Logf("%d text files checked", checked)
}

// The patterns do find what they are meant to find.
func TestSecretShapesMatch(t *testing.T) {
	hex := strings.Repeat("0123456789abcdef", 4)
	for what, sample := range map[string]string{
		"webhook signing secret (whsec_ + 20 or more base64 characters)": "whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw",
		"API key (sbx_ + 8 hex + _ + 64 hex)":                            "sbx_" + hex[:8] + "_" + hex,
		"sandbox access token (sbt_ + 64 hex)":                           "sbt_" + hex,
	} {
		if !secretShapes[what].MatchString(sample) {
			t.Errorf("the pattern for a %s does not match a well-formed one", what)
		}
	}
	for _, placeholder := range []string{"whsec_<base64-secret>", "whsec_...", "sbx_...", "sbt_<64 hex digits>", "whsec_" + "dGVzdA=="} {
		for what, re := range secretShapes {
			if re.MatchString(placeholder) {
				t.Errorf("the placeholder %q matches the pattern for a %s", placeholder, what)
			}
		}
	}
}
