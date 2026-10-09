package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/himanshubaliyan7/SandboxSchedulerAI/internal/core"
)

const templateCols = `id, version, image, runtime, cpus, memory_bytes, pids_limit, workdir_bytes, tmp_bytes,
	nofile_limit, network, idle_timeout_ms, max_lifetime_ms, warm_ttl_ms, warm_target, max_total, refill_rate`

func scanTemplate(row pgx.Row) (core.Template, error) {
	var (
		t                  core.Template
		network            string
		idle, life, warmMs int64
	)
	err := row.Scan(&t.ID, &t.Version, &t.Image, &t.Runtime, &t.Resources.CPUs, &t.Resources.MemoryBytes,
		&t.Resources.PidsLimit, &t.Resources.WorkdirBytes, &t.Resources.TmpBytes, &t.Resources.NofileLimit,
		&network, &idle, &life, &warmMs, &t.WarmTarget, &t.MaxTotal, &t.RefillRate)
	if err != nil {
		return core.Template{}, err
	}
	t.Network = core.NetworkMode(network)
	t.Timeouts = core.Timeouts{
		Idle:        time.Duration(idle) * time.Millisecond,
		MaxLifetime: time.Duration(life) * time.Millisecond,
		WarmTTL:     time.Duration(warmMs) * time.Millisecond,
	}
	return t, nil
}

func templateArgs(t core.Template) []any {
	rt := t.Runtime
	if rt == "" {
		rt = "crun"
	}
	network := t.Network
	if network == "" {
		network = core.NetworkNone
	}
	return []any{t.ID, t.Image, rt, t.Resources.CPUs, t.Resources.MemoryBytes, t.Resources.PidsLimit,
		t.Resources.WorkdirBytes, t.Resources.TmpBytes, t.Resources.NofileLimit, string(network),
		t.Timeouts.Idle.Milliseconds(), t.Timeouts.MaxLifetime.Milliseconds(), t.Timeouts.WarmTTL.Milliseconds(),
		t.WarmTarget, t.MaxTotal, t.RefillRate}
}

// CreateTemplate inserts a template. A zero Version becomes 1. Durations
// are stored with millisecond resolution. The store does not validate the
// resource shape; that is the API layer's job.
func (s *Store) CreateTemplate(ctx context.Context, t core.Template) (core.Template, error) {
	if t.ID == "" {
		return core.Template{}, errors.New("store: template needs an id")
	}
	version := t.Version
	if version <= 0 {
		version = 1
	}
	const q = `INSERT INTO templates (id, image, runtime, cpus, memory_bytes, pids_limit, workdir_bytes, tmp_bytes,
		nofile_limit, network, idle_timeout_ms, max_lifetime_ms, warm_ttl_ms, warm_target, max_total, refill_rate, version)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
	RETURNING ` + templateCols
	out, err := scanTemplate(s.pool.QueryRow(ctx, q, append(templateArgs(t), version)...))
	if err != nil {
		if isUniqueViolation(err) {
			return core.Template{}, fmt.Errorf("%w: template %s already exists", ErrConflict, t.ID)
		}
		return core.Template{}, fmt.Errorf("store: create template: %w", err)
	}
	return out, nil
}

// UpdateTemplate replaces a template's fields. The version is managed by
// the store and t.Version is ignored: it is incremented only when
// something that is baked into an instance changes (image, runtime,
// resources or network mode), which makes the sweeper recycle the WARM
// instances built from the old version. Changing only timeouts or pool
// sizing leaves the version, and the warm pool, alone.
func (s *Store) UpdateTemplate(ctx context.Context, t core.Template) (core.Template, error) {
	const q = `UPDATE templates SET
		version = version + CASE WHEN (image, runtime, cpus, memory_bytes, pids_limit, workdir_bytes, tmp_bytes, nofile_limit, network)
			IS DISTINCT FROM ($2::text, $3::text, $4::double precision, $5::bigint, $6::bigint, $7::bigint, $8::bigint, $9::bigint, $10::text)
			THEN 1 ELSE 0 END,
		image = $2, runtime = $3, cpus = $4, memory_bytes = $5, pids_limit = $6, workdir_bytes = $7, tmp_bytes = $8,
		nofile_limit = $9, network = $10, idle_timeout_ms = $11, max_lifetime_ms = $12, warm_ttl_ms = $13,
		warm_target = $14, max_total = $15, refill_rate = $16, updated_at = now()
	WHERE id = $1
	RETURNING ` + templateCols
	out, err := scanTemplate(s.pool.QueryRow(ctx, q, templateArgs(t)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Template{}, fmt.Errorf("%w: template %s", ErrNotFound, t.ID)
	}
	if err != nil {
		return core.Template{}, fmt.Errorf("store: update template: %w", err)
	}
	return out, nil
}

// GetTemplate returns one template.
func (s *Store) GetTemplate(ctx context.Context, id string) (core.Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx, `SELECT `+templateCols+` FROM templates WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return core.Template{}, fmt.Errorf("%w: template %s", ErrNotFound, id)
	}
	if err != nil {
		return core.Template{}, fmt.Errorf("store: get template %s: %w", id, err)
	}
	return t, nil
}

// ListTemplates returns every template ordered by id.
func (s *Store) ListTemplates(ctx context.Context) ([]core.Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+templateCols+` FROM templates ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("store: list templates: %w", err)
	}
	defer rows.Close()
	var out []core.Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DeleteTemplate removes a template. WARM sandboxes built from it are
// then reported by ListSweepable as template_changed and recycled;
// sandboxes already owned by a tenant keep running until their max
// lifetime or release.
func (s *Store) DeleteTemplate(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM templates WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("store: delete template %s: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: template %s", ErrNotFound, id)
	}
	return nil
}
