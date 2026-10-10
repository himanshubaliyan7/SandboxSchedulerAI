package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// testStore opens the database named by SBX_TEST_DATABASE_URL, or skips.
func testStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("SBX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SBX_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(s.Close)
	return s
}

func uniq(prefix string) string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func testTemplate(id string) core.Template {
	return core.Template{
		ID: id, Image: "docker.io/library/python:3.12-slim", Runtime: "crun",
		Resources: core.Resources{CPUs: 1.5, MemoryBytes: 512 << 20, PidsLimit: 256,
			WorkdirBytes: 128 << 20, TmpBytes: 64 << 20, NofileLimit: 1024},
		Network:    core.NetworkAllowlist,
		Timeouts:   core.Timeouts{Idle: 10 * time.Minute, MaxLifetime: time.Hour, WarmTTL: 30 * time.Minute},
		WarmTarget: 3, MaxTotal: 10, RefillRate: 2,
	}
}

type fixture struct {
	s      *Store
	tmpl   core.Template
	tenant Tenant
	host   string
}

// newFixture creates a private template, tenant and host id so tests do
// not see each other's rows.
func newFixture(t *testing.T, maxConcurrent int) fixture {
	t.Helper()
	s := testStore(t)
	ctx := context.Background()
	tmpl, err := s.CreateTemplate(ctx, testTemplate(uniq("tmpl")))
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	tenant, err := s.CreateTenant(ctx, Tenant{ID: uniq("tenant"), Name: "test", MaxConcurrent: maxConcurrent, RatePerSec: 10, Burst: 20})
	if err != nil {
		t.Fatalf("CreateTenant: %v", err)
	}
	t.Cleanup(func() {
		// Leave no templates behind: a pool controller pointed at this
		// database would start filling pools for them.
		_ = s.DeleteTemplate(context.Background(), tmpl.ID)
		_ = s.DeleteTenant(context.Background(), tenant.ID)
	})
	return fixture{s: s, tmpl: tmpl, tenant: tenant, host: uniq("host")}
}

// warm inserts n sandboxes and marks them WARM.
func (f fixture) warm(t *testing.T, n int) []core.Sandbox {
	t.Helper()
	ctx := context.Background()
	out := make([]core.Sandbox, 0, n)
	for i := 0; i < n; i++ {
		sb, err := f.s.InsertCreating(ctx, NewSandbox{ID: uniq("sbx"), TemplateID: f.tmpl.ID, TemplateVersion: f.tmpl.Version, HostID: f.host})
		if err != nil {
			t.Fatalf("InsertCreating: %v", err)
		}
		sb, err = f.s.MarkWarm(ctx, sb.ID, sb.Generation, "inst-"+sb.ID)
		if err != nil {
			t.Fatalf("MarkWarm: %v", err)
		}
		out = append(out, sb)
	}
	return out
}

func (f fixture) claim(ctx context.Context) (core.Sandbox, error) {
	return f.s.Claim(ctx, ClaimParams{TenantID: f.tenant.ID, TemplateID: f.tmpl.ID, TemplateVersion: f.tmpl.Version,
		HostID: f.host, MaxLifetime: f.tmpl.Timeouts.MaxLifetime})
}

func TestMigrationsApplyTwice(t *testing.T) {
	s := testStore(t) // Open already migrated once
	ctx := context.Background()
	before, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("no migrations recorded after Open")
	}
	for i := 0; i < 2; i++ {
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate run %d: %v", i+2, err)
		}
	}
	// Concurrent migrators must not collide either.
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Migrate(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Migrate: %v", err)
		}
	}
	after, err := s.AppliedMigrations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("migrations recorded: before %v, after %v", before, after)
	}
	for _, table := range []string{"tenants", "api_keys", "templates", "sandboxes", "events"} {
		var ok bool
		if err := s.Pool().QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&ok); err != nil || !ok {
			t.Fatalf("table %s missing (err=%v)", table, err)
		}
	}
}

