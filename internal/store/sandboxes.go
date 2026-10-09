package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

// Event types written by the store itself.
const (
	EventCreated    = "created"    // CREATING row inserted
	EventClaimed    = "claimed"    // WARM -> ASSIGNED
	EventTransition = "transition" // every other state change
)

// sandboxCols is the column list scanSandbox expects, in order.
const sandboxCols = `id, template_id, template_version, host_id, state, tenant_id, generation,
	instance_id, reason, created_at, assigned_at, last_activity_at, expires_at, deadline_at`

func scanSandbox(row pgx.Row, extra ...any) (core.Sandbox, error) {
	var (
		sb     core.Sandbox
		state  string
		tenant *string
		reason string
	)
	dest := []any{&sb.ID, &sb.TemplateID, &sb.TemplateVersion, &sb.HostID, &state, &tenant, &sb.Generation,
		&sb.InstanceID, &reason, &sb.CreatedAt, &sb.AssignedAt, &sb.LastActivityAt, &sb.ExpiresAt, &sb.DeadlineAt}
	if err := row.Scan(append(dest, extra...)...); err != nil {
		return core.Sandbox{}, err
	}
	sb.State = core.State(state)
	sb.Reason = core.TerminationReason(reason)
	if tenant != nil {
		sb.TenantID = *tenant
	}
	return sb, nil
}

func collectSandboxes(rows pgx.Rows) ([]core.Sandbox, error) {
	defer rows.Close()
	var out []core.Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// DefaultDeadline is the deadline a transient state gets when the caller
// does not supply one; zero for states that carry no deadline.
func DefaultDeadline(s core.State) time.Duration {
	switch s {
	case core.StateCreating:
		return core.CreatingDeadline
	case core.StateAssigned:
		return core.AssignedDeadline
	case core.StateRecycling:
		return core.RecyclingDeadline
	}
	return 0
}

func jsonParam(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok && len(m) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("store: encode json: %w", err)
	}
	return string(b), nil
}

// NewSandbox describes a CREATING row.
type NewSandbox struct {
	ID              string
	TemplateID      string
	TemplateVersion int64
	HostID          string
	// Deadline is how long the row may stay CREATING; zero means core.CreatingDeadline.
	Deadline time.Duration
}

// InsertCreating inserts a sandbox in CREATING with generation 0 and a
// deadline, and appends a "created" event in the same statement.
func (s *Store) InsertCreating(ctx context.Context, n NewSandbox) (core.Sandbox, error) {
	if n.ID == "" || n.TemplateID == "" || n.HostID == "" {
		return core.Sandbox{}, errors.New("store: InsertCreating needs id, template id and host id")
	}
	if n.Deadline <= 0 {
		n.Deadline = core.CreatingDeadline
	}
	const q = `
WITH ins AS (
	INSERT INTO sandboxes (id, template_id, template_version, host_id, state, deadline_at)
	VALUES ($1, $2, $3, $4, 'CREATING', now() + $5::bigint * interval '1 microsecond')
	RETURNING *
), ev AS (
	INSERT INTO events (sandbox_id, type, to_state)
	SELECT id, '` + EventCreated + `', state FROM ins
)
SELECT ` + sandboxCols + ` FROM ins`
	sb, err := scanSandbox(s.pool.QueryRow(ctx, q, n.ID, n.TemplateID, n.TemplateVersion, n.HostID, n.Deadline.Microseconds()))
	if err != nil {
		if isUniqueViolation(err) {
			return core.Sandbox{}, fmt.Errorf("%w: sandbox %s already exists", ErrConflict, n.ID)
		}
		return core.Sandbox{}, fmt.Errorf("store: insert sandbox: %w", err)
	}
	return sb, nil
}

// TransitionParams describes one state change.
type TransitionParams struct {
	ID string
	// Generation and From are the caller's belief about the row. The update
	// only happens if both still hold, so a stale actor loses.
	Generation int64
	From       core.State
	To         core.State
	// Reason is recorded on the row when non-empty; an empty reason keeps
	// whatever the row already carries (FAILED -> RECYCLING keeps the
	// reason that explains the failure). The event always records the
	// row's reason after the change.
	Reason core.TerminationReason
	// Deadline overrides the default deadline of a transient target state.
	// It is ignored for non-transient targets, whose deadline is cleared.
	Deadline time.Duration
	// InstanceID, when non-empty, records the backend instance id.
	InstanceID string
	// EventType defaults to EventTransition.
	EventType string
	// Data is stored as the event's JSONB payload.
	Data map[string]any
}

