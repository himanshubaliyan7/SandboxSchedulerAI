// Package core holds the domain types shared by every layer: the sandbox
// state machine, templates, resource shapes and network policy. It has no
// dependencies on other internal packages.
package core

import "time"

// State is a sandbox lifecycle state. The lifecycle is one-way: a sandbox
// that has been assigned to a tenant is never returned to the warm pool.
type State string

const (
	StateCreating  State = "CREATING"  // transient: host agent is creating the instance
	StateWarm      State = "WARM"      // idle, owned by no tenant, counted in the pool
	StateAssigned  State = "ASSIGNED"  // transient: bound to a tenant, session config being injected
	StateRunning   State = "RUNNING"   // serving one tenant session
	StatePaused    State = "PAUSED"    // frozen, still owned by the same tenant
	StateRecycling State = "RECYCLING" // transient: instance and all its storage are being destroyed
	StateDestroyed State = "DESTROYED" // terminal: row kept for audit
	StateFailed    State = "FAILED"    // create failed or instance vanished; reconciler recycles it
	StateUnknown   State = "UNKNOWN"   // host unreachable; reconciler recycles it when the host returns
)

var transitions = map[State][]State{
	StateCreating:  {StateWarm, StateFailed, StateRecycling},
	StateWarm:      {StateAssigned, StateRecycling, StateFailed, StateUnknown},
	StateAssigned:  {StateRunning, StateRecycling, StateFailed, StateUnknown},
	StateRunning:   {StatePaused, StateRecycling, StateFailed, StateUnknown},
	StatePaused:    {StateRunning, StateRecycling, StateFailed, StateUnknown},
	StateFailed:    {StateRecycling},
	StateUnknown:   {StateRecycling},
	StateRecycling: {StateDestroyed},
	StateDestroyed: {},
}

// CanTransition reports whether from -> to is a legal edge. There is
// deliberately no edge back to WARM from any tenant-owned state.
func CanTransition(from, to State) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Transient reports whether a state must carry a deadline enforced by the sweeper.
func (s State) Transient() bool {
	return s == StateCreating || s == StateAssigned || s == StateRecycling
}

// Terminal reports whether the sandbox no longer holds host resources.
func (s State) Terminal() bool { return s == StateDestroyed }

// TerminationReason is recorded on every move to RECYCLING so that no
// sandbox ever disappears without an observable cause.
type TerminationReason string

const (
	ReasonReleased         TerminationReason = "released"
	ReasonIdleTimeout      TerminationReason = "idle_timeout"
	ReasonMaxLifetime      TerminationReason = "max_lifetime"
	ReasonWarmTTL          TerminationReason = "warm_ttl"
	ReasonTemplateChanged  TerminationReason = "template_changed"
	ReasonOOMKilled        TerminationReason = "oom_killed"
	ReasonCreateFailed     TerminationReason = "create_failed"
	ReasonDeadlineExceeded TerminationReason = "deadline_exceeded"
	ReasonInstanceLost     TerminationReason = "instance_lost"
	ReasonOrphan           TerminationReason = "orphan"
	ReasonHostDrain        TerminationReason = "host_drain"
	ReasonError            TerminationReason = "error"
)

// NetworkMode is the egress profile of a sandbox. The default is none.
type NetworkMode string

const (
	NetworkNone      NetworkMode = "none"      // no network namespace interfaces, no proxy
	NetworkAllowlist NetworkMode = "allowlist" // no route out; forced proxy enforces the policy
	NetworkFull      NetworkMode = "full"      // proxy allows any public destination; deny set still applies
)

// DefaultPolicy is the egress policy of a sandbox created from t without
// a policy of its own: the template's mode, and for an allowlist template
// its default allowlist.
func (t Template) DefaultPolicy() NetworkPolicy {
	mode := t.Network
	if mode == "" {
		mode = NetworkNone
	}
	p := NetworkPolicy{Mode: mode}
	if mode == NetworkAllowlist {
		p.AllowDomains = append([]string(nil), t.AllowDomains...)
		p.AllowCIDRs = append([]string(nil), t.AllowCIDRs...)
	}
	return p
}

