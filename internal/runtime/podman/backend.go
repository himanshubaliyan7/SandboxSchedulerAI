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
	"strings"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
)

// Backend implements runtime.Backend on one rootless Podman service.
type Backend struct {
	cfg Config
	// c is the default runtime's service: every call that is not a
	// create or a start goes to it. creators has, per runtime, the
	// service that creates and starts that runtime's sandboxes; see
	// Config.RuntimeSockets.
	c        *client
	creators map[string]*client
	// oom is the conmon OOM evidence of a managed service, or nil when
	// the exit code is all there is (Config.OOMEvidenceDirs).
	oom *oomEvidence
	// createMu serialises the create request (not the start). Podman
	// resolves the keep-id mapping of every create through libsubid,
	// which keeps its /etc/subuid database in process-global state and is
	// not thread-safe: on Podman 4.9.3 a burst of eight concurrent
	// creates was seen to end the whole service with SIGSEGV inside
	// subid_get_uid_ranges, and, more often, to hand a container a wrong
	// mapping, which then fails at start ("mount devpts: Invalid
	// argument", "writing uid_map: Operation not permitted": at least 11 of 288
	// creates at concurrency 8, none of 216 with this lock). The request
	// is a small part of what a sandbox start costs, so creating one at
	// a time changes nothing measurable (51 to 55 ms per sandbox at
	// concurrency 8 either way), and starts still run in parallel.
	createMu sync.Mutex
	// paths caches the PATH of each template image; see imageEnvPath.
	pathMu sync.Mutex
	paths  map[string]imagePathEntry
}

type imagePathEntry struct {
	path string
	at   time.Time
}

// imagePathTTL is how long an image's PATH is remembered. A tag can be
// pulled again with another PATH; a minute bounds how long sandboxes are
// then created with the old one, and keeps the lookup (about 3 ms) off
// all but one create a minute per image.
const imagePathTTL = time.Minute

