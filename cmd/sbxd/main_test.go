package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Secret-looking values for every environment variable that can hold a
// secret. None of them may ever appear in help, usage or error output.
const (
	secretDBPassword   = "Db-Pa55w0rd-DoNotPrint"
	secretMetricsToken = "metrics-T0ken-DoNotPrint"
	secretAPIKey       = "sbx_abcdef_ApiKey-DoNotPrint"
	secretInURL        = "Url-Cr3dential-DoNotPrint"
)

func setSecretEnv(t *testing.T) {
	t.Helper()
	// Port 1: nothing listens there, so a command that does connect
	// fails fast, with an error that is checked too.
	t.Setenv("SBX_DATABASE_URL", "postgres://sbx:"+secretDBPassword+"@127.0.0.1:1/sbx?sslmode=disable&connect_timeout=2")
	t.Setenv("SBX_METRICS_TOKEN", secretMetricsToken)
	t.Setenv("SBX_API_KEY", secretAPIKey)
	t.Setenv("SBX_BASE_URL", "http://admin:"+secretInURL+"@127.0.0.1:8080")
}

func assertNoSecret(t *testing.T, what, out string) {
	t.Helper()
	for _, secret := range []string{secretDBPassword, secretMetricsToken, secretAPIKey, secretInURL, "DoNotPrint"} {
		if strings.Contains(out, secret) {
			t.Errorf("%s prints a secret (%s):\n%s", what, secret, out)
			return
		}
	}
}

// invoke runs sbxd in process and returns everything it printed.
func invoke(args ...string) (code int, out string) {
	var stdout, stderr bytes.Buffer
	code = run(args, &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// TestHelpNeverPrintsSecrets runs the help of every command and of every
// admin action with secrets in the environment. A flag whose default is
// read from a variable prints that default in -h, which is how the
// database URL, password included, used to end up on screen.
func TestHelpNeverPrintsSecrets(t *testing.T) {
	setSecretEnv(t)
	commands := [][]string{{}, {"help"}, {"-h"}, {"--help"}, {"version"}, {"bogus"},
		{"serve", "-h"}, {"serve", "--help"}, {"preflight", "-h"},
		{"admin"}, {"admin", "-h"}, {"admin", "help"}, {"admin", "bogus", "action"}, {"admin", "tenant"}}
	for _, act := range adminActions {
		words := strings.Fields(act.name)
		for _, h := range []string{"-h", "--help"} {
			commands = append(commands, append(append([]string{"admin"}, words...), h))
		}
		// A flag that does not exist prints the usage as well.
		commands = append(commands, append(append([]string{"admin"}, words...), "--no-such-flag"))
	}
	for _, args := range commands {
		code, out := invoke(args...)
		what := "sbxd " + strings.Join(args, " ")
		if out == "" {
			t.Errorf("%s printed nothing (exit %d)", what, code)
		}
		assertNoSecret(t, what, out)
		if len(args) > 0 && (args[len(args)-1] == "-h" || args[len(args)-1] == "--help") && code != 0 {
			t.Errorf("%s exits %d, want 0:\n%s", what, code, out)
		}
	}
	// The help still says that the variable is set, without its value.
	if _, out := invoke("serve", "-h"); !strings.Contains(out, "SBX_DATABASE_URL: set, value not shown") ||
		!strings.Contains(out, "SBX_METRICS_TOKEN: set, value not shown") {
		t.Errorf("serve -h does not say that the secret variables are set:\n%s", out)
	}
	t.Setenv("SBX_DATABASE_URL", "")
	if _, out := invoke("admin", "tenant", "list", "-h"); !strings.Contains(out, "SBX_DATABASE_URL: not set") {
		t.Errorf("tenant list -h with no variable:\n%s", out)
	}
}

// TestErrorsNeverPrintSecrets: the commands really run, against a
// database URL nobody answers on, and with invalid input; what they
// print on failure must not contain the URL's password or the token.
func TestErrorsNeverPrintSecrets(t *testing.T) {
	setSecretEnv(t)
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	failing := [][]string{
		{"admin", "tenant", "list"},
		{"admin", "tenant", "create", "acme"},
		{"admin", "key", "create", "acme"},
		{"admin", "template", "show", "python"},
		{"admin", "template", "apply", "python", "--image", "x"},
		{"admin", "webhook", "list"},
		{"admin", "tenant", "list", "extra", "argument"},
		{"admin", "tenant", "list", "--database-url", "postgres://sbx:" + secretDBPassword + "@127.0.0.1:notaport/sbx"},
		{"admin", "tenant", "list", "--database-url", "host=127.0.0.1 port=1 password=" + secretDBPassword + " sslmode=bogus"},
		{"serve", "--log-level", "bogus"},
		{"serve", "--runtimes", "crun,,"},
		{"serve", "--skip-preflight", "--agent-binary", "/nonexistent/sbx-agent"},
		{"serve", "--skip-preflight", "--podman-socket", filepath.Join(t.TempDir(), "none.sock")},
		{"serve", "--listen", "0.0.0.0:0"},
		{"serve", "--metrics-token"},
	}
	for _, args := range failing {
		code, out := invoke(args...)
		what := "sbxd " + strings.Join(args[:min(len(args), 4)], " ")
		if code == 0 {
			t.Errorf("%s succeeded; it was meant to fail:\n%s", what, out)
		}
		assertNoSecret(t, what, out)
	}
	// Variables that are not secrets are echoed when they are malformed;
	// that message must not drag a secret along.
	t.Setenv("SBX_SHUTDOWN_TIMEOUT", "soon")
	_, out := invoke("serve", "-h")
	assertNoSecret(t, "serve -h with a malformed variable", out)
}

// TestOtherBinariesHelp builds sbx-mcp and sbx-agent and runs their help
// with the same environment: sbx-mcp reads the API key from it.
func TestOtherBinariesHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	setSecretEnv(t)
	dir := t.TempDir()
	for bin, argsets := range map[string][][]string{
		"sbx-mcp":   {{"-h"}, {"--help"}, {"--version"}, {"--no-such-flag"}},
		"sbx-agent": {{"-h"}, {"serve", "-h"}, {"version"}, {"bogus"}},
	} {
		path := filepath.Join(dir, bin)
		if out, err := exec.Command("go", "build", "-o", path, "../"+bin).CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", bin, err, out)
		}
		for _, args := range argsets {
			out, _ := exec.Command(path, args...).CombinedOutput()
			assertNoSecret(t, bin+" "+strings.Join(args, " "), string(out))
		}
	}
}

