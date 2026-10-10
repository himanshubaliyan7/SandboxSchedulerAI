package preflight

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// host is a fixture: a fake filesystem root plus fake commands.
type host struct {
	t        *testing.T
	root     string
	opts     Options
	binaries map[string]string // name -> `--version` output
	pingErr  error
	pingVer  string
	// sdrunArgs are the arguments of the last systemd-run call. The
	// "systemd-run" entry of binaries is its output; non-empty means it
	// failed.
	sdrunArgs []string
}

func (h *host) write(path, content string) {
	h.t.Helper()
	p := filepath.Join(h.root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *host) remove(path string) {
	h.t.Helper()
	if err := os.RemoveAll(filepath.Join(h.root, path)); err != nil {
		h.t.Fatal(err)
	}
}

// keyUsers is /proc/key-users with 6 keys charged to uid 1000 (and other
// users around it, one of them with a uid that has 1000 as a prefix).
func keyUsers(maxKeys string) string {
	return "    0:   569 568/568 112/1000000 2391/25000000\n" +
		"  120:     3 3/3 3/200 31/20000\n" +
		" 1000:     6 6/6 6/" + maxKeys + " 176/20000\n" +
		"10001:    50 50/50 50/200 900/20000\n"
}

const userCgroup = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/cgroup.controllers"

// healthyHost builds a host on which every check is ok or info.
func healthyHost(t *testing.T) *host {
	h := &host{t: t, root: t.TempDir(), pingVer: "4.9.3"}
	h.write("/sys/fs/cgroup/cgroup.controllers", "cpuset cpu io memory hugetlb pids rdma misc\n")
	h.write(userCgroup, "cpuset cpu io memory pids\n")
	h.write("/run/user/1000/systemd/private", "")
	h.write("/var/lib/systemd/linger/alice", "")
	h.write("/etc/subuid", "root:1:10\nalice:100000:65536\nbob:165536:65536\n")
	h.write("/etc/subgid", "alice:100000:65536\n")
	h.write("/proc/sys/user/max_user_namespaces", "62000\n")
	h.write("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "0\n")
	h.write("/proc/sys/fs/inotify/max_user_instances", "8192\n")
	h.write("/proc/sys/kernel/keys/maxkeys", "20000\n")
	h.write("/proc/key-users", keyUsers("20000"))
	h.write("/proc/sys/kernel/osrelease", "6.8.0-45-generic\n")
	h.write("/proc/swaps", "Filename\t\t\t\tType\t\tSize\t\tUsed\t\tPriority\n")
	h.write("/dev/kvm", "")
	h.binaries = map[string]string{
		"podman":    "podman version 4.9.3\n",
		"crun":      "crun version 1.14.1\ncommit: abc\n",
		"runsc":     "runsc version release-20260101.0\nspec: 1.1.0\n",
		"newuidmap": "",
		"newgidmap": "",
	}
	h.opts = Options{
		Root:     h.root,
		UID:      1000,
		Username: "alice",
		Getenv: func(k string) string {
			if k == "XDG_RUNTIME_DIR" {
				return "/run/user/1000"
			}
			return ""
		},
		Rlimit: func() (uint64, uint64, error) { return 1 << 20, 1 << 20, nil },
	}
	return h
}

func (h *host) run() Report {
	opts := h.opts
	opts.LookPath = func(name string) (string, error) {
		if _, ok := h.binaries[name]; ok {
			return "/usr/bin/" + name, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}
	opts.Run = func(_ context.Context, name string, args ...string) (string, error) {
		out, ok := h.binaries[filepath.Base(name)]
		if ok && filepath.Base(name) == "systemd-run" {
			h.sdrunArgs = args
			if out != "" {
				return out, errors.New("exit status 1")
			}
			return "", nil
		}
		if !ok || len(args) != 1 || args[0] != "--version" {
			return "", errors.New("unexpected command")
		}
		return out, nil
	}
	if opts.PingSocket == nil {
		opts.PingSocket = func(_ context.Context, socket string) (string, error) {
			if socket != "/run/user/1000/podman/podman.sock" && h.opts.PodmanSocket == "" {
				h.t.Errorf("pinged %q", socket)
			}
			return h.pingVer, h.pingErr
		}
	}
	return Run(context.Background(), opts)
}

func TestHealthyHost(t *testing.T) {
	r := healthyHost(t).run()
	t.Logf("\n%s", r)
	if !r.OK() || r.Err() != nil || len(r.Warnings()) != 0 {
		t.Fatalf("healthy host: failed=%v warnings=%v", r.Failed(), r.Warnings())
	}
	if len(r.Checks) != 20 {
		t.Errorf("%d checks, want 20", len(r.Checks))
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c.Name == "" || c.Detail == "" || seen[c.Name] {
			t.Errorf("bad or duplicate check: %+v", c)
		}
		seen[c.Name] = true
		// runsc is installed on this host but not enabled: reported, no more.
		wantInfo := c.Name == CheckKVM || c.Name == CheckRunsc
		if (c.Status == StatusInfo) != wantInfo || (c.Status != StatusOK && !wantInfo) {
			t.Errorf("%s: status %s", c.Name, c.Status)
		}
	}
	for name, want := range map[string]string{
		CheckCgroupDelegation: "delegated: cpuset cpu io memory pids",
		CheckSubUID:           "65536 IDs (100000+65536)",
		CheckPodman:           "/usr/bin/podman: version 4.9.3",
		CheckPodmanSocket:     "/run/user/1000/podman/podman.sock: Libpod API 4.9.3",
		CheckCrun:             "/usr/bin/crun: crun version 1.14.1",
		CheckKernel:           "6.8.0-45-generic",
	} {
		if c, _ := r.Get(name); !strings.Contains(c.Detail, want) {
			t.Errorf("%s detail %q, want it to contain %q", name, c.Detail, want)
		}
	}
}

func TestChecks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		break_ func(h *host)
		check  string
		status Status
		detail string // substring
		fix    string // substring of the remediation
	}{
		{"cgroup v1", func(h *host) { h.remove("/sys/fs/cgroup/cgroup.controllers") },
			CheckCgroupV2, StatusFail, "no unified cgroup v2", "unified_cgroup_hierarchy"},
		{"cpu not delegated", func(h *host) { h.write(userCgroup, "memory pids\n") },
			CheckCgroupDelegation, StatusFail, "not delegated to user 1000: cpu (delegated: memory pids)", "Delegate=cpu cpuset io memory pids"},
		{"nothing delegated", func(h *host) { h.write(userCgroup, "\n") },
			CheckCgroupDelegation, StatusFail, "cpu, memory, pids (delegated: none)", "delegate.conf"},
		{"no user service cgroup", func(h *host) { h.remove("/sys/fs/cgroup/user.slice") },
			CheckCgroupDelegation, StatusFail, "cannot read", "systemd user manager"},
		{"systemd default delegation", func(h *host) { h.write(userCgroup, "cpu memory pids\n") },
			CheckCgroupDelegation, StatusWarn, "not delegated: io, cpuset", "Delegate="},
		{"only io missing", func(h *host) { h.write(userCgroup, "cpuset cpu memory pids\n") },
			CheckCgroupDelegation, StatusWarn, "not delegated: io ", ""},
		{"no XDG_RUNTIME_DIR", func(h *host) { h.opts.Getenv = func(string) string { return "" } },
			CheckRuntimeDir, StatusFail, "XDG_RUNTIME_DIR is not set", "su/sudo"},
		{"XDG_RUNTIME_DIR gone", func(h *host) { h.remove("/run/user/1000") },
			CheckRuntimeDir, StatusFail, "is not a directory", ""},
		{"no user manager", func(h *host) { h.remove("/run/user/1000/systemd") },
			CheckUserSession, StatusFail, "no systemd user manager", "enable-linger alice"},
		{"no linger", func(h *host) { h.remove("/var/lib/systemd/linger/alice") },
			CheckLinger, StatusWarn, "not enabled for alice", "loginctl enable-linger alice"},
		{"no linger, required", func(h *host) { h.remove("/var/lib/systemd/linger/alice"); h.opts.RequireLinger = true },
			CheckLinger, StatusFail, "not enabled for alice", "loginctl enable-linger alice"},
		{"no subuid entry", func(h *host) { h.write("/etc/subuid", "bob:100000:65536\n") },
			CheckSubUID, StatusFail, "no range for alice", "usermod --add-subuids"},
		{"no subuid file", func(h *host) { h.remove("/etc/subuid") },
			CheckSubUID, StatusFail, "cannot read /etc/subuid", ""},
		{"small subuid range", func(h *host) { h.write("/etc/subuid", "alice:100000:1000\n") },
			CheckSubUID, StatusWarn, "1000 IDs", ""},
		{"subuid by uid, two ranges", func(h *host) { h.write("/etc/subuid", "1000:100000:40000\n# c\nalice:200000:40000\nbad line\n") },
			CheckSubUID, StatusOK, "80000 IDs (100000+40000, 200000+40000)", ""},
		{"no subgid entry", func(h *host) { h.write("/etc/subgid", "") },
			CheckSubGID, StatusFail, "no range for alice in /etc/subgid", ""},
		{"no newgidmap", func(h *host) { delete(h.binaries, "newgidmap") },
			CheckIDMapTools, StatusFail, "not found in PATH: newgidmap", "uidmap"},
		{"userns disabled", func(h *host) { h.write("/proc/sys/user/max_user_namespaces", "0\n") },
			CheckUserNamespaces, StatusFail, "max_user_namespaces is 0", "sysctl -w user.max_user_namespaces"},
		{"userns sysctl missing", func(h *host) { h.remove("/proc/sys/user/max_user_namespaces") },
			CheckUserNamespaces, StatusFail, "cannot read", "CONFIG_USER_NS"},
		{"apparmor restriction without profile", func(h *host) { h.write("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1\n") },
			CheckAppArmorUserns, StatusWarn, "no /etc/apparmor.d/podman profile", "apparmor_restrict_unprivileged_userns=0"},
		{"apparmor restriction with profile", func(h *host) {
			h.write("/proc/sys/kernel/apparmor_restrict_unprivileged_userns", "1\n")
			h.write("/etc/apparmor.d/podman", "profile podman /usr/bin/podman flags=(unconfined) { userns, }\n")
			h.write("/etc/apparmor.d/crun", "")
		}, CheckAppArmorUserns, StatusOK, "found for: podman, crun", ""},
		{"no apparmor sysctl", func(h *host) { h.remove("/proc/sys/kernel/apparmor_restrict_unprivileged_userns") },
			CheckAppArmorUserns, StatusOK, "not present", ""},
		{"low inotify", func(h *host) { h.write("/proc/sys/fs/inotify/max_user_instances", "128\n") },
			CheckInotify, StatusWarn, "max_user_instances=128 (< 1024)", "sysctl -w fs.inotify.max_user_instances=8192"},
		{"inotify headroom below the planned pool size", func(h *host) {
			h.opts.PlannedSandboxes = 600
			h.write("/proc/sys/fs/inotify/max_user_instances", "512\n")
			h.opts.InotifyInUse = func() (int64, error) { return 42, nil }
		}, CheckInotify, StatusWarn, "max_user_instances=512, 42 in use by alice: room for 470 more; each running sandbox holds one, and the pools may run 600", "max_total"},
		{"inotify headroom covers the planned pool size", func(h *host) {
			h.opts.PlannedSandboxes = 400
			h.write("/proc/sys/fs/inotify/max_user_instances", "512\n")
			h.opts.InotifyInUse = func() (int64, error) { return 42, nil }
		}, CheckInotify, StatusOK, "room for 470 more", ""},
		{"inotify usage unreadable", func(h *host) {
			h.opts.PlannedSandboxes = 10
			h.opts.InotifyInUse = func() (int64, error) { return 0, errors.New("no /proc") }
		}, CheckInotify, StatusWarn, "cannot count the instances in use", ""},
		{"default key quota", func(h *host) {
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
		}, CheckKeys, StatusWarn, "kernel.keys.maxkeys=200, 6 in use by alice: at most 194 more containers can run for this user", "keyring = false"},
		{"key quota below the planned pool size", func(h *host) {
			h.opts.PlannedSandboxes = 300
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
		}, CheckKeys, StatusWarn, "at most 194 more containers can run for this user (one session keyring each), but the pools may run 300 sandboxes", "sysctl -w kernel.keys.maxkeys"},
		{"key quota covers the planned pool size", func(h *host) {
			h.opts.PlannedSandboxes = 100
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
		}, CheckKeys, StatusOK, "at most 194 more containers can run for this user (one session keyring each); the pools may run 100", ""},
		{"user holds no keys", func(h *host) {
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", "    0:   569 568/568 112/1000000 2391/25000000\n10001: 5 5/5 5/200 9/20000\n")
		}, CheckKeys, StatusWarn, "0 in use by alice: at most 200 more", ""},
		{"key quota exhausted", func(h *host) {
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", " 1000:   201 201/201 201/200 9000/20000\n")
		}, CheckKeys, StatusWarn, "at most 0 more", ""},
		{"keyring disabled in containers.conf", func(h *host) {
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
			h.opts.PlannedSandboxes = 1000
			h.write("/usr/share/containers/containers.conf", "[containers]\n#keyring = true\n[engine]\nkeyring = false\n")
			h.write("/etc/containers/containers.conf.d/10-sbx.conf", "[containers]\n  keyring = false # density\n")
		}, CheckKeys, StatusOK, "/etc/containers/containers.conf.d/10-sbx.conf sets keyring = false, so sandboxes take no key", ""},
		{"keyring re-enabled by a later file", func(h *host) {
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
			h.write("/etc/containers/containers.conf", "[containers]\nkeyring = false\n")
			h.write("/home/alice/.config/containers/containers.conf", "[containers]\nkeyring = true\n")
			get := h.opts.Getenv
			h.opts.Getenv = func(k string) string {
				if k == "HOME" {
					return "/home/alice"
				}
				return get(k)
			}
		}, CheckKeys, StatusWarn, "at most 194 more", "keyring = false"},
		{"no maxkeys sysctl", func(h *host) { h.remove("/proc/sys/kernel/keys/maxkeys") },
			CheckKeys, StatusWarn, "cannot read /proc/sys/kernel/keys/maxkeys", ""},
		{"no key-users", func(h *host) { h.remove("/proc/key-users") },
			CheckKeys, StatusWarn, "cannot read the user's usage", ""},
		{"low nofile soft", func(h *host) { h.opts.Rlimit = func() (uint64, uint64, error) { return 1024, 1048576, nil } },
			CheckNofile, StatusWarn, "soft=1024 hard=1048576", "LimitNOFILE"},
		{"low nofile hard", func(h *host) { h.opts.Rlimit = func() (uint64, uint64, error) { return 1024, 4096, nil } },
			CheckNofile, StatusWarn, "soft=1024 hard=4096", "needs root"},
		{"old kernel", func(h *host) { h.write("/proc/sys/kernel/osrelease", "5.10.0-28-amd64\n") },
			CheckKernel, StatusWarn, "older than 5.13", "5.14"},
		{"kernel 5.13", func(h *host) { h.write("/proc/sys/kernel/osrelease", "5.13.19\n") },
			CheckKernel, StatusWarn, "cgroup.kill needs 5.14", ""},
		{"kernel 5.14", func(h *host) { h.write("/proc/sys/kernel/osrelease", "5.14.0-570.el9.x86_64\n") },
			CheckKernel, StatusOK, "5.14.0-570", ""},
		{"odd kernel string", func(h *host) { h.write("/proc/sys/kernel/osrelease", "weird\n") },
			CheckKernel, StatusWarn, "cannot parse", ""},
		{"no podman", func(h *host) { delete(h.binaries, "podman") },
			CheckPodman, StatusFail, "podman not found", "install podman"},
		{"podman 3", func(h *host) { h.binaries["podman"] = "podman version 3.4.4\n" },
			CheckPodman, StatusFail, "3.4.4 is older than 4.0", ""},
		{"podman 4.3", func(h *host) { h.binaries["podman"] = "podman version 4.3.1\n" },
			CheckPodman, StatusWarn, "4.3.1", ""},
		{"podman 6", func(h *host) { h.binaries["podman"] = "podman version 6.0.1\n" },
			CheckPodman, StatusOK, "version 6.0.1", ""},
		{"socket down", func(h *host) { h.pingErr = errors.New("connect: connection refused") },
			CheckPodmanSocket, StatusFail, "podman.sock: connect: connection refused", "systemctl --user enable --now podman.socket"},
		{"socket without libpod header", func(h *host) { h.pingVer = "" },
			CheckPodmanSocket, StatusWarn, "no Libpod-API-Version", "Docker"},
		{"explicit socket", func(h *host) { h.opts.PodmanSocket = "/tmp/other.sock" },
			CheckPodmanSocket, StatusOK, "/tmp/other.sock: Libpod API 4.9.3", ""},
		{"no crun", func(h *host) { delete(h.binaries, "crun") },
			CheckCrun, StatusFail, "crun not found", "install crun"},
		{"no runsc", func(h *host) { delete(h.binaries, "runsc") },
			CheckRunsc, StatusInfo, "runsc (gVisor) not found", ""},
		{"no runsc, required", func(h *host) { delete(h.binaries, "runsc"); h.opts.RequireRunsc = true },
			CheckRunsc, StatusFail, "runsc (gVisor) not found", "gvisor.dev"},
		{"runsc found, not enabled", func(h *host) {},
			CheckRunsc, StatusInfo, "not enabled (enable the gVisor tier with --runtimes crun,runsc)", ""},
		{"runsc enabled, missing", func(h *host) {
			delete(h.binaries, "runsc")
			h.opts.Runtimes = []Runtime{{Name: "crun"}, {Name: "runsc"}}
		}, CheckRunsc, StatusFail, "runsc (gVisor) not found", "--runtimes crun,runsc=/path/to/runsc"},
		{"runsc enabled, bad path", func(h *host) {
			h.opts.Runtimes = []Runtime{{Name: "crun"}, {Name: "runsc", Path: "/opt/gvisor/runsc"}}
		}, CheckRunsc, StatusFail, "/opt/gvisor/runsc does not exist", ""},
		{"runsc enabled by path", func(h *host) {
			h.write("/opt/gvisor/runsc", "")
			h.binaries["systemd-run"] = ""
			h.opts.Runtimes = []Runtime{{Name: "crun"}, {Name: "runsc", Path: "/opt/gvisor/runsc"}}
		}, CheckRunsc, StatusOK, "/opt/gvisor/runsc: runsc version release-20260101.0; enabled and usable", ""},
		{"runsc enabled, no systemd-run", func(h *host) {
			h.opts.Runtimes = []Runtime{{Name: "crun"}, {Name: "runsc"}}
		}, CheckRunsc, StatusFail, "systemd-run is not in PATH", "systemd"},
		{"runsc enabled, scope refused", func(h *host) {
			h.binaries["systemd-run"] = "Failed to start transient scope unit: Access denied"
			h.opts.Runtimes = []Runtime{{Name: "crun"}, {Name: "runsc"}}
		}, CheckRunsc, StatusFail, "refused a transient scope with limits: exit status 1: Failed to start transient scope unit", "systemd user manager"},
		{"crun by path, missing", func(h *host) { h.opts.Runtimes = []Runtime{{Name: "crun", Path: "/opt/crun"}} },
			CheckCrun, StatusFail, "/opt/crun does not exist", "--runtimes"},
		{"managed service, not probed", func(h *host) {
			h.opts.ManagedPodman, h.opts.SkipServiceProbe = true, true
			h.opts.PingSocket = func(context.Context, string) (string, error) { h.t.Error("the socket was pinged"); return "", nil }
		}, CheckPodmanService, StatusInfo, "sbxd starts its own `podman system service`", ""},
		{"managed service, probe ok", func(h *host) {
			h.opts.ManagedPodman = true
			h.opts.ProbeService = func(context.Context) (string, error) { return "4.9.3", nil }
		}, CheckPodmanService, StatusOK, "starts and answers (Libpod API 4.9.3)", ""},
		{"managed service, probe fails", func(h *host) {
			h.opts.ManagedPodman = true
			h.opts.ProbeService = func(context.Context) (string, error) { return "", errors.New("exited before its socket answered") }
		}, CheckPodmanService, StatusFail, "could not be started: exited before its socket answered", "--podman-socket"},
		{"managed service takes no keys", func(h *host) {
			h.opts.ManagedPodman, h.opts.SkipServiceProbe = true, true
			h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
			h.write("/proc/key-users", keyUsers("200"))
			h.opts.PlannedSandboxes = 5000
		}, CheckKeys, StatusOK, "runs with keyring = false, so sandboxes take no key", ""},
		{"no kvm", func(h *host) { h.remove("/dev/kvm") },
			CheckKVM, StatusInfo, "/dev/kvm is absent", ""},
		{"swap on", func(h *host) {
			h.write("/proc/swaps", "Filename\tType\tSize\tUsed\tPriority\n/swap.img\tfile\t4194300\t0\t-2\n/dev/zram0\tpartition\t1\t0\t5\n")
		}, CheckSwap, StatusWarn, "swap is enabled (/swap.img, /dev/zram0)", "swapoff -a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := healthyHost(t)
			tc.break_(h)
			r := h.run()
			c, found := r.Get(tc.check)
			if !found {
				t.Fatalf("no check %q", tc.check)
			}
			if c.Status != tc.status || !strings.Contains(c.Detail, tc.detail) || !strings.Contains(c.Remediation, tc.fix) {
				t.Errorf("got  %s %q\n     fix %q\nwant %s detail~%q fix~%q", c.Status, c.Detail, c.Remediation, tc.status, tc.detail, tc.fix)
			}
			if c.Status == StatusFail && c.Remediation == "" && tc.fix != "" {
				t.Error("failed check has no remediation")
			}
			// OK() is exactly "no check failed", and warnings never fail it.
			if r.OK() != (tc.status != StatusFail) && tc.name != "no XDG_RUNTIME_DIR" && tc.name != "XDG_RUNTIME_DIR gone" && tc.name != "no user service cgroup" {
				t.Errorf("OK() = %v with %s status %s; failed: %v", r.OK(), tc.check, tc.status, r.Failed())
			}
			if (r.Err() == nil) != r.OK() {
				t.Errorf("Err() = %v but OK() = %v", r.Err(), r.OK())
			}
			if tc.status == StatusFail && !strings.Contains(r.Err().Error(), tc.check) {
				t.Errorf("Err() %q does not name %s", r.Err(), tc.check)
			}
		})
	}
}

