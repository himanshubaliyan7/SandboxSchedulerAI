package podman

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

func testCfg(t *testing.T, mutate func(*Config)) Config {
	t.Helper()
	cfg := Config{SocketPath: "/nonexistent/podman.sock", SeccompProfilePath: "/etc/sbx/seccomp.json"}
	if mutate != nil {
		mutate(&cfg)
	}
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func testSpec() runtime.Spec {
	return runtime.Spec{
		SandboxID:  "sb-123",
		HostID:     "host-a",
		Generation: 4,
		AgentEpoch: "epoch-9",
		Template: core.Template{
			ID:    "py312",
			Image: "registry.example/py:3.12",
			Resources: core.Resources{
				CPUs: 1.5, MemoryBytes: 512 << 20, PidsLimit: 256,
				WorkdirBytes: 128 << 20, TmpBytes: 64 << 20, NofileLimit: 4096,
			},
			Network: core.NetworkNone,
		},
		HostRunDir:      "/run/user/1000/sbx/sb-123",
		AgentBinaryPath: "/opt/sbx/sbx-agent",
	}
}

// wantCreateJSON is the exact request body for testSpec with the default
// configuration. Any change to the hardened spec has to show up here.
const wantCreateJSON = `{
  "name": "sbx-sb-123",
  "image": "registry.example/py:3.12",
  "entrypoint": ["/usr/local/bin/sbx-agent"],
  "command": ["serve", "--socket", "/run/sbx/agent.sock"],
  "env": {"HOME": "/workspace", "PYTHONPYCACHEPREFIX": "/tmp/pycache", "TMPDIR": "/tmp"},
  "work_dir": "/workspace",
  "hostname": "sandbox",
  "labels": {
    "sbx.agent_epoch": "epoch-9",
    "sbx.generation": "4",
    "sbx.host_id": "host-a",
    "sbx.managed": "true",
    "sbx.sandbox_id": "sb-123",
    "sbx.template_id": "py312"
  },
  "terminal": false,
  "stdin": false,
  "remove": false,
  "env_host": false,
  "httpproxy": false,
  "init": false,
  "stop_timeout": 0,
  "restart_policy": "no",
  "log_configuration": {"driver": "none"},
  "oci_runtime": "crun",
  "image_volume_mode": "ignore",
  "mounts": [
    {"destination": "/workspace", "type": "tmpfs", "source": "tmpfs",
     "options": ["rw", "nosuid", "nodev", "size=134217728", "mode=0755", "exec", "tmpcopyup", "U"]},
    {"destination": "/tmp", "type": "tmpfs", "source": "tmpfs",
     "options": ["rw", "nosuid", "nodev", "size=67108864", "mode=1777", "exec", "notmpcopyup"]},
    {"destination": "/var/tmp", "type": "tmpfs", "source": "tmpfs",
     "options": ["rw", "nosuid", "nodev", "size=1048576", "mode=1777", "noexec", "notmpcopyup"]},
    {"destination": "/run", "type": "tmpfs", "source": "tmpfs",
     "options": ["rw", "nosuid", "nodev", "size=1048576", "mode=0755", "noexec", "notmpcopyup"]},
    {"destination": "/run/sbx", "type": "bind", "source": "/run/user/1000/sbx/sb-123",
     "options": ["rbind", "rw", "nosuid", "nodev", "noexec", "rprivate"]},
    {"destination": "/usr/local/bin/sbx-agent", "type": "bind", "source": "/opt/sbx/sbx-agent",
     "options": ["rbind", "ro", "nosuid", "nodev", "exec", "rprivate"]}
  ],
  "shm_size": 16777216,
  "user": "1000:1000",
  "privileged": false,
  "cap_add": [],
  "cap_drop": ["ALL"],
  "no_new_privileges": true,
  "seccomp_profile_path": "/etc/sbx/seccomp.json",
  "userns": {"nsmode": "keep-id", "value": "uid=1000,gid=1000"},
  "idmappings": {"HostUIDMapping": false, "HostGIDMapping": false},
  "read_only_filesystem": true,
  "read_write_tmpfs": true,
  "pidns": {"nsmode": "private"},
  "ipcns": {"nsmode": "private"},
  "utsns": {"nsmode": "private"},
  "cgroupns": {"nsmode": "private"},
  "netns": {"nsmode": "none"},
  "cgroups_mode": "enabled",
  "resource_limits": {
    "cpu": {"quota": 150000, "period": 100000},
    "memory": {"limit": 536870912, "swap": 536870912},
    "pids": {"limit": 256},
    "unified": {"memory.oom.group": "1"}
  },
  "r_limits": [{"type": "nofile", "hard": 4096, "soft": 4096}]
}`

func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, raw)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func marshalSpec(t *testing.T, cfg Config, spec runtime.Spec) []byte {
	t.Helper()
	s, err := BuildCreateSpec(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBuildCreateSpecExactJSON(t *testing.T) {
	got := canonicalJSON(t, marshalSpec(t, testCfg(t, nil), testSpec()))
	want := canonicalJSON(t, []byte(wantCreateJSON))
	if got != want {
		t.Errorf("create request differs.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestAgentCommandMatchesContract(t *testing.T) {
	if AgentCommand[0] != runtime.AgentBinary {
		t.Errorf("AgentCommand[0] = %q, want runtime.AgentBinary %q", AgentCommand[0], runtime.AgentBinary)
	}
	if got, want := AgentCommand[len(AgentCommand)-1], runtime.RunDir+"/"+runtime.AgentSocket; got != want {
		t.Errorf("agent socket argument = %q, want %q", got, want)
	}
}

// Every network mode gets the same interface-less namespace: egress is a
// proxy over a Unix socket, never a route.
func TestNetworkIsAlwaysNone(t *testing.T) {
	for _, mode := range []core.NetworkMode{"", core.NetworkNone, core.NetworkAllowlist, core.NetworkFull} {
		spec := testSpec()
		spec.Template.Network = mode
		s, err := buildCreateSpec(testCfg(t, nil), spec)
		if err != nil {
			t.Fatalf("mode %q: %v", mode, err)
		}
		if s.NetNS != (namespace{NSMode: "none"}) {
			t.Errorf("mode %q: netns = %+v, want none", mode, s.NetNS)
		}
	}
	spec := testSpec()
	spec.Template.Network = "host"
	if _, err := buildCreateSpec(testCfg(t, nil), spec); err == nil {
		t.Error("unknown network mode accepted")
	}
}

func TestUsernsModes(t *testing.T) {
	for _, tc := range []struct {
		mode   UsernsMode
		userns string
		idmap  string
	}{
		{UsernsKeepID, `{"nsmode":"keep-id","value":"uid=2000,gid=3000"}`, `{"HostUIDMapping":false,"HostGIDMapping":false}`},
		{UsernsNoMap, `{"nsmode":"no-map"}`, `{"HostUIDMapping":false,"HostGIDMapping":false}`},
		{UsernsHost, `{"nsmode":"host"}`, `{"HostUIDMapping":true,"HostGIDMapping":true}`},
		{UsernsAuto, `{"nsmode":"auto"}`, `{"HostUIDMapping":false,"HostGIDMapping":false,"AutoUserNs":true}`},
	} {
		cfg := testCfg(t, func(c *Config) { c.UsernsMode = tc.mode; c.ContainerUID = 2000; c.ContainerGID = 3000 })
		var got struct {
			UserNS     json.RawMessage `json:"userns"`
			IDMappings json.RawMessage `json:"idmappings"`
			User       string          `json:"user"`
		}
		if err := json.Unmarshal(marshalSpec(t, cfg, testSpec()), &got); err != nil {
			t.Fatal(err)
		}
		if string(got.UserNS) != tc.userns || string(got.IDMappings) != tc.idmap || got.User != "2000:3000" {
			t.Errorf("%s: userns=%s idmappings=%s user=%s", tc.mode, got.UserNS, got.IDMappings, got.User)
		}
	}
}

func TestSpecVariants(t *testing.T) {
	// Runtime selection.
	cfg := testCfg(t, func(c *Config) { c.Runtimes = []string{"crun", "runsc"} })
	spec := testSpec()
	spec.Template.Runtime = "runsc"
	s, err := buildCreateSpec(cfg, spec)
	if err != nil || s.OCIRuntime != "runsc" {
		t.Errorf("runsc: runtime=%v err=%v", s, err)
	}
	spec.Template.Runtime = "runc"
	if _, err := buildCreateSpec(cfg, spec); err == nil {
		t.Error("a runtime that is not enabled was accepted")
	}

	// No seccomp path: the field is omitted (Podman default), never unconfined.
	raw := marshalSpec(t, testCfg(t, func(c *Config) { c.SeccompProfilePath = "" }), testSpec())
	if bytes.Contains(raw, []byte("seccomp")) {
		t.Errorf("seccomp field present without a profile: %s", raw)
	}

	// RunDirChown adds U to the run dir bind only.
	s, err = buildCreateSpec(testCfg(t, func(c *Config) { c.RunDirChown = true; c.UsernsMode = UsernsNoMap }), testSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Mounts {
		hasU := strings.Contains(","+strings.Join(m.Options, ",")+",", ",U,")
		if want := m.Destination == runtime.RunDir || m.Destination == runtime.Workdir; hasU != want {
			t.Errorf("mount %s: U option = %v, want %v", m.Destination, hasU, want)
		}
	}

	// Command override without an agent binary: no agent mount.
	s, err = buildCreateSpec(testCfg(t, func(c *Config) { c.Command = []string{"sleep", "60"} }), func() runtime.Spec {
		sp := testSpec()
		sp.AgentBinaryPath = ""
		return sp
	}())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Entrypoint) != 1 || s.Entrypoint[0] != "sleep" || len(s.Command) != 1 || s.Command[0] != "60" {
		t.Errorf("override argv: %v %v", s.Entrypoint, s.Command)
	}
	for _, m := range s.Mounts {
		if m.Destination == runtime.AgentBinary {
			t.Error("agent binary mounted although no path was given")
		}
	}

	// Fractional CPUs and the quota floor.
	for cpus, want := range map[float64]int64{0.25: 25000, 2: 200000, 0.001: 1000} {
		if got := cpuQuota(cpus); got != want {
			t.Errorf("cpuQuota(%v) = %d, want %d", cpus, got, want)
		}
	}
}

func TestBuildCreateSpecRejects(t *testing.T) {
	for name, mutate := range map[string]func(*runtime.Spec){
		"empty sandbox id":   func(s *runtime.Spec) { s.SandboxID = "" },
		"empty host id":      func(s *runtime.Spec) { s.HostID = "" },
		"bad host id":        func(s *runtime.Spec) { s.HostID = "host a" },
		"bad sandbox id":     func(s *runtime.Spec) { s.SandboxID = "a/b" },
		"no image":           func(s *runtime.Spec) { s.Template.Image = "" },
		"relative run dir":   func(s *runtime.Spec) { s.HostRunDir = "run" },
		"no agent binary":    func(s *runtime.Spec) { s.AgentBinaryPath = "" },
		"zero cpus":          func(s *runtime.Spec) { s.Template.Resources.CPUs = 0 },
		"zero memory":        func(s *runtime.Spec) { s.Template.Resources.MemoryBytes = 0 },
		"zero pids":          func(s *runtime.Spec) { s.Template.Resources.PidsLimit = 0 },
		"zero workdir":       func(s *runtime.Spec) { s.Template.Resources.WorkdirBytes = 0 },
		"zero tmp":           func(s *runtime.Spec) { s.Template.Resources.TmpBytes = 0 },
		"zero nofile":        func(s *runtime.Spec) { s.Template.Resources.NofileLimit = 0 },
		"tmpfs over memory":  func(s *runtime.Spec) { s.Template.Resources.WorkdirBytes = 500 << 20 },
		"negative pids":      func(s *runtime.Spec) { s.Template.Resources.PidsLimit = -1 },
		"unknown net mode":   func(s *runtime.Spec) { s.Template.Network = "bridge" },
		"runtime not listed": func(s *runtime.Spec) { s.Template.Runtime = "runsc" },
	} {
		spec := testSpec()
		mutate(&spec)
		if _, err := BuildCreateSpec(testCfg(t, nil), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for name, cfg := range map[string]Config{
		"unconfined seccomp": {SocketPath: "/s", SeccompProfilePath: "unconfined"},
		"relative seccomp":   {SocketPath: "/s", SeccompProfilePath: "profile.json"},
		"bad userns":         {SocketPath: "/s", UsernsMode: "private"},
		"root uid":           {SocketPath: "/s", ContainerUID: 0, ContainerGID: 1000},
		"default not listed": {SocketPath: "/s", DefaultRuntime: "crun", Runtimes: []string{"runsc"}},
		"empty command":      {SocketPath: "/s", Command: []string{}},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if _, err := New(Config{}); err == nil {
		t.Error("no socket and no XDG_RUNTIME_DIR: accepted")
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1234")
	b, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := b.Config()
	if cfg.SocketPath != "/run/user/1234/podman/podman.sock" || cfg.DefaultRuntime != "crun" ||
		cfg.UsernsMode != UsernsKeepID || cfg.CgroupRoot != "/sys/fs/cgroup" || cfg.ContainerUID != 1000 {
		t.Errorf("defaults: %+v", cfg)
	}
}

func TestCapabilitiesSharedKernel(t *testing.T) {
	for _, tc := range []struct {
		runtimes []string
		def      string
		shared   bool
	}{
		{nil, "", true},
		{[]string{"crun", "runsc"}, "crun", true},
		{[]string{"runsc"}, "runsc", false},
		{[]string{"/usr/local/bin/runsc"}, "/usr/local/bin/runsc", false},
		{[]string{"mystery"}, "mystery", true},
	} {
		b, err := New(Config{SocketPath: "/s", Runtimes: tc.runtimes, DefaultRuntime: tc.def})
		if err != nil {
			t.Fatal(err)
		}
		caps := b.Capabilities()
		if caps.SharedKernel != tc.shared {
			t.Errorf("runtimes %v: SharedKernel = %v, want %v", tc.runtimes, caps.SharedKernel, tc.shared)
		}
		if caps.NeedsKVM || caps.Snapshot || !caps.Pause || len(caps.NetworkModes) != 3 {
			t.Errorf("capabilities: %+v", caps)
		}
	}
	if SharedKernel("runsc") || !SharedKernel("crun") || !SharedKernel("runc") {
		t.Error("SharedKernel classification is wrong")
	}
}