func TestConcurrentClaimNoDoubleAssignment(t *testing.T) {
	f := newFixture(t, 1000)
	f.warm(t, 20)
	ctx := context.Background()

	const claimers = 50
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		got     = map[string]int{}
		empty   int
		others  []error
		start   = make(chan struct{})
		claimed []core.Sandbox
	)
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sb, err := f.claim(ctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				got[sb.ID]++
				claimed = append(claimed, sb)
			case errors.Is(err, ErrPoolEmpty):
				empty++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if len(got) != 20 || len(claimed) != 20 {
		t.Fatalf("claimed %d sandboxes (%d distinct), want exactly 20 distinct", len(claimed), len(got))
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("sandbox %s was assigned %d times", id, n)
		}
	}
	if empty != 30 {
		t.Fatalf("ErrPoolEmpty count = %d, want 30", empty)
	}
	for _, sb := range claimed {
		if sb.State != core.StateAssigned || sb.TenantID != f.tenant.ID || sb.Generation != 1 {
			t.Fatalf("claimed row is wrong: %+v", sb)
		}
		if sb.AssignedAt == nil || sb.DeadlineAt == nil || sb.ExpiresAt == nil || sb.LastActivityAt == nil {
			t.Fatalf("claimed row is missing timestamps: %+v", sb)
		}
	}
	counts, err := f.s.CountByTemplateState(ctx, f.host)
	if err != nil {
		t.Fatal(err)
	}
	if c := counts[f.tmpl.ID]; c[core.StateAssigned] != 20 || c[core.StateWarm] != 0 {
		t.Fatalf("counts = %v, want 20 ASSIGNED and 0 WARM", c)
	}
}

func TestClaimOrderIsOldestFirst(t *testing.T) {
	f := newFixture(t, 10)
	warm := f.warm(t, 3)
	for i := range warm {
		sb, err := f.claim(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if sb.ID != warm[i].ID {
			t.Fatalf("claim %d returned %s, want oldest %s", i, sb.ID, warm[i].ID)
		}
	}
}

func TestClaimQuotaUnderConcurrency(t *testing.T) {
	const quota = 5
	f := newFixture(t, quota)
	f.warm(t, 20)
	ctx := context.Background()

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ok       []core.Sandbox
		exceeded int
		others   []error
		start    = make(chan struct{})
	)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			sb, err := f.claim(ctx)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok = append(ok, sb)
			case errors.Is(err, ErrQuotaExceeded):
				exceeded++
			default:
				others = append(others, err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if len(others) > 0 {
		t.Fatalf("unexpected errors: %v", others)
	}
	if len(ok) != quota || exceeded != 30-quota {
		t.Fatalf("got %d claims and %d quota errors, want %d and %d", len(ok), exceeded, quota, 30-quota)
	}

	// Releasing one frees exactly one slot.
	sb := ok[0]
	if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation,
		From: core.StateAssigned, To: core.StateRecycling, Reason: core.ReasonReleased}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.claim(ctx); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if _, err := f.claim(ctx); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("claim at quota: err = %v, want ErrQuotaExceeded", err)
	}

	// Unknown tenant and empty pool are distinct errors.
	if _, err := f.s.Claim(ctx, ClaimParams{TenantID: "no-such-tenant", TemplateID: f.tmpl.ID}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown tenant: err = %v, want ErrNotFound", err)
	}
	if _, err := f.s.Claim(ctx, ClaimParams{TenantID: f.tenant.ID, TemplateID: "no-such-template"}); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("at quota the quota error comes first: err = %v", err)
	}
}

func TestClaimRespectsTemplateVersionAndHost(t *testing.T) {
	f := newFixture(t, 10)
	f.warm(t, 1)
	ctx := context.Background()
	p := ClaimParams{TenantID: f.tenant.ID, TemplateID: f.tmpl.ID, HostID: f.host, TemplateVersion: f.tmpl.Version + 1}
	if _, err := f.s.Claim(ctx, p); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("wrong version: err = %v, want ErrPoolEmpty", err)
	}
	p.TemplateVersion, p.HostID = f.tmpl.Version, "another-host"
	if _, err := f.s.Claim(ctx, p); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("wrong host: err = %v, want ErrPoolEmpty", err)
	}
	p.HostID = ""
	policy := core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: []string{"*.example.com"}}
	p.NetworkPolicy = &policy
	p.AccessTokenHash = []byte{1, 2, 3}
	sb, err := f.s.Claim(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if sb.ExpiresAt != nil {
		t.Fatalf("no MaxLifetime was given but expires_at = %v", sb.ExpiresAt)
	}
	acc, err := f.s.GetSandboxAccess(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(acc.AccessTokenHash) != "\x01\x02\x03" || acc.NetworkPolicy == nil ||
		acc.NetworkPolicy.Mode != core.NetworkAllowlist || len(acc.NetworkPolicy.AllowDomains) != 1 {
		t.Fatalf("access record not stored: %+v", acc)
	}
	newPolicy := core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: []string{"pypi.org"}}
	if err := f.s.SetNetworkPolicy(ctx, sb.ID, sb.Generation, newPolicy); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetNetworkPolicy(ctx, sb.ID, sb.Generation+1, newPolicy); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("SetNetworkPolicy with wrong generation: %v", err)
	}
	acc, _ = f.s.GetSandboxAccess(ctx, sb.ID)
	if acc.NetworkPolicy == nil || acc.NetworkPolicy.AllowDomains[0] != "pypi.org" {
		t.Fatalf("policy not replaced: %+v", acc.NetworkPolicy)
	}
}

