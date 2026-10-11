package podman

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// The types below are the subset of Libpod's SpecGenerator
// (pkg/specgen/specgen.go) that this backend sends to
// POST /libpod/containers/create. Every JSON name was checked against the
// SpecGenerator source on both the v4.9 branch and main (6.x); fields that
// exist on only one of them are not used. Security-relevant booleans carry
// no omitempty on purpose: the server seeds some of them (privileged, for
// one) from containers.conf before decoding, so they are always sent.
//
// Two things a sandbox gets from the Podman service cannot be changed from
// this request, on v4.9 or on main; both are the service's containers.conf
// (read when the service starts). That is why sbxd runs a Podman service
// of its own by default, with a generated file that sets both (Service,
// ContainersConf); what follows describes a service it does not manage,
// where package preflight can only report them:
//
//   - The session keyring. Libpod passes --no-new-keyring to the OCI
//     runtime only when containers.conf has `keyring = false`
//     (ConmonOCIRuntime.enableKeyring); the SpecGenerator has no field for
//     it. Each running container therefore holds one key of the user's
//     kernel.keys.maxkeys quota (200 by default), which caps the number of
//     running sandboxes per user before memory does.
//   - The default sysctls. containers.conf `default_sysctls` (as shipped:
//     net.ipv4.ping_group_range=0 0) is added to every container whose
//     network namespace is not the host's, a pod's or another container's,
//     so also to these, which use "none". The "sysctl" field can replace a
//     value but cannot remove the entry, so crun always writes
//     /proc/sys/net/ipv4/ping_group_range at start. That write was seen to
//     fail with EINVAL in 2 of 640 concurrent starts; the pool retries a
//     failed create for that reason (pool.Config.CreateAttempts).
type createSpec struct {
	Name       string            `json:"name"`
	Image      string            `json:"image"`
	Entrypoint []string          `json:"entrypoint"`
	Command    []string          `json:"command"`
	Env        map[string]string `json:"env"`
	WorkDir    string            `json:"work_dir"`
	Hostname   string            `json:"hostname"`
	Labels     map[string]string `json:"labels"`

	Terminal  bool `json:"terminal"`
	Stdin     bool `json:"stdin"`
	Remove    bool `json:"remove"`
	EnvHost   bool `json:"env_host"`
	HTTPProxy bool `json:"httpproxy"`
	Init      bool `json:"init"`

	StopTimeout      uint      `json:"stop_timeout"`
	RestartPolicy    string    `json:"restart_policy"`
	LogConfiguration logConfig `json:"log_configuration"`
	OCIRuntime       string    `json:"oci_runtime"`

	ImageVolumeMode string      `json:"image_volume_mode"`
	Mounts          []ociMount  `json:"mounts"`
	ShmSize         int64       `json:"shm_size"`
	User            string      `json:"user"`
	Privileged      bool        `json:"privileged"`
	CapAdd          []string    `json:"cap_add"`
	CapDrop         []string    `json:"cap_drop"`
	NoNewPrivileges bool        `json:"no_new_privileges"`
	SeccompProfile  string      `json:"seccomp_profile_path,omitempty"`
	UserNS          namespace   `json:"userns"`
	IDMappings      *idMappings `json:"idmappings,omitempty"`
	// PasswdEntry and GroupEntry are the lines Libpod writes to the
	// container's /etc/passwd and /etc/group for User; see SandboxUser.
	PasswdEntry string `json:"passwd_entry"`
	GroupEntry  string `json:"group_entry"`
	// Mask is added to Libpod's default masked paths.
	Mask []string `json:"mask"`

	ReadOnlyFilesystem bool `json:"read_only_filesystem"`
	ReadWriteTmpfs     bool `json:"read_write_tmpfs"`

	PidNS    namespace `json:"pidns"`
	IpcNS    namespace `json:"ipcns"`
	UtsNS    namespace `json:"utsns"`
	CgroupNS namespace `json:"cgroupns"`
	NetNS    namespace `json:"netns"`

	CgroupsMode    string         `json:"cgroups_mode"`
	ResourceLimits resourceLimits `json:"resource_limits"`
	Rlimits        []rlimit       `json:"r_limits"`
}

