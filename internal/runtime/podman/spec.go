package podman

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

// The types below are the subset of Libpod's SpecGenerator
// (pkg/specgen/specgen.go) that this backend sends to
// POST /libpod/containers/create. Every JSON name was checked against the
// SpecGenerator source on both the v4.9 branch and main (6.x); fields that
// exist on only one of them are not used. Security-relevant booleans carry
// no omitempty on purpose: the server seeds some of them (privileged, for
// one) from containers.conf before decoding, so they are always sent.
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
	// memoryHighPercent puts memory.high below memory.max so a sandbox is
	// throttled into reclaim before it is OOM-killed.
	memoryHighPercent = 90
	// smallTmpfsBytes caps the two incidental tmpfs mounts (/run and
	// /var/tmp); see BuildCreateSpec.
	smallTmpfsBytes = 1 << 20
	pageSize        = 4096
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,200}$`)

// ContainerName is the Podman container name of a sandbox.
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
		// HOME and TMPDIR point at the only two writable places. The
		// host environment and host proxy variables are never forwarded
		// (env_host, httpproxy): the proxy is configured per session by
		// the agent, and host variables may hold secrets.
		Env:      map[string]string{"HOME": runtime.Workdir, "TMPDIR": "/tmp"},
		WorkDir:  runtime.Workdir,
		Hostname: DefaultHostname,

		// Labels let a restarted host agent rebuild its view from the
		// runtime alone and find orphans.
		Labels: map[string]string{
			runtime.LabelManaged:    "true",
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
		OCIRuntime:       ociRuntime,

		// VOLUME declarations in the image would otherwise become
		// anonymous volumes: writable, uncapped host disk.
		ImageVolumeMode: "ignore",
		ShmSize:         cfg.ShmBytes,

		// Non-root inside the container, no capabilities in any set, and
		// no way to regain them through setuid binaries or file caps.
		User:            user,
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
			// pids.max is the fork-bomb control. An nproc rlimit is
			// deliberately not used: it is per host user, so it would
			// be shared by every sandbox of the pool.
			Pids: pidsLimits{Limit: res.PidsLimit},
			// memory.high throttles into reclaim before the hard limit,
			// and memory.oom.group makes an OOM kill take the whole
			// sandbox, not one process of a then half-dead tree.
			Unified: map[string]string{
				"memory.high":      strconv.FormatInt(memoryHigh(res.MemoryBytes), 10),
				"memory.oom.group": "1",
			},
		},
		Rlimits: []rlimit{{Type: "nofile", Hard: uint64(res.NofileLimit), Soft: uint64(res.NofileLimit)}},
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
	return s, nil
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

// memoryHigh is memoryHighPercent of the limit, rounded down to a page.
func memoryHigh(limit int64) int64 {
	h := limit / 100 * memoryHighPercent
	h -= h % pageSize
	if h < pageSize {
		h = pageSize
	}
	return h
}
