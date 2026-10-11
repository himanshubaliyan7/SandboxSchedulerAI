// Package store is the Postgres state store: the single source of truth
// for tenants, API keys, templates, sandboxes and the event log.
//
// Postgres owns the clock. Every timestamp the store writes or compares
// (assigned_at, deadline_at, expires_at, the sweep predicates) uses the
// database's now(), so that several processes agree on what "past the
// deadline" means. Callers pass durations, never absolute times.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	neturl "net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Typed errors. Use errors.Is; they are often wrapped with detail.
var (
	// ErrPoolEmpty: the template has no WARM sandbox to claim right now.
	ErrPoolEmpty = errors.New("store: warm pool is empty")
	// ErrQuotaExceeded: the tenant is at its max_concurrent limit.
	ErrQuotaExceeded = errors.New("store: tenant concurrent sandbox quota exceeded")
	// ErrStaleGeneration: the caller's generation is not the row's; a stale actor lost.
	ErrStaleGeneration = errors.New("store: stale generation")
	// ErrNotFound: no such row.
	ErrNotFound = errors.New("store: not found")
	// ErrInvalidTransition: the edge is not in core's state machine, or the
	// row is not in the expected source state.
	ErrInvalidTransition = errors.New("store: invalid state transition")
	// ErrConflict: a unique constraint was violated (duplicate id or key).
	ErrConflict = errors.New("store: conflict")
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is the advisory lock that serialises concurrent migrators.
const migrationLockKey int64 = 0x5342585f4d494752 // "SBX_MIGR"

// Store is a handle on the database. It is safe for concurrent use.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres, verifies the connection and applies any
// pending migrations.
func Open(ctx context.Context, url string) (*Store, error) {
	st, err := open(ctx, url)
	if err != nil {
		return nil, redactError(err, url)
	}
	return st, nil
}

func open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: parse database url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	s := &Store{pool: pool}
	if err := s.Migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

// redactedError is an error whose text has had a secret removed. The
// original stays reachable for errors.Is and errors.As, which look at
// types and values, not at text.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

var dsnPasswordRE = regexp.MustCompile(`(?i)\bpassword\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// redactError makes sure an error from opening the database cannot carry
// the database URL's password, whatever the driver put in its text: the
// error ends up on a terminal, in a log and in bug reports. The URL
// itself, its password (as written and percent-decoded) and the value of
// a keyword/value "password=" are each replaced.
func redactError(err error, dsn string) error {
	secrets := []string{}
	if dsn != "" {
		secrets = append(secrets, dsn)
	}
	if u, perr := neturl.Parse(dsn); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			secrets = append(secrets, pw, neturl.QueryEscape(pw), neturl.PathEscape(pw))
		}
		if pw := u.Query().Get("password"); pw != "" {
			secrets = append(secrets, pw, neturl.QueryEscape(pw))
		}
	}
	// A URL that does not parse still has its password between the first
	// ':' after the scheme and the last '@'.
	if i := strings.Index(dsn, "://"); i >= 0 {
		rest := dsn[i+3:]
		if at := strings.LastIndexByte(rest, '@'); at >= 0 {
			if colon := strings.IndexByte(rest[:at], ':'); colon >= 0 && colon+1 < at {
				secrets = append(secrets, rest[colon+1:at])
			}
		}
	}
	for _, m := range dsnPasswordRE.FindAllStringSubmatch(dsn, -1) {
		secrets = append(secrets, m[1], strings.Trim(m[1], "'"))
	}
	msg := err.Error()
	// Longest first, so that a secret containing another is replaced whole.
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	for _, sec := range secrets {
		if sec != "" {
			msg = strings.ReplaceAll(msg, sec, "REDACTED")
		}
	}
	if msg == err.Error() {
		return err
	}
	return &redactedError{msg: msg, err: err}
}

// New wraps an existing pool without running migrations.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Close releases every connection.
func (s *Store) Close() { s.pool.Close() }

// Pool exposes the underlying pool (health checks, ad-hoc queries in tests).
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Migrate applies every embedded migration that is not yet recorded in
// schema_migrations, in file-name order, each in its own transaction. It
// is idempotent and safe to run from several processes at once: an advisory
// transaction lock serialises them.
func (s *Store) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrationFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: list migrations: %w", err)
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		body, err := migrationFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", version, err)
		}
		if err := s.applyMigration(ctx, version, string(body)); err != nil {
			return fmt.Errorf("store: migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version, body string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied); err != nil {
		return err
	}
	if applied {
		return nil
	}
	// The simple protocol lets one Exec carry a multi-statement file.
	if _, err := tx.Exec(ctx, body, pgx.QueryExecModeSimpleProtocol); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// AppliedMigrations lists the recorded migration versions in order.
func (s *Store) AppliedMigrations(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// isUniqueViolation reports whether err is Postgres error 23505.
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