func TestStaleGenerationRejected(t *testing.T) {
	f := newFixture(t, 10)
	f.warm(t, 1)
	ctx := context.Background()
	sb, err := f.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stale := sb.Generation - 1 // what an actor that saw the WARM row holds

	_, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: stale, From: core.StateAssigned, To: core.StateRunning})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale transition: err = %v, want ErrStaleGeneration", err)
	}
	_, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: stale, From: core.StateAssigned,
		To: core.StateRecycling, Reason: core.ReasonReleased})
	if !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale release: err = %v, want ErrStaleGeneration", err)
	}
	if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateAssigned, To: core.StateRunning}); err != nil {
		t.Fatalf("current generation: %v", err)
	}
	if err := f.s.Touch(ctx, sb.ID, stale); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("stale touch: err = %v, want ErrStaleGeneration", err)
	}
	if err := f.s.Touch(ctx, sb.ID, sb.Generation); err != nil {
		t.Fatalf("touch: %v", err)
	}
	cur, err := f.s.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.State != core.StateRunning || cur.Generation != sb.Generation {
		t.Fatalf("row changed by a stale actor: %+v", cur)
	}
	if cur.DeadlineAt != nil {
		t.Fatalf("RUNNING is not transient but deadline_at = %v", cur.DeadlineAt)
	}
	_, err = f.s.Transition(ctx, TransitionParams{ID: "sbx-does-not-exist", Generation: 0, From: core.StateWarm, To: core.StateRecycling})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: err = %v, want ErrNotFound", err)
	}
}

func TestIllegalTransitionsRejected(t *testing.T) {
	f := newFixture(t, 10)
	f.warm(t, 1)
	ctx := context.Background()
	sb, err := f.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sb, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateAssigned, To: core.StateRunning})
	if err != nil {
		t.Fatal(err)
	}

	// No edge back to WARM from any state, whatever the caller claims.
	for _, from := range []core.State{core.StateRunning, core.StateAssigned, core.StatePaused, core.StateRecycling, core.StateFailed, core.StateDestroyed} {
		_, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: from, To: core.StateWarm})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("%s -> WARM: err = %v, want ErrInvalidTransition", from, err)
		}
	}
	// Not an edge.
	if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateRunning, To: core.StateDestroyed}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("RUNNING -> DESTROYED: err = %v", err)
	}
	// A legal edge, but the row is not in the expected source state.
	if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StatePaused, To: core.StateRunning}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("PAUSED -> RUNNING on a RUNNING row: err = %v", err)
	}
	cur, _ := f.s.GetSandbox(ctx, sb.ID)
	if cur.State != core.StateRunning {
		t.Fatalf("state = %s after rejected transitions", cur.State)
	}

	// Walk to the terminal state; nothing leaves DESTROYED.
	sb, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateRunning, To: core.StateRecycling, Reason: core.ReasonReleased})
	if err != nil {
		t.Fatal(err)
	}
	if sb.DeadlineAt == nil || sb.Reason != core.ReasonReleased {
		t.Fatalf("RECYCLING row needs a deadline and a reason: %+v", sb)
	}
	sb, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateRecycling, To: core.StateDestroyed})
	if err != nil {
		t.Fatal(err)
	}
	if sb.DeadlineAt != nil || sb.Reason != core.ReasonReleased {
		t.Fatalf("DESTROYED row: %+v", sb)
	}
	for _, to := range []core.State{core.StateWarm, core.StateRunning, core.StateRecycling} {
		if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateDestroyed, To: to}); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("DESTROYED -> %s: err = %v", to, err)
		}
	}
	// A destroyed sandbox cannot be claimed.
	if _, err := f.claim(ctx); !errors.Is(err, ErrPoolEmpty) {
		t.Fatalf("claim on drained pool: %v", err)
	}
}