func TestReportString(t *testing.T) {
	h := healthyHost(t)
	h.remove("/var/lib/systemd/linger/alice")
	h.write(userCgroup, "memory pids\n")
	out := h.run().String()
	for _, want := range []string{
		"FAIL  cgroup_delegation",
		"-> as root, create /etc/systemd/system/user@.service.d/delegate.conf containing",
		"WARN  linger",
		"-> loginctl enable-linger alice",
		"OK    cgroup_v2",
		"INFO  kvm",
		"20 checks: 1 failed, 1 warnings",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
}

func TestPingSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "sbxpf")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "p.sock")
	ctx := context.Background()
	if _, err := pingSocket(ctx, sock); err == nil {
		t.Error("missing socket accepted")
	}
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pingSocket(ctx, sock); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("regular file: %v", err)
	}
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	status := http.StatusOK
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" {
			t.Errorf("path %q", r.URL.Path)
		}
		w.Header().Set("Libpod-API-Version", "6.0.0")
		w.WriteHeader(status)
	})}
	go srv.Serve(ln)
	defer srv.Close()
	if v, err := pingSocket(ctx, sock); err != nil || v != "6.0.0" {
		t.Errorf("ping: %q %v", v, err)
	}
	status = http.StatusInternalServerError
	if _, err := pingSocket(ctx, sock); err == nil {
		t.Error("500 accepted")
	}
}

