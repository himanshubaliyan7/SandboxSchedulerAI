package pooltest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

// MemStore is an in-memory implementation of pool.Store with the same
// semantics as *store.Store: atomic oldest-first claim with tenant quota,
// conditional transitions fenced by state and generation, deadlines for
// transient states, one event per state change. Its clock is injected, so
// the sweep predicates follow a FakeClock.
type MemStore struct {
	mu        sync.Mutex
	now       func() time.Time
	templates map[string]core.Template
	quota     map[string]int // tenant -> max concurrent
	rows      []*memRow      // insertion order = created_at order
	events    []store.Event
	failWith  error
}

type memRow struct {
	sb      core.Sandbox
	token   []byte
	policy  *core.NetworkPolicy
	updated time.Time
}

// NewMemStore returns an empty store reading time from now.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, templates: map[string]core.Template{}, quota: map[string]int{}}
}

// --- test helpers ----------------------------------------------------------

// PutTemplate inserts or replaces a template verbatim (a zero version becomes 1).
func (m *MemStore) PutTemplate(t core.Template) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t.Version == 0 {
		t.Version = 1
	}
	m.templates[t.ID] = t
}

// DeleteTemplate removes a template.
func (m *MemStore) DeleteTemplate(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.templates, id)
}

// PutTenant creates or updates a tenant's concurrent-sandbox quota.
func (m *MemStore) PutTenant(id string, maxConcurrent int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quota[id] = maxConcurrent
}

// InsertRaw inserts a row exactly as given, bypassing the state machine,
// to set up crash scenarios.
func (m *MemStore) InsertRaw(sb core.Sandbox) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if sb.CreatedAt.IsZero() {
		sb.CreatedAt = m.now()
	}
	m.rows = append(m.rows, &memRow{sb: sb, updated: m.now()})
}

// SetError makes every store call fail with err until cleared with nil.
func (m *MemStore) SetError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failWith = err
}

// Sandboxes returns every row, oldest first.
func (m *MemStore) Sandboxes() []core.Sandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]core.Sandbox, len(m.rows))
	for i, r := range m.rows {
		out[i] = r.sb
	}
	return out
}

// Events returns the events of one sandbox, or of all when id is empty.
func (m *MemStore) Events(sandboxID string) []store.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Event
	for _, e := range m.events {
		if sandboxID == "" || e.SandboxID == sandboxID {
			out = append(out, e)
		}
	}
	return out
}

// NetworkPolicy returns the policy stored for a sandbox at claim time.
func (m *MemStore) NetworkPolicy(sandboxID string) *core.NetworkPolicy {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.find(sandboxID); r != nil {
		return r.policy
	}
	return nil
}

// --- internals -------------------------------------------------------------

// AccessTokenHash returns the access-token hash stored by the claim.
func (m *MemStore) AccessTokenHash(sandboxID string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.find(sandboxID); r != nil {
		return r.token
	}
	return nil
}

// Update applies f to a stored row under the store's lock and reports
// whether the row exists. It is for fakes layered on top of MemStore that
// need a write this type does not model.
func (m *MemStore) Update(sandboxID string, f func(*core.Sandbox)) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.find(sandboxID)
	if r != nil {
		f(&r.sb)
	}
	return r != nil
}

func (m *MemStore) find(id string) *memRow {
	for _, r := range m.rows {
		if r.sb.ID == id {
			return r
		}
	}
	return nil
}

func (m *MemStore) event(sb core.Sandbox, typ string, from core.State, data map[string]any) {
	m.events = append(m.events, store.Event{
		ID: int64(len(m.events) + 1), SandboxID: sb.ID, TenantID: sb.TenantID, Type: typ,
		FromState: from, ToState: sb.State, Reason: sb.Reason, At: m.now(), Data: data,
	})
}

func ptr(t time.Time) *time.Time { return &t }

func (m *MemStore) miss(id string, gen int64) error {
	r := m.find(id)
	switch {
	case r == nil:
		return fmt.Errorf("%w: sandbox %s", store.ErrNotFound, id)
	case r.sb.Generation != gen:
		return fmt.Errorf("%w: sandbox %s is at generation %d, caller has %d", store.ErrStaleGeneration, id, r.sb.Generation, gen)
	}
	return fmt.Errorf("%w: sandbox %s is %s", store.ErrInvalidTransition, id, r.sb.State)
}

// --- pool.Store ------------------------------------------------------------