// namespace is specgen.Namespace.
type namespace struct {
	NSMode string `json:"nsmode"`
	Value  string `json:"value,omitempty"`
}

// logConfig is specgen.LogConfig.
type logConfig struct {
	Driver string `json:"driver"`
}

// ociMount is the OCI runtime-spec Mount.
type ociMount struct {
	Destination string   `json:"destination"`
	Type        string   `json:"type"`
	Source      string   `json:"source"`
	Options     []string `json:"options"`
}

// resourceLimits is the OCI runtime-spec LinuxResources subset in use.
type resourceLimits struct {
	CPU    cpuLimits    `json:"cpu"`
	Memory memoryLimits `json:"memory"`
	Pids   pidsLimits   `json:"pids"`
	// Unified is raw cgroup v2 "file: value" pairs. This is where
	// `podman run --cgroup-conf` ends up. The SpecGenerator's own
	// top-level "unified" field is not it: the CLI copies that into
	// here on the client side and the API server ignores it (verified
	// on Podman 4.9.3, where it is accepted and silently has no effect).
	Unified map[string]string `json:"unified"`
}

type cpuLimits struct {
	Quota  int64  `json:"quota"`
	Period uint64 `json:"period"`
}

type memoryLimits struct {
	Limit int64 `json:"limit"`
	// Swap is memory+swap in the OCI spec (it is the cgroup v1
	// memsw value), not swap alone.
	Swap int64 `json:"swap"`
}

type pidsLimits struct {
	Limit int64 `json:"limit"`
}

// rlimit is the OCI runtime-spec POSIXRlimit. Libpod upper-cases Type and
// prefixes RLIMIT_ itself.
type rlimit struct {
	Type string `json:"type"`
	Hard uint64 `json:"hard"`
	Soft uint64 `json:"soft"`
}

// idMappings is containers/storage types.IDMappingOptions, which has no
// JSON tags, hence the Go field names on the wire.
type idMappings struct {
	HostUIDMapping bool `json:"HostUIDMapping"`
	HostGIDMapping bool `json:"HostGIDMapping"`
	AutoUserNs     bool `json:"AutoUserNs,omitempty"`
}

const (
	cpuPeriodUsec = 100_000
	// smallTmpfsBytes caps the two incidental tmpfs mounts (/run and
	// /var/tmp); see BuildCreateSpec.
	smallTmpfsBytes = 1 << 20
)

// PycachePrefix is where Python keeps bytecode in a sandbox.
const PycachePrefix = "/tmp/pycache"