func TestEventPerTransition(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	sb := f.warm(t, 1)[0]
	sb, err := f.claim(ctx)
	if err != nil {
		t.Fatal(err)
	}
	steps := []TransitionParams{
		{From: core.StateAssigned, To: core.StateRunning},
		{From: core.StateRunning, To: core.StatePaused},
		{From: core.StatePaused, To: core.StateRunning},
		{From: core.StateRunning, To: core.StateRecycling, Reason: core.ReasonIdleTimeout, Data: map[string]any{"idle_ms": 600000}},
		{From: core.StateRecycling, To: core.StateDestroyed},
	}
	for _, p := range steps {
		p.ID, p.Generation = sb.ID, sb.Generation
		if _, err := f.s.Transition(ctx, p); err != nil {
			t.Fatalf("%s -> %s: %v", p.From, p.To, err)
		}
	}
	// A rejected transition must not leave an event behind.
	if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateRunning, To: core.StateRecycling}); err == nil {
		t.Fatal("transition from the wrong state succeeded")
	}

	events, err := f.s.ListEvents(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	type step struct {
		typ      string
		from, to core.State
		tenant   bool
	}
	want := []step{
		{EventCreated, "", core.StateCreating, false},
		{EventTransition, core.StateCreating, core.StateWarm, false},
		{EventClaimed, core.StateWarm, core.StateAssigned, true},
		{EventTransition, core.StateAssigned, core.StateRunning, true},
		{EventTransition, core.StateRunning, core.StatePaused, true},
		{EventTransition, core.StatePaused, core.StateRunning, true},
		{EventTransition, core.StateRunning, core.StateRecycling, true},
		{EventTransition, core.StateRecycling, core.StateDestroyed, true},
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		if e.Type != w.typ || e.FromState != w.from || e.ToState != w.to || (e.TenantID == f.tenant.ID) != w.tenant {
			t.Fatalf("event %d = %+v, want %+v", i, e, w)
		}
		if e.At.IsZero() {
			t.Fatalf("event %d has no timestamp", i)
		}
	}
	if e := events[6]; e.Reason != core.ReasonIdleTimeout || e.Data["idle_ms"] != float64(600000) {
		t.Fatalf("recycling event lost its reason or data: %+v", e)
	}
	if e := events[7]; e.Reason != core.ReasonIdleTimeout {
		t.Fatalf("destroyed event should carry the row's reason: %+v", e)
	}

	if err := f.s.AppendEvent(ctx, Event{SandboxID: sb.ID, Type: "orphan_destroyed", Reason: core.ReasonOrphan}); err != nil {
		t.Fatal(err)
	}
	events, _ = f.s.ListEvents(ctx, sb.ID)
	if len(events) != len(want)+1 {
		t.Fatalf("AppendEvent did not append")
	}
}

// backdate shifts a row's timestamps into the past so timer predicates fire.
func (f fixture) backdate(t *testing.T, id, column string, by time.Duration) {
	t.Helper()
	_, err := f.s.Pool().Exec(context.Background(),
		`UPDATE sandboxes SET `+column+` = now() - $2::bigint * interval '1 microsecond' WHERE id = $1`, id, by.Microseconds())
	if err != nil {
		t.Fatal(err)
	}
}

