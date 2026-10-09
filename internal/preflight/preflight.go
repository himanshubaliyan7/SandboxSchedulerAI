// Package preflight checks, at start-up, that this host can run rootless
// Podman sandboxes with enforced limits. Rootless resource limits fail
// hard and per container when a prerequisite is missing (cgroup v1, an
// undelegated controller, no user session), so the host agent runs these
// checks once and refuses to start instead of failing every create.
//
// Every filesystem root, environment lookup, command lookup and command
// execution is injectable through Options, so the checks are tested
// against fixture directories.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Status is the outcome of one check.
type Status string

const (
	StatusOK   Status = "ok"
	StatusWarn Status = "warn" // works, but degraded or risky; start-up continues
	StatusFail Status = "fail" // a hard requirement is missing; do not start
	StatusInfo Status = "info" // informational only
)

// Check is the result of one named check.
type Check struct {
	Name        string `json:"name"`
	Status      Status `json:"status"`
	Detail      string `json:"detail"`
	Remediation string `json:"remediation,omitempty"`
}

// Report is the result of Run.
type Report struct {
	Checks []Check `json:"checks"`
}

// OK reports whether no hard requirement failed. Warnings do not count.
func (r Report) OK() bool { return len(r.Failed()) == 0 }

// Failed returns the checks with StatusFail.
func (r Report) Failed() []Check { return r.withStatus(StatusFail) }

// Warnings returns the checks with StatusWarn.
func (r Report) Warnings() []Check { return r.withStatus(StatusWarn) }

func (r Report) withStatus(s Status) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.Status == s {
			out = append(out, c)
		}
	}
	return out
}

