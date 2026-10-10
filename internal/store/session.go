package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// This file holds what the API layer needs on top of the pool
// controller's view of a sandbox: session metadata, changing the lifetime
// of a live session, and filtered per-tenant listings.

// SetMetadata replaces the metadata of a tenant-owned sandbox, fenced by
// generation.
func (s *Store) SetMetadata(ctx context.Context, id string, generation int64, metadata map[string]string) error {
	var param any
	if len(metadata) > 0 {
		b, err := json.Marshal(metadata)
		if err != nil {
			return fmt.Errorf("store: encode metadata: %w", err)
		}
		param = string(b)
	}
	tag, err := s.pool.Exec(ctx, `UPDATE sandboxes SET metadata = $3::jsonb, updated_at = now()
		WHERE id = $1 AND generation = $2 AND state IN ('ASSIGNED', 'RUNNING', 'PAUSED')`, id, generation, param)
	if err != nil {
		return fmt.Errorf("store: set metadata %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return s.explainMiss(ctx, id, generation, "", "")
	}
	return nil
}

// SetLifetime moves the max-lifetime expiry of a tenant-owned sandbox to
// fromNow from the database's now(), fenced by generation. When maxTotal
// is positive the expiry is capped at assigned_at + maxTotal, so a
// session can be shortened or extended but never past the cap. It
// returns the updated row.
func (s *Store) SetLifetime(ctx context.Context, id string, generation int64, fromNow, maxTotal time.Duration) (core.Sandbox, error) {
	if fromNow <= 0 {
		return core.Sandbox{}, errors.New("store: SetLifetime needs a positive duration")
	}
	sb, err := scanSandbox(s.pool.QueryRow(ctx, `UPDATE sandboxes SET
			expires_at = CASE WHEN $4::bigint > 0
				THEN LEAST(now() + $3::bigint * interval '1 microsecond', assigned_at + $4::bigint * interval '1 microsecond')
				ELSE now() + $3::bigint * interval '1 microsecond' END,
			updated_at = now()
		WHERE id = $1 AND generation = $2 AND state IN ('ASSIGNED', 'RUNNING', 'PAUSED')
		RETURNING `+sandboxCols, id, generation, fromNow.Microseconds(), maxTotal.Microseconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Sandbox{}, s.explainMiss(ctx, id, generation, "", "")
	}
	if err != nil {
		return core.Sandbox{}, fmt.Errorf("store: set lifetime %s: %w", id, err)
	}
	return sb, nil
}

// SandboxFilter narrows ListSandboxAccess.
type SandboxFilter struct {
	// States restricts the listing; empty means every state.
	States []core.State
	// TemplateID restricts the listing to one template when non-empty.
	TemplateID string
	// Metadata keeps only sandboxes whose metadata contains every pair.
	Metadata map[string]string
	// Limit caps the result; zero or negative means 100.
	Limit int
}

// ListSandboxAccess lists a tenant's sandboxes with their network policy
// and metadata, newest assignment first.
func (s *Store) ListSandboxAccess(ctx context.Context, tenantID string, f SandboxFilter) ([]SandboxAccess, error) {
	if f.Limit <= 0 {
		f.Limit = 100
	}
	states := make([]string, len(f.States))
	for i, st := range f.States {
		states[i] = string(st)
	}
	var meta any
	if len(f.Metadata) > 0 {
		b, err := json.Marshal(f.Metadata)
		if err != nil {
			return nil, fmt.Errorf("store: encode metadata filter: %w", err)
		}
		meta = string(b)
	}
	rows, err := s.pool.Query(ctx, `SELECT `+sandboxCols+`, access_token_hash, network_policy, metadata FROM sandboxes
		WHERE tenant_id = $1
		  AND (cardinality($2::text[]) = 0 OR state = ANY($2::text[]))
		  AND ($3::text = '' OR template_id = $3::text)
		  AND ($4::jsonb IS NULL OR metadata @> $4::jsonb)
		ORDER BY assigned_at DESC NULLS LAST, id LIMIT $5`, tenantID, states, f.TemplateID, meta, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: list sandboxes: %w", err)
	}
	defer rows.Close()
	var out []SandboxAccess
	for rows.Next() {
		a, err := scanSandboxAccess(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// scanSandboxAccess scans sandboxCols followed by access_token_hash,
// network_policy and metadata.
func scanSandboxAccess(row pgx.Row) (SandboxAccess, error) {
	var (
		out          SandboxAccess
		policy, meta []byte
	)
	sb, err := scanSandbox(row, &out.AccessTokenHash, &policy, &meta)
	if err != nil {
		return SandboxAccess{}, err
	}
	out.Sandbox = sb
	if len(policy) > 0 {
		var np core.NetworkPolicy
		if err := json.Unmarshal(policy, &np); err != nil {
			return SandboxAccess{}, fmt.Errorf("store: decode network policy of %s: %w", sb.ID, err)
		}
		out.NetworkPolicy = &np
	}
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &out.Metadata); err != nil {
			return SandboxAccess{}, fmt.Errorf("store: decode metadata of %s: %w", sb.ID, err)
		}
	}
	return out, nil
}
