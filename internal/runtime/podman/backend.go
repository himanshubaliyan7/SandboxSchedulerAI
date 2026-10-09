package podman

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime"
)

// Backend implements runtime.Backend on one rootless Podman service.
type Backend struct {
	cfg Config
	c   *client
}

var _ runtime.Backend = (*Backend)(nil)

// New validates cfg and returns a Backend. It does not contact the Podman
// service; use Ping (or package preflight) to check it is reachable.
func New(cfg Config) (*Backend, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	return &Backend{cfg: cfg, c: newClient(cfg.SocketPath, cfg.DialTimeout, cfg.RequestTimeout)}, nil
}

// Config returns the effective configuration, defaults applied.
func (b *Backend) Config() Config { return b.cfg }

// Name implements runtime.Backend.
func (b *Backend) Name() string { return "podman" }

// Ping checks that the service answers and returns its Libpod API version.
func (b *Backend) Ping(ctx context.Context) (string, error) {
	return b.c.apiVersion(ctx)
}

// Capabilities implements runtime.Backend. SharedKernel is true as soon as
// one enabled runtime runs sandboxes on the host kernel (crun, runc or
// anything unrecognised); it is false only for a backend restricted to
// runsc, krun or kata. Use SharedKernel(runtime) for a per-pool answer.
func (b *Backend) Capabilities() runtime.Capabilities {
	shared := false
	for _, r := range b.cfg.Runtimes {
		shared = shared || SharedKernel(r)
	}
	return runtime.Capabilities{
		Pause:        true, // cgroup v2 freezer through the runtime
		Snapshot:     false,
		SharedKernel: shared,
		NeedsKVM:     false,
		Rootless:     os.Geteuid() != 0,
		// All three modes use the same no-interface network namespace;
		// allowlist and full differ only in the egress proxy's policy.
		NetworkModes:  []core.NetworkMode{core.NetworkNone, core.NetworkAllowlist, core.NetworkFull},
		RuntimeLabels: append([]string(nil), b.cfg.Runtimes...),
	}
}

// Create implements runtime.Backend: create, start, then confirm the
// runtime reports the container running. If anything after the create
// call fails, the half-made container is removed before returning.
func (b *Backend) Create(ctx context.Context, spec runtime.Spec) (runtime.Instance, error) {
	body, err := buildCreateSpec(b.cfg, spec)
	if err != nil {
		return runtime.Instance{}, err
	}
	var created struct {
		ID string `json:"Id"`
	}
	if _, err := b.c.do(ctx, http.MethodPost, "/containers/create", nil, body, &created, http.StatusCreated); err != nil {
		return runtime.Instance{}, fmt.Errorf("create sandbox %s: %w", spec.SandboxID, err)
	}
	if created.ID == "" {
		return runtime.Instance{}, fmt.Errorf("create sandbox %s: podman returned no container id", spec.SandboxID)
	}
	fail := func(err error) (runtime.Instance, error) {
		// The caller's context may be the reason for the failure, so
		// clean up on a fresh one.
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.RequestTimeout)
		defer cancel()
		if derr := b.Destroy(cctx, created.ID); derr != nil {
			err = errors.Join(err, fmt.Errorf("cleanup of %s also failed: %w", created.ID, derr))
		}
		return runtime.Instance{}, fmt.Errorf("create sandbox %s: %w", spec.SandboxID, err)
	}
	// 204 started, 304 already running.
	if _, err := b.c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start", nil, nil, nil,
		http.StatusNoContent, http.StatusNotModified); err != nil {
		return fail(err)
	}
	inst, _, err := b.inspect(ctx, created.ID)
	if err != nil {
		return fail(err)
	}
	if !inst.Running {
		return fail(errors.New("container exited immediately after start"))
	}
	return inst, nil
}

// Destroy implements runtime.Backend: force-remove with SIGKILL and no
// grace period, together with the container's anonymous volumes. All
// other writable storage is tmpfs and dies with the mount namespace. A
// container that is already gone is success.
func (b *Backend) Destroy(ctx context.Context, instanceID string) error {
	if instanceID == "" {
		return errors.New("podman: destroy: empty instance id")
	}
	q := url.Values{"force": {"true"}, "volumes": {"true"}, "timeout": {"0"}}
	_, err := b.c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(instanceID), q, nil, nil)
	if err != nil && !IsStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// List implements runtime.Backend: every container labelled
