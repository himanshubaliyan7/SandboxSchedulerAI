package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
	"github.com/himanshubaliyan7/warmbox/internal/pool"
	"github.com/himanshubaliyan7/warmbox/internal/runtime"
	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// Limits on what a create request may carry.
const (
	maxEnvVars       = 256
	maxEnvBytes      = 64 << 10
	maxMetadataPairs = 64
	maxMetadataBytes = 16 << 10
	maxPolicyEntries = 512
	maxListLimit     = 1000
)

// publicState is the API spelling of a lifecycle state: the core state
// in lower case. PAUSED is not part of the public API: no route pauses a
// sandbox, so the state machine's PAUSED is unreachable today. Should a
// row ever be in it (a future pause feature, an operator's hand), the
// sandbox is still held by its tenant and still counts and expires like
// a running one, so it is reported as "running" rather than as a value
// clients were never told about.
func publicState(s core.State) string {
	if s == core.StatePaused {
		s = core.StateRunning
	}
	return strings.ToLower(string(s))
}

// SandboxView is the public view of a sandbox. It deliberately has no
// generation, host or instance id: those are internal.
type SandboxView struct {
	ID              string              `json:"id"`
	TemplateID      string              `json:"template_id"`
	TemplateVersion int64               `json:"template_version"`
	State           string              `json:"state"`
	Reason          string              `json:"reason,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
	StartedAt       *time.Time          `json:"started_at,omitempty"`
	LastActivityAt  *time.Time          `json:"last_activity_at,omitempty"`
	ExpiresAt       *time.Time          `json:"expires_at,omitempty"`
	Network         *core.NetworkPolicy `json:"network,omitempty"`
	Metadata        map[string]string   `json:"metadata,omitempty"`
	// AccessToken is present only in the response of POST /v1/sandboxes.
	AccessToken string `json:"access_token,omitempty"`
}

func sandboxView(acc store.SandboxAccess) SandboxView {
	sb := acc.Sandbox
	return SandboxView{
		ID: sb.ID, TemplateID: sb.TemplateID, TemplateVersion: sb.TemplateVersion,
		State: publicState(sb.State), Reason: string(sb.Reason),
		CreatedAt: sb.CreatedAt, StartedAt: sb.AssignedAt, LastActivityAt: sb.LastActivityAt, ExpiresAt: sb.ExpiresAt,
		Network: acc.NetworkPolicy, Metadata: acc.Metadata,
	}
}

// decodeJSON reads a bounded JSON body into v. An empty body leaves v
// untouched when optional is true. Unknown fields are rejected so that a
// typo is an error and not a silently ignored setting.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, v any, optional bool) error {
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxJSONBytes)
	b, err := io.ReadAll(body)
	if err != nil {
		var maxb *http.MaxBytesError
		if errors.As(err, &maxb) {
			return err
		}
		return badRequest("could not read the request body")
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		if optional {
			return nil
		}
		return badRequest("a JSON request body is required")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	if dec.More() {
		return badRequest("invalid JSON body: trailing data")
	}
	return nil
}

// CreateSandboxRequest is the body of POST /v1/sandboxes.
type CreateSandboxRequest struct {
	TemplateID string `json:"template_id"`
	// TimeoutMs is the sandbox's maximum lifetime. It may be at most the
	// template's max lifetime; zero means the template's.
	TimeoutMs int64               `json:"timeout_ms,omitempty"`
	Env       map[string]string   `json:"env,omitempty"`
	Metadata  map[string]string   `json:"metadata,omitempty"`
	Network   *core.NetworkPolicy `json:"network,omitempty"`
}

func validateEnv(env map[string]string) error {
	if len(env) > maxEnvVars {
		return badRequest("env has more than %d variables", maxEnvVars)
	}
	n := 0
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return badRequest("env has an invalid variable name or value: %q", k)
		}
		n += len(k) + len(v)
	}
	if n > maxEnvBytes {
		return badRequest("env exceeds %d bytes", maxEnvBytes)
	}
	return nil
}

func validateMetadata(md map[string]string) error {
	if len(md) > maxMetadataPairs {
		return badRequest("metadata has more than %d entries", maxMetadataPairs)
	}
	n := 0
	for k, v := range md {
		if k == "" {
			return badRequest("metadata has an empty key")
		}
		n += len(k) + len(v)
	}
	if n > maxMetadataBytes {
		return badRequest("metadata exceeds %d bytes", maxMetadataBytes)
	}
	return nil
}

func (s *Server) validatePolicy(p *core.NetworkPolicy) error {
	if p == nil {
		return nil
	}
	switch p.Mode {
	case "", core.NetworkNone, core.NetworkAllowlist, core.NetworkFull:
	default:
		return badRequest("network.mode must be one of none, allowlist, full")
	}
	if len(p.AllowDomains)+len(p.AllowCIDRs)+len(p.DenyCIDRs) > maxPolicyEntries {
		return badRequest("network policy has more than %d entries", maxPolicyEntries)
	}
	if s.cfg.ValidatePolicy != nil {
		if err := s.cfg.ValidatePolicy(*p); err != nil {
			return badRequest("invalid network policy: %s", strings.TrimPrefix(err.Error(), "egress: "))
		}
	}
	return nil
}

func newAccessToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("api: generate access token: %w", err)
	}
	return AccessTokenPrefix + hex.EncodeToString(b), nil
}

func (s *Server) handleCreateSandbox(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	var req CreateSandboxRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.TemplateID == "" {
		return badRequest("template_id is required")
	}
	if err := validateEnv(req.Env); err != nil {
		return err
	}
	if err := validateMetadata(req.Metadata); err != nil {
		return err
	}
	if err := s.validatePolicy(req.Network); err != nil {
		return err
	}
	// The claim may queue on an empty pool for ClaimWait, on top of the
	// usual budget.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout+max(s.cfg.ClaimWait, 0))
	defer cancel()

	tmpl, err := s.store.GetTemplate(ctx, req.TemplateID)
	if errors.Is(err, store.ErrNotFound) {
		return errf(http.StatusNotFound, CodeTemplateNotFound, "template %q not found", req.TemplateID)
	}
	if err != nil {
		return err
	}
	lifetime := time.Duration(req.TimeoutMs) * time.Millisecond
	switch {
	case req.TimeoutMs < 0:
		return errf(http.StatusBadRequest, CodeInvalidTimeout, "timeout_ms must not be negative")
	case tmpl.Timeouts.MaxLifetime > 0 && lifetime > tmpl.Timeouts.MaxLifetime:
		e := errf(http.StatusBadRequest, CodeInvalidTimeout, "timeout_ms exceeds the template's max lifetime of %d ms",
			tmpl.Timeouts.MaxLifetime.Milliseconds())
		e.Details = map[string]any{"max_timeout_ms": tmpl.Timeouts.MaxLifetime.Milliseconds()}
		return e
	}
	token, err := newAccessToken()
	if err != nil {
		return err
	}
	sb, err := s.pool.Claim(ctx, tenant.ID, tmpl.ID, pool.ClaimOptions{
		Env: req.Env, Network: req.Network, AccessTokenHash: hashToken(token),
		Wait: s.cfg.ClaimWait, Lifetime: lifetime,
	})
	if err != nil {
		return err
	}
	if len(req.Metadata) > 0 {
		if err := s.store.SetMetadata(ctx, sb.ID, sb.Generation, req.Metadata); err != nil {
			// Do not hand out a sandbox that is not what was asked for.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.RequestTimeout)
			defer rcancel()
			if rerr := s.pool.Release(rctx, sb.ID, sb.Generation, core.ReasonError); rerr != nil {
				s.log.Warn("release after failed create", "sandbox", sb.ID, "err", rerr)
			}
			return fmt.Errorf("store metadata of %s: %w", sb.ID, err)
		}
	}
	acc, err := s.store.GetSandboxAccess(ctx, sb.ID)
	if err != nil {
		return err
	}
	view := sandboxView(acc)
	view.AccessToken = token
	w.Header().Set("Location", "/v1/sandboxes/"+sb.ID)
	writeJSON(w, http.StatusCreated, view)
	return nil
}

var liveStates = []core.State{core.StateAssigned, core.StateRunning, core.StatePaused}

func (s *Server) handleListSandboxes(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	q := r.URL.Query()
	f := store.SandboxFilter{TemplateID: q.Get("template_id"), Limit: 100}
	switch state := q.Get("state"); state {
	case "":
		f.States = liveStates
	case "all":
	default:
		for _, name := range strings.Split(state, ",") {
			st := core.State(strings.ToUpper(strings.TrimSpace(name)))
			switch st {
			case core.StateRunning:
				// A paused sandbox is reported as running (publicState),
				// so the running filter has to find it.
				f.States = append(f.States, core.StateRunning, core.StatePaused)
			case core.StateAssigned, core.StatePaused, core.StateRecycling,
				core.StateDestroyed, core.StateFailed, core.StateUnknown:
				// "paused" is no longer documented; it is still accepted so
				// that a client written against the older enum is not
				// answered with a 400.
				f.States = append(f.States, st)
			default:
				return badRequest("unknown state %q", name)
			}
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			return badRequest("limit must be between 1 and %d", maxListLimit)
		}
		f.Limit = n
	}
	for _, pair := range q["metadata"] {
		k, v, ok := strings.Cut(pair, "=")
		if !ok || k == "" {
			return badRequest("metadata filters have the form key=value")
		}
		if f.Metadata == nil {
			f.Metadata = map[string]string{}
		}
		f.Metadata[k] = v
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	rows, err := s.store.ListSandboxAccess(ctx, tenant.ID, f)
	if err != nil {
		return err
	}
	out := make([]SandboxView, 0, len(rows))
	for _, acc := range rows {
		out = append(out, sandboxView(acc))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
	return nil
}

func (s *Server) handleGetSandbox(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	acc, err := s.ownedSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, sandboxView(acc))
	return nil
}

// handleDeleteSandbox answers 204 as soon as the sandbox is fenced (state
// recycling: its access token is dead and its egress is detached) and
// lets the pool destroy the container in the background, which is the
// slow part of a release. With ?wait=true it answers only after the
// teardown has been attempted, which is what the route did before; the
// sandbox is then normally already destroyed.
func (s *Server) handleDeleteSandbox(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	wait := false
	switch v := r.URL.Query().Get("wait"); v {
	case "", "false":
	case "true":
		wait = true
	default:
		return badRequest("wait must be true or false")
	}
	ctx, cancel := s.opCtx(r)
	defer cancel()
	acc, err := s.ownedSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	sb := acc.Sandbox
	s.sessions.drop(sb.ID)
	switch sb.State {
	case core.StateAssigned, core.StateRunning, core.StatePaused:
		release := s.pool.ReleaseAsync
		if wait {
			release = s.pool.Release
		}
		if err := release(ctx, sb.ID, sb.Generation, core.ReasonReleased); err != nil {
			return err
		}
	default:
		// Already on its way out, or gone: delete is idempotent.
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// isLive reports whether a sandbox is still held by its tenant.
func isLive(s core.State) bool {
	return s == core.StateAssigned || s == core.StateRunning || s == core.StatePaused
}

// liveSandbox is ownedSandbox plus the requirement that the sandbox is
// still held by the tenant.
func (s *Server) liveSandbox(ctx context.Context, tenantID, id string) (store.SandboxAccess, error) {
	acc, err := s.ownedSandbox(ctx, tenantID, id)
	if err != nil {
		return acc, err
	}
	if !isLive(acc.Sandbox.State) {
		return acc, notRunning(acc.Sandbox)
	}
	return acc, nil
}

// SetTimeoutRequest is the body of POST /v1/sandboxes/{id}/timeout.
type SetTimeoutRequest struct {
	// TimeoutMs is the new remaining lifetime, counted from now.
	TimeoutMs int64 `json:"timeout_ms"`
}

func (s *Server) handleSetTimeout(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	// Ownership first: whatever the body, another tenant's sandbox is a 404.
	acc, err := s.ownedSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	var req SetTimeoutRequest
	if err := s.decodeJSON(w, r, &req, false); err != nil {
		return err
	}
	if req.TimeoutMs <= 0 {
		return errf(http.StatusBadRequest, CodeInvalidTimeout, "timeout_ms must be positive")
	}
	if !isLive(acc.Sandbox.State) {
		return notRunning(acc.Sandbox)
	}
	// The cap is the template's current max lifetime, measured from the
	// assignment. A template that has since been deleted imposes none.
	var maxTotal time.Duration
	tmpl, err := s.store.GetTemplate(ctx, acc.Sandbox.TemplateID)
	switch {
	case err == nil:
		maxTotal = tmpl.Timeouts.MaxLifetime
	case !errors.Is(err, store.ErrNotFound):
		return err
	}
	sb, err := s.store.SetLifetime(ctx, acc.Sandbox.ID, acc.Sandbox.Generation,
		time.Duration(req.TimeoutMs)*time.Millisecond, maxTotal)
	if err != nil {
		return err
	}
	acc.Sandbox = sb
	writeJSON(w, http.StatusOK, sandboxView(acc))
	return nil
}

func (s *Server) handleSetNetwork(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	acc, err := s.ownedSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	var policy core.NetworkPolicy
	if err := s.decodeJSON(w, r, &policy, false); err != nil {
		return err
	}
	if err := s.validatePolicy(&policy); err != nil {
		return err
	}
	if !isLive(acc.Sandbox.State) {
		return notRunning(acc.Sandbox)
	}
	if err := s.pool.UpdateNetwork(ctx, acc.Sandbox.ID, acc.Sandbox.Generation, policy); err != nil {
		if errors.Is(err, pool.ErrNoEgress) {
			return errf(http.StatusConflict, CodeNetworkNotAttached,
				"sandbox %s was created with network mode none and has no egress proxy; create a sandbox with mode allowlist or full to change its policy later", acc.Sandbox.ID)
		}
		return err
	}
	acc, err = s.store.GetSandboxAccess(ctx, acc.Sandbox.ID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, sandboxView(acc))
	return nil
}

// EventView is one entry of a sandbox's event log.
type EventView struct {
	ID        int64          `json:"id"`
	Type      string         `json:"type"`
	FromState string         `json:"from_state,omitempty"`
	ToState   string         `json:"to_state,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	At        time.Time      `json:"at"`
	Data      map[string]any `json:"data,omitempty"`
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	acc, err := s.ownedSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	events, err := s.store.ListEvents(ctx, acc.Sandbox.ID)
	if err != nil {
		return err
	}
	want := r.URL.Query().Get("type")
	out := make([]EventView, 0, len(events))
	for _, e := range events {
		// The log also holds the sandbox's life before it was assigned
		// (created, warm); that part belongs to no tenant and is not shown.
		if e.TenantID != tenant.ID || (want != "" && e.Type != want) {
			continue
		}
		out = append(out, EventView{ID: e.ID, Type: e.Type, FromState: publicState(e.FromState), ToState: publicState(e.ToState),
			Reason: string(e.Reason), At: e.At, Data: e.Data})
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": out})
	return nil
}

// StatsView is the resource usage of a running sandbox, read from its
// cgroup: the signals behind "why was my code slow or killed".
type StatsView struct {
	MemoryBytes    int64 `json:"memory_bytes"`
	OOMKills       int64 `json:"oom_kills"`
	CPUUsageUsec   int64 `json:"cpu_usage_usec"`
	NrThrottled    int64 `json:"cpu_throttled_periods"`
	ThrottledUsec  int64 `json:"cpu_throttled_usec"`
	PidsCurrent    int64 `json:"pids_current"`
	PidsLimitHits  int64 `json:"pids_limit_hits"`
	MemoryHighHits int64 `json:"memory_high_hits"`
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, tenant store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	acc, err := s.liveSandbox(ctx, tenant.ID, r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.cfg.Stats == nil {
		return errf(http.StatusNotImplemented, CodeNotImplemented, "this server does not expose sandbox stats")
	}
	st, err := s.cfg.Stats.Stats(ctx, acc.Sandbox.InstanceID)
	if errors.Is(err, runtime.ErrNotRunning) || errors.Is(err, runtime.ErrNotFound) {
		sess := s.sessions.get(acc.Sandbox, func() *session { return s.newSession(acc.Sandbox) })
		if term := s.terminated(sess); term != nil {
			return term
		}
	}
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, StatsView{
		MemoryBytes: st.MemoryBytes, OOMKills: st.OOMKills, CPUUsageUsec: st.CPUUsageUsec, NrThrottled: st.NrThrottled,
		ThrottledUsec: st.ThrottledUsec, PidsCurrent: st.PidsCurrent, PidsLimitHits: st.PidsLimitHits, MemoryHighHits: st.MemoryHighHits,
	})
	return nil
}

// TemplateView is what a tenant may know about a template.
type TemplateView struct {
	ID            string         `json:"id"`
	Version       int64          `json:"version"`
	Image         string         `json:"image"`
	Runtime       string         `json:"runtime"`
	Resources     core.Resources `json:"resources"`
	Network       string         `json:"network"`
	IdleTimeoutMs int64          `json:"idle_timeout_ms"`
	MaxLifetimeMs int64          `json:"max_lifetime_ms"`
}

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request, _ store.Tenant) error {
	ctx, cancel := s.opCtx(r)
	defer cancel()
	templates, err := s.store.ListTemplates(ctx)
	if err != nil {
		return err
	}
	out := make([]TemplateView, 0, len(templates))
	for _, t := range templates {
		out = append(out, TemplateView{ID: t.ID, Version: t.Version, Image: t.Image, Runtime: t.Runtime, Resources: t.Resources,
			Network: string(t.Network), IdleTimeoutMs: t.Timeouts.Idle.Milliseconds(), MaxLifetimeMs: t.Timeouts.MaxLifetime.Milliseconds()})
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
	return nil
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.baseCtx.Err() != nil {
		writeError(w, requestID(r), &Error{Status: http.StatusServiceUnavailable, Code: CodeShuttingDown, Message: "the server is shutting down"})
		return
	}
	if s.cfg.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := s.cfg.Ready(ctx); err != nil {
			s.log.Warn("not ready", "err", err)
			writeError(w, requestID(r), &Error{Status: http.StatusServiceUnavailable, Code: "not_ready", Message: "a dependency is not ready"})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}
