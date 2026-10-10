package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// This file is the storage side of lifecycle webhooks (internal/webhook):
// the endpoints a tenant configured, each endpoint's cursor into the event
// log, the lease that keeps two workers off the same endpoint, and the
// attempt log.
//
// The events table is the outbox. Nothing here is written when an event
// is produced; a webhook is "behind" exactly when events of its tenant
// sort after its cursor. See migrations/0003_webhooks.sql for why the
// cursor is (txid, id) and not an event id.

// Typed errors of the webhook methods.
var (
	// ErrWebhookLimit: the tenant already has as many webhooks as allowed.
	ErrWebhookLimit = errors.New("store: webhook limit reached")
	// ErrWebhookLeaseLost: the caller's lease token is no longer the
	// row's. Another worker claimed the webhook after the lease expired,
	// or its configuration changed, or it was deleted. Nothing was written.
	ErrWebhookLeaseLost = errors.New("store: webhook lease lost")
)

// Reasons a webhook is disabled (Webhook.DisabledReason).
const (
	WebhookDisabledByUser = "user"
	WebhookDisabledFailed = "failing"
	WebhookDisabledGone   = "endpoint_gone"
)

// Statuses of a WebhookDelivery.
const (
	DeliverySucceeded = "succeeded"
	DeliveryFailed    = "failed"  // will be retried
	DeliveryDropped   = "dropped" // given up; the cursor moved on
)

// Webhook is one row of the webhooks table.
type Webhook struct {
	ID          string
	TenantID    string
	URL         string
	Secret      string
	EventTypes  []string
	Description string
	Enabled     bool
	// DisabledReason is one of the WebhookDisabled* constants while
	// Enabled is false, and empty otherwise.
	DisabledReason string
	DisabledAt     *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time

	// PreviousSecret also signs until PreviousSecretExpiresAt; empty when
	// no rotation is in its grace period.
	PreviousSecret          string
	PreviousSecretExpiresAt *time.Time

	// Delivery state.
	CursorTxid    int64
	CursorEventID int64
	Attempts      int // failed attempts on the event after the cursor
	FailingSince  *time.Time
	NextAttemptAt time.Time
	LastSuccessAt *time.Time
	LastFailureAt *time.Time
	LastError     string
	DroppedEvents int64
	LeaseToken    int64
	LeaseUntil    *time.Time
}

const webhookCols = `id, tenant_id, url, secret, event_types, description, enabled, disabled_reason, disabled_at,
	created_at, updated_at, previous_secret, previous_secret_expires_at, cursor_txid, cursor_event_id, attempts,
	failing_since, next_attempt_at, last_success_at, last_failure_at, last_error, dropped_events, lease_token, lease_until`

func scanWebhook(row pgx.Row) (Webhook, error) {
	var w Webhook
	err := row.Scan(&w.ID, &w.TenantID, &w.URL, &w.Secret, &w.EventTypes, &w.Description, &w.Enabled, &w.DisabledReason, &w.DisabledAt,
		&w.CreatedAt, &w.UpdatedAt, &w.PreviousSecret, &w.PreviousSecretExpiresAt, &w.CursorTxid, &w.CursorEventID, &w.Attempts,
		&w.FailingSince, &w.NextAttemptAt, &w.LastSuccessAt, &w.LastFailureAt, &w.LastError, &w.DroppedEvents, &w.LeaseToken, &w.LeaseUntil)
	return w, err
}

