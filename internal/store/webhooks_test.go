package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type webhookFixture struct {
	fixture
	ctx context.Context
}

func newWebhookFixture(t *testing.T) webhookFixture {
	t.Helper()
	return webhookFixture{fixture: newFixture(t, 5), ctx: context.Background()}
}

func (f webhookFixture) hook(t *testing.T, types ...string) Webhook {
	t.Helper()
	if len(types) == 0 {
		types = []string{"*"}
	}
	w, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: f.tenant.ID,
		URL: "https://hooks.example.com/x", Secret: "whsec_dGVzdA==", EventTypes: types}, 0)
	if err != nil {
		t.Fatalf("CreateWebhook: %v", err)
	}
	return w
}

// emit appends n events for the fixture's tenant and returns nothing; the
// sandbox id only labels them.
func (f webhookFixture) emit(t *testing.T, sandbox string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := f.s.AppendEvent(f.ctx, Event{SandboxID: sandbox, TenantID: f.tenant.ID, Type: "command_finished",
			Data: map[string]any{"n": i}}); err != nil {
			t.Fatalf("AppendEvent: %v", err)
		}
	}
}

func (f webhookFixture) pending(t *testing.T, w Webhook) []WebhookEvent {
	t.Helper()
	evs, err := f.s.PendingWebhookEvents(f.ctx, w.TenantID, w.CursorTxid, w.CursorEventID, 1000)
	if err != nil {
		t.Fatalf("PendingWebhookEvents: %v", err)
	}
	return evs
}