// TestAdminPositionalIDs: an action that takes exactly one id accepts it
// as an argument, before or after the flags, and --id keeps working.
func TestAdminPositionalIDs(t *testing.T) {
	dsn := os.Getenv("SBX_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SBX_TEST_DATABASE_URL is not set")
	}
	t.Setenv("SBX_DATABASE_URL", dsn)
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	tenant, tmpl := "cli-"+hex.EncodeToString(b), "clitpl-"+hex.EncodeToString(b)
	ok := func(args ...string) string {
		t.Helper()
		code, out := invoke(args...)
		if code != 0 {
			t.Fatalf("sbxd %s: exit %d\n%s", strings.Join(args, " "), code, out)
		}
		return out
	}
	fails := func(want string, args ...string) {
		t.Helper()
		code, out := invoke(args...)
		if code == 0 || !strings.Contains(out, want) {
			t.Errorf("sbxd %s: exit %d, want a failure mentioning %q:\n%s", strings.Join(args, " "), code, want, out)
		}
	}
	ok("admin", "tenant", "create", tenant, "--max-concurrent", "3")
	t.Cleanup(func() { invoke("admin", "tenant", "delete", tenant); invoke("admin", "template", "delete", tmpl) })
	if out := ok("admin", "tenant", "update", "--rate", "7", tenant); !strings.Contains(out, "max_concurrent=3 rate=7/s") {
		t.Errorf("tenant update with the id after a flag: %s", out)
	}
	var key struct {
		ID, Key string
	}
	if err := json.Unmarshal([]byte(ok("admin", "key", "create", tenant, "--json")), &key); err != nil || key.ID == "" {
		t.Fatalf("key create <tenant> --json: %v %+v", err, key)
	}
	if out := ok("admin", "key", "list", "--json", tenant); !strings.Contains(out, key.ID) {
		t.Errorf("key list --json <tenant>: %s", out)
	}
	ok("admin", "key", "revoke", key.ID)

	ok("admin", "template", "apply", tmpl, "--image", "docker.io/library/alpine:3.20", "--memory", "256m", "--workdir-size", "64m", "--tmp-size", "32m",
		"--network", "allowlist", "--allow-domains", "pypi.org, files.pythonhosted.org", "--allow-cidrs", "203.0.113.0/24", "--warm-target", "0")
	var shown templateFile
	for _, args := range [][]string{{tmpl}, {"--id", tmpl}, {tmpl, "--json"}, {"--json", tmpl}, {"--id", tmpl, tmpl}} {
		out := ok(append([]string{"admin", "template", "show"}, args...)...)
		if err := json.Unmarshal([]byte(out), &shown); err != nil || shown.ID != tmpl {
			t.Errorf("template show %v: %v\n%s", args, err, out)
		}
	}
	if len(shown.AllowDomains) != 2 || shown.AllowDomains[1] != "files.pythonhosted.org" || len(shown.AllowCIDRs) != 1 || shown.Network != "allowlist" {
		t.Errorf("template show lost the default allowlist: %+v", shown)
	}
	if out := ok("admin", "template", "list"); !strings.Contains(out, "allowlist (3 allowed)") {
		t.Errorf("template list does not show the default allowlist:\n%s", out)
	}
	// What `show` prints is what `apply --file` reads.
	file := filepath.Join(t.TempDir(), "t.json")
	shown.Version = 0
	raw, _ := json.Marshal(shown)
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	ok("admin", "template", "apply", "--file", file)

	fails("unexpected argument", "admin", "template", "show", tmpl, "another")
	fails("disagree", "admin", "template", "show", "--id", "other", tmpl)
	fails("unexpected argument", "admin", "template", "list", tmpl)
	fails("a template id is required", "admin", "template", "show")
	fails("default allowlist of an allowlist template", "admin", "template", "apply", tmpl, "--image", "x", "--network", "none", "--allow-domains", "pypi.org")
	fails("default allowlist", "admin", "template", "apply", tmpl, "--image", "x", "--network", "allowlist", "--allow-domains", "not a host!")
	fails("default allowlist", "admin", "template", "apply", tmpl, "--image", "x", "--network", "allowlist", "--allow-cidrs", "999.1.1.1/8")

	ok("admin", "template", "delete", tmpl)
	ok("admin", "tenant", "delete", tenant)
}

// TestUsageListsEveryCommand: the top-level usage names every command,
// and the admin usage every action with its argument.
func TestUsageListsEveryCommand(t *testing.T) {
	_, out := invoke("help")
	for _, want := range []string{"serve", "preflight", "admin <resource> <action> [id]", "version"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage lacks %q:\n%s", want, out)
		}
	}
	_, out = invoke("admin", "-h")
	for _, act := range adminActions {
		if !strings.Contains(out, "  "+strings.TrimSpace(act.name+" "+act.args)) {
			t.Errorf("admin usage lacks %q with its arguments %q:\n%s", act.name, act.args, out)
		}
	}
	for _, want := range []string{"webhook create", "template show <id>", "key create <tenant>"} {
		if !strings.Contains(out, want) {
			t.Errorf("admin usage lacks %q", want)
		}
	}
	_, out = invoke("admin", "template", "apply", "-h")
	for _, want := range []string{"usage: sbxd admin template apply <id>", "ENTRYPOINT and CMD are ignored", "USER is ignored", "uid 1000", "--allow-domains"} {
		if !strings.Contains(out, want) {
			t.Errorf("template apply -h lacks %q:\n%s", want, out)
		}
	}
}