// Transition is the single path for every state change after the claim.
// It rejects edges that core.CanTransition does not allow, applies the
// change only if the row is still in From with the expected generation,
// sets deadline_at for transient targets and clears it otherwise, and
// appends the event in the same statement (one data-modifying CTE, hence
// one transaction).
//
// Errors: ErrInvalidTransition (illegal edge, or the row is in another
// state), ErrStaleGeneration, ErrNotFound.
func (s *Store) Transition(ctx context.Context, p TransitionParams) (core.Sandbox, error) {
	if !core.CanTransition(p.From, p.To) {
		return core.Sandbox{}, fmt.Errorf("%w: %s -> %s is not an edge", ErrInvalidTransition, p.From, p.To)
	}
	var deadline *int64
	if p.To.Transient() {
		d := p.Deadline
		if d <= 0 {
			d = DefaultDeadline(p.To)
		}
		us := d.Microseconds()
		deadline = &us
	}
	evType := p.EventType
	if evType == "" {
		evType = EventTransition
	}
	data, err := jsonParam(p.Data)
	if err != nil {
		return core.Sandbox{}, err
	}
	const q = `
WITH upd AS (
	UPDATE sandboxes SET
		state       = $4::text,
		reason      = CASE WHEN $5::text <> '' THEN $5::text ELSE reason END,
		deadline_at = now() + $6::bigint * interval '1 microsecond',
		instance_id = CASE WHEN $7::text <> '' THEN $7::text ELSE instance_id END,
		updated_at  = now()
	WHERE id = $1 AND generation = $2 AND state = $3::text
	RETURNING *
), ev AS (
	INSERT INTO events (sandbox_id, tenant_id, type, from_state, to_state, reason, data)
	SELECT id, tenant_id, $8::text, $3::text, $4::text, reason, $9::jsonb FROM upd
)
SELECT ` + sandboxCols + ` FROM upd`
	sb, err := scanSandbox(s.pool.QueryRow(ctx, q, p.ID, p.Generation, string(p.From), string(p.To),
		string(p.Reason), deadline, p.InstanceID, evType, data))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Sandbox{}, s.explainMiss(ctx, p.ID, p.Generation, p.From, p.To)
	}
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: transition %s %s -> %s: %w", p.ID, p.From, p.To, err)
	}
	return sb, nil
}

// explainMiss turns "the conditional update matched nothing" into the
// typed error that says why.
func (s *Store) explainMiss(ctx context.Context, id string, gen int64, from, to core.State) error {
	var (
		state  string
		curGen int64
	)
	err := s.pool.QueryRow(ctx, `SELECT state, generation FROM sandboxes WHERE id = $1`, id).Scan(&state, &curGen)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("store: look up sandbox %s: %w", id, err)
	}
	if curGen != gen {
		return fmt.Errorf("%w: sandbox %s is at generation %d, caller has %d", ErrStaleGeneration, id, curGen, gen)
	}
	if to == "" {
		return fmt.Errorf("%w: sandbox %s is %s", ErrInvalidTransition, id, state)
	}
	return fmt.Errorf("%w: sandbox %s is %s, not %s (wanted -> %s)", ErrInvalidTransition, id, state, from, to)
}

// MarkWarm moves CREATING -> WARM and records the instance id. Call it
// only after the agent readiness check has passed.
func (s *Store) MarkWarm(ctx context.Context, id string, generation int64, instanceID string) (core.Sandbox, error) {
	return s.Transition(ctx, TransitionParams{
		ID: id, Generation: generation, From: core.StateCreating, To: core.StateWarm, InstanceID: instanceID,
	})
}

// ClaimParams describes a claim.
type ClaimParams struct {
	TenantID   string
	TemplateID string
	// TemplateVersion, when non-zero, restricts the claim to WARM
	// sandboxes of that version so a claim never receives an instance
	// built from a superseded template.
	TemplateVersion int64
	// HostID, when non-empty, restricts the claim to one host.
	HostID string
	// AssignedDeadline is how long the row may stay ASSIGNED; zero means core.AssignedDeadline.
	AssignedDeadline time.Duration
	// MaxLifetime sets expires_at = now() + MaxLifetime; zero means no expiry.
	MaxLifetime time.Duration
	// AccessTokenHash is the SHA-256 of the per-sandbox access token, if any.
	AccessTokenHash []byte
	// NetworkPolicy is stored as JSONB; nil stores NULL.
	NetworkPolicy *core.NetworkPolicy
}