// waitPending polls until the horizon has passed n events. Other tests
// share the database, and a transaction of theirs that is open at the
// wrong moment holds the horizon back for as long as it runs.
func (f webhookFixture) waitPending(t *testing.T, w Webhook, n int) []WebhookEvent {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		evs := f.pending(t, w)
		if len(evs) >= n {
			return evs
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d events became visible below the horizon", len(evs), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f webhookFixture) claim(t *testing.T, limit int) []Webhook {
	t.Helper()
	ws, err := f.s.ClaimWebhooks(f.ctx, WebhookClaim{Limit: limit, Lease: time.Minute, TenantID: f.tenant.ID})
	if err != nil {
		t.Fatalf("ClaimWebhooks: %v", err)
	}
	return ws
}

func TestWebhookCRUD(t *testing.T) {
	f := newWebhookFixture(t)
	other, err := f.s.CreateTenant(f.ctx, Tenant{ID: uniq("tenant"), Name: "other", MaxConcurrent: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.s.DeleteTenant(context.Background(), other.ID) })

	w, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: f.tenant.ID, URL: "https://a.example/h",
		Secret: "whsec_one", EventTypes: []string{"sandbox.created", "command.finished"}, Description: "first"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !w.Enabled || w.DisabledReason != "" || w.Secret != "whsec_one" || w.CursorTxid == 0 || w.Attempts != 0 ||
		len(w.EventTypes) != 2 || w.Description != "first" || w.LeaseUntil != nil {
		t.Fatalf("created webhook: %+v", w)
	}

	// Tenant isolation: another tenant's lookup is "not found", and so
	// are its update, rotation and delete.
	if _, err := f.s.GetWebhook(f.ctx, other.ID, w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get by another tenant: %v", err)
	}
	url := "https://b.example/h"
	if _, err := f.s.UpdateWebhook(f.ctx, other.ID, w.ID, WebhookUpdate{URL: &url}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update by another tenant: %v", err)
	}
	if _, err := f.s.RotateWebhookSecret(f.ctx, other.ID, w.ID, "whsec_x", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("rotate by another tenant: %v", err)
	}
	if err := f.s.DeleteWebhook(f.ctx, other.ID, w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete by another tenant: %v", err)
	}
	if got, err := f.s.GetWebhook(f.ctx, "", w.ID); err != nil || got.ID != w.ID {
		t.Errorf("admin get: %v %v", got.ID, err)
	}

	// The per-tenant limit.
	if _, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: f.tenant.ID, URL: "https://a.example/2",
		Secret: "s", EventTypes: []string{"*"}, Disabled: true}, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: f.tenant.ID, URL: "https://a.example/3",
		Secret: "s", EventTypes: []string{"*"}}, 2); !errors.Is(err, ErrWebhookLimit) {
		t.Errorf("third webhook with a limit of 2: %v", err)
	}
	if _, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: other.ID, URL: "https://a.example/3",
		Secret: "s", EventTypes: []string{"*"}}, 2); err != nil {
		t.Errorf("the limit is per tenant: %v", err)
	}
	if _, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: "no-such-tenant", URL: "https://a.example/3",
		Secret: "s", EventTypes: []string{"*"}}, 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown tenant: %v", err)
	}
	list, err := f.s.ListWebhooks(f.ctx, f.tenant.ID)
	if err != nil || len(list) != 2 || list[0].ID != w.ID {
		t.Fatalf("list: %d %v", len(list), err)
	}
	if list[1].Enabled || list[1].DisabledReason != WebhookDisabledByUser || list[1].DisabledAt == nil {
		t.Errorf("created disabled: %+v", list[1])
	}

	// Update: partial, and it invalidates a lease.
	desc := "second"
	u, err := f.s.UpdateWebhook(f.ctx, f.tenant.ID, w.ID, WebhookUpdate{Description: &desc, EventTypes: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	if u.Description != "second" || u.URL != w.URL || len(u.EventTypes) != 1 || u.LeaseToken != w.LeaseToken+1 || !u.UpdatedAt.After(w.CreatedAt.Add(-time.Second)) {
		t.Errorf("updated: %+v", u)
	}

	// Rotation with and without a grace period.
	r, err := f.s.RotateWebhookSecret(f.ctx, f.tenant.ID, w.ID, "whsec_two", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if r.Secret != "whsec_two" || r.PreviousSecret != "whsec_one" || r.PreviousSecretExpiresAt == nil ||
		time.Until(*r.PreviousSecretExpiresAt) < 50*time.Minute {
		t.Errorf("rotated with grace: %+v", r)
	}
	r, err = f.s.RotateWebhookSecret(f.ctx, f.tenant.ID, w.ID, "whsec_three", 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.Secret != "whsec_three" || r.PreviousSecret != "" || r.PreviousSecretExpiresAt != nil {
		t.Errorf("rotated without grace: %+v", r)
	}

	if err := f.s.DeleteWebhook(f.ctx, f.tenant.ID, w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.GetWebhook(f.ctx, f.tenant.ID, w.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	// Deleting the tenant removes its webhooks.
	if err := f.s.DeleteTenant(f.ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	if list, _ := f.s.ListWebhooks(f.ctx, other.ID); len(list) != 0 {
		t.Errorf("%d webhooks survived their tenant", len(list))
	}
}

func TestWebhookLimitUnderConcurrency(t *testing.T) {
	f := newWebhookFixture(t)
	const limit, tries = 3, 12
	var (
		wg      sync.WaitGroup
		created atomic.Int32
	)
	for i := 0; i < tries; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.s.CreateWebhook(f.ctx, NewWebhook{ID: uniq("wh"), TenantID: f.tenant.ID, URL: "https://a.example/h",
				Secret: "s", EventTypes: []string{"*"}}, limit)
			switch {
			case err == nil:
				created.Add(1)
			case !errors.Is(err, ErrWebhookLimit):
				t.Errorf("CreateWebhook: %v", err)
			}
		}()
	}
	wg.Wait()
	list, _ := f.s.ListWebhooks(f.ctx, f.tenant.ID)
	if created.Load() != limit || len(list) != limit {
		t.Fatalf("%d creates succeeded and %d rows exist; want %d", created.Load(), len(list), limit)
	}
}

// Only events written after the webhook was created are pending, in order,
// and with what the sandbox row adds.
func TestWebhookPendingEvents(t *testing.T) {
	f := newWebhookFixture(t)
	f.emit(t, "sb-before", 3)
	w := f.hook(t)
	if got := f.claim(t, 10); len(got) != 0 {
		t.Fatalf("a webhook with nothing after its cursor was claimed: %+v", got)
	}

	// A real sandbox, so that the join has something to find.
	sb, err := f.s.InsertCreating(f.ctx, NewSandbox{ID: uniq("sb"), TemplateID: f.tmpl.ID, TemplateVersion: f.tmpl.Version, HostID: f.host})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.MarkWarm(f.ctx, sb.ID, sb.Generation, "inst"); err != nil {
		t.Fatal(err)
	}
	claimed, err := f.s.Claim(f.ctx, ClaimParams{TenantID: f.tenant.ID, TemplateID: f.tmpl.ID, HostID: f.host})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetMetadata(f.ctx, claimed.ID, claimed.Generation, map[string]string{"job": "42"}); err != nil {
		t.Fatal(err)
	}
	f.emit(t, claimed.ID, 2)

	evs := f.waitPending(t, w, 3)
	if len(evs) != 3 {
		t.Fatalf("pending = %d events, want 3 (claimed + 2): %+v", len(evs), evs)
	}
	if evs[0].Type != EventClaimed || evs[0].SandboxID != claimed.ID || evs[0].TemplateID != f.tmpl.ID || evs[0].Metadata["job"] != "42" ||
		evs[0].TenantID != f.tenant.ID || evs[0].ToState != "ASSIGNED" {
		t.Errorf("first pending event: %+v", evs[0])
	}
	for i := 1; i < len(evs); i++ {
		a, b := evs[i-1], evs[i]
		if b.Txid < a.Txid || (b.Txid == a.Txid && b.ID <= a.ID) {
			t.Errorf("events out of (txid, id) order: %d/%d then %d/%d", a.Txid, a.ID, b.Txid, b.ID)
		}
	}
	// The warm-pool part of the sandbox's life has no tenant and is not there.
	for _, e := range evs {
		if e.Type == EventCreated || e.ToState == "WARM" {
			t.Errorf("a tenant-less event is pending: %+v", e)
		}
	}
	if got := f.pending(t, Webhook{TenantID: f.tenant.ID, CursorTxid: evs[1].Txid, CursorEventID: evs[1].ID}); len(got) != 1 || got[0].ID != evs[2].ID {
		t.Errorf("pending after the second event: %+v", got)
	}
}

// An event of a transaction that commits late is not skipped: nothing at
// or above the oldest running transaction is handed out, so the cursor
// can never move past an event that is still invisible.
func TestWebhookCursorDoesNotSkipLateCommit(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)

	// Transaction A writes an event and stays open.
	tx, err := f.s.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(f.ctx) //nolint:errcheck
	var slowID int64
	if err := tx.QueryRow(f.ctx, `INSERT INTO events (sandbox_id, tenant_id, type) VALUES ('sb-slow', $1, 'command_finished') RETURNING id`,
		f.tenant.ID).Scan(&slowID); err != nil {
		t.Fatal(err)
	}
	// B starts later, takes a larger id and commits first.
	f.emit(t, "sb-fast", 1)

	time.Sleep(50 * time.Millisecond)
	if evs := f.pending(t, w); len(evs) != 0 {
		t.Fatalf("an event above the horizon was handed out while an older transaction was open: %+v", evs)
	}
	if got := f.claim(t, 10); len(got) != 0 {
		t.Fatalf("the webhook was claimed with nothing deliverable: %+v", got)
	}
	if err := tx.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	evs := f.waitPending(t, w, 2)
	if len(evs) != 2 || evs[0].ID != slowID || evs[0].SandboxID != "sb-slow" || evs[1].SandboxID != "sb-fast" {
		t.Fatalf("after the late commit: %+v (want sb-slow, id %d, first)", evs, slowID)
	}
	if evs[0].ID > evs[1].ID {
		t.Fatalf("the slow transaction should hold the smaller id: %d, %d", evs[0].ID, evs[1].ID)
	}
}

// The cursor is a position in (txid, id) order, which need not be id
// order: a transaction that started first but inserted last sorts first.
// Moving the cursor to its event must not hide the smaller id behind it.
func TestWebhookCursorOrderIsTxidThenID(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)

	older, err := f.s.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer older.Rollback(f.ctx) //nolint:errcheck
	var olderTxid int64
	if err := older.QueryRow(f.ctx, `SELECT `+currentTxid).Scan(&olderTxid); err != nil {
		t.Fatal(err)
	}
	newer, err := f.s.pool.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.Rollback(f.ctx) //nolint:errcheck
	const ins = `INSERT INTO events (sandbox_id, tenant_id, type) VALUES ($1, $2, 'command_finished') RETURNING id, txid`
	var newerID, newerTxid, olderID, gotOlderTxid int64
	if err := newer.QueryRow(f.ctx, ins, "sb-newer", f.tenant.ID).Scan(&newerID, &newerTxid); err != nil {
		t.Fatal(err)
	}
	if err := older.QueryRow(f.ctx, ins, "sb-older", f.tenant.ID).Scan(&olderID, &gotOlderTxid); err != nil {
		t.Fatal(err)
	}
	if gotOlderTxid != olderTxid || olderTxid >= newerTxid || olderID <= newerID {
		t.Fatalf("setup: older tx %d wrote id %d, newer tx %d wrote id %d", gotOlderTxid, olderID, newerTxid, newerID)
	}
	if err := newer.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := older.Commit(f.ctx); err != nil {
		t.Fatal(err)
	}
	evs := f.waitPending(t, w, 2)
	if len(evs) != 2 || evs[0].ID != olderID || evs[1].ID != newerID {
		t.Fatalf("order: %+v; want id %d (txid %d) before id %d (txid %d)", evs, olderID, olderTxid, newerID, newerTxid)
	}
	rest := f.pending(t, Webhook{TenantID: f.tenant.ID, CursorTxid: evs[0].Txid, CursorEventID: evs[0].ID})
	if len(rest) != 1 || rest[0].ID != newerID {
		t.Fatalf("after the cursor moved to id %d, the smaller id %d is gone: %+v", olderID, newerID, rest)
	}
}

func TestWebhookProgressIsAtomicAndFenced(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)
	f.emit(t, "sb-1", 3)
	evs := f.waitPending(t, w, 3)

	claimed := f.claim(t, 10)
	if len(claimed) != 1 || claimed[0].ID != w.ID || claimed[0].LeaseToken != w.LeaseToken+1 || claimed[0].LeaseUntil == nil {
		t.Fatalf("claim: %+v", claimed)
	}
	c := claimed[0]
	// While the lease runs nobody else gets it.
	if again := f.claim(t, 10); len(again) != 0 {
		t.Fatalf("a leased webhook was claimed again: %+v", again)
	}

	// A failure: no cursor movement, a run of failures begins.
	err := f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c.ID, LeaseToken: c.LeaseToken,
		Outcome: WebhookFailed, Attempts: 1, Error: "boom", RetryIn: time.Hour, Release: true,
		Deliveries: []WebhookDelivery{{EventID: evs[0].ID, MessageID: "evt_1", EventType: "command.finished", Attempt: 1,
			Status: DeliveryFailed, ResponseStatus: 500, DurationMs: 12, ErrorCode: "http_status", Error: "boom"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := f.s.GetWebhook(f.ctx, "", w.ID)
	if got.CursorEventID != w.CursorEventID || got.CursorTxid != w.CursorTxid || got.Attempts != 1 || got.FailingSince == nil ||
		got.LastError != "boom" || got.LastFailureAt == nil || got.LeaseUntil != nil || time.Until(got.NextAttemptAt) < 50*time.Minute {
		t.Fatalf("after a failure: %+v", got)
	}
	// Not due: not claimable although it has pending events.
	if again := f.claim(t, 10); len(again) != 0 {
		t.Fatalf("a webhook that is backing off was claimed: %+v", again)
	}
	// Make it due again and claim.
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE webhooks SET next_attempt_at = now() WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	c2 := f.claim(t, 10)[0]

	// The first lease is stale now: its progress is refused and nothing
	// of it is written, neither the cursor nor the log row.
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c.ID, LeaseToken: c.LeaseToken,
		Advance: true, CursorTxid: evs[2].Txid, CursorEventID: evs[2].ID, Outcome: WebhookSucceeded,
		Deliveries: []WebhookDelivery{{EventID: evs[2].ID, MessageID: "stale", Status: DeliverySucceeded}}})
	if !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("stale lease: %v", err)
	}
	got, _ = f.s.GetWebhook(f.ctx, "", w.ID)
	if got.CursorEventID != w.CursorEventID || got.FailingSince == nil {
		t.Fatalf("a stale worker changed the row: %+v", got)
	}
	dels, _ := f.s.ListWebhookDeliveries(f.ctx, w.ID, 0)
	if len(dels) != 1 || dels[0].MessageID != "evt_1" || dels[0].Status != DeliveryFailed || dels[0].ResponseStatus != 500 ||
		dels[0].DurationMs != 12 || dels[0].ErrorCode != "http_status" || dels[0].EventID != evs[0].ID {
		t.Fatalf("attempt log: %+v", dels)
	}

	// A success under the current lease: cursor, state and log together.
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c2.ID, LeaseToken: c2.LeaseToken,
		Advance: true, CursorTxid: evs[0].Txid, CursorEventID: evs[0].ID, Outcome: WebhookSucceeded, Dropped: 2,
		Deliveries: []WebhookDelivery{{EventID: evs[0].ID, MessageID: "evt_1", EventType: "command.finished", Attempt: 2, Status: DeliverySucceeded, ResponseStatus: 204}}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = f.s.GetWebhook(f.ctx, "", w.ID)
	if got.CursorEventID != evs[0].ID || got.CursorTxid != evs[0].Txid || got.Attempts != 0 || got.FailingSince != nil ||
		got.LastError != "" || got.LastSuccessAt == nil || got.DroppedEvents != 2 || got.LeaseUntil == nil {
		t.Fatalf("after a success: %+v", got)
	}
	if rest := f.pending(t, got); len(rest) != 2 || rest[0].ID != evs[1].ID {
		t.Fatalf("pending after the cursor moved: %+v", rest)
	}

	// A log row that cannot be written takes the cursor move down with it.
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c2.ID, LeaseToken: c2.LeaseToken,
		Advance: true, CursorTxid: evs[1].Txid, CursorEventID: evs[1].ID, Outcome: WebhookSucceeded,
		Deliveries: []WebhookDelivery{{EventID: evs[1].ID, Status: "no-such-status"}}})
	if err == nil {
		t.Fatal("an invalid attempt-log row was accepted")
	}
	if after, _ := f.s.GetWebhook(f.ctx, "", w.ID); after.CursorEventID != evs[0].ID {
		t.Fatalf("the cursor moved although the transaction failed: %+v", after)
	}

	// Disabling through progress ends the lease and takes it out of claims.
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c2.ID, LeaseToken: c2.LeaseToken,
		Outcome: WebhookFailed, Attempts: 1, Error: "gone", Disable: WebhookDisabledGone})
	if err != nil {
		t.Fatal(err)
	}
	got, _ = f.s.GetWebhook(f.ctx, "", w.ID)
	if got.Enabled || got.DisabledReason != WebhookDisabledGone || got.DisabledAt == nil || got.LeaseUntil != nil {
		t.Fatalf("after disable: %+v", got)
	}
	if again := f.claim(t, 10); len(again) != 0 {
		t.Fatalf("a disabled webhook was claimed: %+v", again)
	}

	// Enabling it again starts from the present with a clean slate.
	f.emit(t, "sb-2", 1)
	on := true
	en, err := f.s.UpdateWebhook(f.ctx, f.tenant.ID, w.ID, WebhookUpdate{Enabled: &on})
	if err != nil {
		t.Fatal(err)
	}
	if !en.Enabled || en.DisabledReason != "" || en.DisabledAt != nil || en.FailingSince != nil || en.Attempts != 0 || en.LastError != "" {
		t.Fatalf("after enable: %+v", en)
	}
	time.Sleep(20 * time.Millisecond)
	if evs := f.pending(t, en); len(evs) != 0 {
		t.Fatalf("events from the disabled period are pending after enabling: %+v", evs)
	}
	f.emit(t, "sb-3", 1)
	if evs := f.waitPending(t, en, 1); evs[0].SandboxID != "sb-3" {
		t.Fatalf("first event after enabling: %+v", evs[0])
	}
}