// Get returns the check with the given name.
func (r Report) Get(name string) (Check, bool) {
	for _, c := range r.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// Err returns nil when OK, otherwise an error naming every failed check.
func (r Report) Err() error {
	failed := r.Failed()
	if len(failed) == 0 {
		return nil
	}
	parts := make([]string, len(failed))
	for i, c := range failed {
		parts[i] = c.Name + ": " + c.Detail
	}
	return errors.New("preflight failed: " + strings.Join(parts, "; "))
}

// String renders the report as aligned text, one check per line, with the
// remediation of every check that is not ok underneath it.
func (r Report) String() string {
	width := 0
	for _, c := range r.Checks {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	var b strings.Builder
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%-4s  %-*s  %s\n", strings.ToUpper(string(c.Status)), width, c.Name, c.Detail)
		if c.Remediation != "" && (c.Status == StatusFail || c.Status == StatusWarn) {
			fmt.Fprintf(&b, "      %-*s  -> %s\n", width, "", c.Remediation)
		}
	}
	fmt.Fprintf(&b, "%d checks: %d failed, %d warnings\n", len(r.Checks), len(r.Failed()), len(r.Warnings()))
	return b.String()
}

// Check names.
const (
	CheckCgroupV2         = "cgroup_v2"
	CheckCgroupDelegation = "cgroup_delegation"
	CheckRuntimeDir       = "xdg_runtime_dir"
	CheckUserSession      = "systemd_user_session"
	CheckLinger           = "linger"
	CheckSubUID           = "subuid"
	CheckSubGID           = "subgid"
	CheckIDMapTools       = "newuidmap"
	CheckUserNamespaces   = "user_namespaces"
	CheckAppArmorUserns   = "apparmor_userns"
	CheckInotify          = "inotify_instances"
	CheckNofile           = "nofile_rlimit"
	CheckKernel           = "kernel_version"
	CheckPodman           = "podman_binary"
	CheckPodmanSocket     = "podman_socket"
	CheckCrun             = "crun"
	CheckRunsc            = "runsc"
	CheckKVM              = "kvm"
	CheckSwap             = "swap"
)

// Options configures Run. The zero value checks the real host as the
// current user.
type Options struct {
	// Root is prepended to every absolute path the checks read
	// (/sys, /proc, /etc, /var, /dev). Default "/".
	Root string
	// UID and Username identify the pool user. Default: the current user.
	// A zero UID means "current user"; running sandboxes as root is not a
	// supported configuration.
	UID      int
	Username string
	// Getenv replaces os.Getenv.
	Getenv func(string) string
	// LookPath replaces exec.LookPath.
	LookPath func(file string) (string, error)
	// Run runs a command and returns its combined output. It is only
	// used for `<binary> --version`.
	Run func(ctx context.Context, name string, args ...string) (string, error)
	// Rlimit returns the soft and hard RLIMIT_NOFILE of this process.
	Rlimit func() (soft, hard uint64, err error)
	// PodmanSocket is the API socket to probe. Default
	// $XDG_RUNTIME_DIR/podman/podman.sock. It is used as given, not
	// prefixed with Root.
	PodmanSocket string
	// PingSocket probes the Podman API socket and returns the Libpod API
	// version it reports. Default: GET /_ping over the Unix socket.
	PingSocket func(ctx context.Context, socket string) (string, error)

	// RequireLinger turns a missing linger file from a warning into a
	// failure. Set it for anything but an interactive development host:
	// without lingering, every sandbox dies when the user's last login
	// session ends.
	RequireLinger bool
	// RequireRunsc makes a missing runsc a failure (a pool uses the
	// gVisor tier). By default it is informational.
	RequireRunsc bool
	// Thresholds; zero means the default.
	MinSubIDs           int64  // default 65536
	MinInotifyInstances int64  // default 1024
	MinNofile           uint64 // default 65536 (soft limit)
}

// Controllers the pool user must (and should) have delegated.
var (
	requiredControllers    = []string{"cpu", "memory", "pids"}
	recommendedControllers = []string{"io", "cpuset"}
)

const delegateFix = "as root, create /etc/systemd/system/user@.service.d/delegate.conf containing `[Service]` and " +
	"`Delegate=cpu cpuset io memory pids`, run `systemctl daemon-reload`, then log the user out and back in (or reboot)"

type env struct {
	Options
	ctx context.Context
}

// Run performs every check and returns the report. It does not modify
// the host.
func Run(ctx context.Context, opts Options) Report {
	e := env{Options: opts, ctx: ctx}
	if e.Root == "" {
		e.Root = "/"
	}
	if e.UID == 0 {
		e.UID = os.Getuid()
	}
	if e.Username == "" {
		if u, err := user.LookupId(strconv.Itoa(e.UID)); err == nil {
			e.Username = u.Username
		}
	}
	if e.Getenv == nil {
		e.Getenv = os.Getenv
	}
	if e.LookPath == nil {
		e.LookPath = exec.LookPath
	}
	if e.Run == nil {
		e.Run = func(ctx context.Context, name string, args ...string) (string, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
			return string(out), err
		}
	}
	if e.Rlimit == nil {
		e.Rlimit = func() (uint64, uint64, error) {
			var l syscall.Rlimit
			err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &l)
			return l.Cur, l.Max, err
		}
	}
	if e.PingSocket == nil {
		e.PingSocket = pingSocket
	}
	if e.MinSubIDs == 0 {
		e.MinSubIDs = 65536
	}
	if e.MinInotifyInstances == 0 {
		e.MinInotifyInstances = 1024
	}
	if e.MinNofile == 0 {
		e.MinNofile = 65536
	}

	return Report{Checks: []Check{
		e.cgroupV2(),
		e.cgroupDelegation(),
		e.runtimeDir(),
		e.userSession(),
		e.linger(),
		e.subID(CheckSubUID, "/etc/subuid"),
		e.subID(CheckSubGID, "/etc/subgid"),
		e.idMapTools(),
		e.userNamespaces(),
		e.appArmorUserns(),
		e.inotify(),
		e.nofile(),
		e.kernel(),
		e.podman(),
		e.podmanSocket(),
		e.crun(),
		e.runsc(),
		e.kvm(),
		e.swap(),
	}}
}

func (e env) path(p string) string { return filepath.Join(e.Root, p) }

func (e env) read(p string) (string, error) {
	b, err := os.ReadFile(e.path(p))
	return strings.TrimSpace(string(b)), err
}

func (e env) exists(p string) bool {
	_, err := os.Stat(e.path(p))
	return err == nil
}

func (e env) readInt(p string) (int64, error) {
	s, err := e.read(p)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(s, 10, 64)
}