// claimSQL pops the oldest WARM sandbox of a template in ONE statement.
// The subquery locks the chosen row with FOR UPDATE SKIP LOCKED, so
// concurrent claimers never wait on, and never pick, the same row: each
// one skips rows another transaction has locked and takes the next. The
// LockRows node sits below the LIMIT, so a row that was claimed and
// committed between this statement's snapshot and its lock attempt fails
// the re-check and the scan moves on to the next candidate instead of
// returning nothing. The event is appended by the same statement.
const claimSQL = `
WITH upd AS (
	UPDATE sandboxes SET
		state             = 'ASSIGNED',
		tenant_id         = $1,
		generation        = generation + 1,
		assigned_at       = now(),
		last_activity_at  = now(),
		deadline_at       = now() + $3::bigint * interval '1 microsecond',
		expires_at        = CASE WHEN $4::bigint > 0 THEN now() + $4::bigint * interval '1 microsecond' END,
		access_token_hash = $5,
		network_policy    = $6::jsonb,
		updated_at        = now()
	WHERE state = 'WARM' AND id = (
		SELECT id FROM sandboxes
		WHERE template_id = $2 AND state = 'WARM'
		  AND ($7::text = '' OR host_id = $7::text)
		  AND ($8::bigint = 0 OR template_version = $8::bigint)
		ORDER BY created_at
		LIMIT 1
		FOR UPDATE SKIP LOCKED)
	RETURNING *
), ev AS (
	INSERT INTO events (sandbox_id, tenant_id, type, from_state, to_state)
	SELECT id, tenant_id, '` + EventClaimed + `', 'WARM', state FROM upd
)
SELECT ` + sandboxCols + ` FROM upd`

// Claim atomically assigns the oldest WARM sandbox of a template to a
// tenant, bumping its generation, and enforces the tenant's concurrent
// sandbox quota in the same transaction.
//
// How the quota is made race-free: the transaction first takes a row lock
// on the tenant (SELECT ... FOR UPDATE). Every claim for that tenant takes
// the same lock, so claims of one tenant are serialised while claims of
// different tenants do not touch each other. Holding the lock, it counts
// the tenant's ASSIGNED/RUNNING/PAUSED sandboxes in a SEPARATE statement.
// That matters: under READ COMMITTED each statement gets a fresh snapshot,
// so the count sees the row a previous claimer committed just before
// releasing the lock. (A count folded into the locking statement as a
// subquery would use the snapshot taken before the lock wait and could
// miss it.) The claim then runs and the lock is held until commit. Moves
// out of the counted states (release, sweep) do not take the lock; they
// can only make the count an over-estimate, which errs on the safe side.
//
// Errors: ErrNotFound (no such tenant), ErrQuotaExceeded, ErrPoolEmpty.
func (s *Store) Claim(ctx context.Context, p ClaimParams) (core.Sandbox, error) {
	if p.TenantID == "" || p.TemplateID == "" {
		return core.Sandbox{}, errors.New("store: Claim needs tenant id and template id")
	}
	if p.AssignedDeadline <= 0 {
		p.AssignedDeadline = core.AssignedDeadline
	}
	var policy any
	if p.NetworkPolicy != nil {
		b, err := json.Marshal(p.NetworkPolicy)
		if err != nil {
			return core.Sandbox{}, fmt.Errorf("store: encode network policy: %w", err)
		}
		policy = string(b)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: begin claim: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	var maxConcurrent int
	err = tx.QueryRow(ctx, `SELECT max_concurrent FROM tenants WHERE id = $1 FOR UPDATE`, p.TenantID).Scan(&maxConcurrent)
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Sandbox{}, fmt.Errorf("%w: tenant %s", ErrNotFound, p.TenantID)
	}
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: lock tenant: %w", err)
	}
	var active int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM sandboxes
		WHERE tenant_id = $1 AND state IN ('ASSIGNED', 'RUNNING', 'PAUSED')`, p.TenantID).Scan(&active)
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: count tenant sandboxes: %w", err)
	}
	if active >= maxConcurrent {
		return core.Sandbox{}, fmt.Errorf("%w: tenant %s has %d of %d", ErrQuotaExceeded, p.TenantID, active, maxConcurrent)
	}

	sb, err := scanSandbox(tx.QueryRow(ctx, claimSQL, p.TenantID, p.TemplateID, p.AssignedDeadline.Microseconds(),
		p.MaxLifetime.Microseconds(), p.AccessTokenHash, policy, p.HostID, p.TemplateVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Sandbox{}, fmt.Errorf("%w: template %s", ErrPoolEmpty, p.TemplateID)
	}
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: claim: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return core.Sandbox{}, fmt.Errorf("store: commit claim: %w", err)
	}
	return sb, nil
}

// Touch records activity on a tenant-owned sandbox (it resets the idle
// timer). It is fenced by generation. Callers on a hot path should
// throttle it; one write per sandbox every few seconds is plenty.
func (s *Store) Touch(ctx context.Context, id string, generation int64) error {
	tag, err := s.pool.Exec(ctx, `UPDATE sandboxes SET last_activity_at = now()
		WHERE id = $1 AND generation = $2 AND state IN ('RUNNING', 'PAUSED')`, id, generation)
	if err != nil {
		return fmt.Errorf("store: touch %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.explainMiss(ctx, id, generation, "", "")
	}
	return nil
}

// SetNetworkPolicy replaces the stored network policy of a tenant-owned
// sandbox, fenced by generation.
func (s *Store) SetNetworkPolicy(ctx context.Context, id string, generation int64, policy core.NetworkPolicy) error {
	b, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("store: encode network policy: %w", err)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sandboxes SET network_policy = $3::jsonb, updated_at = now()
		WHERE id = $1 AND generation = $2 AND state IN ('ASSIGNED', 'RUNNING', 'PAUSED')`, id, generation, string(b))
	if err != nil {
		return fmt.Errorf("store: set network policy %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.explainMiss(ctx, id, generation, "", "")
	}
	return nil
}