// A configuration change fences the worker that holds the lease.
func TestWebhookUpdateInvalidatesLease(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)
	f.emit(t, "sb-1", 1)
	evs := f.waitPending(t, w, 1)
	c := f.claim(t, 1)[0]

	url := "https://new.example/h"
	u, err := f.s.UpdateWebhook(f.ctx, f.tenant.ID, w.ID, WebhookUpdate{URL: &url})
	if err != nil {
		t.Fatal(err)
	}
	if u.LeaseUntil != nil || u.LeaseToken != c.LeaseToken+1 {
		t.Fatalf("update left the lease in place: %+v", u)
	}
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c.ID, LeaseToken: c.LeaseToken, Advance: true,
		CursorTxid: evs[0].Txid, CursorEventID: evs[0].ID, Outcome: WebhookSucceeded})
	if !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("progress under the old configuration: %v", err)
	}
	// The event is still pending and the webhook claimable at once.
	if again := f.claim(t, 1); len(again) != 1 || again[0].URL != url {
		t.Fatalf("claim after update: %+v", again)
	}
	if err := f.s.DeleteWebhook(f.ctx, "", w.ID); err != nil {
		t.Fatal(err)
	}
	err = f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: c.ID, LeaseToken: c.LeaseToken + 2, Release: true})
	if !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("progress on a deleted webhook: %v", err)
	}
}

