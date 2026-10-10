// Package webhooktest holds an in-memory stand-in for the webhook tables
// of the store, so that the worker, the management service and the API
// routes can be tested without Postgres.
package webhooktest

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/store"
)

// MemStore is webhook.Store and webhook.ServiceStore in memory, with the
// semantics of the SQL in internal/store/webhooks.go: claims lease,
// progress is fenced by the lease token, and "now" is the test's clock.
// The event log is its own (Emit) unless a source is set (SetSource).
type MemStore struct {
	now func() time.Time

	mu         sync.Mutex
	hooks      map[string]*store.Webhook
	events     []store.WebhookEvent // in (txid, id) order
	source     func() []store.WebhookEvent
	deliveries []store.WebhookDelivery
	nextID     int64
	failRecord error // returned by the next RecordWebhookProgress
}

// NewMemStore returns an empty store whose clock is now.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{now: now, hooks: map[string]*store.Webhook{}}
}

// SetSource makes the store read its event log from src instead of from
// what Emit appended. src returns every event so far, in order, with Txid
// equal to ID.
func (m *MemStore) SetSource(src func() []store.WebhookEvent) {
	m.mu.Lock()
	m.source = src
	m.mu.Unlock()
}

// log returns the event log. The caller holds m.mu.
func (m *MemStore) log() []store.WebhookEvent {
	if m.source != nil {
		return m.source()
	}
	return m.events
}

// headLocked is the position of the newest event.
func (m *MemStore) headLocked() int64 {
	if m.source != nil {
		var head int64
		for _, e := range m.source() {
			head = max(head, e.ID)
		}
		return head
	}
	return m.nextID
}

// Head is the id of the newest event.
func (m *MemStore) Head() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.headLocked()
}

// FailNextRecord makes the next RecordWebhookProgress return err and
// write nothing.
func (m *MemStore) FailNextRecord(err error) {
	m.mu.Lock()
	m.failRecord = err
	m.mu.Unlock()
}

// Edit changes a webhook row in place.
func (m *MemStore) Edit(id string, f func(*store.Webhook)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f(m.hooks[id])
}

// Emit appends an event of a tenant; at is when it happened.
func (m *MemStore) Emit(tenant string, e store.Event, at time.Time) store.WebhookEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	e.ID, e.TenantID, e.At = m.nextID, tenant, at
	we := store.WebhookEvent{Event: e, Txid: m.nextID, TemplateID: "py"}
	m.events = append(m.events, we)
	return we
}

// Hook returns a copy of a webhook row.
func (m *MemStore) Hook(id string) store.Webhook {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.hooks[id]
}

// Log returns the delivery log of a webhook, oldest first.
func (m *MemStore) Log(id string) []store.WebhookDelivery {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.WebhookDelivery
	for _, d := range m.deliveries {
		if d.WebhookID == id {
			out = append(out, d)
		}
	}
	return out
}

func (m *MemStore) CreateWebhook(_ context.Context, n store.NewWebhook, max int) (store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if max > 0 {
		have := 0
		for _, h := range m.hooks {
			if h.TenantID == n.TenantID {
				have++
			}
		}
		if have >= max {
			return store.Webhook{}, fmt.Errorf("%w: %d", store.ErrWebhookLimit, have)
		}
	}
	now := m.now()
	w := &store.Webhook{ID: n.ID, TenantID: n.TenantID, URL: n.URL, Secret: n.Secret, EventTypes: n.EventTypes,
		Description: n.Description, Enabled: !n.Disabled, CreatedAt: now, UpdatedAt: now, NextAttemptAt: now,
		CursorTxid: m.headLocked(), CursorEventID: m.headLocked()}
	if n.Disabled {
		w.DisabledReason, w.DisabledAt = store.WebhookDisabledByUser, &now
	}
	m.hooks[w.ID] = w
	return *w, nil
}

func (m *MemStore) get(tenant, id string) (*store.Webhook, error) {
	w := m.hooks[id]
	if w == nil || (tenant != "" && w.TenantID != tenant) {
		return nil, fmt.Errorf("%w: webhook %s", store.ErrNotFound, id)
	}
	return w, nil
}

func (m *MemStore) GetWebhook(_ context.Context, tenant, id string) (store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.get(tenant, id)
	if err != nil {
		return store.Webhook{}, err
	}
	return *w, nil
}

