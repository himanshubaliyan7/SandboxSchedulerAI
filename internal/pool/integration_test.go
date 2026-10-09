package pool_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/pool/pooltest"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/runtime/fake"
	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/store"
)

func randID(prefix string) string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return prefix + "-" + hex.EncodeToString(b)
}

// TestControllerOnPostgres runs the controller against the real store
// (and the real clock) with the fake backend: fill, claim, release,
// lost-instance reconcile, restart.
func TestControllerOnPostgres(t *testing.T) {
	url := os.Getenv("SBX_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("SBX_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	st, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	tmpl := baseTemplate()
	tmpl.ID, tmpl.Version, tmpl.WarmTarget = randID("tmpl"), 0, 2
	tmpl, err = st.CreateTemplate(ctx, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	tn, err := st.CreateTenant(ctx, store.Tenant{ID: randID("tenant"), Name: "it", MaxConcurrent: 2, RatePerSec: 1, Burst: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = st.DeleteTemplate(context.Background(), tmpl.ID)
		_ = st.DeleteTenant(context.Background(), tn.ID)
	}()

	hostID := randID("host")
	be := fake.New()
	agent := &pooltest.FakeAgent{}
	dir := t.TempDir()
	newController := func() *pool.Controller {
		c, err := pool.New(pool.Config{Store: st, Backend: be, Agent: agent, Egress: &pooltest.FakeEgress{},
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), HostID: hostID, RunDirBase: dir,
			RefillInterval: 50 * time.Millisecond, SweepInterval: 50 * time.Millisecond, ReconcileInterval: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(c.Stop)
		return c
	}
	count := func(s core.State) int {
		counts, err := st.CountByTemplateState(ctx, hostID)
		if err != nil {
			t.Fatal(err)
		}
		return counts[tmpl.ID][s]
	}
	state := func(id string) core.Sandbox {
		sb, err := st.GetSandbox(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return sb
	}
	warmSettled := func(n int) {
		t.Helper()
		eventually(t, "warm pool on postgres", func() bool { return count(core.StateWarm) == n && count(core.StateCreating) == 0 })
	}

	c := newController()
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	warmSettled(2)

	a, err := c.Claim(ctx, tn.ID, tmpl.ID, pool.ClaimOptions{Env: map[string]string{"K": "V"}, AccessTokenHash: []byte{9}})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if a.State != core.StateRunning || a.Generation != 1 || a.TenantID != tn.ID || a.ExpiresAt == nil {
		t.Fatalf("claimed row: %+v", a)
	}
	acc, err := st.GetSandboxAccess(ctx, a.ID)
	if err != nil || len(acc.AccessTokenHash) != 1 || acc.NetworkPolicy == nil || acc.NetworkPolicy.Mode != core.NetworkAllowlist {
		t.Fatalf("access record: %+v, %v", acc, err)
	}
	b, err := c.Claim(ctx, tn.ID, tmpl.ID, pool.ClaimOptions{Wait: 10 * time.Second})
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if _, err := c.Claim(ctx, tn.ID, tmpl.ID, pool.ClaimOptions{Wait: 10 * time.Second}); !errors.Is(err, pool.ErrQuotaExceeded) {
		t.Fatalf("third claim with quota 2: %v", err)
	}
	warmSettled(2)

	if err := c.Release(ctx, a.ID, a.Generation-1, ""); !errors.Is(err, pool.ErrStaleGeneration) {
		t.Fatalf("stale release: %v", err)
	}
	if err := c.Release(ctx, a.ID, a.Generation, ""); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if got := state(a.ID); got.State != core.StateDestroyed || got.Reason != core.ReasonReleased {
		t.Fatalf("released row: %+v", got)
	}
	events, err := st.ListEvents(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	var path []core.State
	for _, e := range events {
		path = append(path, e.ToState)
	}
	want := []core.State{core.StateCreating, core.StateWarm, core.StateAssigned, core.StateRunning, core.StateRecycling, core.StateDestroyed}
	if len(path) != len(want) {
		t.Fatalf("history %v, want %v", path, want)
	}
	for i := range want {
		if path[i] != want[i] {
			t.Fatalf("history %v, want %v", path, want)
		}
	}

	// The reconcile loop notices a vanished instance on its own.
	be.Vanish(b.InstanceID)
	eventually(t, "lost sandbox to be destroyed", func() bool { return state(b.ID).State == core.StateDestroyed })
	if got := state(b.ID); got.Reason != core.ReasonInstanceLost {
		t.Fatalf("lost row: %+v", got)
	}
	warmSettled(2)

	// Restart: the warm rows in Postgres are re-adopted, nothing is
	// recreated. The controller serves every template in the database, and
	// other test packages may have templates there at the same time, so
	// everything below looks only at this test's template.
	warmOf := func() map[string]string {
		rows, err := st.ListActiveByHost(ctx, hostID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, r := range rows {
			if r.TemplateID == tmpl.ID && r.State == core.StateWarm {
				out[r.ID] = r.InstanceID
			}
		}
		return out
	}
	c.Stop()
	before := warmOf()
	if len(before) != 2 {
		t.Fatalf("%d warm rows before restart, want 2", len(before))
	}
	c2 := newController()
	if err := c2.Start(ctx); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if m := c2.Metrics(); m.Adopted < 2 {
		t.Fatalf("adopted %d warm sandboxes after restart, want at least this template's 2", m.Adopted)
	}
	warmSettled(2)
	after := warmOf()
	for id, inst := range before {
		if after[id] != inst {
			t.Fatalf("warm sandbox %s was not re-adopted: before %v, after %v", id, before, after)
		}
		if be.WasDestroyed(inst) {
			t.Fatalf("instance %s of an adoptable warm sandbox was destroyed", inst)
		}
	}
	d, err := c2.Claim(ctx, tn.ID, tmpl.ID, pool.ClaimOptions{})
	if err != nil {
		t.Fatalf("claim after restart: %v", err)
	}
	if err := c2.Release(ctx, d.ID, d.Generation, core.ReasonReleased); err != nil {
		t.Fatal(err)
	}

	// Deleting the template drains its warm pool (template_changed).
	if err := st.DeleteTemplate(ctx, tmpl.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, "pool of a deleted template to drain", func() bool {
		rows, err := st.ListActiveByHost(ctx, hostID)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			if r.TemplateID == tmpl.ID {
				n++
			}
		}
		return n == 0
	})
	c2.Stop()
	for _, inst := range mustList(t, be) {
		if inst.TemplateID == tmpl.ID {
			t.Fatalf("instance %s of the deleted template is still running", inst.ID)
		}
	}
}

func mustList(t *testing.T, be *fake.Backend) []fakeInstance {
	t.Helper()
	list, err := be.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := make([]fakeInstance, len(list))
	for i, x := range list {
		out[i] = fakeInstance{ID: x.ID, TemplateID: x.TemplateID}
	}
	return out
}

type fakeInstance struct{ ID, TemplateID string }