func (m *MemStore) ListTemplates(ctx context.Context) ([]core.Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return nil, m.failWith
	}
	out := make([]core.Template, 0, len(m.templates))
	for _, t := range m.templates {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) GetTemplate(ctx context.Context, id string) (core.Template, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return core.Template{}, m.failWith
	}
	t, ok := m.templates[id]
	if !ok {
		return core.Template{}, fmt.Errorf("%w: template %s", store.ErrNotFound, id)
	}
	return t, nil
}

func (m *MemStore) InsertCreating(ctx context.Context, n store.NewSandbox) (core.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return core.Sandbox{}, m.failWith
	}
	if m.find(n.ID) != nil {
		return core.Sandbox{}, fmt.Errorf("%w: sandbox %s already exists", store.ErrConflict, n.ID)
	}
	if n.Deadline <= 0 {
		n.Deadline = core.CreatingDeadline
	}
	now := m.now()
	sb := core.Sandbox{ID: n.ID, TemplateID: n.TemplateID, TemplateVersion: n.TemplateVersion, HostID: n.HostID,
		State: core.StateCreating, CreatedAt: now, DeadlineAt: ptr(now.Add(n.Deadline))}
	m.rows = append(m.rows, &memRow{sb: sb, updated: now})
	m.event(sb, store.EventCreated, "", nil)
	return sb, nil
}

func (m *MemStore) MarkWarm(ctx context.Context, id string, generation int64, instanceID string) (core.Sandbox, error) {
	return m.Transition(ctx, store.TransitionParams{ID: id, Generation: generation,
		From: core.StateCreating, To: core.StateWarm, InstanceID: instanceID})
}

func (m *MemStore) Transition(ctx context.Context, p store.TransitionParams) (core.Sandbox, error) {
	if !core.CanTransition(p.From, p.To) {
		return core.Sandbox{}, fmt.Errorf("%w: %s -> %s is not an edge", store.ErrInvalidTransition, p.From, p.To)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return core.Sandbox{}, m.failWith
	}
	r := m.find(p.ID)
	if r == nil || r.sb.Generation != p.Generation || r.sb.State != p.From {
		return core.Sandbox{}, m.miss(p.ID, p.Generation)
	}
	now := m.now()
	r.sb.State = p.To
	if p.Reason != "" {
		r.sb.Reason = p.Reason
	}
	if p.InstanceID != "" {
		r.sb.InstanceID = p.InstanceID
	}
	r.sb.DeadlineAt = nil
	if p.To.Transient() {
		d := p.Deadline
		if d <= 0 {
			d = store.DefaultDeadline(p.To)
		}
		r.sb.DeadlineAt = ptr(now.Add(d))
	}
	r.updated = now
	typ := p.EventType
	if typ == "" {
		typ = store.EventTransition
	}
	m.event(r.sb, typ, p.From, p.Data)
	return r.sb, nil
}

func tenantOwned(s core.State) bool {
	return s == core.StateAssigned || s == core.StateRunning || s == core.StatePaused
}

func (m *MemStore) Claim(ctx context.Context, p store.ClaimParams) (core.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return core.Sandbox{}, m.failWith
	}
	max, ok := m.quota[p.TenantID]
	if !ok {
		return core.Sandbox{}, fmt.Errorf("%w: tenant %s", store.ErrNotFound, p.TenantID)
	}
	active := 0
	for _, r := range m.rows {
		if r.sb.TenantID == p.TenantID && tenantOwned(r.sb.State) {
			active++
		}
	}
	if active >= max {
		return core.Sandbox{}, fmt.Errorf("%w: tenant %s has %d of %d", store.ErrQuotaExceeded, p.TenantID, active, max)
	}
	for _, r := range m.rows {
		if r.sb.State != core.StateWarm || r.sb.TemplateID != p.TemplateID ||
			(p.HostID != "" && r.sb.HostID != p.HostID) ||
			(p.TemplateVersion != 0 && r.sb.TemplateVersion != p.TemplateVersion) {
			continue
		}
		now := m.now()
		if p.AssignedDeadline <= 0 {
			p.AssignedDeadline = core.AssignedDeadline
		}
		r.sb.State = core.StateAssigned
		r.sb.TenantID = p.TenantID
		r.sb.Generation++
		r.sb.AssignedAt, r.sb.LastActivityAt = ptr(now), ptr(now)
		r.sb.DeadlineAt = ptr(now.Add(p.AssignedDeadline))
		r.sb.ExpiresAt = nil
		if p.MaxLifetime > 0 {
			r.sb.ExpiresAt = ptr(now.Add(p.MaxLifetime))
		}
		r.token, r.policy, r.updated = p.AccessTokenHash, p.NetworkPolicy, now
		m.event(r.sb, store.EventClaimed, core.StateWarm, nil)
		return r.sb, nil
	}
	return core.Sandbox{}, fmt.Errorf("%w: template %s", store.ErrPoolEmpty, p.TemplateID)
}

