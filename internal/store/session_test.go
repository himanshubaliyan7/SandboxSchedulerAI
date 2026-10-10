package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

func TestSessionMetadataLifetimeAndListing(t *testing.T) {
	f := newFixture(t, 10)
	ctx := context.Background()
	f.warm(t, 3)
	policy := core.NetworkPolicy{Mode: core.NetworkAllowlist, AllowDomains: []string{"pypi.org"}}
	claim := func() core.Sandbox {
		t.Helper()
		sb, err := f.s.Claim(ctx, ClaimParams{TenantID: f.tenant.ID, TemplateID: f.tmpl.ID, HostID: f.host,
			MaxLifetime: 10 * time.Minute, AccessTokenHash: []byte("hash"), NetworkPolicy: &policy})
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		return sb
	}
	a, b := claim(), claim()

	// Metadata is fenced by generation and comes back with the row.
	if err := f.s.SetMetadata(ctx, a.ID, a.Generation, map[string]string{"job": "one", "team": "x"}); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := f.s.SetMetadata(ctx, b.ID, b.Generation, map[string]string{"job": "two", "team": "x"}); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := f.s.SetMetadata(ctx, a.ID, a.Generation+1, map[string]string{"job": "stale"}); !errors.Is(err, ErrStaleGeneration) {
		t.Errorf("stale SetMetadata: %v, want ErrStaleGeneration", err)
	}
	if err := f.s.SetMetadata(ctx, "sbx-nope", 1, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMetadata on a missing row: %v, want ErrNotFound", err)
	}
	acc, err := f.s.GetSandboxAccess(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetSandboxAccess: %v", err)
	}
	if acc.Metadata["job"] != "one" || len(acc.Metadata) != 2 || string(acc.AccessTokenHash) != "hash" ||
		acc.NetworkPolicy == nil || acc.NetworkPolicy.AllowDomains[0] != "pypi.org" {
		t.Errorf("access: %+v", acc)
	}

	// Listing: per tenant, filtered by state, template and metadata.
	ids := func(flt SandboxFilter) []string {
		t.Helper()
		rows, err := f.s.ListSandboxAccess(ctx, f.tenant.ID, flt)
		if err != nil {
			t.Fatalf("ListSandboxAccess(%+v): %v", flt, err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.Sandbox.ID)
		}
		return out
	}
	if got := ids(SandboxFilter{}); len(got) != 2 {
		t.Errorf("all: %v", got)
	}
	if got := ids(SandboxFilter{Metadata: map[string]string{"job": "two"}}); len(got) != 1 || got[0] != b.ID {
		t.Errorf("metadata filter: %v", got)
	}
	if got := ids(SandboxFilter{Metadata: map[string]string{"team": "x", "job": "one"}}); len(got) != 1 || got[0] != a.ID {
		t.Errorf("two-key metadata filter: %v", got)
	}
	if got := ids(SandboxFilter{Metadata: map[string]string{"job": "three"}}); len(got) != 0 {
		t.Errorf("metadata filter with no match: %v", got)
	}
	if got := ids(SandboxFilter{States: []core.State{core.StateRunning, core.StatePaused}}); len(got) != 0 {
		t.Errorf("state filter (both are ASSIGNED): %v", got)
	}
	if got := ids(SandboxFilter{States: []core.State{core.StateAssigned}, TemplateID: f.tmpl.ID, Limit: 1}); len(got) != 1 {
		t.Errorf("state+template+limit: %v", got)
	}
	if got := ids(SandboxFilter{TemplateID: "another-template"}); len(got) != 0 {
		t.Errorf("template filter: %v", got)
	}
	if rows, err := f.s.ListSandboxAccess(ctx, "another-tenant", SandboxFilter{}); err != nil || len(rows) != 0 {
		t.Errorf("another tenant sees %d rows (%v)", len(rows), err)
	}

	// Lifetime: counted from now, capped at assigned_at + maxTotal.
	sb, err := f.s.SetLifetime(ctx, a.ID, a.Generation, 30*time.Minute, time.Hour)
	if err != nil {
		t.Fatalf("SetLifetime: %v", err)
	}
	if d := sb.ExpiresAt.Sub(*sb.AssignedAt); d < 30*time.Minute || d > 31*time.Minute {
		t.Errorf("extended to %s after assignment, want about 30m", d)
	}
	sb, err = f.s.SetLifetime(ctx, a.ID, a.Generation, 5*time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("SetLifetime: %v", err)
	}
	if d := sb.ExpiresAt.Sub(*sb.AssignedAt); d != time.Hour {
		t.Errorf("capped at %s after assignment, want exactly 1h", d)
	}
	sb, err = f.s.SetLifetime(ctx, a.ID, a.Generation, 5*time.Hour, 0)
	if err != nil {
		t.Fatalf("SetLifetime: %v", err)
	}
	if d := sb.ExpiresAt.Sub(*sb.AssignedAt); d < 5*time.Hour {
		t.Errorf("uncapped: %s after assignment, want at least 5h", d)
	}
	if _, err := f.s.SetLifetime(ctx, a.ID, a.Generation+1, time.Minute, 0); !errors.Is(err, ErrStaleGeneration) {
		t.Errorf("stale SetLifetime: %v, want ErrStaleGeneration", err)
	}
	if _, err := f.s.SetLifetime(ctx, a.ID, a.Generation, 0, 0); err == nil {
		t.Error("SetLifetime accepted a zero duration")
	}
	// A sandbox no tenant holds has no lifetime to set.
	rec, err := f.s.Transition(ctx, TransitionParams{ID: b.ID, Generation: b.Generation, From: core.StateAssigned, To: core.StateRecycling, Reason: core.ReasonReleased})
	if err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if _, err := f.s.SetLifetime(ctx, rec.ID, rec.Generation, time.Minute, 0); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("SetLifetime on a recycling sandbox: %v, want ErrInvalidTransition", err)
	}
	// The shortened expiry is what the sweeper sees.
	if _, err := f.s.Transition(ctx, TransitionParams{ID: a.ID, Generation: a.Generation, From: core.StateAssigned, To: core.StateRunning}); err != nil {
		t.Fatalf("Transition: %v", err)
	}
	if _, err := f.s.SetLifetime(ctx, a.ID, a.Generation, time.Millisecond, time.Hour); err != nil {
		t.Fatalf("SetLifetime: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	items, err := f.s.ListSweepable(ctx, f.host)
	if err != nil {
		t.Fatalf("ListSweepable: %v", err)
	}
	found := false
	for _, it := range items {
		found = found || (it.Sandbox.ID == a.ID && it.Cause == SweepMaxLifetime)
	}
	if !found {
		t.Errorf("the shortened sandbox is not swept for max lifetime: %+v", items)
	}
}
