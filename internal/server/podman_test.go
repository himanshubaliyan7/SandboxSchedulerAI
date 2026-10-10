package server

import (
	"context"
	"strings"
	"testing"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime/podman"
)

func TestResolvePodmanMode(t *testing.T) {
	for _, tc := range []struct{ mode, socket, want string }{
		{"", "", PodmanManaged},
		{"", "/run/user/1000/podman/podman.sock", PodmanExternal}, // what every existing deployment passes
		{"external", "", PodmanExternal},
		{"external", "/s", PodmanExternal},
		{"managed", "", PodmanManaged},
	} {
		if got, err := ResolvePodmanMode(tc.mode, tc.socket); err != nil || got != tc.want {
			t.Errorf("mode %q socket %q: %q, %v; want %q", tc.mode, tc.socket, got, err, tc.want)
		}
	}
	if _, err := ResolvePodmanMode("managed", "/s"); err == nil || !strings.Contains(err.Error(), "external") {
		t.Errorf("managed with a socket: %v", err)
	}
	if _, err := ResolvePodmanMode("private", ""); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestRuntimeConfiguration(t *testing.T) {
	if got, err := normaliseRuntimes(nil); err != nil || len(got) != 1 || got[0].Name != "crun" {
		t.Errorf("default runtimes: %+v, %v", got, err)
	}
	if _, err := normaliseRuntimes([]podman.OCIRuntime{{Name: "runsc"}}); err == nil {
		t.Error("a runtime set without crun was accepted")
	}
	both := []podman.OCIRuntime{{Name: "crun"}, {Name: "runsc", Path: "/opt/gvisor/runsc"}}

	// A server on somebody else's service has that service's one runtime.
	cfg := Config{DatabaseURL: "x", AgentBinary: "x", RunDir: "/tmp/sbx-test", PodmanSocket: "/s", Runtimes: both}
	if err := cfg.defaults(); err == nil || !strings.Contains(err.Error(), "needs --podman-mode managed") {
		t.Errorf("two runtimes in external mode: %v", err)
	}
	cfg = Config{DatabaseURL: "x", AgentBinary: "x", RunDir: "/tmp/sbx-test", PodmanSocket: "/s"}
	if err := cfg.defaults(); err != nil || cfg.PodmanMode != PodmanExternal {
		t.Errorf("external mode with the default runtime: %v, mode %q", err, cfg.PodmanMode)
	}
	cfg = Config{DatabaseURL: "x", AgentBinary: "x", RunDir: "/tmp/sbx-test", Runtimes: both}
	if err := cfg.defaults(); err != nil || cfg.PodmanMode != PodmanManaged || len(cfg.Runtimes) != 2 {
		t.Errorf("managed mode with two runtimes: %v, mode %q", err, cfg.PodmanMode)
	}
	if _, err := PreflightWith(context.Background(), PreflightConfig{PodmanSocket: "/s", Runtimes: both}); err == nil {
		t.Error("preflight accepted two runtimes in external mode")
	}

	if dir, socket := managedPaths("/run/user/1000/sbx", "crun"); dir != "/run/user/1000/sbx/podman" || socket != "/run/user/1000/sbx/podman.sock" {
		t.Errorf("crun service paths: %s %s", dir, socket)
	}
	if dir, socket := managedPaths("/run/user/1000/sbx", "runsc"); dir != "/run/user/1000/sbx/podman-runsc" || socket != "/run/user/1000/sbx/podman-runsc.sock" {
		t.Errorf("runsc service paths: %s %s", dir, socket)
	}
}

func TestCheckTemplateRuntimes(t *testing.T) {
	templates := []core.Template{{ID: "a", Runtime: "crun"}, {ID: "b"}, {ID: "g", Runtime: "runsc"}, {ID: "k", Runtime: "krun"}}
	if err := CheckTemplateRuntimes(templates[:2], []string{"crun"}); err != nil {
		t.Errorf("crun templates on a crun server: %v", err)
	}
	err := CheckTemplateRuntimes(templates, []string{"crun"})
	if err == nil {
		t.Fatal("templates of runtimes that are not enabled were accepted")
	}
	for _, want := range []string{"g (runtime runsc)", "k (runtime krun)", "enabled: crun", "--runtimes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "a (") || strings.Contains(err.Error(), "b (") {
		t.Errorf("error names templates that are fine: %v", err)
	}
	if err := CheckTemplateRuntimes(templates[:3], []string{"crun", "runsc"}); err != nil {
		t.Errorf("with runsc enabled: %v", err)
	}
}