func (m *MemStore) Touch(ctx context.Context, id string, generation int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	r := m.find(id)
	if r == nil || r.sb.Generation != generation || (r.sb.State != core.StateRunning && r.sb.State != core.StatePaused) {
		return m.miss(id, generation)
	}
	r.sb.LastActivityAt = ptr(m.now())
	return nil
}

func (m *MemStore) SetNetworkPolicy(ctx context.Context, id string, generation int64, policy core.NetworkPolicy) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	r := m.find(id)
	if r == nil || r.sb.Generation != generation || !tenantOwned(r.sb.State) {
		return m.miss(id, generation)
	}
	r.policy = &policy
	return nil
}

func (m *MemStore) GetSandbox(ctx context.Context, id string) (core.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return core.Sandbox{}, m.failWith
	}
	r := m.find(id)
	if r == nil {
		return core.Sandbox{}, fmt.Errorf("%w: sandbox %s", store.ErrNotFound, id)
	}
	return r.sb, nil
}

func (m *MemStore) CountByTemplateState(ctx context.Context, hostID string) (map[string]map[core.State]int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return nil, m.failWith
	}
	out := map[string]map[core.State]int{}
	for _, r := range m.rows {
		if r.sb.State == core.StateDestroyed || (hostID != "" && r.sb.HostID != hostID) {
			continue
		}
		if out[r.sb.TemplateID] == nil {
			out[r.sb.TemplateID] = map[core.State]int{}
		}
		out[r.sb.TemplateID][r.sb.State]++
	}
	return out, nil
}

func (m *MemStore) ListSweepable(ctx context.Context, hostID string) ([]store.SweepItem, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return nil, m.failWith
	}
	now := m.now()
	past := func(t *time.Time) bool { return t != nil && t.Before(now) }
	var out []store.SweepItem
	for _, r := range m.rows {
		sb := r.sb
		if sb.HostID != hostID {
			continue
		}
		t, haveTmpl := m.templates[sb.TemplateID]
		var cause store.SweepCause
		switch {
		case sb.State.Transient() && past(sb.DeadlineAt):
			cause = store.SweepDeadline
		case (sb.State == core.StateRunning || sb.State == core.StatePaused) && past(sb.ExpiresAt):
			cause = store.SweepMaxLifetime
		case sb.State == core.StateRunning && haveTmpl && t.Timeouts.Idle > 0 &&
			sb.LastActivityAt != nil && sb.LastActivityAt.Add(t.Timeouts.Idle).Before(now):
			cause = store.SweepIdle
		case sb.State == core.StateWarm && (!haveTmpl || t.Version != sb.TemplateVersion):
			cause = store.SweepTemplateChanged
		case sb.State == core.StateWarm && t.Timeouts.WarmTTL > 0 && sb.CreatedAt.Add(t.Timeouts.WarmTTL).Before(now):
			cause = store.SweepWarmTTL
		default:
			continue
		}
		out = append(out, store.SweepItem{Sandbox: sb, Cause: cause})
	}
	return out, nil
}

func (m *MemStore) ListActiveByHost(ctx context.Context, hostID string) ([]core.Sandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return nil, m.failWith
	}
	var out []core.Sandbox
	for _, r := range m.rows {
		if r.sb.HostID == hostID && r.sb.State != core.StateDestroyed {
			out = append(out, r.sb)
		}
	}
	return out, nil
}

func (m *MemStore) AppendEvent(ctx context.Context, e store.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return m.failWith
	}
	e.ID, e.At = int64(len(m.events)+1), m.now()
	m.events = append(m.events, e)
	return nil
}

func (m *MemStore) GCDestroyed(ctx context.Context, olderThan time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failWith != nil {
		return 0, m.failWith
	}
	cutoff := m.now().Add(-olderThan)
	var (
		keep []*memRow
		n    int64
	)
	for _, r := range m.rows {
		if r.sb.State == core.StateDestroyed && r.updated.Before(cutoff) {
			n++
			continue
		}
		keep = append(keep, r)
	}
	m.rows = keep
	return n, nil
}