func collectWebhooks(rows pgx.Rows) ([]Webhook, error) {
	defer rows.Close()
	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// currentTxid is the SQL for the transaction id of the running statement,
// in the representation of events.txid.
const currentTxid = `pg_current_xact_id()::text::bigint`

// eventHorizon is the SQL for the id of the oldest transaction still
// running. Events below it can no longer be joined by an event that sorts
// before them.
const eventHorizon = `pg_snapshot_xmin(pg_current_snapshot())::text::bigint`

// NewWebhook describes a webhook to create.
type NewWebhook struct {
	ID          string
	TenantID    string
	URL         string
	Secret      string
	EventTypes  []string
	Description string
	Disabled    bool
}

// CreateWebhook inserts a webhook. Its cursor starts at the present: only
// events written after the creation are delivered. With maxPerTenant > 0
// it fails with ErrWebhookLimit when the tenant already has that many;
// the count and the insert are serialised per tenant, so concurrent
// creates cannot exceed the limit.
//
// Errors: ErrWebhookLimit, ErrNotFound (no such tenant), ErrConflict.
func (s *Store) CreateWebhook(ctx context.Context, n NewWebhook, maxPerTenant int) (Webhook, error) {
	if n.ID == "" || n.TenantID == "" || n.URL == "" || n.Secret == "" || len(n.EventTypes) == 0 {
		return Webhook{}, errors.New("store: CreateWebhook needs id, tenant id, url, secret and event types")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Webhook{}, fmt.Errorf("store: begin create webhook: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if maxPerTenant > 0 {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('sbx.webhooks:' || $1::text, 0))`, n.TenantID); err != nil {
			return Webhook{}, fmt.Errorf("store: lock tenant webhooks: %w", err)
		}
		var have int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM webhooks WHERE tenant_id = $1`, n.TenantID).Scan(&have); err != nil {
			return Webhook{}, fmt.Errorf("store: count webhooks: %w", err)
		}
		if have >= maxPerTenant {
			return Webhook{}, fmt.Errorf("%w: tenant %s has %d of %d", ErrWebhookLimit, n.TenantID, have, maxPerTenant)
		}
	}
	disabledReason := ""
	if n.Disabled {
		disabledReason = WebhookDisabledByUser
	}
	w, err := scanWebhook(tx.QueryRow(ctx, `INSERT INTO webhooks
			(id, tenant_id, url, secret, event_types, description, enabled, disabled_reason, disabled_at, cursor_txid)
		VALUES ($1, $2, $3, $4, $5, $6, $7::boolean, $8, CASE WHEN $7::boolean THEN NULL ELSE now() END, `+currentTxid+`)
		RETURNING `+webhookCols,
		n.ID, n.TenantID, n.URL, n.Secret, n.EventTypes, n.Description, !n.Disabled, disabledReason))
	if err != nil {
		switch {
		case isUniqueViolation(err):
			return Webhook{}, fmt.Errorf("%w: webhook %s already exists", ErrConflict, n.ID)
		case isForeignKeyViolation(err):
			return Webhook{}, fmt.Errorf("%w: tenant %s", ErrNotFound, n.TenantID)
		}
		return Webhook{}, fmt.Errorf("store: create webhook: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Webhook{}, fmt.Errorf("store: commit create webhook: %w", err)
	}
	return w, nil
}

// isForeignKeyViolation reports whether err is Postgres error 23503.
func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

// GetWebhook returns one webhook. A non-empty tenantID restricts the
// lookup to that tenant: another tenant's webhook is ErrNotFound, the
// same as one that does not exist.
func (s *Store) GetWebhook(ctx context.Context, tenantID, id string) (Webhook, error) {
	w, err := scanWebhook(s.pool.QueryRow(ctx, `SELECT `+webhookCols+` FROM webhooks
		WHERE id = $1 AND ($2::text = '' OR tenant_id = $2::text)`, id, tenantID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, fmt.Errorf("%w: webhook %s", ErrNotFound, id)
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: get webhook %s: %w", id, err)
	}
	return w, nil
}

// ListWebhooks lists a tenant's webhooks, oldest first; an empty tenantID
// lists every tenant's.
func (s *Store) ListWebhooks(ctx context.Context, tenantID string) ([]Webhook, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+webhookCols+` FROM webhooks
		WHERE ($1::text = '' OR tenant_id = $1::text) ORDER BY tenant_id, created_at, id`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("store: list webhooks: %w", err)
	}
	return collectWebhooks(rows)
}

// WebhookUpdate changes the configuration of a webhook; nil fields keep
// the stored value.
type WebhookUpdate struct {
	URL         *string
	EventTypes  []string // nil keeps; must not be empty otherwise
	Description *string
	Enabled     *bool
}

// UpdateWebhook applies u to a tenant's webhook (tenantID may be empty to
// address any tenant's).
//
// Enabling a disabled webhook moves its cursor to the present and clears
// its failure state: events that occurred while it was disabled are not
// delivered. Changing the URL clears the retry state, so the event at the
// head is attempted against the new URL at once. Every update invalidates
// the lease of a worker that is delivering to the webhook right now, so a
// delivery in flight to the old configuration is not recorded and the
// event is sent again under the new one.
func (s *Store) UpdateWebhook(ctx context.Context, tenantID, id string, u WebhookUpdate) (Webhook, error) {
	if u.EventTypes != nil && len(u.EventTypes) == 0 {
		return Webhook{}, errors.New("store: UpdateWebhook: event types must not be empty")
	}
	// In the SET list every right-hand side sees the row as it was.
	const (
		enabling   = `(coalesce($6::boolean, enabled) AND NOT enabled)`
		disabling  = `(NOT coalesce($6::boolean, enabled) AND enabled)`
		urlChanged = `($3::text IS NOT NULL AND $3::text <> url)`
		reset      = `(` + enabling + ` OR ` + urlChanged + `)`
	)
	w, err := scanWebhook(s.pool.QueryRow(ctx, `UPDATE webhooks SET
			url         = coalesce($3::text, url),
			event_types = coalesce($4::text[], event_types),
			description = coalesce($5::text, description),
			enabled     = coalesce($6::boolean, enabled),
			disabled_reason = CASE WHEN `+enabling+` THEN '' WHEN `+disabling+` THEN '`+WebhookDisabledByUser+`' ELSE disabled_reason END,
			disabled_at     = CASE WHEN `+enabling+` THEN NULL WHEN `+disabling+` THEN now() ELSE disabled_at END,
			cursor_txid     = CASE WHEN `+enabling+` THEN `+currentTxid+` ELSE cursor_txid END,
			cursor_event_id = CASE WHEN `+enabling+` THEN 0 ELSE cursor_event_id END,
			attempts        = CASE WHEN `+reset+` THEN 0 ELSE attempts END,
			failing_since   = CASE WHEN `+reset+` THEN NULL ELSE failing_since END,
			next_attempt_at = CASE WHEN `+reset+` THEN now() ELSE next_attempt_at END,
			last_error      = CASE WHEN `+reset+` THEN '' ELSE last_error END,
			lease_token = lease_token + 1,
			lease_until = NULL,
			updated_at  = now()
		WHERE id = $1 AND ($2::text = '' OR tenant_id = $2::text)
		RETURNING `+webhookCols, id, tenantID, u.URL, u.EventTypes, u.Description, u.Enabled))
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, fmt.Errorf("%w: webhook %s", ErrNotFound, id)
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: update webhook %s: %w", id, err)
	}
	return w, nil
}

// RotateWebhookSecret replaces the signing secret. With keepPrevious > 0
// the old secret keeps signing, next to the new one, for that long, so a
// receiver can switch over without rejecting anything.
func (s *Store) RotateWebhookSecret(ctx context.Context, tenantID, id, newSecret string, keepPrevious time.Duration) (Webhook, error) {
	if newSecret == "" {
		return Webhook{}, errors.New("store: RotateWebhookSecret needs a secret")
	}
	w, err := scanWebhook(s.pool.QueryRow(ctx, `UPDATE webhooks SET
			previous_secret = CASE WHEN $4::bigint > 0 THEN secret ELSE '' END,
			previous_secret_expires_at = CASE WHEN $4::bigint > 0 THEN now() + $4::bigint * interval '1 microsecond' END,
			secret = $3,
			lease_token = lease_token + 1,
			lease_until = NULL,
			updated_at = now()
		WHERE id = $1 AND ($2::text = '' OR tenant_id = $2::text)
		RETURNING `+webhookCols, id, tenantID, newSecret, keepPrevious.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Webhook{}, fmt.Errorf("%w: webhook %s", ErrNotFound, id)
	}
	if err != nil {
		return Webhook{}, fmt.Errorf("store: rotate webhook secret %s: %w", id, err)
	}
	return w, nil
}

// DeleteWebhook removes a webhook and, by cascade, its attempt log.
func (s *Store) DeleteWebhook(ctx context.Context, tenantID, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhooks WHERE id = $1 AND ($2::text = '' OR tenant_id = $2::text)`, id, tenantID)
	if err != nil {
		return fmt.Errorf("store: delete webhook %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: webhook %s", ErrNotFound, id)
	}
	return nil
}

// WebhookClaim selects the webhooks ClaimWebhooks may lease.
type WebhookClaim struct {
	// Limit is the largest number of webhooks to lease (at least 1).
	Limit int
	// Lease is how long the caller owns each webhook. The caller must be
	// done, or have stopped, before it ends.
	Lease time.Duration
	// TenantID, when not empty, restricts the claim to one tenant. A
	// server leaves it empty; tests that share a database set it.
	TenantID string
}

// ClaimWebhooks leases up to c.Limit webhooks that have work: enabled,
// due (next_attempt_at has passed), not leased by anyone else, and with
// at least one event after the cursor. The webhook that has waited
// longest comes first.
//
// The candidates are locked with FOR UPDATE SKIP LOCKED and marked in the
// same statement, so two callers, in one process or in two servers that
// share the database, never receive the same webhook while a lease is
// running. Each claim increments lease_token; RecordWebhookProgress only
// accepts the token of the latest claim.
func (s *Store) ClaimWebhooks(ctx context.Context, c WebhookClaim) ([]Webhook, error) {
	if c.Limit <= 0 || c.Lease <= 0 {
		return nil, errors.New("store: ClaimWebhooks needs a positive limit and lease")
	}
	rows, err := s.pool.Query(ctx, `
WITH horizon AS (
	SELECT `+eventHorizon+` AS txid
), due AS (
	SELECT w.id FROM webhooks w, horizon h
	WHERE w.enabled AND w.next_attempt_at <= now()
	  AND (w.lease_until IS NULL OR w.lease_until <= now())
	  AND ($3::text = '' OR w.tenant_id = $3::text)
	  AND EXISTS (SELECT 1 FROM events e
	              WHERE e.tenant_id = w.tenant_id
	                AND (e.txid, e.id) > (w.cursor_txid, w.cursor_event_id)
	                AND e.txid < h.txid)
	ORDER BY w.next_attempt_at, w.id
	LIMIT $1
	FOR UPDATE OF w SKIP LOCKED
)
UPDATE webhooks w SET
	lease_until = now() + $2::bigint * interval '1 microsecond',
	lease_token = w.lease_token + 1
FROM due WHERE w.id = due.id
RETURNING `+qualify("w", webhookCols), c.Limit, c.Lease.Microseconds(), c.TenantID)
	if err != nil {
		return nil, fmt.Errorf("store: claim webhooks: %w", err)
	}
	out, err := collectWebhooks(rows)
	if err != nil {
		return nil, fmt.Errorf("store: claim webhooks: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextAttemptAt.Equal(out[j].NextAttemptAt) {
			return out[i].NextAttemptAt.Before(out[j].NextAttemptAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// qualify prefixes every column of a comma-separated list with an alias.
func qualify(alias, cols string) string {
	out := make([]byte, 0, len(cols)+16*len(alias))
	start := true
	for i := 0; i < len(cols); i++ {
		ch := cols[i]
		if start && ch != ' ' && ch != '\n' && ch != '\t' {
			out = append(out, alias...)
			out = append(out, '.')
			start = false
		}
		if ch == ',' {
			start = true
		}
		out = append(out, ch)
	}
	return string(out)
}

// WebhookEvent is an event of the log as the webhook worker reads it:
// the row, its position, and what the sandbox row adds to it.
type WebhookEvent struct {
	Event
	Txid int64
	// TemplateID and Metadata come from the sandbox row as it is now;
	// both are empty once that row has been garbage-collected.
	TemplateID string
	Metadata   map[string]string
}

// PendingWebhookEvents returns, in delivery order, up to limit events of
// the tenant that sort after the cursor (afterTxid, afterID) and are
// final: written by transactions older than every transaction still
// running. An event is therefore never skipped because a slower
// transaction commits an earlier-sorting one later.
func (s *Store) PendingWebhookEvents(ctx context.Context, tenantID string, afterTxid, afterID int64, limit int) ([]WebhookEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id, e.txid, e.sandbox_id, e.type, e.from_state, e.to_state, e.reason, e.at, e.data,
			coalesce(s.template_id, ''), s.metadata
		FROM events e LEFT JOIN sandboxes s ON s.id = e.sandbox_id AND s.tenant_id = e.tenant_id
		WHERE e.tenant_id = $1 AND (e.txid, e.id) > ($2::bigint, $3::bigint) AND e.txid < `+eventHorizon+`
		ORDER BY e.txid, e.id LIMIT $4`, tenantID, afterTxid, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending webhook events: %w", err)
	}
	defer rows.Close()
	var out []WebhookEvent
	for rows.Next() {
		var (
			e             WebhookEvent
			from, to, why string
			data, meta    []byte
		)
		if err := rows.Scan(&e.ID, &e.Txid, &e.SandboxID, &e.Type, &from, &to, &why, &e.At, &data, &e.TemplateID, &meta); err != nil {
			return nil, err
		}
		e.TenantID = tenantID
		e.FromState, e.ToState, e.Reason = core.State(from), core.State(to), core.TerminationReason(why)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &e.Data); err != nil {
				return nil, fmt.Errorf("store: decode event %d: %w", e.ID, err)
			}
		}
		if len(meta) > 0 {
			if err := json.Unmarshal(meta, &e.Metadata); err != nil {
				return nil, fmt.Errorf("store: decode metadata of %s: %w", e.SandboxID, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// WebhookDelivery is one row of the attempt log.
type WebhookDelivery struct {
	ID        int64
	WebhookID string
	// EventID is the events.id; zero for a test event.
	EventID        int64
	MessageID      string
	EventType      string
	Attempt        int
	Status         string // DeliverySucceeded, DeliveryFailed or DeliveryDropped
	ResponseStatus int    // HTTP status; zero when no response arrived
	DurationMs     int
	ErrorCode      string
	Error          string
	// DroppedEvents is how many events a DeliveryDropped row accounts for.
	DroppedEvents int
	CreatedAt     time.Time
}

// WebhookOutcome says what an attempt did to the webhook's failure state.
type WebhookOutcome int

const (
	// WebhookNoAttempt: nothing was sent (events were filtered out or
	// dropped); the failure state is left alone.
	WebhookNoAttempt WebhookOutcome = iota
	// WebhookSucceeded: the endpoint accepted an event; the run of
	// failures, if any, is over.
	WebhookSucceeded
	// WebhookFailed: an attempt failed; a run of failures starts or continues.
	WebhookFailed
)

// WebhookProgress is one atomic step of a webhook's delivery.
type WebhookProgress struct {
	WebhookID  string
	LeaseToken int64

	// Advance moves the cursor to (CursorTxid, CursorEventID).
	Advance       bool
	CursorTxid    int64
	CursorEventID int64

	Outcome WebhookOutcome
	// Attempts is the new count of failed attempts on the event after the
	// cursor (zero after the cursor moved).
	Attempts int
	// Error is stored as last_error when Outcome is WebhookFailed.
	Error string
	// RetryIn is when the webhook is next due, from the database's now().
	RetryIn time.Duration
	// Dropped is added to the webhook's count of events given up on.
	Dropped int64
	// Disable, when not empty, disables the webhook with that reason.
	Disable string
	// Release ends the lease.
	Release bool

	// Deliveries are appended to the attempt log; WebhookID and ID are
	// set by the store.
	Deliveries []WebhookDelivery
}

// RecordWebhookProgress applies p in one transaction: the cursor, the
// retry state and the attempt-log rows change together or not at all. It
// is fenced by the lease token: if the caller no longer holds the lease it
// returns ErrWebhookLeaseLost and writes nothing, so a worker that was
// overtaken can neither move the cursor backwards nor record an attempt
// twice.
func (s *Store) RecordWebhookProgress(ctx context.Context, p WebhookProgress) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin webhook progress: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	tag, err := tx.Exec(ctx, `UPDATE webhooks SET
			cursor_txid     = CASE WHEN $3::boolean THEN $4::bigint ELSE cursor_txid END,
			cursor_event_id = CASE WHEN $3::boolean THEN $5::bigint ELSE cursor_event_id END,
			attempts        = $6::int,
			failing_since   = CASE $7::int WHEN 1 THEN NULL WHEN 2 THEN coalesce(failing_since, now()) ELSE failing_since END,
			last_success_at = CASE WHEN $7::int = 1 THEN now() ELSE last_success_at END,
			last_failure_at = CASE WHEN $7::int = 2 THEN now() ELSE last_failure_at END,
			last_error      = CASE $7::int WHEN 1 THEN '' WHEN 2 THEN $8::text ELSE last_error END,
			next_attempt_at = now() + $9::bigint * interval '1 microsecond',
			dropped_events  = dropped_events + $10::bigint,
			enabled         = CASE WHEN $11::text <> '' THEN false ELSE enabled END,
			disabled_reason = CASE WHEN $11::text <> '' THEN $11::text ELSE disabled_reason END,
			disabled_at     = CASE WHEN $11::text <> '' THEN now() ELSE disabled_at END,
			lease_until     = CASE WHEN $12::boolean OR $11::text <> '' THEN NULL ELSE lease_until END
		WHERE id = $1 AND lease_token = $2`,
		p.WebhookID, p.LeaseToken, p.Advance, p.CursorTxid, p.CursorEventID, p.Attempts, int(p.Outcome), p.Error,
		p.RetryIn.Microseconds(), p.Dropped, p.Disable, p.Release)
	if err != nil {
		return fmt.Errorf("store: webhook progress %s: %w", p.WebhookID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: webhook %s", ErrWebhookLeaseLost, p.WebhookID)
	}
	for _, d := range p.Deliveries {
		d.WebhookID = p.WebhookID
		if err := insertWebhookDelivery(ctx, tx, d); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit webhook progress %s: %w", p.WebhookID, err)
	}
	return nil
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func insertWebhookDelivery(ctx context.Context, q execer, d WebhookDelivery) error {
	var eventID *int64
	if d.EventID != 0 {
		eventID = &d.EventID
	}
	if d.Attempt <= 0 {
		d.Attempt = 1
	}
	_, err := q.Exec(ctx, `INSERT INTO webhook_deliveries
			(webhook_id, event_id, message_id, event_type, attempt, status, response_status, duration_ms, error_code, error, dropped_events)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		d.WebhookID, eventID, d.MessageID, d.EventType, d.Attempt, d.Status, d.ResponseStatus, d.DurationMs, d.ErrorCode, d.Error, d.DroppedEvents)
	if err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: webhook %s", ErrNotFound, d.WebhookID)
		}
		return fmt.Errorf("store: record webhook delivery: %w", err)
	}
	return nil
}

// AppendWebhookDelivery adds one row to the attempt log outside of a
// lease: the record of a test event. It does not touch the cursor or the
// retry state.
func (s *Store) AppendWebhookDelivery(ctx context.Context, d WebhookDelivery) error {
	return insertWebhookDelivery(ctx, s.pool, d)
}

// ListWebhookDeliveries returns the attempt log of a webhook, newest
// first. limit is capped at 500; zero or negative means 50.
func (s *Store) ListWebhookDeliveries(ctx context.Context, webhookID string, limit int) ([]WebhookDelivery, error) {
	switch {
	case limit <= 0:
		limit = 50
	case limit > 500:
		limit = 500
	}
	rows, err := s.pool.Query(ctx, `SELECT id, webhook_id, coalesce(event_id, 0), message_id, event_type, attempt, status,
			response_status, duration_ms, error_code, error, dropped_events, created_at
		FROM webhook_deliveries WHERE webhook_id = $1 ORDER BY id DESC LIMIT $2`, webhookID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list webhook deliveries: %w", err)
	}
	defer rows.Close()
	var out []WebhookDelivery
	for rows.Next() {
		var d WebhookDelivery
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.EventID, &d.MessageID, &d.EventType, &d.Attempt, &d.Status,
			&d.ResponseStatus, &d.DurationMs, &d.ErrorCode, &d.Error, &d.DroppedEvents, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// PruneWebhookDeliveries deletes attempt-log rows older than olderThan
// and returns how many were removed.
func (s *Store) PruneWebhookDeliveries(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhook_deliveries
		WHERE created_at < now() - $1::bigint * interval '1 microsecond'`, olderThan.Microseconds())
	if err != nil {
		return 0, fmt.Errorf("store: prune webhook deliveries: %w", err)
	}
	return tag.RowsAffected(), nil
}

// RepairWebhookCursors moves to the present every cursor that points at
// a transaction id the database has not reached yet, and returns how many
// it moved. That only happens after the database was rebuilt from a
// logical dump (pg_dump and restore start the transaction counter again,
// while the rows keep the old ids): without the repair such a webhook
// would wait until the counter caught up. Events between the dump and the
// repair are not delivered to the repaired webhooks.
func (s *Store) RepairWebhookCursors(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE webhooks SET cursor_txid = `+currentTxid+`, cursor_event_id = 0,
			lease_token = lease_token + 1, lease_until = NULL
		WHERE cursor_txid > pg_snapshot_xmax(pg_current_snapshot())::text::bigint`)
	if err != nil {
		return 0, fmt.Errorf("store: repair webhook cursors: %w", err)
	}
	return tag.RowsAffected(), nil
}

// webhookBacklogCap bounds the per-webhook count in WebhookStats.
const webhookBacklogCap = 10000

// WebhookStats summarises every webhook in the database, for metrics.
type WebhookStats struct {
	Enabled  int
	Disabled int
	// Backlog is the number of events after the cursors of the enabled
	// webhooks, whether or not their subscriptions match, each webhook
	// counted up to 10000.
	Backlog int64
	// Lag is the age of the oldest such event; zero when there is none.
	Lag time.Duration
	// Failing is the number of enabled webhooks whose last attempt failed.
	Failing int
}

// WebhookStats reads the backlog and lag of webhook delivery.
func (s *Store) WebhookStats(ctx context.Context) (WebhookStats, error) {
	var (
		st  WebhookStats
		lag float64
	)
	err := s.pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE w.enabled),
       count(*) FILTER (WHERE NOT w.enabled),
       count(*) FILTER (WHERE w.enabled AND w.failing_since IS NOT NULL),
       coalesce(sum(b.n), 0)::bigint,
       coalesce(extract(epoch FROM now() - min(b.oldest)), 0)::double precision
FROM webhooks w
LEFT JOIN LATERAL (
	SELECT count(*) AS n, min(p.at) AS oldest FROM (
		SELECT e.at FROM events e
		WHERE e.tenant_id = w.tenant_id AND (e.txid, e.id) > (w.cursor_txid, w.cursor_event_id)
		ORDER BY e.txid, e.id LIMIT `+fmt.Sprint(webhookBacklogCap)+`) p
) b ON w.enabled`).Scan(&st.Enabled, &st.Disabled, &st.Failing, &st.Backlog, &lag)
	if err != nil {
		return WebhookStats{}, fmt.Errorf("store: webhook stats: %w", err)
	}
	if lag > 0 {
		st.Lag = time.Duration(lag * float64(time.Second))
	}
	return st, nil
}
