// Package runscshim is the program rootless Podman runs as the OCI
// runtime "runsc". It sits between conmon and gVisor's runsc and does the
// three things runsc cannot do for itself under a rootless engine:
//
//   - Resource limits. runsc applies limits either through the system
//     D-Bus (--systemd-cgroup, which a non-root user may not use:
//     "Interactive authentication required") or by writing the cgroup
//     tree from its root (which a non-root user may not either). The shim
//     therefore runs `runsc create` inside a transient scope of the
//     user's systemd manager that carries the limits of the OCI spec
//     (memory, swap, pids, CPU quota, and the raw cgroup v2 keys of
//     "unified"), and tells runsc to leave cgroups alone
//     (--ignore-cgroups). The sandbox and gofer processes that `create`
//     leaves behind stay in that scope, so the limits hold for the whole
//     sandbox from its first instruction, and the scope has the name and
//     place Podman expects (libpod-<id>.scope), so Podman reports its
//     path and conmon watches its memory.events as for any container.
//   - Flags. Podman's runtime table holds a path and nothing else. The
//     shim adds the runsc flags this product depends on (--host-uds=all
//     for the agent and egress sockets in the bind-mounted run dir,
//     --network=none) and any the operator configured.
//   - Arguments runsc does not know: --systemd-cgroup, and conmon's
//     --no-new-keyring (runsc creates no session keyring to begin with).
//
// It is invoked through a generated wrapper script, as
//
//	sbxd runsc-shim --runsc /path/to/runsc [--flag=<runsc flag>]... -- <OCI runtime arguments>
package runscshim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultFlags are the runsc flags every sandbox of this product needs.
// Flags configured by the operator follow them on the command line and so
// override them.
var DefaultFlags = []string{
	// Limits are the scope's; see the package comment.
	"--ignore-cgroups",
	// The agent creates its socket in the bind-mounted run dir and the
	// host connects to it (create), and the host creates the egress
	// socket there and the agent connects to it (open).
	"--host-uds=all",
	// No interface but loopback, in gVisor's own network stack. Podman
	// already hands over an empty network namespace; this makes the
	// sandbox not even look at it.
	"--network=none",
}

// SystemdRun is the systemd-run binary; a variable for tests.
var SystemdRun = "systemd-run"

// valueFlags are the runsc top-level flags that take their value as the
// next argument when written without '='. Only those Podman and conmon
// are known to pass that way need to be here.
var valueFlags = map[string]bool{"log": true, "root": true, "log-format": true, "debug-log": true, "panic-log": true}

// invocation is a parsed OCI runtime command line.
type invocation struct {
	globals []string // top-level flags, as given
	sub     string   // create, start, kill, delete, state, ...
	args    []string // everything after the subcommand
}

func parseOCIArgs(args []string) invocation {
	var inv invocation
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			inv.sub, inv.args = a, args[i+1:]
			return inv
		}
		if a == "--" {
			if i+1 < len(args) {
				inv.sub, inv.args = args[i+1], args[i+2:]
			}
			return inv
		}
		inv.globals = append(inv.globals, a)
		name := strings.TrimLeft(a, "-")
		if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
			i++
			inv.globals = append(inv.globals, args[i])
		}
	}
	return inv
}

// dropFlag removes a boolean flag (in its -f, --f, --f=true forms).
func dropFlag(args []string, name string) []string {
	out := args[:0:0]
	for _, a := range args {
		n, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && n == name {
			continue
		}
		out = append(out, a)
	}
	return out
}

// flagValue returns the value of a string flag of a subcommand
// ("--bundle X", "--bundle=X", "-bundle X").
func flagValue(args []string, name string) string {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			continue
		}
		n, v, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if n != name {
			continue
		}
		if hasValue {
			return v
		}
		if i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// ociSpec is the part of the bundle's config.json the shim reads.