// sbx.managed=true, in any state.
func (b *Backend) List(ctx context.Context) ([]runtime.Instance, error) {
	filters, err := json.Marshal(map[string][]string{"label": {runtime.LabelManaged + "=true"}})
	if err != nil {
		return nil, err
	}
	q := url.Values{"all": {"true"}, "filters": {string(filters)}}
	var rows []struct {
		ID       string            `json:"Id"`
		Labels   map[string]string `json:"Labels"`
		State    string            `json:"State"`
		Created  time.Time         `json:"Created"`
		Exited   bool              `json:"Exited"`
		ExitCode int               `json:"ExitCode"`
	}
	if _, err := b.c.do(ctx, http.MethodGet, "/containers/json", q, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]runtime.Instance, 0, len(rows))
	for _, r := range rows {
		// Do not trust the server-side filter alone: an instance that is
		// reported here may be destroyed as an orphan.
		if r.Labels[runtime.LabelManaged] != "true" {
			continue
		}
		// A paused container still holds its resources and its tenant.
		inst := instanceFrom(r.ID, r.Labels, r.State == "running" || r.State == "paused", r.Created)
		if !inst.Running && (r.Exited || r.State == "exited") {
			inst.ExitCode = r.ExitCode
			inst.OOMKilled = killedFromOutside(r.ExitCode)
		}
		out = append(out, inst)
	}
	return out, nil
}

// Inspect implements runtime.Backend.
func (b *Backend) Inspect(ctx context.Context, instanceID string) (runtime.Instance, error) {
	inst, _, err := b.inspect(ctx, instanceID)
	return inst, err
}

type inspectState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	OOMKilled  bool   `json:"OOMKilled"`
	ExitCode   int    `json:"ExitCode"`
	CgroupPath string `json:"CgroupPath"`
}

// sigkillExitCode is what Podman reports for a container whose init
// process was ended by SIGKILL (128 + 9).
const sigkillExitCode = 137

// killedFromOutside reports whether an exited container's init process
// was ended by SIGKILL. That is this backend's evidence of an OOM kill
// when Podman has none of its own: on rootless Podman 4.9.3 the cgroup of
// a dead container is removed together with its memory.events, and
// State.OOMKilled stays false. The inference rests on three facts: the
// kernel never delivers SIGKILL to a PID namespace's init from inside
// that namespace, so code in the sandbox cannot produce this exit status;
// this backend's own kill (Destroy) removes the container, so a container
// it killed is never inspected afterwards; and memory.oom.group=1 makes
// the OOM killer take init with everything else. What is left is the OOM
// killer and a SIGKILL sent on the host by someone else (an operator, or
// the user session being torn down), which is why callers should record
// the exit code next to the reason.
func killedFromOutside(exitCode int) bool { return exitCode == sigkillExitCode }

func (b *Backend) inspect(ctx context.Context, instanceID string) (runtime.Instance, inspectState, error) {
	if instanceID == "" {
		return runtime.Instance{}, inspectState{}, errors.New("podman: inspect: empty instance id")
	}
	var data struct {
		ID      string        `json:"Id"`
		Created time.Time     `json:"Created"`
		State   *inspectState `json:"State"`
		Config  *struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if _, err := b.c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(instanceID)+"/json", nil, nil, &data); err != nil {
		return runtime.Instance{}, inspectState{}, notFound(err)
	}
	var st inspectState
	if data.State != nil {
		st = *data.State
	}
	var labels map[string]string
	if data.Config != nil {
		labels = data.Config.Labels
	}
	inst := instanceFrom(data.ID, labels, st.Running || st.Paused, data.Created)
	if !inst.Running && st.Status == "exited" {
		inst.ExitCode = st.ExitCode
		inst.OOMKilled = st.OOMKilled || killedFromOutside(st.ExitCode)
	}
	return inst, st, nil
}

// Pause implements runtime.Backend.
func (b *Backend) Pause(ctx context.Context, instanceID string) error {
	return b.lifecycle(ctx, instanceID, "pause")
}

// Unpause implements runtime.Backend.
func (b *Backend) Unpause(ctx context.Context, instanceID string) error {
	return b.lifecycle(ctx, instanceID, "unpause")
}

func (b *Backend) lifecycle(ctx context.Context, instanceID, verb string) error {
	if instanceID == "" {
		return fmt.Errorf("podman: %s: empty instance id", verb)
	}
	_, err := b.c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(instanceID)+"/"+verb, nil, nil, nil)
	return notFound(err)
}

// notFound makes a 404 satisfy errors.Is(err, runtime.ErrNotFound) while
// keeping the APIError reachable through errors.As.
func notFound(err error) error {
	if IsStatus(err, http.StatusNotFound) {
		return fmt.Errorf("%w: %w", runtime.ErrNotFound, err)
	}
	return err
}

func instanceFrom(id string, labels map[string]string, running bool, created time.Time) runtime.Instance {
	gen, _ := strconv.ParseInt(labels[runtime.LabelGeneration], 10, 64)
	return runtime.Instance{
		ID:         id,
		SandboxID:  labels[runtime.LabelSandboxID],
		Generation: gen,
		AgentEpoch: labels[runtime.LabelAgentEpoch],
		TemplateID: labels[runtime.LabelTemplateID],
		Running:    running,
		CreatedAt:  created,
	}
}