func (m *MemStore) ListWebhooks(_ context.Context, tenant string) ([]store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Webhook
	for _, w := range m.hooks {
		if tenant == "" || w.TenantID == tenant {
			out = append(out, *w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *MemStore) UpdateWebhook(_ context.Context, tenant, id string, u store.WebhookUpdate) (store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.get(tenant, id)
	if err != nil {
		return store.Webhook{}, err
	}
	now := m.now()
	reset := false
	if u.URL != nil && *u.URL != w.URL {
		w.URL, reset = *u.URL, true
	}
	if u.EventTypes != nil {
		w.EventTypes = u.EventTypes
	}
	if u.Description != nil {
		w.Description = *u.Description
	}
	if u.Enabled != nil && *u.Enabled != w.Enabled {
		w.Enabled = *u.Enabled
		if w.Enabled {
			w.DisabledReason, w.DisabledAt, reset = "", nil, true
			w.CursorTxid, w.CursorEventID = m.headLocked(), m.headLocked()
		} else {
			w.DisabledReason, w.DisabledAt = store.WebhookDisabledByUser, &now
		}
	}
	if reset {
		w.Attempts, w.FailingSince, w.NextAttemptAt, w.LastError = 0, nil, now, ""
	}
	w.LeaseToken++
	w.LeaseUntil, w.UpdatedAt = nil, now
	return *w, nil
}

func (m *MemStore) RotateWebhookSecret(_ context.Context, tenant, id, secret string, keep time.Duration) (store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.get(tenant, id)
	if err != nil {
		return store.Webhook{}, err
	}
	w.PreviousSecret, w.PreviousSecretExpiresAt = "", nil
	if keep > 0 {
		exp := m.now().Add(keep)
		w.PreviousSecret, w.PreviousSecretExpiresAt = w.Secret, &exp
	}
	w.Secret = secret
	w.LeaseToken++
	w.LeaseUntil = nil
	return *w, nil
}

func (m *MemStore) DeleteWebhook(_ context.Context, tenant, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.get(tenant, id); err != nil {
		return err
	}
	delete(m.hooks, id)
	return nil
}

func (m *MemStore) pendingLocked(tenant string, txid, id int64, limit int) []store.WebhookEvent {
	var out []store.WebhookEvent
	for _, e := range m.log() {
		if e.TenantID != tenant || e.Txid < txid || (e.Txid == txid && e.ID <= id) {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out
}

func (m *MemStore) ClaimWebhooks(_ context.Context, c store.WebhookClaim) ([]store.Webhook, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var due []*store.Webhook
	for _, w := range m.hooks {
		if !w.Enabled || w.NextAttemptAt.After(now) || (w.LeaseUntil != nil && w.LeaseUntil.After(now)) ||
			(c.TenantID != "" && w.TenantID != c.TenantID) {
			continue
		}
		if len(m.pendingLocked(w.TenantID, w.CursorTxid, w.CursorEventID, 1)) == 0 {
			continue
		}
		due = append(due, w)
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].NextAttemptAt.Equal(due[j].NextAttemptAt) {
			return due[i].NextAttemptAt.Before(due[j].NextAttemptAt)
		}
		return due[i].ID < due[j].ID
	})
	if len(due) > c.Limit {
		due = due[:c.Limit]
	}
	out := make([]store.Webhook, 0, len(due))
	for _, w := range due {
		until := now.Add(c.Lease)
		w.LeaseUntil = &until
		w.LeaseToken++
		out = append(out, *w)
	}
	return out, nil
}

func (m *MemStore) PendingWebhookEvents(_ context.Context, tenant string, txid, id int64, limit int) ([]store.WebhookEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingLocked(tenant, txid, id, limit), nil
}

func (m *MemStore) RecordWebhookProgress(_ context.Context, p store.WebhookProgress) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failRecord; err != nil {
		m.failRecord = nil
		return err
	}
	w := m.hooks[p.WebhookID]
	if w == nil || w.LeaseToken != p.LeaseToken {
		return fmt.Errorf("%w: webhook %s", store.ErrWebhookLeaseLost, p.WebhookID)
	}
	now := m.now()
	if p.Advance {
		if p.CursorTxid < w.CursorTxid || (p.CursorTxid == w.CursorTxid && p.CursorEventID < w.CursorEventID) {
			panic(fmt.Sprintf("cursor of %s moved backwards: %d/%d -> %d/%d", w.ID, w.CursorTxid, w.CursorEventID, p.CursorTxid, p.CursorEventID))
		}
		w.CursorTxid, w.CursorEventID = p.CursorTxid, p.CursorEventID
	}
	w.Attempts = p.Attempts
	switch p.Outcome {
	case store.WebhookSucceeded:
		w.FailingSince, w.LastSuccessAt, w.LastError = nil, &now, ""
	case store.WebhookFailed:
		if w.FailingSince == nil {
			w.FailingSince = &now
		}
		w.LastFailureAt, w.LastError = &now, p.Error
	}
	w.NextAttemptAt = now.Add(p.RetryIn)
	w.DroppedEvents += p.Dropped
	if p.Disable != "" {
		w.Enabled, w.DisabledReason, w.DisabledAt = false, p.Disable, &now
	}
	if p.Release || p.Disable != "" {
		w.LeaseUntil = nil
	}
	for _, d := range p.Deliveries {
		d.WebhookID, d.CreatedAt = w.ID, now
		d.ID = int64(len(m.deliveries) + 1)
		m.deliveries = append(m.deliveries, d)
	}
	return nil
}

func (m *MemStore) AppendWebhookDelivery(_ context.Context, d store.WebhookDelivery) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d.ID, d.CreatedAt = int64(len(m.deliveries)+1), m.now()
	m.deliveries = append(m.deliveries, d)
	return nil
}

func (m *MemStore) ListWebhookDeliveries(_ context.Context, id string, limit int) ([]store.WebhookDelivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.WebhookDelivery
	for i := len(m.deliveries) - 1; i >= 0 && (limit <= 0 || len(out) < limit); i-- {
		if m.deliveries[i].WebhookID == id {
			out = append(out, m.deliveries[i])
		}
	}
	return out, nil
}

func (m *MemStore) PruneWebhookDeliveries(context.Context, time.Duration) (int64, error) {
	return 0, nil
}
func (m *MemStore) RepairWebhookCursors(context.Context) (int64, error) { return 0, nil }

func (m *MemStore) WebhookStats(context.Context) (store.WebhookStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st store.WebhookStats
	for _, w := range m.hooks {
		if !w.Enabled {
			st.Disabled++
			continue
		}
		st.Enabled++
		st.Backlog += int64(len(m.pendingLocked(w.TenantID, w.CursorTxid, w.CursorEventID, 0)))
	}
	return st, nil
}