// imageEnvPath returns the PATH the image sets in its ENV, or "" when it
// sets none. DefaultEnv needs it to put the user's bin directory in front
// of the image's PATH instead of replacing it. An image that does not
// exist returns "": the create that follows reports that properly.
func (b *Backend) imageEnvPath(ctx context.Context, c *client, image string) (string, error) {
	b.pathMu.Lock()
	e, ok := b.paths[image]
	b.pathMu.Unlock()
	if ok && time.Since(e.at) < imagePathTTL {
		return e.path, nil
	}
	var data struct {
		Config struct {
			Env []string `json:"Env"`
		} `json:"Config"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(image)+"/json", nil, nil, &data); err != nil {
		if IsStatus(err, http.StatusNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("inspect image %s: %w", image, err)
	}
	path := ""
	for _, kv := range data.Config.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	b.pathMu.Lock()
	if b.paths == nil {
		b.paths = map[string]imagePathEntry{}
	}
	b.paths[image] = imagePathEntry{path: path, at: time.Now()}
	b.pathMu.Unlock()
	return path, nil
}

var _ runtime.Backend = (*Backend)(nil)

// New validates cfg and returns a Backend. It does not contact the Podman
// service; use Ping (or package preflight) to check it is reachable.
func New(cfg Config) (*Backend, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	b := &Backend{cfg: cfg, c: newClient(cfg.SocketPath, cfg.DialTimeout, cfg.RequestTimeout), creators: map[string]*client{}}
	for _, r := range cfg.Runtimes {
		if r == cfg.DefaultRuntime {
			b.creators[r] = b.c
		} else {
			b.creators[r] = newClient(cfg.RuntimeSockets[r], cfg.DialTimeout, cfg.RequestTimeout)
		}
	}
	if len(cfg.OOMEvidenceDirs) > 0 {
		b.oom = newOOMEvidence(cfg.OOMEvidenceDirs)
	}
	return b, nil
}

// Close releases what New started (the OOM evidence watcher). The
// Backend must not be used afterwards.
func (b *Backend) Close() {
	if b.oom != nil {
		b.oom.close()
	}
}

// Config returns the effective configuration, defaults applied.
func (b *Backend) Config() Config { return b.cfg }

// Name implements runtime.Backend.
func (b *Backend) Name() string { return "podman" }

// Ping checks that the service of every enabled runtime answers and
// returns the Libpod API version of the default one.
func (b *Backend) Ping(ctx context.Context) (string, error) {
	version, err := b.c.apiVersion(ctx)
	if err != nil {
		return "", err
	}
	for name, c := range b.creators {
		if c == b.c {
			continue
		}
		if _, err := c.apiVersion(ctx); err != nil {
			return "", fmt.Errorf("service of runtime %s: %w", name, err)
		}
	}
	return version, nil
}

// Capabilities implements runtime.Backend. SharedKernel is true as soon as
// one enabled runtime runs sandboxes on the host kernel (crun, runc or
// anything unrecognised); it is false only for a backend restricted to
// runsc, krun or kata. Capabilities().Runtimes, or SharedKernel(runtime),
// has the answer for one runtime, which is the one a template gets.
func (b *Backend) Capabilities() runtime.Capabilities {
	shared, pause := false, true
	infos := make([]runtime.RuntimeInfo, 0, len(b.cfg.Runtimes))
	for _, r := range b.cfg.Runtimes {
		info := RuntimeInfo(r)
		shared = shared || info.SharedKernel
		pause = pause && info.Pause
		infos = append(infos, info)
	}
	return runtime.Capabilities{
		Pause:        pause,
		Snapshot:     false,
		SharedKernel: shared,
		NeedsKVM:     false,
		Rootless:     os.Geteuid() != 0,
		// All three modes use the same no-interface network namespace;
		// allowlist and full differ only in the egress proxy's policy.
		NetworkModes:  []core.NetworkMode{core.NetworkNone, core.NetworkAllowlist, core.NetworkFull},
		RuntimeLabels: append([]string(nil), b.cfg.Runtimes...),
		Runtimes:      infos,
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
	// The service decides the runtime, so the runtime decides the service.
	c := b.creators[body.OCIRuntime]
	if c == nil {
		return runtime.Instance{}, fmt.Errorf("create sandbox %s: no Podman service for runtime %q", spec.SandboxID, body.OCIRuntime)
	}
	imagePath, err := b.imageEnvPath(ctx, c, spec.Template.Image)
	if err != nil {
		return runtime.Instance{}, fmt.Errorf("create sandbox %s: %w", spec.SandboxID, err)
	}
	body.Env = DefaultEnv(imagePath)
	b.createMu.Lock()
	_, err = c.do(ctx, http.MethodPost, "/containers/create", nil, body, &created, http.StatusCreated)
	b.createMu.Unlock()
	if err != nil {
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
	// Started through the same service: conmon inherits that service's
	// working directory and configuration.
	if _, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(created.ID)+"/start", nil, nil, nil,
		http.StatusNoContent, http.StatusNotModified); err != nil {
		return fail(err)
	}
	inst, st, err := b.inspect(ctx, created.ID)
	if err != nil {
		return fail(err)
	}
	// A template's runtime is its isolation boundary. If Podman ran the
	// sandbox with another one (it does not report having ignored the
	// request), the sandbox must not exist, let alone be handed out.
	if st.ociRuntime != body.OCIRuntime {
		return fail(fmt.Errorf("podman ran the container with OCI runtime %q although %q was asked for; "+
			"the service on %s must have %q as its default runtime", st.ociRuntime, body.OCIRuntime, c.socket, body.OCIRuntime))
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
// sbx.managed=true, in any state and of any host id. Instance.HostID tells
// the caller whose each one is.
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
		ExitedAt int64             `json:"ExitedAt"` // Unix seconds
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
			// The end is known to the second here; Inspect is exact.
			inst.OOMKilled = b.oomKilled(r.ExitCode, time.Unix(r.ExitedAt, 0), time.Second)
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
	Status     string    `json:"Status"`
	Running    bool      `json:"Running"`
	Paused     bool      `json:"Paused"`
	OOMKilled  bool      `json:"OOMKilled"`
	ExitCode   int       `json:"ExitCode"`
	CgroupPath string    `json:"CgroupPath"`
	FinishedAt time.Time `json:"FinishedAt"`
	// ociRuntime is the container's top-level OCIRuntime, copied here by
	// inspect: the name of the runtime Podman created it with.
	ociRuntime string
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
//
// With a managed service there is better evidence, and oomKilled uses it
// to tell those two apart; see oomEvidence.
func killedFromOutside(exitCode int) bool { return exitCode == sigkillExitCode }

// oomKilled reports whether a container that exited with exitCode at end
// (known to within slack) was OOM-killed: by conmon's report when this
// backend has a managed service's evidence, by the exit code alone
// otherwise.
func (b *Backend) oomKilled(exitCode int, end time.Time, slack time.Duration) bool {
	if !killedFromOutside(exitCode) {
		return false
	}
	if b.oom == nil {
		return true
	}
	return b.oom.killed(end, slack)
}

// exitSettle is how long inspect waits for a container whose process is
// gone to reach the "exited" state. In between ("stopping", "stopped")
// Podman has not yet recorded how it ended, and a caller that asked "why
// did it die" would be told "exit code 0, not OOM-killed".
const exitSettle = time.Second

func (b *Backend) inspect(ctx context.Context, instanceID string) (runtime.Instance, inspectState, error) {
	if instanceID == "" {
		return runtime.Instance{}, inspectState{}, errors.New("podman: inspect: empty instance id")
	}
	deadline := time.Now().Add(exitSettle)
	for {
		inst, st, err := b.inspectOnce(ctx, instanceID)
		if err != nil || (st.Status != "stopping" && st.Status != "stopped") || time.Now().After(deadline) {
			return inst, st, err
		}
		select {
		case <-ctx.Done():
			return inst, st, nil
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (b *Backend) inspectOnce(ctx context.Context, instanceID string) (runtime.Instance, inspectState, error) {
	var data struct {
		ID         string        `json:"Id"`
		Created    time.Time     `json:"Created"`
		OCIRuntime string        `json:"OCIRuntime"`
		State      *inspectState `json:"State"`
		Config     *struct {
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
	st.ociRuntime = data.OCIRuntime
	var labels map[string]string
	if data.Config != nil {
		labels = data.Config.Labels
	}
	inst := instanceFrom(data.ID, labels, st.Running || st.Paused, data.Created)
	if !inst.Running && (st.Status == "exited" || st.Status == "stopped") {
		inst.ExitCode = st.ExitCode
		inst.OOMKilled = st.OOMKilled || b.oomKilled(st.ExitCode, st.FinishedAt, 0)
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
		HostID:     labels[runtime.LabelHostID],
		Generation: gen,
		AgentEpoch: labels[runtime.LabelAgentEpoch],
		TemplateID: labels[runtime.LabelTemplateID],
		Running:    running,
		CreatedAt:  created,
	}
}