type ociSpec struct {
	Linux *struct {
		CgroupsPath string `json:"cgroupsPath"`
		Resources   *struct {
			Memory *struct {
				Limit *int64 `json:"limit"`
				Swap  *int64 `json:"swap"` // memory + swap
			} `json:"memory"`
			CPU *struct {
				Quota  *int64  `json:"quota"`
				Period *uint64 `json:"period"`
			} `json:"cpu"`
			Pids *struct {
				Limit int64 `json:"limit"`
			} `json:"pids"`
			Unified map[string]string `json:"unified"`
		} `json:"resources"`
	} `json:"linux"`
}

// scope is the transient systemd scope a sandbox is created in.
type scope struct {
	Unit       string   // libpod-<id>.scope
	Slice      string   // user.slice
	Properties []string // systemd-run -p values
	Unified    []string // key=value, written to the scope's cgroup directly
}

// scopeFor derives the scope from the OCI spec. cgroupsPath has the
// systemd form "slice:prefix:name" that Podman's systemd cgroup manager
// produces; anything else means the service is not using that manager,
// and then the limits could not be applied at all, which is an error and
// never a sandbox without limits.
func scopeFor(spec ociSpec) (scope, error) {
	var sc scope
	if spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return sc, errors.New("the OCI spec has no cgroupsPath: the Podman service must use the systemd cgroup manager with cgroups enabled")
	}
	parts := strings.Split(spec.Linux.CgroupsPath, ":")
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return sc, fmt.Errorf("cgroupsPath %q is not of the systemd form slice:prefix:name: the Podman service must use the systemd cgroup manager", spec.Linux.CgroupsPath)
	}
	sc.Slice, sc.Unit = parts[0], parts[1]+"-"+parts[2]+".scope"
	if sc.Slice == "" {
		sc.Slice = "user.slice"
	}
	for _, s := range []string{sc.Slice, sc.Unit} {
		if strings.ContainsAny(s, "/ \t\n") {
			return sc, fmt.Errorf("cgroupsPath %q does not name a systemd unit", spec.Linux.CgroupsPath)
		}
	}
	// Delegate: the cgroup belongs to the sandbox's runtime, systemd must
	// not rearrange it. The accounting switches make the controllers'
	// files exist even for a limit that is not set.
	sc.Properties = []string{"Delegate=yes", "MemoryAccounting=yes", "TasksAccounting=yes", "CPUAccounting=yes"}
	res := spec.Linux.Resources
	if res == nil {
		return sc, nil
	}
	if m := res.Memory; m != nil && m.Limit != nil && *m.Limit > 0 {
		sc.Properties = append(sc.Properties, "MemoryMax="+strconv.FormatInt(*m.Limit, 10))
		if m.Swap != nil && *m.Swap >= *m.Limit {
			// The OCI value is memory plus swap.
			sc.Properties = append(sc.Properties, "MemorySwapMax="+strconv.FormatInt(*m.Swap-*m.Limit, 10))
		}
	}
	if p := res.Pids; p != nil && p.Limit > 0 {
		sc.Properties = append(sc.Properties, "TasksMax="+strconv.FormatInt(p.Limit, 10))
	}
	if c := res.CPU; c != nil && c.Quota != nil && *c.Quota > 0 {
		period := uint64(100_000)
		if c.Period != nil && *c.Period > 0 {
			period = *c.Period
		}
		// CPUQuota is a percentage of one CPU; systemd keeps two decimals.
		pct := float64(*c.Quota) * 100 / float64(period)
		sc.Properties = append(sc.Properties,
			"CPUQuota="+strconv.FormatFloat(pct, 'f', 2, 64)+"%",
			"CPUQuotaPeriodSec="+strconv.FormatUint(period, 10)+"us")
	}
	for k, v := range res.Unified {
		if k == "" || strings.ContainsAny(k, "/=\x00") || strings.HasPrefix(k, ".") || strings.ContainsAny(v, "\x00\n") {
			return sc, fmt.Errorf("unified cgroup key %q cannot be applied", k)
		}
		sc.Unified = append(sc.Unified, k+"="+v)
	}
	return sc, nil
}

// Main runs the shim with the arguments after "runsc-shim" and returns an
// exit status; on success it does not return (it becomes runsc).
func Main(args []string, stderr io.Writer) int {
	err := run(args)
	fmt.Fprintf(stderr, "sbxd runsc-shim: %v\n", err)
	return 125
}