func ok(name, detail string) Check   { return Check{Name: name, Status: StatusOK, Detail: detail} }
func info(name, detail string) Check { return Check{Name: name, Status: StatusInfo, Detail: detail} }
func warn(name, detail, fix string) Check {
	return Check{Name: name, Status: StatusWarn, Detail: detail, Remediation: fix}
}
func fail(name, detail, fix string) Check {
	return Check{Name: name, Status: StatusFail, Detail: detail, Remediation: fix}
}

// cgroupV2: rootless Podman supports no resource limits at all on v1.
func (e env) cgroupV2() Check {
	ctrl, err := e.read("/sys/fs/cgroup/cgroup.controllers")
	if err != nil {
		return fail(CheckCgroupV2, "no unified cgroup v2 hierarchy at /sys/fs/cgroup (cgroup.controllers is missing)",
			"boot with systemd.unified_cgroup_hierarchy=1 (or use a distribution release that defaults to cgroup v2)")
	}
	return ok(CheckCgroupV2, "unified hierarchy; root controllers: "+ctrl)
}

// cgroupDelegation: limits only work for controllers systemd delegates to
// the user's service manager.
func (e env) cgroupDelegation() Check {
	p := fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.controllers", e.UID, e.UID)
	raw, err := e.read(p)
	if err != nil {
		return fail(CheckCgroupDelegation, fmt.Sprintf("cannot read %s: %v", p, errText(err)),
			"the user needs a running systemd user manager (log in through PAM/logind or `loginctl enable-linger`), on cgroup v2")
	}
	have := map[string]bool{}
	for _, c := range strings.Fields(raw) {
		have[c] = true
	}
	var missing, missingRec []string
	for _, c := range requiredControllers {
		if !have[c] {
			missing = append(missing, c)
		}
	}
	for _, c := range recommendedControllers {
		if !have[c] {
			missingRec = append(missingRec, c)
		}
	}
	switch {
	case len(missing) > 0:
		return fail(CheckCgroupDelegation,
			fmt.Sprintf("controllers not delegated to user %d: %s (delegated: %s)", e.UID, strings.Join(missing, ", "), orNone(raw)), delegateFix)
	case len(missingRec) > 0:
		return warn(CheckCgroupDelegation,
			fmt.Sprintf("delegated: %s; not delegated: %s (no I/O throttling or CPU pinning)", raw, strings.Join(missingRec, ", ")), delegateFix)
	}
	return ok(CheckCgroupDelegation, "delegated: "+raw)
}

func (e env) runtimeDir() Check {
	dir := e.Getenv("XDG_RUNTIME_DIR")
	fix := "run the agent from a real login session or a systemd user unit, not through su/sudo; `loginctl enable-linger` keeps /run/user/UID alive"
	if dir == "" {
		return fail(CheckRuntimeDir, "XDG_RUNTIME_DIR is not set", fix)
	}
	fi, err := os.Stat(e.path(dir))
	if err != nil || !fi.IsDir() {
		return fail(CheckRuntimeDir, fmt.Sprintf("XDG_RUNTIME_DIR=%s is not a directory", dir), fix)
	}
	return ok(CheckRuntimeDir, dir)
}

// userSession: Podman's systemd cgroup manager talks to the user's
// service manager; its private socket lives in the runtime directory.
func (e env) userSession() Check {
	dir := e.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", e.UID)
	}
	if e.exists(filepath.Join(dir, "systemd", "private")) {
		return ok(CheckUserSession, "systemd user manager is running ("+filepath.Join(dir, "systemd")+")")
	}
	return fail(CheckUserSession, "no systemd user manager for this user ("+filepath.Join(dir, "systemd", "private")+" is missing)",
		"log in through logind (ssh, console, `machinectl shell`) or run `loginctl enable-linger "+e.userOrUID()+"`")
}

func (e env) linger() Check {
	if e.Username == "" {
		return warn(CheckLinger, fmt.Sprintf("cannot resolve a user name for uid %d", e.UID), "")
	}
	if e.exists("/var/lib/systemd/linger/" + e.Username) {
		return ok(CheckLinger, "enabled for "+e.Username)
	}
	detail := "lingering is not enabled for " + e.Username + ": every sandbox is killed when the user's last session ends"
	fix := "loginctl enable-linger " + e.Username
	if e.RequireLinger {
		return fail(CheckLinger, detail, fix)
	}
	return warn(CheckLinger, detail, fix)
}

