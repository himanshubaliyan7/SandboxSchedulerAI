package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

func TestDefaultHostIDIsStableAndPerRunDir(t *testing.T) {
	a1, err := DefaultHostID("/run/user/1000/sbx")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := DefaultHostID("/run/user/1000/sbx/") // the same directory
	b, _ := DefaultHostID("/run/user/1000/sbx-dev")
	if a1 != a2 {
		t.Errorf("the same run dir gave two host ids: %q and %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("two run dirs share the host id %q", a1)
	}
	for _, id := range []string{a1, b} {
		if err := runtime.ValidHostID(id); err != nil {
			t.Errorf("default host id: %v", err)
		}
	}
	host, _ := os.Hostname()
	if len(host) <= 32 && runtime.ValidHostID(host) == nil && !strings.HasPrefix(a1, host+"-u") {
		t.Errorf("default host id %q does not start with the hostname %q and the uid", a1, host)
	}
	t.Logf("default host id for /run/user/1000/sbx on this machine: %s", a1)
}

func baseConfig(t *testing.T) Config {
	return Config{DatabaseURL: "postgres://unused", AgentBinary: "/unused", RunDir: t.TempDir()}
}

func TestConfigHostID(t *testing.T) {
	c := baseConfig(t)
	if err := c.defaults(); err != nil {
		t.Fatal(err)
	}
	want, _ := DefaultHostID(c.RunDir)
	if c.HostID != want {
		t.Errorf("default host id %q, want %q", c.HostID, want)
	}
	c = baseConfig(t)
	c.HostID = "pool-1.example"
	if err := c.defaults(); err != nil || c.HostID != "pool-1.example" {
		t.Errorf("explicit host id: %q, %v", c.HostID, err)
	}
	for _, bad := range []string{"has space", "-leading", "a/b", strings.Repeat("x", 64)} {
		c = baseConfig(t)
		c.HostID = bad
		if err := c.defaults(); err == nil || !strings.Contains(err.Error(), "host id") {
			t.Errorf("host id %q accepted: %v", bad, err)
		}
	}
}

// TestMetricsExposureRule: unauthenticated metrics are only served on a
// loopback API listener. Anything else needs a token or a listener of
// its own, or the server refuses to start.
func TestMetricsExposureRule(t *testing.T) {
	for _, tc := range []struct {
		listen, metricsListen, token string
		ok                           bool
	}{
		{"", "", "", true}, // the default, 127.0.0.1:8080
		{"127.0.0.1:8080", "", "", true},
		{"localhost:8080", "", "", true},
		{"[::1]:8080", "", "", true},
		{"0.0.0.0:8080", "", "", false},
		{":8080", "", "", false},
		{"[::]:8080", "", "", false},
		{"192.168.1.10:8080", "", "", false},
		{"sbx.example.com:8080", "", "", false},
		{"0.0.0.0:8080", "", "tok", true},
		{"0.0.0.0:8080", "127.0.0.1:9090", "", true},
		{"0.0.0.0:8080", "0.0.0.0:9090", "", true}, // the operator's explicit choice; warned about at start
		{"0.0.0.0:8080", "0.0.0.0:9090", "tok", true},
	} {
		c := baseConfig(t)
		c.Listen, c.MetricsListen, c.MetricsToken = tc.listen, tc.metricsListen, tc.token
		err := c.defaults()
		if (err == nil) != tc.ok {
			t.Errorf("listen=%q metrics-listen=%q token=%q: err=%v, want ok=%v", tc.listen, tc.metricsListen, tc.token, err, tc.ok)
		}
		if err != nil && (!strings.Contains(err.Error(), "--metrics-token") || !strings.Contains(err.Error(), "--metrics-listen")) {
			t.Errorf("the refusal does not say how to fix it: %v", err)
		}
	}
}

func TestRunDirLockIsExclusive(t *testing.T) {
	dir := t.TempDir()
	release, err := lockRunDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockRunDir(dir); err == nil || !strings.Contains(err.Error(), "in use by another sbxd") {
		t.Fatalf("second lock on the same run dir: %v", err)
	}
	other, err := lockRunDir(t.TempDir())
	if err != nil {
		t.Fatalf("lock on another run dir: %v", err)
	}
	other()
	release()
	again, err := lockRunDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again()
	if _, err := os.Stat(filepath.Join(dir, "sbxd.lock")); err != nil {
		t.Error(err)
	}
}

func TestPlannedSandboxes(t *testing.T) {
	planned, uncapped := plannedSandboxes([]core.Template{
		{ID: "a", MaxTotal: 16, WarmTarget: 3}, {ID: "b", MaxTotal: 4}, {ID: "c", WarmTarget: 5}, {ID: "d"},
	})
	if planned != 25 || len(uncapped) != 2 || uncapped[0] != "c" || uncapped[1] != "d" {
		t.Errorf("planned=%d uncapped=%v, want 25 and [c d]", planned, uncapped)
	}
	// Smoke: the capacity check runs against this host without panicking.
	checkCapacity(context.Background(), discardLogger(), "", []core.Template{{ID: "a", MaxTotal: 8}})
}

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