func run(args []string) error {
	var (
		runsc   string
		flags   []string
		inScope bool
		unified []string
	)
	i := 0
loop:
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			i++
			break loop
		case a == "--runsc" && i+1 < len(args):
			i++
			runsc = args[i]
		case strings.HasPrefix(a, "--flag="):
			flags = append(flags, strings.TrimPrefix(a, "--flag="))
		case a == "--in-scope":
			inScope = true
		case strings.HasPrefix(a, "--unified="):
			unified = append(unified, strings.TrimPrefix(a, "--unified="))
		default:
			return fmt.Errorf("unexpected argument %q before --", a)
		}
	}
	rest := args[i:]
	if !filepath.IsAbs(runsc) {
		return fmt.Errorf("--runsc must be the absolute path of the runsc binary, got %q", runsc)
	}

	if inScope {
		// Second stage: this process is the scope's first member. Apply
		// the raw cgroup keys, then become runsc.
		if err := writeUnified(unified); err != nil {
			return err
		}
		return execve(runsc, rest)
	}

	inv := parseOCIArgs(rest)
	if inv.sub == "" {
		// `runsc --version` and the like.
		return execve(runsc, rest)
	}
	globals := dropFlag(inv.globals, "systemd-cgroup")
	argv := append(append([]string{}, DefaultFlags...), flags...)
	argv = append(argv, globals...)
	argv = append(argv, inv.sub)

	if inv.sub != "create" && inv.sub != "run" {
		return execve(runsc, append(argv, inv.args...))
	}
	subArgs := dropFlag(inv.args, "no-new-keyring")
	argv = append(argv, subArgs...)

	bundle := flagValue(subArgs, "bundle")
	if bundle == "" {
		bundle = "."
	}
	raw, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return fmt.Errorf("read the OCI spec: %w", err)
	}
	var spec ociSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(bundle, "config.json"), err)
	}
	sc, err := scopeFor(spec)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	sdrun, err := exec.LookPath(SystemdRun)
	if err != nil {
		return fmt.Errorf("systemd-run is needed to give a gVisor sandbox its resource limits: %w", err)
	}
	// --collect: a scope that ended in failure is forgotten instead of
	// blocking its name.
	cmd := []string{sdrun, "--user", "--scope", "--quiet", "--collect", "--unit=" + sc.Unit, "--slice=" + sc.Slice}
	for _, p := range sc.Properties {
		cmd = append(cmd, "-p", p)
	}
	cmd = append(cmd, "--", self, "runsc-shim", "--runsc", runsc, "--in-scope")
	for _, u := range sc.Unified {
		cmd = append(cmd, "--unified="+u)
	}
	cmd = append(cmd, "--")
	cmd = append(cmd, argv...)
	return execve(sdrun, cmd[1:])
}

// writeUnified writes raw cgroup v2 "file=value" pairs into the cgroup
// this process is in.
func writeUnified(pairs []string) error {
	if len(pairs) == 0 {
		return nil
	}
	dir, err := ownCgroupDir()
	if err != nil {
		return err
	}
	for _, kv := range pairs {
		k, v, _ := strings.Cut(kv, "=")
		if k == "" || strings.ContainsAny(k, "/\x00") || strings.HasPrefix(k, ".") {
			return fmt.Errorf("unified cgroup key %q cannot be applied", k)
		}
		f, err := os.OpenFile(filepath.Join(dir, k), os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("set %s: %w", k, err)
		}
		_, werr := f.WriteString(v)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			return fmt.Errorf("set %s=%s: %w", k, v, errors.Join(werr, cerr))
		}
	}
	return nil
}

// ownCgroupDir is the cgroup v2 directory of this process.
func ownCgroupDir() (string, error) {
	raw, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok {
			return filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+p)), nil
		}
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup")
}

// execve replaces this process; it only returns on failure.
func execve(path string, args []string) error {
	err := syscall.Exec(path, append([]string{path}, args...), os.Environ())
	return fmt.Errorf("exec %s: %w", path, err)
}