// DefaultEnv is the environment every sandbox starts with, on top of the
// image's own ENV (Libpod applies the image's variables first, so these
// win over an image that sets the same names). It is the one place where
// sandbox defaults are set, and it is here, in the create spec, rather
// than in the in-sandbox agent, because each value is a consequence of
// this spec: which paths are writable is decided by the mounts below, and
// a backend with another layout would need other values. The agent
// inherits them as PID 1 and starts every process from its own
// environment, with the session env (POST /v1/sandboxes "env") and then
// the per-command env layered on top, so a caller's value for any of
// these names still wins.
//
//   - HOME and TMPDIR point at the two writable, exec-capable tmpfs
//     mounts. Tools that keep per-user state or caches (pip, npm, git)
//     put them under HOME, i.e. in the work directory, which is the larger
//     of the two; XDG_CACHE_HOME, PIP_CACHE_DIR and npm_config_cache are
//     deliberately left at those defaults instead of being moved to /tmp,
//     whose cap is smaller and which build tools also need.
//   - PYTHONPYCACHEPREFIX: with a read-only root Python cannot write
//     __pycache__ next to the standard library, and the official images
//     ship no .pyc files, so without this every interpreter recompiles
//     every module it imports (measured: 30 stdlib imports 1153 ms
//     instead of 173 ms, interpreter start 24.7 ms instead of 16.8 ms).
//     With the prefix the first import of a module in a sandbox compiles
//     it once and later processes load the cached bytecode. It is under
//     /tmp, not the work directory, so that the cache does not show up
//     among the user's files; it counts against the /tmp cap, and Python
//     ignores a failed bytecode write, so a full /tmp costs speed, not
//     correctness. The cost: an image that ships precompiled __pycache__
//     directories has them ignored (the prefix replaces the lookup, it
//     does not add to it); callers of such a template can pass
//     PYTHONPYCACHEPREFIX="" in the sandbox env, which Python treats as
//     unset.
//   - PYTHONUNBUFFERED=1: a command's stdout is a pipe to the agent, and
//     Python block-buffers a pipe, so without this a caller streaming the
//     output of `python3 -m http.server` or of a long-running script sees
//     nothing until 8 KiB have accumulated or the process ends, which is
//     not what someone watching a stream expects. The cost is one write
//     per print instead of one per 8 KiB (measured in the README: a
//     print-heavy loop is slower, most of all under gVisor); a caller who
//     wants the buffering back passes PYTHONUNBUFFERED="" in the sandbox
//     or command env, which Python treats as unset.
//   - PATH is the image's own PATH with UserBinDir in front. The root is
//     read-only and the process is uid 1000, so `pip install` falls back
//     to a user install under HOME (/workspace/.local), and the console
//     scripts it writes land in /workspace/.local/bin; without this entry
//     an installed tool is not found by name. The image's PATH is passed
//     in by the backend, which reads it from the image (Backend.Create);
//     it has to be, because a value set here replaces the image's
//     variable instead of extending it, and images do extend PATH (Go,
//     Rust and conda images, for example).
func DefaultEnv(imagePath string) map[string]string {
	if imagePath == "" {
		imagePath = StandardPath
	}
	return map[string]string{
		"HOME":                runtime.Workdir,
		"TMPDIR":              "/tmp",
		"PYTHONPYCACHEPREFIX": PycachePrefix,
		"PYTHONUNBUFFERED":    "1",
		"PATH":                UserBinDir + ":" + imagePath,
	}
}

// UserBinDir is where per-user installs put their programs when HOME is
// the work directory (`pip install --user`, pipx, cargo and others follow
// the same ~/.local/bin convention).
const UserBinDir = runtime.Workdir + "/.local/bin"

// StandardPath is the PATH Podman gives a container whose image sets
// none; DefaultEnv extends it when the image's own is not known.
const StandardPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// SandboxUser is the name of the sandbox user and group inside every
// sandbox, whatever the host account is called.
//
// Left to itself, Libpod names the container user after the host: with
// the keep-id mapping it copies the service user's passwd entry (login
// name and full name) into the container's /etc/passwd, so `whoami`,
// `id` and getpwuid() in a sandbox print the operator's account name to
// untrusted code. passwd_entry and group_entry replace that line with a
// fixed one. The home directory is the work directory (as HOME in
// DefaultEnv) and the shell is /bin/sh, which every supported image has.
const SandboxUser = "sandbox"

// maskedPaths are hidden from a sandbox in addition to Libpod's default
// masked paths: the DMI tables name the host's vendor, model and serial
// numbers, which identify the machine and are of no use to sandboxed
// code. (Under runsc /sys is gVisor's own and has no such directory; the
// entry is then ignored.)
var maskedPaths = []string{dmiPath}