func TestListSweepable(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	warm := f.warm(t, 7)

	running := func() core.Sandbox {
		t.Helper()
		sb, err := f.claim(ctx)
		if err != nil {
			t.Fatal(err)
		}
		sb, err = f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateAssigned, To: core.StateRunning})
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	healthy := running() // must not be listed
	idle := running()
	f.backdate(t, idle.ID, "last_activity_at", 11*time.Minute)
	expired := running()
	f.backdate(t, expired.ID, "expires_at", time.Second)
	f.backdate(t, expired.ID, "last_activity_at", time.Hour) // max lifetime wins over idle
	stuck, err := f.claim(ctx)                               // stays ASSIGNED
	if err != nil {
		t.Fatal(err)
	}
	f.backdate(t, stuck.ID, "deadline_at", time.Second)
	hung, err := f.s.InsertCreating(ctx, NewSandbox{ID: uniq("sbx"), TemplateID: f.tmpl.ID, TemplateVersion: f.tmpl.Version, HostID: f.host})
	if err != nil {
		t.Fatal(err)
	}
	f.backdate(t, hung.ID, "deadline_at", time.Second)

	// warm[0..3] were claimed above (oldest first); warm[4..6] are still WARM.
	old := warm[4]
	f.backdate(t, old.ID, "created_at", 31*time.Minute)
	stale := warm[5]
	if _, err := f.s.Pool().Exec(ctx, `UPDATE sandboxes SET template_version = template_version - 1 WHERE id = $1`, stale.ID); err != nil {
		t.Fatal(err)
	}
	fresh := warm[6] // must not be listed

	items, err := f.s.ListSweepable(ctx, f.host)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]SweepCause{}
	for _, it := range items {
		if _, dup := got[it.Sandbox.ID]; dup {
			t.Fatalf("sandbox %s listed twice", it.Sandbox.ID)
		}
		got[it.Sandbox.ID] = it.Cause
	}
	want := map[string]SweepCause{
		idle.ID: SweepIdle, expired.ID: SweepMaxLifetime, stuck.ID: SweepDeadline,
		hung.ID: SweepDeadline, old.ID: SweepWarmTTL, stale.ID: SweepTemplateChanged,
	}
	for id, cause := range want {
		if got[id] != cause {
			t.Errorf("sandbox %s: cause %q, want %q", id, got[id], cause)
		}
	}
	if len(got) != len(want) {
		t.Errorf("listed %d rows, want %d: %v (healthy=%s fresh=%s)", len(got), len(want), got, healthy.ID, fresh.ID)
	}

	// A template version bump (image change) makes every WARM row stale;
	// a sizing-only change does not.
	tmpl := f.tmpl
	tmpl.WarmTarget = 9
	updated, err := f.s.UpdateTemplate(ctx, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != f.tmpl.Version || updated.WarmTarget != 9 {
		t.Fatalf("sizing change: %+v", updated)
	}
	tmpl.Image = "docker.io/library/python:3.13-slim"
	updated, err = f.s.UpdateTemplate(ctx, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Version != f.tmpl.Version+1 {
		t.Fatalf("image change did not bump the version: %+v", updated)
	}
	items, _ = f.s.ListSweepable(ctx, f.host)
	changed := 0
	for _, it := range items {
		if it.Cause == SweepTemplateChanged {
			changed++
		}
	}
	if changed != 3 {
		t.Fatalf("after the version bump %d WARM rows are template_changed, want 3", changed)
	}

	active, err := f.s.ListActiveByHost(ctx, f.host)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 8 {
		t.Fatalf("ListActiveByHost = %d rows, want 8", len(active))
	}
	// Deleting the template leaves WARM rows listed as template_changed.
	if err := f.s.DeleteTemplate(ctx, f.tmpl.ID); err != nil {
		t.Fatal(err)
	}
	items, _ = f.s.ListSweepable(ctx, f.host)
	changed = 0
	for _, it := range items {
		if it.Cause == SweepTemplateChanged {
			changed++
		}
	}
	if changed != 3 {
		t.Fatalf("after deleting the template %d WARM rows are template_changed, want 3", changed)
	}
}

func TestGCDestroyed(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	warm := f.warm(t, 3)
	destroy := func(sb core.Sandbox) {
		t.Helper()
		sb, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateWarm, To: core.StateRecycling, Reason: core.ReasonWarmTTL})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.Transition(ctx, TransitionParams{ID: sb.ID, Generation: sb.Generation, From: core.StateRecycling, To: core.StateDestroyed}); err != nil {
			t.Fatal(err)
		}
	}
	destroy(warm[0])
	destroy(warm[1])
	f.backdate(t, warm[0].ID, "updated_at", 400*24*time.Hour)

	// Other tests' rows are minutes old at most, so a 300-day horizon only
	// touches the row backdated above.
	n, err := f.s.GCDestroyed(ctx, 300*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("GCDestroyed removed %d rows, want 1", n)
	}
	if _, err := f.s.GetSandbox(ctx, warm[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old destroyed row still present: %v", err)
	}
	for _, id := range []string{warm[1].ID, warm[2].ID} {
		if _, err := f.s.GetSandbox(ctx, id); err != nil {
			t.Fatalf("row %s should have survived: %v", id, err)
		}
	}
	if ev, _ := f.s.ListEvents(ctx, warm[0].ID); len(ev) == 0 {
		t.Fatal("events of a collected sandbox must be kept")
	}
}

func TestTemplateCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	in := testTemplate(uniq("tmpl"))
	got, err := s.CreateTemplate(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	in.Version = 1
	if got != in {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, in)
	}
	if _, err := s.CreateTemplate(ctx, in); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate: err = %v, want ErrConflict", err)
	}
	fetched, err := s.GetTemplate(ctx, in.ID)
	if err != nil || fetched != in {
		t.Fatalf("GetTemplate = %+v, %v", fetched, err)
	}
	list, err := s.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range list {
		found = found || x.ID == in.ID
	}
	if !found {
		t.Fatal("ListTemplates does not include the new template")
	}
	in.Resources.MemoryBytes = 1 << 30
	in.Timeouts.Idle = 90 * time.Second
	up, err := s.UpdateTemplate(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if up.Version != 2 || up.Resources.MemoryBytes != 1<<30 || up.Timeouts.Idle != 90*time.Second {
		t.Fatalf("update: %+v", up)
	}
	if err := s.DeleteTemplate(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTemplate(ctx, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.DeleteTemplate(ctx, in.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if _, err := s.UpdateTemplate(ctx, in); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a deleted template: %v", err)
	}
}

func TestTenantAndAPIKeyCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tn, err := s.CreateTenant(ctx, Tenant{ID: uniq("tenant"), Name: "acme", MaxConcurrent: 3, RatePerSec: 2.5, Burst: 7})
	if err != nil {
		t.Fatal(err)
	}
	if tn.Weight != 1 || tn.CreatedAt.IsZero() {
		t.Fatalf("defaults: %+v", tn)
	}
	if _, err := s.CreateTenant(ctx, tn); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate tenant: %v", err)
	}
	tn.MaxConcurrent, tn.Weight = 9, 4
	up, err := s.UpdateTenant(ctx, tn)
	if err != nil || up.MaxConcurrent != 9 || up.Weight != 4 || up.RatePerSec != 2.5 {
		t.Fatalf("UpdateTenant = %+v, %v", up, err)
	}
	if list, err := s.ListTenants(ctx); err != nil || len(list) == 0 {
		t.Fatalf("ListTenants = %d, %v", len(list), err)
	}

	plain, key, err := s.CreateAPIKey(ctx, tn.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prefix, ok := APIKeyPrefix(plain); !ok || prefix != key.Prefix {
		t.Fatalf("prefix of %q = %q, row has %q", plain, prefix, key.Prefix)
	}
	// Only the hash is stored.
	var stored []byte
	if err := s.Pool().QueryRow(ctx, `SELECT key_hash FROM api_keys WHERE id = $1`, key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) != 32 || string(stored) != string(HashAPIKey(plain)) {
		t.Fatal("stored value is not the SHA-256 of the key")
	}
	gotKey, gotTenant, err := s.AuthenticateAPIKey(ctx, plain)
	if err != nil || gotKey.ID != key.ID || gotTenant.ID != tn.ID {
		t.Fatalf("AuthenticateAPIKey = %+v, %+v, %v", gotKey, gotTenant, err)
	}
	for _, bad := range []string{"", "nonsense", plain + "x", "sbx_" + key.Prefix + "_" + "00"} {
		if _, _, err := s.AuthenticateAPIKey(ctx, bad); !errors.Is(err, ErrNotFound) {
			t.Fatalf("AuthenticateAPIKey(%q): err = %v, want ErrNotFound", bad, err)
		}
	}
	if _, _, err := s.CreateAPIKey(ctx, "no-such-tenant"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("key for a missing tenant: %v", err)
	}
	if err := s.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AuthenticateAPIKey(ctx, plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked key still authenticates: %v", err)
	}
	keys, err := s.ListAPIKeys(ctx, tn.ID)
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("ListAPIKeys = %+v, %v", keys, err)
	}
	if err := s.DeleteTenant(ctx, tn.ID); err != nil {
		t.Fatal(err)
	}
	if keys, _ := s.ListAPIKeys(ctx, tn.ID); len(keys) != 0 {
		t.Fatal("api keys must be deleted with their tenant")
	}
	if _, err := s.GetTenant(ctx, tn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}