// Many claimers, as two sbxd processes on one database would be: a
// webhook is never held by two of them at once, no event is handed to two
// deliveries at the same time, and with no lease expiring every event is
// delivered exactly once and in order.
func TestWebhookConcurrentWorkersNeverOverlap(t *testing.T) {
	f := newWebhookFixture(t)
	const hooks, events, workers = 4, 30, 6
	ws := make([]Webhook, hooks)
	for i := range ws {
		ws[i] = f.hook(t)
	}
	f.emit(t, "sb-1", events)
	f.waitPending(t, ws[0], events)

	var (
		mu        sync.Mutex
		holding   = map[string]int{}     // webhook id -> workers inside
		inFlight  = map[string]int{}     // webhook id + event id -> deliveries in progress
		delivered = map[string][]int64{} // webhook id -> event ids in delivery order
		overlap   []string
		wg        sync.WaitGroup
		done      atomic.Int32
	)
	total := int32(hooks * events)
	ctx, cancel := context.WithTimeout(f.ctx, 60*time.Second)
	defer cancel()
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for done.Load() < total && ctx.Err() == nil {
				claimed, err := f.s.ClaimWebhooks(ctx, WebhookClaim{Limit: 2, Lease: time.Minute, TenantID: f.tenant.ID})
				if err != nil {
					if ctx.Err() == nil {
						t.Errorf("ClaimWebhooks: %v", err)
					}
					return
				}
				if len(claimed) == 0 {
					time.Sleep(time.Millisecond)
					continue
				}
				for _, w := range claimed {
					mu.Lock()
					if holding[w.ID]++; holding[w.ID] > 1 {
						overlap = append(overlap, "webhook "+w.ID+" held twice")
					}
					mu.Unlock()
					evs, err := f.s.PendingWebhookEvents(ctx, w.TenantID, w.CursorTxid, w.CursorEventID, 7)
					if err != nil {
						t.Errorf("PendingWebhookEvents: %v", err)
					}
					for j, e := range evs {
						key := fmt.Sprintf("%s/%d", w.ID, e.ID)
						mu.Lock()
						if inFlight[key]++; inFlight[key] > 1 {
							overlap = append(overlap, "event "+key+" in two deliveries at once")
						}
						mu.Unlock()
						time.Sleep(200 * time.Microsecond) // the HTTP request
						mu.Lock()
						inFlight[key]--
						delivered[w.ID] = append(delivered[w.ID], e.ID)
						mu.Unlock()
						if err := f.s.RecordWebhookProgress(ctx, WebhookProgress{WebhookID: w.ID, LeaseToken: w.LeaseToken,
							Advance: true, CursorTxid: e.Txid, CursorEventID: e.ID, Outcome: WebhookSucceeded,
							Release: j == len(evs)-1}); err != nil {
							t.Errorf("RecordWebhookProgress: %v", err)
						}
						done.Add(1)
					}
					mu.Lock()
					holding[w.ID]--
					mu.Unlock()
					if len(evs) == 0 {
						_ = f.s.RecordWebhookProgress(ctx, WebhookProgress{WebhookID: w.ID, LeaseToken: w.LeaseToken, Release: true})
					}
				}
			}
		}()
	}
	wg.Wait()
	if len(overlap) > 0 {
		t.Fatalf("concurrent delivery: %v", overlap)
	}
	for _, w := range ws {
		got := delivered[w.ID]
		if len(got) != events {
			t.Fatalf("webhook %s: %d deliveries, want %d", w.ID, len(got), events)
		}
		for i := 1; i < len(got); i++ {
			if got[i] <= got[i-1] {
				t.Fatalf("webhook %s: event %d delivered after %d", w.ID, got[i], got[i-1])
			}
		}
	}
}