// subID: the user namespace mapping needs subordinate IDs.
func (e env) subID(name, file string) Check {
	fix := fmt.Sprintf("as root: usermod --add-subuids 100000-165535 --add-subgids 100000-165535 %s; then `podman system migrate`", e.userOrUID())
	raw, err := e.read(file)
	if err != nil {
		return fail(name, fmt.Sprintf("cannot read %s: %v", file, errText(err)), fix)
	}
	var total int64
	var ranges []string
	uid := strconv.Itoa(e.UID)
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Split(strings.TrimSpace(line), ":")
		if len(f) != 3 || (f[0] != uid && (e.Username == "" || f[0] != e.Username)) {
			continue
		}
		start, err1 := strconv.ParseInt(f[1], 10, 64)
		size, err2 := strconv.ParseInt(f[2], 10, 64)
		if err1 != nil || err2 != nil || size <= 0 {
			continue
		}
		total += size
		ranges = append(ranges, fmt.Sprintf("%d+%d", start, size))
	}
	switch {
	case total == 0:
		return fail(name, fmt.Sprintf("no range for %s in %s", e.userOrUID(), file), fix)
	case total < e.MinSubIDs:
		return warn(name, fmt.Sprintf("%d IDs (%s); fewer than %d, so images with high UIDs may not unpack", total, strings.Join(ranges, ", "), e.MinSubIDs), fix)
	}
	return ok(name, fmt.Sprintf("%d IDs (%s)", total, strings.Join(ranges, ", ")))
}

func (e env) idMapTools() Check {
	var missing []string
	for _, tool := range []string{"newuidmap", "newgidmap"} {
		if _, err := e.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fail(CheckIDMapTools, "not found in PATH: "+strings.Join(missing, ", "), "install the uidmap (Debian/Ubuntu) or shadow-utils (Fedora/RHEL) package")
	}
	return ok(CheckIDMapTools, "newuidmap and newgidmap found")
}

func (e env) userNamespaces() Check {
	const p = "/proc/sys/user/max_user_namespaces"
	n, err := e.readInt(p)
	if err != nil {
		return fail(CheckUserNamespaces, fmt.Sprintf("cannot read %s: %v", p, errText(err)), "the kernel must be built with CONFIG_USER_NS")
	}
	if n <= 0 {
		return fail(CheckUserNamespaces, "user.max_user_namespaces is 0: rootless containers cannot start",
			"as root: sysctl -w user.max_user_namespaces=62000 (note: 0 is a common emergency mitigation for kernel CVEs; a rootless fleet cannot use it)")
	}
	return ok(CheckUserNamespaces, fmt.Sprintf("user.max_user_namespaces=%d", n))
}

// appArmorUserns: Ubuntu 23.10+ denies unprivileged user namespaces to
// programs without an AppArmor profile that allows them.
func (e env) appArmorUserns() Check {
	const p = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"
	n, err := e.readInt(p)
	if err != nil {
		return ok(CheckAppArmorUserns, "kernel.apparmor_restrict_unprivileged_userns is not present (no restriction)")
	}
	if n == 0 {
		return ok(CheckAppArmorUserns, "kernel.apparmor_restrict_unprivileged_userns=0")
	}
	var profiles []string
	for _, name := range []string{"podman", "crun", "runc", "runsc"} {
		if e.exists("/etc/apparmor.d/" + name) {
			profiles = append(profiles, name)
		}
	}
	if has(profiles, "podman") {
		return ok(CheckAppArmorUserns, fmt.Sprintf("restriction is on (=%d); AppArmor profiles allowing user namespaces found for: %s", n, strings.Join(profiles, ", ")))
	}
	return warn(CheckAppArmorUserns,
		fmt.Sprintf("kernel.apparmor_restrict_unprivileged_userns=%d and no /etc/apparmor.d/podman profile: user namespace creation may be denied", n),
		"install the distribution's podman package (it ships the profile), add a profile granting `userns,` to the podman binary in use, or as root: sysctl -w kernel.apparmor_restrict_unprivileged_userns=0")
}

