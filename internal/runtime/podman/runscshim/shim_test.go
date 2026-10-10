package runscshim

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestParseOCIArgs(t *testing.T) {
	// What Podman 4.9.3 and conmon 2.1.10 pass for a create.
	inv := parseOCIArgs([]string{"--systemd-cgroup", "--log-format=json", "--log", "/run/x/oci-log",
		"create", "--bundle", "/b", "--pid-file", "/p", "--no-new-keyring", "abc"})
	if inv.sub != "create" ||
		!reflect.DeepEqual(inv.globals, []string{"--systemd-cgroup", "--log-format=json", "--log", "/run/x/oci-log"}) ||
		!reflect.DeepEqual(inv.args, []string{"--bundle", "/b", "--pid-file", "/p", "--no-new-keyring", "abc"}) {
		t.Errorf("create: %+v", inv)
	}
	if got := dropFlag(inv.globals, "systemd-cgroup"); !reflect.DeepEqual(got, []string{"--log-format=json", "--log", "/run/x/oci-log"}) {
		t.Errorf("dropFlag: %v", got)
	}
	if got := dropFlag([]string{"-systemd-cgroup=true", "--x"}, "systemd-cgroup"); !reflect.DeepEqual(got, []string{"--x"}) {
		t.Errorf("dropFlag with a value: %v", got)
	}
	if got := flagValue(inv.args, "bundle"); got != "/b" {
		t.Errorf("bundle = %q", got)
	}
	if got := flagValue([]string{"-bundle=/c", "id"}, "bundle"); got != "/c" {
		t.Errorf("bundle= form: %q", got)
	}
	for _, tc := range []struct {
		args []string
		sub  string
		rest []string
	}{
		{[]string{"start", "abc"}, "start", []string{"abc"}},
		{[]string{"kill", "abc", "KILL"}, "kill", []string{"abc", "KILL"}},
		{[]string{"--root", "/r", "delete", "--force", "abc"}, "delete", []string{"--force", "abc"}},
		{[]string{"--version"}, "", nil},
	} {
		if inv := parseOCIArgs(tc.args); inv.sub != tc.sub || !reflect.DeepEqual(inv.args, tc.rest) {
			t.Errorf("%v: %+v", tc.args, inv)
		}
	}
}

func TestScopeFor(t *testing.T) {
	// The resources block Podman writes for a 256 MiB, half-CPU, 64-pid
	// sandbox with swap equal to memory and memory.oom.group.
	const cfg = `{"linux":{"cgroupsPath":"user.slice:libpod:0123abcd","resources":{
		"memory":{"limit":268435456,"swap":268435456},"cpu":{"quota":50000,"period":100000},
		"pids":{"limit":512},"unified":{"memory.oom.group":"1"}}}}`
	var spec ociSpec
	if err := json.Unmarshal([]byte(cfg), &spec); err != nil {
		t.Fatal(err)
	}
	sc, err := scopeFor(spec)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Unit != "libpod-0123abcd.scope" || sc.Slice != "user.slice" {
		t.Errorf("unit %q in slice %q", sc.Unit, sc.Slice)
	}
	props := strings.Join(sc.Properties, " ")
	for _, want := range []string{"Delegate=yes", "MemoryMax=268435456", "MemorySwapMax=0", "TasksMax=512", "CPUQuota=50.00%", "CPUQuotaPeriodSec=100000us"} {
		if !strings.Contains(props, want) {
			t.Errorf("properties lack %s: %s", want, props)
		}
	}
	if !reflect.DeepEqual(sc.Unified, []string{"memory.oom.group=1"}) {
		t.Errorf("unified: %v", sc.Unified)
	}

	// No limits must never be a silent result of a cgroup manager the
	// shim cannot work with.
	for name, cfg := range map[string]string{
		"no cgroupsPath":  `{"linux":{}}`,
		"no linux":        `{}`,
		"cgroupfs path":   `{"linux":{"cgroupsPath":"/libpod_parent/libpod-abc"}}`,
		"slash in a part": `{"linux":{"cgroupsPath":"user.slice:libpod:a/b"}}`,
		"unified slash":   `{"linux":{"cgroupsPath":"user.slice:libpod:a","resources":{"unified":{"../x":"1"}}}}`,
	} {
		var spec ociSpec
		if err := json.Unmarshal([]byte(cfg), &spec); err != nil {
			t.Fatal(err)
		}
		if sc, err := scopeFor(spec); err == nil {
			t.Errorf("%s: accepted: %+v", name, sc)
		}
	}

	// Swap of 1.5x the limit leaves half the limit as swap; an empty
	// slice means the user slice.
	var s2 ociSpec
	_ = json.Unmarshal([]byte(`{"linux":{"cgroupsPath":":libpod:x","resources":{"memory":{"limit":100,"swap":150},"cpu":{"quota":150000}}}}`), &s2)
	sc, err = scopeFor(s2)
	if err != nil || sc.Slice != "user.slice" || !strings.Contains(strings.Join(sc.Properties, " "), "MemorySwapMax=50") ||
		!strings.Contains(strings.Join(sc.Properties, " "), "CPUQuota=150.00%") {
		t.Errorf("swap and default period: %+v %v", sc, err)
	}
}

func TestRunRejectsBadInvocation(t *testing.T) {
	for _, args := range [][]string{
		{"--", "create"},                    // no --runsc
		{"--runsc", "relative/runsc", "--"}, // not absolute
		{"--bogus", "--"},
	} {
		if err := run(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	// A create without a readable bundle fails before anything is run.
	err := run([]string{"--runsc", "/nonexistent/runsc", "--", "create", "--bundle", t.TempDir(), "id"})
	if err == nil || !strings.Contains(err.Error(), "OCI spec") {
		t.Errorf("create without config.json: %v", err)
	}
}