// NetworkPolicy is the per-sandbox egress policy enforced by the egress
// proxy. Deny always wins over allow. It can be replaced on a live sandbox.
type NetworkPolicy struct {
	Mode NetworkMode `json:"mode"`
	// AllowDomains are exact hostnames or "*.example.com" wildcards. A
	// wildcard matches any depth of subdomain but not the apex.
	AllowDomains []string `json:"allow_domains,omitempty"`
	AllowCIDRs   []string `json:"allow_cidrs,omitempty"`
	// DenyCIDRs are added to the built-in deny set (loopback, link-local,
	// cloud metadata, RFC 1918, CGNAT, ULA, host interface addresses).
	DenyCIDRs []string `json:"deny_cidrs,omitempty"`
}

// Resources is the resource shape of a sandbox. Every field is enforced
// from outside the sandbox; zero values are rejected by validation.
type Resources struct {
	CPUs        float64 `json:"cpus"`
	MemoryBytes int64   `json:"memory_bytes"`
	PidsLimit   int64   `json:"pids_limit"`
	// WorkdirBytes and TmpBytes cap the two writable tmpfs mounts. tmpfs is
	// charged to the memory cgroup, so their sum must fit in MemoryBytes.
	WorkdirBytes int64 `json:"workdir_bytes"`
	TmpBytes     int64 `json:"tmp_bytes"`
	NofileLimit  int64 `json:"nofile_limit"`
}

// Timeouts are the three independent sandbox timers.
type Timeouts struct {
	Idle        time.Duration `json:"idle"`         // no activity while RUNNING
	MaxLifetime time.Duration `json:"max_lifetime"` // hard cap from assignment
	WarmTTL     time.Duration `json:"warm_ttl"`     // max age in WARM, so pooled instances do not outlive image patches
}

// Template is what a pool is made of. A claim cannot mutate it; anything a
// claim varies (env, files, tenant identity) is injected through the
// in-sandbox agent after assignment.
type Template struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
	Image   string `json:"image"`
	// Runtime selects the isolation tier: "crun" (default) or "runsc".
	Runtime   string      `json:"runtime"`
	Resources Resources   `json:"resources"`
	Network   NetworkMode `json:"network"`
	// AllowDomains and AllowCIDRs are the template's default egress
	// allowlist: the policy of a sandbox that is created without one of
	// its own, when Network is allowlist. A create that names a policy
	// replaces them (it does not add to them), and stays within Network
	// as always. They are not part of what is baked into an instance.
	AllowDomains []string `json:"allow_domains,omitempty"`
	AllowCIDRs   []string `json:"allow_cidrs,omitempty"`
	Timeouts     Timeouts `json:"timeouts"`
	// Pool sizing.
	WarmTarget int `json:"warm_target"`
	MaxTotal   int `json:"max_total"`
	RefillRate int `json:"refill_rate"` // max concurrent creates
}

// Sandbox is one row of the sandboxes table.
type Sandbox struct {
	ID              string
	TemplateID      string
	TemplateVersion int64
	HostID          string
	State           State
	TenantID        string // empty until ASSIGNED
	// Generation fences stale actors: every agent RPC and every release is
	// keyed by (ID, Generation) and rejected on mismatch.
	Generation     int64
	InstanceID     string // backend instance (container) id
	Reason         TerminationReason
	CreatedAt      time.Time
	AssignedAt     *time.Time
	LastActivityAt *time.Time
	ExpiresAt      *time.Time // max lifetime
	DeadlineAt     *time.Time // set while in a transient state
}

// Default deadlines for transient states.
const (
	CreatingDeadline  = 30 * time.Second
	AssignedDeadline  = 5 * time.Second
	RecyclingDeadline = 30 * time.Second
)