// inotify: instances are a per-user limit shared by every sandbox.
func (e env) inotify() Check {
	const p = "/proc/sys/fs/inotify/max_user_instances"
	n, err := e.readInt(p)
	if err != nil {
		return warn(CheckInotify, fmt.Sprintf("cannot read %s: %v", p, errText(err)), "")
	}
	if n < e.MinInotifyInstances {
		return warn(CheckInotify, fmt.Sprintf("fs.inotify.max_user_instances=%d (< %d): shared by all sandboxes of the user, a dense pool will exhaust it", n, e.MinInotifyInstances),
			fmt.Sprintf("as root: sysctl -w fs.inotify.max_user_instances=%d (persist it in /etc/sysctl.d)", e.MinInotifyInstances*8))
	}
	return ok(CheckInotify, fmt.Sprintf("fs.inotify.max_user_instances=%d", n))
}

func (e env) nofile() Check {
	soft, hard, err := e.Rlimit()
	if err != nil {
		return warn(CheckNofile, "cannot read RLIMIT_NOFILE: "+err.Error(), "")
	}
	detail := fmt.Sprintf("soft=%d hard=%d", soft, hard)
	if soft < e.MinNofile {
		fix := fmt.Sprintf("raise the soft limit to at least %d (LimitNOFILE= in the agent's systemd unit, or `ulimit -n`)", e.MinNofile)
		if hard < e.MinNofile {
			fix += fmt.Sprintf("; the hard limit %d is also too low and needs root (limits.conf or DefaultLimitNOFILE)", hard)
		}
		return warn(CheckNofile, detail+fmt.Sprintf(" (soft < %d): the agent holds several descriptors per sandbox, and a sandbox's nofile limit cannot exceed the hard limit", e.MinNofile), fix)
	}
	return ok(CheckNofile, detail)
}

func (e env) kernel() Check {
	rel, err := e.read("/proc/sys/kernel/osrelease")
	if err != nil {
		return warn(CheckKernel, "cannot read /proc/sys/kernel/osrelease: "+errText(err), "")
	}
	major, minor, parsed := parseVersion(rel)
	if !parsed {
		return warn(CheckKernel, "cannot parse kernel release "+strconv.Quote(rel), "")
	}
	switch {
	case less(major, minor, 5, 13):
		return warn(CheckKernel, rel+": older than 5.13, so rootless overlayfs is not native (fuse-overlayfs or vfs: slower starts), and cgroup.kill (5.14) is missing",
			"use a distribution kernel 5.14 or newer")
	case less(major, minor, 5, 14):
		return warn(CheckKernel, rel+": native rootless overlay is available, but cgroup.kill needs 5.14", "use a kernel 5.14 or newer")
	}
	return ok(CheckKernel, rel+" (native rootless overlayfs and cgroup.kill available)")
}

func (e env) podman() Check {
	fix := "install podman 4.4 or newer (Ubuntu 24.04: apt install podman)"
	path, err := e.LookPath("podman")
	if err != nil {
		return fail(CheckPodman, "podman not found in PATH", fix)
	}
	out, err := e.Run(e.ctx, path, "--version")
	if err != nil {
		return fail(CheckPodman, fmt.Sprintf("%s --version failed: %v", path, err), fix)
	}
	ver := lastField(out)
	major, minor, parsed := parseVersion(ver)
	switch {
	case !parsed:
		return warn(CheckPodman, fmt.Sprintf("%s: cannot parse version from %q", path, strings.TrimSpace(out)), "")
	case major < 4:
		return fail(CheckPodman, fmt.Sprintf("%s: version %s is older than 4.0 (Libpod API v4 is required)", path, ver), fix)
	case less(major, minor, 4, 4):
		return warn(CheckPodman, fmt.Sprintf("%s: version %s; 4.4 or newer is recommended", path, ver), fix)
	}
	return ok(CheckPodman, fmt.Sprintf("%s: version %s", path, ver))
}