// An expired lease can be taken over, and the worker that lost it is fenced.
func TestWebhookExpiredLeaseIsTakenOver(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)
	f.emit(t, "sb-1", 1)
	f.waitPending(t, w, 1)
	first, err := f.s.ClaimWebhooks(f.ctx, WebhookClaim{Limit: 1, Lease: 30 * time.Millisecond, TenantID: f.tenant.ID})
	if err != nil || len(first) != 1 {
		t.Fatalf("claim: %v %v", first, err)
	}
	var second []Webhook
	deadline := time.Now().Add(5 * time.Second)
	for len(second) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		second = f.claim(t, 1)
	}
	if len(second) != 1 || second[0].LeaseToken != first[0].LeaseToken+1 {
		t.Fatalf("takeover: %+v", second)
	}
	if err := f.s.RecordWebhookProgress(f.ctx, WebhookProgress{WebhookID: w.ID, LeaseToken: first[0].LeaseToken, Release: true}); !errors.Is(err, ErrWebhookLeaseLost) {
		t.Fatalf("the overtaken worker was not fenced: %v", err)
	}
}

func TestWebhookClaimOrderIsLongestWaitingFirst(t *testing.T) {
	f := newWebhookFixture(t)
	a, b, c := f.hook(t), f.hook(t), f.hook(t)
	for id, ago := range map[string]int{a.ID: 10, b.ID: 30, c.ID: 20} {
		if _, err := f.s.pool.Exec(f.ctx, `UPDATE webhooks SET next_attempt_at = now() - $2 * interval '1 second' WHERE id = $1`, id, ago); err != nil {
			t.Fatal(err)
		}
	}
	f.emit(t, "sb-1", 1)
	f.waitPending(t, a, 1)
	got := f.claim(t, 2)
	if len(got) != 2 || got[0].ID != b.ID || got[1].ID != c.ID {
		t.Fatalf("claim order: %v, %v; want %s then %s", got[0].ID, got[1].ID, b.ID, c.ID)
	}
	if rest := f.claim(t, 2); len(rest) != 1 || rest[0].ID != a.ID {
		t.Fatalf("the rest: %+v", rest)
	}
}