const dmiPath = "/sys/devices/virtual/dmi"

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,200}$`)

// ContainerName is the Podman container name of a sandbox. It does not
// include the host id: sandbox ids carry 64 random bits, so two schedulers
// sharing a Podman user do not collide in practice, and if they ever did
// the second create would fail with a name conflict (and be retried under
// a new id) without touching the first scheduler's container, because
// ownership is decided by the sbx.host_id label and never by the name.
func ContainerName(sandboxID string) string { return "sbx-" + sandboxID }

// BuildCreateSpec maps a runtime.Spec to the hardened Libpod create
// request. It is the one place where the sandbox's isolation properties
// are decided; it returns a value that marshals to the request body.
//
// cfg must have been through New (defaults applied).
func BuildCreateSpec(cfg Config, spec runtime.Spec) (any, error) {
	return buildCreateSpec(cfg, spec)
}

func buildCreateSpec(cfg Config, spec runtime.Spec) (*createSpec, error) {
	tpl := spec.Template
	res := tpl.Resources
	if !nameRE.MatchString(spec.SandboxID) {
		return nil, fmt.Errorf("podman: invalid sandbox id %q", spec.SandboxID)
	}
	// Every instance must say whose it is: the reconciler of another
	// scheduler on the same Podman user would otherwise have to guess.
	if err := runtime.ValidHostID(spec.HostID); err != nil {
		return nil, fmt.Errorf("podman: %w", err)
	}
	if tpl.Image == "" {
		return nil, errors.New("podman: template has no image")
	}
	if !filepath.IsAbs(spec.HostRunDir) {
		return nil, fmt.Errorf("podman: HostRunDir %q must be an absolute path", spec.HostRunDir)
	}
	command := AgentCommand
	mountAgent := true
	if cfg.Command != nil {
		command = cfg.Command
		mountAgent = spec.AgentBinaryPath != ""
	}
	if mountAgent && !filepath.IsAbs(spec.AgentBinaryPath) {
		return nil, fmt.Errorf("podman: AgentBinaryPath %q must be an absolute path", spec.AgentBinaryPath)
	}
	if len(command) == 0 {
		return nil, errors.New("podman: empty container command")
	}
	if res.CPUs <= 0 || math.IsNaN(res.CPUs) || math.IsInf(res.CPUs, 0) ||
		res.MemoryBytes <= 0 || res.PidsLimit <= 0 ||
		res.WorkdirBytes <= 0 || res.TmpBytes <= 0 || res.NofileLimit <= 0 {
		return nil, fmt.Errorf("podman: every resource limit must be set and positive: %+v", res)
	}
	// tmpfs pages are charged to the memory cgroup. If the two caps did
	// not fit inside the memory limit, filling the "disk" would OOM-kill
	// the sandbox instead of returning ENOSPC.
	if res.WorkdirBytes > res.MemoryBytes-res.TmpBytes {
		return nil, fmt.Errorf("podman: workdir (%d) + tmp (%d) tmpfs caps exceed the memory limit (%d)",
			res.WorkdirBytes, res.TmpBytes, res.MemoryBytes)
	}
	switch tpl.Network {
	case "", core.NetworkNone, core.NetworkAllowlist, core.NetworkFull:
	default:
		return nil, fmt.Errorf("podman: unknown network mode %q", tpl.Network)
	}
	ociRuntime := tpl.Runtime
	if ociRuntime == "" {
		ociRuntime = cfg.DefaultRuntime
	}
	allowed := false
	for _, r := range cfg.Runtimes {
		allowed = allowed || r == ociRuntime
	}
	if !allowed {
		return nil, fmt.Errorf("podman: runtime %q is not enabled on this backend (enabled: %v)", ociRuntime, cfg.Runtimes)
	}

	user := fmt.Sprintf("%d:%d", cfg.ContainerUID, cfg.ContainerGID)

	s := &createSpec{
		Name:  ContainerName(spec.SandboxID),
		Image: tpl.Image,

		// The image's ENTRYPOINT and CMD are both replaced: a non-empty
		// entrypoint suppresses the image CMD in Libpod's makeCommand.
		Entrypoint: command[:1],
		Command:    command[1:],
		// The host environment and host proxy variables are never
		// forwarded (env_host, httpproxy): the proxy is configured per
		// session by the agent, and host variables may hold secrets.
		// PATH is derived from the image's own; Backend.Create replaces
		// this default with it (imageEnvPath).
		Env:      DefaultEnv(""),
		WorkDir:  runtime.Workdir,
		Hostname: DefaultHostname,

		// Labels let a restarted host agent rebuild its view from the
		// runtime alone and find orphans.
		Labels: map[string]string{
			runtime.LabelManaged:    "true",
			runtime.LabelHostID:     spec.HostID,
			runtime.LabelSandboxID:  spec.SandboxID,
			runtime.LabelGeneration: strconv.FormatInt(spec.Generation, 10),
			runtime.LabelAgentEpoch: spec.AgentEpoch,
			runtime.LabelTemplateID: tpl.ID,
		},

		Terminal: false, Stdin: false, EnvHost: false, HTTPProxy: false,
		// Removal is the scheduler's explicit Destroy, so that a sandbox
		// that died is still there to be inspected (OOMKilled) first.
		Remove: false,
		// No Podman-injected init: the agent is PID 1 and reaps children.
		Init: false,
		// Untrusted code gets no grace period: stop means SIGKILL.
		StopTimeout:   0,
		RestartPolicy: "no",
		// Output is streamed and recorded by the agent. A Podman log
		// would be a second, unbounded copy on the host disk, and "none"
		// is also one of the documented start-latency levers.
		LogConfiguration: logConfig{Driver: "none"},
		// Sent for the record and for Podman versions that honour it;
		// 4.9.3 does not, see Config.RuntimeSockets.
		OCIRuntime: ociRuntime,

		// VOLUME declarations in the image would otherwise become
		// anonymous volumes: writable, uncapped host disk.
		ImageVolumeMode: "ignore",
		ShmSize:         cfg.ShmBytes,

		// Non-root inside the container, no capabilities in any set, and
		// no way to regain them through setuid binaries or file caps.
		User:            user,
		PasswdEntry:     fmt.Sprintf("%s:x:%d:%d:%s:%s:/bin/sh", SandboxUser, cfg.ContainerUID, cfg.ContainerGID, SandboxUser, runtime.Workdir),
		GroupEntry:      fmt.Sprintf("%s:x:%d:", SandboxUser, cfg.ContainerGID),
		Mask:            append([]string(nil), maskedPaths...),
		Privileged:      false,
		CapAdd:          []string{},
		CapDrop:         []string{"ALL"},
		NoNewPrivileges: true,
		// Empty means Podman's default profile, never "unconfined"
		// (rejected in Config). AppArmor/SELinux and the default masked
		// and read-only /proc and /sys paths are left at Podman's
		// defaults, which are the restrictive ones.
		SeccompProfile: cfg.SeccompProfilePath,

		// Read-only root filesystem; the only writable storage is the
		// size-capped tmpfs mounts below, so "disk" is bounded without
		// XFS project quotas. read_write_tmpfs stays on because with it
		// off Libpod mounts /dev/shm read-only, which breaks POSIX
		// semaphores (Python multiprocessing). Its side effect, uncapped
		// tmpfs on /tmp, /var/tmp and /run, is pre-empted by mounting all
		// three explicitly with caps.
		ReadOnlyFilesystem: true,
		ReadWriteTmpfs:     true,

		// Every namespace is private to the sandbox.
		PidNS:    namespace{NSMode: "private"},
		IpcNS:    namespace{NSMode: "private"},
		UtsNS:    namespace{NSMode: "private"},
		CgroupNS: namespace{NSMode: "private"},
		// The same for every core.NetworkMode: a network namespace with
		// no interfaces but loopback, and no route out. Allowlist and
		// full egress are served by the host proxy over the Unix socket
		// in the run directory, bridged to 127.0.0.1 by the agent. The
		// container never gets pasta, a bridge or a resolver, so the
		// egress policy is a routing fact and not an advisory variable.
		NetNS: namespace{NSMode: "none"},

		CgroupsMode: "enabled",
		ResourceLimits: resourceLimits{
			// cpu.max = "<quota> <period>".
			CPU: cpuLimits{Quota: cpuQuota(res.CPUs), Period: cpuPeriodUsec},
			// memory.max, and memory+swap equal to it, which makes
			// memory.swap.max 0. Left unset, Podman allows swap equal to
			// the memory limit again, doubling what a sandbox can hold.
			Memory: memoryLimits{Limit: res.MemoryBytes, Swap: res.MemoryBytes},
			// pids.max is the fork-bomb control. An nproc rlimit set
			// here is deliberately not used. Measured on kernel 7.0 with
			// RLIMIT_NPROC=30 on each of two containers: 12 forks
			// succeeded in one under keep-id (the other did not start),
			// 10 and 11 under nomap, 28 and 0 under the host mapping. The
			// kernel counts the limit per user namespace, but a limit
			// that is in force when the namespace is created is also
			// applied to the parent namespace's count, which holds every
			// sandbox of the pool. See "Process limit" in
			// docs/operations.md for why the agent does not lower it for
			// its children either.
			Pids: pidsLimits{Limit: res.PidsLimit},
			// memory.oom.group makes an OOM kill take the whole sandbox,
			// not one process of a then half-dead tree.
			//
			// memory.high is deliberately NOT set. With swap disabled
			// (memory.swap.max is 0, see above) a sandbox above
			// memory.high has nothing to reclaim but its own page cache,
			// so the kernel only stalls its allocations: measured on
			// kernel 7.0 with Podman 4.9.3, a Python process allocating
			// without bound in a 256 MiB sandbox took 109 s to reach
			// memory.max and be OOM-killed with memory.high at 90%, and
			// 0.4 s without it. A memory bomb must die quickly, so the
			// hard limit is the only memory control.
			Unified: map[string]string{
				"memory.oom.group": "1",
			},
		},
		Rlimits: []rlimit{{Type: "nofile", Hard: uint64(res.NofileLimit), Soft: uint64(res.NofileLimit)}},
	}

	if ociRuntime == RunscRuntime {
		gvisorSpec(s, res)
	}

	// User namespace. For keep-id, nomap and host the idmappings object
	// mirrors what the podman CLI sends for the same --userns value: the
	// server derives the actual ranges from the service user's
	// /etc/subuid and /etc/subgid.
	switch cfg.UsernsMode {
	case UsernsKeepID:
		s.UserNS = namespace{NSMode: "keep-id", Value: fmt.Sprintf("uid=%d,gid=%d", cfg.ContainerUID, cfg.ContainerGID)}
		s.IDMappings = &idMappings{}
	case UsernsNoMap:
		s.UserNS = namespace{NSMode: "no-map"} // the API spelling of --userns=nomap
		s.IDMappings = &idMappings{}
	case UsernsHost:
		s.UserNS = namespace{NSMode: "host"}
		s.IDMappings = &idMappings{HostUIDMapping: true, HostGIDMapping: true}
	case UsernsAuto:
		// Without AutoUserNs the service accepts "auto" and silently
		// behaves like "host" (observed on Podman 4.9.3).
		s.UserNS = namespace{NSMode: "auto"}
		s.IDMappings = &idMappings{AutoUserNs: true}
	default:
		return nil, fmt.Errorf("podman: unknown UsernsMode %q", cfg.UsernsMode)
	}

	// Mounts. Libpod validates options against a whitelist (uid=, gid=
	// and nr_inodes= are rejected), so tmpfs ownership uses Libpod's "U"
	// option, which becomes uid=/gid= of the container user.
	runDirOpts := []string{"rbind", "rw", "nosuid", "nodev", "noexec", "rprivate"}
	if cfg.RunDirChown {
		runDirOpts = append(runDirOpts, "U")
	}
	s.Mounts = []ociMount{
		// The work directory. exec is required: this is where the code
		// under test lives (scripts, virtualenvs, compiled extensions).
		// tmpcopyup lets a template image ship a pre-populated
		// /workspace; its content then counts against the cap.
		tmpfs(runtime.Workdir, res.WorkdirBytes, "0755", "exec", "tmpcopyup", "U"),
		// /tmp also allows exec. pip and other build tools unpack and
		// run build backends and freshly compiled shared objects from
		// $TMPDIR, and ctypes/cffi need an executable temp file for
		// closures. noexec here would buy little: the sandbox user can
		// already execute anything in /workspace, the content is
		// untrusted by definition, and nosuid plus no-new-privileges
		// already rule out privilege gain through a planted binary.
		tmpfs("/tmp", res.TmpBytes, "1777", "exec", "notmpcopyup"),
		// Incidental writable paths, small and non-executable. They are
		// declared so that Libpod does not add uncapped ones.
		tmpfs("/var/tmp", smallTmpfsBytes, "1777", "noexec", "notmpcopyup"),
		tmpfs("/run", smallTmpfsBytes, "0755", "noexec", "notmpcopyup"),
		// The per-sandbox host directory holding the agent socket and
		// the egress proxy socket: the sandbox's only channel to the
		// host. Sockets need rw; nothing in it is ever executed.
		{Destination: runtime.RunDir, Type: "bind", Source: spec.HostRunDir, Options: runDirOpts},
	}
	if mountAgent {
		// The static agent binary, read-only so the sandbox cannot
		// replace the program the host talks to.
		s.Mounts = append(s.Mounts, ociMount{
			Destination: runtime.AgentBinary, Type: "bind", Source: spec.AgentBinaryPath,
			Options: []string{"rbind", "ro", "nosuid", "nodev", "exec", "rprivate"},
		})
	}
	if ociRuntime == RunscRuntime {
		// runsc does not apply masked paths to its own sysfs, which
		// publishes the host's DMI product name; an empty read-only tmpfs
		// over the directory hides it the way the mask does under crun.
		s.Mounts = append(s.Mounts, ociMount{Destination: dmiPath + "/id", Type: "tmpfs", Source: "tmpfs",
			Options: []string{"ro", "nosuid", "nodev", "noexec", "size=4096", "mode=0555"}})
	}
	return s, nil
}

// gVisor host-task allowance: the pids.max of a runsc sandbox's cgroup; see
// gvisorSpec.
const (
	gvisorTasksPerPid  = 4
	gvisorTasksReserve = 256
)

// gvisorSpec adjusts the create request for the runsc tier. Everything
// else in the hardened spec is sent unchanged; what gVisor does with each
// part is listed here so that the differences from crun are in one place.
//
//   - Process limit. Under crun pids.max counts the sandbox's own tasks.
//     Under gVisor the cgroup holds the sentry, and the sentry's host
//     threads are not the sandbox's processes: an idle sandbox has about
//     50, each sandbox process adds about two, and a sentry that cannot
//     create a thread does not return EAGAIN to the application, it dies
//     (measured: a fork bomb against pids.max=64 ended the whole sandbox).
//     The limit a tenant sees is therefore enforced by gVisor itself, as
//     RLIMIT_NPROC: the sentry counts tasks per user inside this one
//     sandbox, so the reason this rlimit is useless under crun (it is per
//     host user, shared by every sandbox) does not apply, and fork fails
//     with EAGAIN at the limit as it does under crun. The cgroup's
//     pids.max stays as a backstop for the sentry, sized well above what
//     the rlimit lets the sandbox reach.
//   - Memory, swap, CPU, memory.oom.group: applied to the sandbox's scope
//     by the runsc shim (package runscshim), not by runsc; the values are
//     the same. An OOM kill takes the sentry and with it the sandbox.
//     The sentry is not told the limits (runsc learns them only from a
//     cgroup it configured itself), so /proc/meminfo and nproc inside the
//     sandbox show the host's totals.
//   - Seccomp profile: runsc ignores an OCI seccomp profile unless it is
//     run with --oci-seccomp. The host kernel is protected by the sentry's
//     own filter, and inside the sandbox system calls are gVisor's
//     implementation, which is the point of the tier. The profile is
//     still sent, so an operator who adds that flag gets it applied.
//   - tmpfs mounts are the sentry's own (size caps honoured, charged to
//     the memory limit); /dev/shm and the two bind mounts are served by
//     the gofer, and Unix sockets in the run dir cross the boundary
//     because the shim passes --host-uds=all.
//   - User namespace: the sentry and gofer run as the container's root,
//     which keep-id maps to the user's first subordinate UID, not to the
//     pool user as the sandbox process is under crun.
func gvisorSpec(s *createSpec, res core.Resources) {
	s.Rlimits = append(s.Rlimits, rlimit{Type: "nproc", Hard: uint64(res.PidsLimit), Soft: uint64(res.PidsLimit)})
	s.ResourceLimits.Pids.Limit = res.PidsLimit*gvisorTasksPerPid + gvisorTasksReserve
}

func tmpfs(dest string, size int64, mode string, extra ...string) ociMount {
	opts := []string{"rw", "nosuid", "nodev", "size=" + strconv.FormatInt(size, 10), "mode=" + mode}
	return ociMount{Destination: dest, Type: "tmpfs", Source: "tmpfs", Options: append(opts, extra...)}
}

// cpuQuota converts a CPU count to a CFS quota for cpuPeriodUsec. The
// kernel's minimum quota is 1000 microseconds.
func cpuQuota(cpus float64) int64 {
	q := int64(math.Round(cpus * cpuPeriodUsec))
	if q < 1000 {
		q = 1000
	}
	return q
}