func (e env) podmanSocket() Check {
	socket := e.PodmanSocket
	if socket == "" {
		dir := e.Getenv("XDG_RUNTIME_DIR")
		if dir == "" {
			return fail(CheckPodmanSocket, "no socket path: XDG_RUNTIME_DIR is not set", "")
		}
		socket = filepath.Join(dir, "podman", "podman.sock")
	}
	ctx, cancel := context.WithTimeout(e.ctx, 10*time.Second)
	defer cancel()
	ver, err := e.PingSocket(ctx, socket)
	if err != nil {
		return fail(CheckPodmanSocket, fmt.Sprintf("%s: %v", socket, errText(err)),
			"systemctl --user enable --now podman.socket (or run `podman system service --time=0 unix://"+socket+"`)")
	}
	if ver == "" {
		return warn(CheckPodmanSocket, socket+" answers /_ping but reports no Libpod-API-Version", "check that the socket belongs to Podman and not to Docker")
	}
	return ok(CheckPodmanSocket, fmt.Sprintf("%s: Libpod API %s", socket, ver))
}

func (e env) crun() Check {
	path, err := e.LookPath("crun")
	if err != nil {
		return fail(CheckCrun, "crun not found in PATH (it is the default OCI runtime)", "install crun (apt install crun / dnf install crun)")
	}
	return ok(CheckCrun, e.versioned(path))
}

func (e env) runsc() Check {
	path, err := e.LookPath("runsc")
	if err != nil {
		detail := "runsc (gVisor) not found: the hardened no-KVM tier is unavailable; crun sandboxes share the host kernel"
		fix := "install gVisor (https://gvisor.dev/docs/user_guide/install/) and register it under [engine.runtimes] in containers.conf"
		if e.RequireRunsc {
			return fail(CheckRunsc, detail, fix)
		}
		return info(CheckRunsc, detail)
	}
	return ok(CheckRunsc, e.versioned(path))
}

// versioned returns "path: <first line of --version>".
func (e env) versioned(path string) string {
	out, err := e.Run(e.ctx, path, "--version")
	if err != nil {
		return path + " (version unknown: " + err.Error() + ")"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	return path + ": " + strings.TrimSpace(line)
}

func (e env) kvm() Check {
	if e.exists("/dev/kvm") {
		return info(CheckKVM, "/dev/kvm is present (a microVM tier is possible on this host; nothing here needs it)")
	}
	return info(CheckKVM, "/dev/kvm is absent (not needed; a microVM tier would not be possible)")
}

// swap: sandboxes get memory.swap.max=0, but host swap still lets the
// host itself swap under pressure, which makes latency unpredictable.
func (e env) swap() Check {
	raw, err := e.read("/proc/swaps")
	if err != nil {
		return warn(CheckSwap, "cannot read /proc/swaps: "+errText(err), "")
	}
	var devs []string
	for i, line := range strings.Split(raw, "\n") {
		if f := strings.Fields(line); i > 0 && len(f) > 0 {
			devs = append(devs, f[0])
		}
	}
	if len(devs) == 0 {
		return ok(CheckSwap, "no swap enabled")
	}
	return warn(CheckSwap, "swap is enabled ("+strings.Join(devs, ", ")+"): sandboxes cannot use it (memory.swap.max=0), but the host can, which hides memory pressure behind latency",
		"as root: swapoff -a and remove the swap entries from /etc/fstab on dedicated sandbox hosts")
}

func (e env) userOrUID() string {
	if e.Username != "" {
		return e.Username
	}
	return strconv.Itoa(e.UID)
}

// pingSocket is the default Options.PingSocket.
func pingSocket(ctx context.Context, socket string) (string, error) {
	if fi, err := os.Stat(socket); err != nil {
		return "", err
	} else if fi.Mode()&os.ModeSocket == 0 {
		return "", errors.New("not a socket")
	}
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://d/_ping", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("/_ping returned %s", resp.Status)
	}
	return resp.Header.Get("Libpod-API-Version"), nil
}

// parseVersion extracts major.minor from strings like "6.8.0-45-generic",
// "4.9.3" or "5.14".
func parseVersion(s string) (major, minor int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	digits := parts[1]
	for i, r := range digits {
		if r < '0' || r > '9' {
			digits = digits[:i]
			break
		}
	}
	minor, err = strconv.Atoi(digits)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

func less(major, minor, wantMajor, wantMinor int) bool {
	return major < wantMajor || (major == wantMajor && minor < wantMinor)
}

func lastField(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	f := strings.Fields(line)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// errText drops the fixture/host path prefix noise from *PathError.
func errText(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}