func TestWebhookStatsPruneAndRepair(t *testing.T) {
	f := newWebhookFixture(t)
	w := f.hook(t)
	before, err := f.s.WebhookStats(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.emit(t, "sb-1", 4)
	f.waitPending(t, w, 4)
	after, err := f.s.WebhookStats(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Other tests share the database, so only lower bounds hold.
	if after.Enabled < 1 || after.Backlog < 4 || after.Lag <= 0 {
		t.Errorf("stats: before %+v, after %+v", before, after)
	}

	if err := f.s.AppendWebhookDelivery(f.ctx, WebhookDelivery{WebhookID: w.ID, MessageID: "evt_test_1", EventType: "webhook.test",
		Status: DeliverySucceeded, ResponseStatus: 200}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.AppendWebhookDelivery(f.ctx, WebhookDelivery{WebhookID: "wh-none", Status: DeliverySucceeded}); !errors.Is(err, ErrNotFound) {
		t.Errorf("delivery for an unknown webhook: %v", err)
	}
	if _, err := f.s.pool.Exec(f.ctx, `INSERT INTO webhook_deliveries (webhook_id, status, created_at) VALUES ($1, 'failed', now() - interval '10 days')`, w.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.PruneWebhookDeliveries(f.ctx, 7*24*time.Hour); err != nil || n < 1 {
		t.Fatalf("prune: %d %v", n, err)
	}
	dels, _ := f.s.ListWebhookDeliveries(f.ctx, w.ID, 10)
	if len(dels) != 1 || dels[0].EventType != "webhook.test" || dels[0].EventID != 0 || dels[0].Attempt != 1 {
		t.Fatalf("attempt log after prune: %+v", dels)
	}

	// A cursor from a transaction id the database has not reached is
	// moved to the present.
	if _, err := f.s.pool.Exec(f.ctx, `UPDATE webhooks SET cursor_txid = 9000000000000 WHERE id = $1`, w.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.s.RepairWebhookCursors(f.ctx); err != nil || n < 1 {
		t.Fatalf("repair: %d %v", n, err)
	}
	got, _ := f.s.GetWebhook(f.ctx, "", w.ID)
	if got.CursorTxid >= 9000000000000 || got.CursorTxid <= w.CursorTxid {
		t.Fatalf("cursor after repair: %d (created at %d)", got.CursorTxid, w.CursorTxid)
	}
	if n, err := f.s.RepairWebhookCursors(f.ctx); err != nil || n != 0 {
		// Another test's webhook cannot be in that state.
		t.Fatalf("second repair: %d %v", n, err)
	}
}