func TestParseVersion(t *testing.T) {
	for in, want := range map[string][2]int{
		"6.8.0-45-generic": {6, 8}, "4.9.3": {4, 9}, "5.14": {5, 14}, "7.0.0-28-generic": {7, 0}, "5.15+bpo": {5, 15},
	} {
		if maj, min, ok := parseVersion(in); !ok || maj != want[0] || min != want[1] {
			t.Errorf("parseVersion(%q) = %d.%d %v", in, maj, min, ok)
		}
	}
	for _, in := range []string{"", "six", "6", "x.y"} {
		if _, _, ok := parseVersion(in); ok {
			t.Errorf("parseVersion(%q) accepted", in)
		}
	}
}

// TestRealHost runs the checks against this machine with default options
// and logs the report (go test -v -run TestRealHost). It asserts only
// that the run is well-formed: whether the host passes is the host's
// business, not the test's.
func TestRealHost(t *testing.T) {
	r := Run(context.Background(), Options{})
	t.Logf("real host preflight (OK=%v):\n%s", r.OK(), r)
	if len(r.Checks) != 20 {
		t.Errorf("%d checks, want 20", len(r.Checks))
	}
	for _, c := range r.Checks {
		if c.Name == "" || c.Detail == "" || c.Status == "" {
			t.Errorf("malformed check: %+v", c)
		}
	}
}