// GetSandbox returns one row.
func (s *Store) GetSandbox(ctx context.Context, id string) (core.Sandbox, error) {
	sb, err := scanSandbox(s.pool.QueryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Sandbox{}, fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: get sandbox %s: %w", id, err)
	}
	return sb, nil
}

// SandboxAccess is the per-sandbox credential, policy and metadata that
// live beside the core.Sandbox fields.
type SandboxAccess struct {
	Sandbox         core.Sandbox
	AccessTokenHash []byte              // nil when no token was set at claim time
	NetworkPolicy   *core.NetworkPolicy // nil before the claim
	Metadata        map[string]string   // nil unless SetMetadata stored some
}

// GetSandboxAccess returns a row together with its access-token hash,
// network policy and metadata.
func (s *Store) GetSandboxAccess(ctx context.Context, id string) (SandboxAccess, error) {
	out, err := scanSandboxAccess(s.pool.QueryRow(ctx,
		`SELECT `+sandboxCols+`, access_token_hash, network_policy, metadata FROM sandboxes WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return SandboxAccess{}, fmt.Errorf("%w: sandbox %s", ErrNotFound, id)
	}
	if err != nil {
		return SandboxAccess{}, fmt.Errorf("store: get sandbox %s: %w", id, err)
	}
	return out, nil
}

// ListSandboxesByTenant lists a tenant's sandboxes, newest first. With
// liveOnly it returns only ASSIGNED, RUNNING and PAUSED rows.
func (s *Store) ListSandboxesByTenant(ctx context.Context, tenantID string, liveOnly bool, limit int) ([]core.Sandbox, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT ` + sandboxCols + ` FROM sandboxes WHERE tenant_id = $1`
	if liveOnly {
		q += ` AND state IN ('ASSIGNED', 'RUNNING', 'PAUSED')`
	}
	q += ` ORDER BY assigned_at DESC NULLS LAST, id LIMIT $2`
	rows, err := s.pool.Query(ctx, q, tenantID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sandboxes: %w", err)
	}
	return collectSandboxes(rows)
}

// ListActiveByHost returns every non-terminal row of a host: the
// reconciler's desired set.
func (s *Store) ListActiveByHost(ctx context.Context, hostID string) ([]core.Sandbox, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sandboxCols+` FROM sandboxes
		WHERE host_id = $1 AND state <> 'DESTROYED' ORDER BY created_at, id`, hostID)
	if err != nil {
		return nil, fmt.Errorf("store: list active sandboxes: %w", err)
	}
	return collectSandboxes(rows)
}

// CountByTemplateState counts a host's non-terminal sandboxes per template
// and state. An empty hostID counts every host.
func (s *Store) CountByTemplateState(ctx context.Context, hostID string) (map[string]map[core.State]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT template_id, state, count(*) FROM sandboxes
		WHERE ($1::text = '' OR host_id = $1::text) AND state <> 'DESTROYED' GROUP BY template_id, state`, hostID)
	if err != nil {
		return nil, fmt.Errorf("store: count sandboxes: %w", err)
	}
	defer rows.Close()
	out := map[string]map[core.State]int{}
	for rows.Next() {
		var (
			tmpl, state string
			n           int
		)
		if err := rows.Scan(&tmpl, &state, &n); err != nil {
			return nil, err
		}
		if out[tmpl] == nil {
			out[tmpl] = map[core.State]int{}
		}
		out[tmpl][core.State(state)] = n
	}
	return out, rows.Err()
}

// SweepCause says why a row needs the sweeper's attention.
type SweepCause string

const (
	SweepDeadline        SweepCause = "deadline"         // transient state past deadline_at
	SweepIdle            SweepCause = "idle"             // RUNNING with no activity for the template's idle timeout
	SweepMaxLifetime     SweepCause = "max_lifetime"     // RUNNING or PAUSED past expires_at
	SweepWarmTTL         SweepCause = "warm_ttl"         // WARM for longer than the template's warm TTL
	SweepTemplateChanged SweepCause = "template_changed" // WARM built from an old or deleted template version
)

// SweepItem is one row the sweeper must act on.
type SweepItem struct {
	Sandbox core.Sandbox
	Cause   SweepCause
}

// sweepLimit bounds one sweep pass; the rest is picked up by the next.
const sweepLimit = "500"

// ListSweepable returns the rows of a host that a timer has caught, using
// the database clock. Each branch matches one partial index. A row appears
// at most once: the branches cover disjoint states, except idle and max
// lifetime, where max lifetime wins.
func (s *Store) ListSweepable(ctx context.Context, hostID string) ([]SweepItem, error) {
	const cols = `s.id, s.template_id, s.template_version, s.host_id, s.state, s.tenant_id, s.generation,
		s.instance_id, s.reason, s.created_at, s.assigned_at, s.last_activity_at, s.expires_at, s.deadline_at`
	const q = `
(SELECT ` + cols + `, '` + string(SweepDeadline) + `' AS cause FROM sandboxes s
  WHERE s.host_id = $1 AND s.deadline_at IS NOT NULL AND s.deadline_at < now()
    AND s.state IN ('CREATING', 'ASSIGNED', 'RECYCLING'))
UNION ALL
(SELECT ` + cols + `, '` + string(SweepMaxLifetime) + `' FROM sandboxes s
  WHERE s.host_id = $1 AND s.state IN ('RUNNING', 'PAUSED') AND s.expires_at < now())
UNION ALL
(SELECT ` + cols + `, '` + string(SweepIdle) + `' FROM sandboxes s
  JOIN templates t ON t.id = s.template_id
  WHERE s.host_id = $1 AND s.state = 'RUNNING' AND t.idle_timeout_ms > 0
    AND s.last_activity_at < now() - t.idle_timeout_ms * interval '1 millisecond'
    AND (s.expires_at IS NULL OR s.expires_at >= now()))
UNION ALL
(SELECT ` + cols + `, '` + string(SweepWarmTTL) + `' FROM sandboxes s
  JOIN templates t ON t.id = s.template_id AND t.version = s.template_version
  WHERE s.host_id = $1 AND s.state = 'WARM' AND t.warm_ttl_ms > 0
    AND s.created_at < now() - t.warm_ttl_ms * interval '1 millisecond')
UNION ALL
(SELECT ` + cols + `, '` + string(SweepTemplateChanged) + `' FROM sandboxes s
  LEFT JOIN templates t ON t.id = s.template_id
  WHERE s.host_id = $1 AND s.state = 'WARM' AND (t.id IS NULL OR t.version <> s.template_version))
LIMIT ` + sweepLimit
	rows, err := s.pool.Query(ctx, q, hostID)
	if err != nil {
		return nil, fmt.Errorf("store: list sweepable: %w", err)
	}
	defer rows.Close()
	var out []SweepItem
	for rows.Next() {
		var cause string
		sb, err := scanSandbox(rows, &cause)
		if err != nil {
			return nil, err
		}
		out = append(out, SweepItem{Sandbox: sb, Cause: SweepCause(cause)})
	}
	return out, rows.Err()
}

// GCDestroyed deletes DESTROYED rows whose last change is older than
// olderThan and returns how many were removed. Events are kept: they are
// the audit and metering record.
func (s *Store) GCDestroyed(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM sandboxes
		WHERE state = 'DESTROYED' AND updated_at < now() - $1::bigint * interval '1 microsecond'`, olderThan.Microseconds())
	if err != nil {
		return 0, fmt.Errorf("store: gc destroyed: %w", err)
	}
	return tag.RowsAffected(), nil
}
