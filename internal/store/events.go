package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/himanshubaliyan7/warmbox/internal/core"
)

// Event is one row of the append-only events table.
type Event struct {
	ID        int64
	SandboxID string
	TenantID  string // empty when the sandbox had no tenant
	Type      string
	FromState core.State
	ToState   core.State
	Reason    core.TerminationReason
	At        time.Time
	Data      map[string]any
}

// AppendEvent appends an event that is not a state change (state changes
// write their own event inside Transition and Claim). ID and At are
// assigned by the database.
func (s *Store) AppendEvent(ctx context.Context, e Event) error {
	if e.SandboxID == "" || e.Type == "" {
		return errors.New("store: event needs a sandbox id and a type")
	}
	data, err := jsonParam(e.Data)
	if err != nil {
		return err
	}
	var tenant *string
	if e.TenantID != "" {
		tenant = &e.TenantID
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO events (sandbox_id, tenant_id, type, from_state, to_state, reason, data)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)`,
		e.SandboxID, tenant, e.Type, string(e.FromState), string(e.ToState), string(e.Reason), data)
	if err != nil {
		return fmt.Errorf("store: append event: %w", err)
	}
	return nil
}

// ListEvents returns a sandbox's events in the order they were written.
func (s *Store) ListEvents(ctx context.Context, sandboxID string) ([]Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, sandbox_id, tenant_id, type, from_state, to_state, reason, at, data
		FROM events WHERE sandbox_id = $1 ORDER BY id`, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("store: list events: %w", err)
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var (
			e             Event
			tenant        *string
			from, to, why string
			data          []byte
		)
		if err := rows.Scan(&e.ID, &e.SandboxID, &tenant, &e.Type, &from, &to, &why, &e.At, &data); err != nil {
			return nil, err
		}
		if tenant != nil {
			e.TenantID = *tenant
		}
		e.FromState, e.ToState, e.Reason = core.State(from), core.State(to), core.TerminationReason(why)
		if len(data) > 0 {
			if err := json.Unmarshal(data, &e.Data); err != nil {
				return nil, fmt.Errorf("store: decode event %d: %w", e.ID, err)
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