func TestCapacityRunsOnlyTheQuotaChecks(t *testing.T) {
	h := healthyHost(t)
	h.write("/proc/sys/kernel/keys/maxkeys", "200\n")
	h.write("/proc/key-users", keyUsers("200"))
	opts := h.opts
	opts.PlannedSandboxes = 250
	opts.InotifyInUse = func() (int64, error) { return 10, nil }
	r := Capacity(context.Background(), opts)
	if len(r.Checks) != 2 || !r.OK() {
		t.Fatalf("capacity report: %+v", r)
	}
	if c, _ := r.Get(CheckKeys); c.Status != StatusWarn || !strings.Contains(c.Detail, "the pools may run 250") {
		t.Errorf("keys: %+v", c)
	}
	if c, _ := r.Get(CheckInotify); c.Status != StatusOK || !strings.Contains(c.Detail, "room for 8182 more") {
		t.Errorf("inotify: %+v", c)
	}
}

// TestInotifyInUseCountsThisProcess: the real /proc scan sees an inotify
// instance this test opens.
func TestInotifyInUseCountsThisProcess(t *testing.T) {
	before, err := inotifyInUse(os.Getuid())
	if err != nil {
		t.Skipf("cannot scan /proc: %v", err)
	}
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		t.Skipf("inotify_init1: %v", err)
	}
	defer syscall.Close(fd)
	after, err := inotifyInUse(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	// Other processes of the user come and go; ours alone guarantees >= 1.
	if after < 1 {
		t.Errorf("inotify instances in use: %d before, %d after opening one", before, after)
	}
	t.Logf("inotify instances held by uid %d: %d", os.Getuid(), after)
}
