// Package store provides typed access to the tmux-remux SQLite event store.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	// Register the modernc.org/sqlite driver under the name "sqlite".
	_ "modernc.org/sqlite"

	"github.com/noamsto/tmux-remux/internal/store/migrations"
)

// Store wraps a *sql.DB connection to the tmux-remux SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at path, runs any pending
// migrations, and returns a *Store. WAL is enabled, foreign keys are on.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB returns the underlying *sql.DB. Callers may use it for ad-hoc queries
// not yet wrapped by typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the underlying database connection.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	files, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var version int
		if _, err := fmt.Sscanf(name, "%04d_", &version); err != nil {
			return fmt.Errorf("parse migration name %q: %w", name, err)
		}

		var current int
		if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
			return fmt.Errorf("read user_version: %w", err)
		}
		if version <= current {
			continue
		}
		if version != current+1 {
			return fmt.Errorf("migration gap: have %d, next is %d", current, version)
		}

		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", name, err)
		}

		if err := s.applyMigration(ctx, version, string(body)); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("exec body: %w", err)
	}
	// PRAGMA does not accept bound parameters — use Sprintf.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// Event is a row in the events table.
type Event struct {
	ID            int64
	Ts            int64
	Kind          string
	Scope         string
	Reason        string
	Host          string
	ParentEventID *int64
	ManifestJSON  string
}

// InsertEvent inserts a new event row and returns its id.
func (s *Store) InsertEvent(ctx context.Context, ev Event) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO events (ts, kind, scope, reason, host, parent_event_id, manifest_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, ev.Ts, ev.Kind, ev.Scope, ev.Reason, ev.Host, ev.ParentEventID, ev.ManifestJSON)
	if err != nil {
		return 0, fmt.Errorf("insert event: %w", err)
	}
	return res.LastInsertId()
}

// LatestSnapshotBefore returns the newest snapshot event whose timestamp is
// strictly less than `ts` (id as tiebreak for same-millisecond saves), or
// (nil, nil) when none exists. Used by the close-event picker to recover the
// pre-close state for diffing + restoration, and by restore to anchor
// selection to before the current server started.
func (s *Store) LatestSnapshotBefore(ctx context.Context, ts int64) (*Event, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, ts, kind, scope, reason, host, parent_event_id, manifest_json
		FROM events
		WHERE kind = 'snapshot' AND ts < ?
		ORDER BY ts DESC, id DESC
		LIMIT 1
	`, ts)
	var ev Event
	err := row.Scan(&ev.ID, &ev.Ts, &ev.Kind, &ev.Scope, &ev.Reason, &ev.Host, &ev.ParentEventID, &ev.ManifestJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query latest snapshot before %d: %w", ts, err)
	}
	return &ev, nil
}

// LatestSnapshot returns the newest snapshot event by timestamp (id as
// tiebreak for same-millisecond saves), or (nil, nil) when no snapshot
// exists.
func (s *Store) LatestSnapshot(ctx context.Context) (*Event, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, ts, kind, scope, reason, host, parent_event_id, manifest_json
		FROM events
		WHERE kind = 'snapshot'
		ORDER BY ts DESC, id DESC
		LIMIT 1
	`)
	var ev Event
	err := row.Scan(&ev.ID, &ev.Ts, &ev.Kind, &ev.Scope, &ev.Reason, &ev.Host, &ev.ParentEventID, &ev.ManifestJSON)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("query latest snapshot: %w", err)
	}
	return &ev, nil
}

// ListOpts filters and limits ListEvents queries.
type ListOpts struct {
	Kinds        []string // include only these kinds (OR semantics); empty = no filter
	ExcludeKinds []string // exclude these kinds (AND semantics)
	Limit        int      // 0 = no limit
}

// ListEvents returns events matching opts, ordered by ts DESC, id DESC.
func (s *Store) ListEvents(ctx context.Context, opts ListOpts) ([]Event, error) {
	var b strings.Builder
	b.WriteString(`SELECT id, ts, kind, scope, reason, host, parent_event_id, manifest_json FROM events`)
	var clauses []string
	var args []any
	if len(opts.Kinds) > 0 {
		placeholders := make([]string, len(opts.Kinds))
		for i, k := range opts.Kinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		clauses = append(clauses, "kind IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(opts.ExcludeKinds) > 0 {
		placeholders := make([]string, len(opts.ExcludeKinds))
		for i, k := range opts.ExcludeKinds {
			placeholders[i] = "?"
			args = append(args, k)
		}
		clauses = append(clauses, "kind NOT IN ("+strings.Join(placeholders, ",")+")")
	}
	if len(clauses) > 0 {
		b.WriteString(" WHERE ")
		b.WriteString(strings.Join(clauses, " AND "))
	}
	b.WriteString(" ORDER BY ts DESC, id DESC")
	if opts.Limit > 0 {
		b.WriteString(" LIMIT ?")
		args = append(args, opts.Limit)
	}

	rows, err := s.db.QueryContext(ctx, b.String(), args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Event
	for rows.Next() {
		var ev Event
		if err := rows.Scan(&ev.ID, &ev.Ts, &ev.Kind, &ev.Scope, &ev.Reason, &ev.Host, &ev.ParentEventID, &ev.ManifestJSON); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// PruneSnapshots deletes snapshot events beyond the keep newest, except that
// the newest snapshot of each UTC calendar day in the 7 days before nowMs
// also survives. The per-day floor is the retention safety net: with 60s
// timer saves, keep-N alone evicts the pre-shutdown snapshot ~N minutes into
// a new server's life — exactly when a clobbered auto-restore most needs it.
func (s *Store) PruneSnapshots(ctx context.Context, keep int, nowMs int64) error {
	weekAgo := nowMs - 7*24*int64(time.Hour/time.Millisecond)
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE kind = 'snapshot'
		  AND id NOT IN (
		      SELECT id FROM events
		      WHERE kind = 'snapshot'
		      ORDER BY ts DESC
		      LIMIT ?
		  )
		  AND id NOT IN (
		      SELECT id FROM events
		      WHERE kind = 'snapshot' AND ts >= ?
		        AND ts IN (
		            SELECT max(ts)
		            FROM events
		            WHERE kind = 'snapshot' AND ts >= ?
		            GROUP BY date(ts/1000, 'unixepoch')
		        )
		  )
	`, keep, weekAgo, weekAgo)
	if err != nil {
		return fmt.Errorf("prune snapshots: %w", err)
	}
	return nil
}

// PruneCloseEvents deletes non-snapshot events beyond the keep newest.
func (s *Store) PruneCloseEvents(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM events
		WHERE kind != 'snapshot'
		  AND id NOT IN (
		      SELECT id FROM events
		      WHERE kind != 'snapshot'
		      ORDER BY ts DESC
		      LIMIT ?
		  )
	`, keep)
	if err != nil {
		return fmt.Errorf("prune close events: %w", err)
	}
	return nil
}

// UpsertScrollback inserts or updates a scrollback row by sha256, leaving
// refcount alone (linking happens via LinkEventScrollback).
func (s *Store) UpsertScrollback(ctx context.Context, sha string, bytes, lastUsedTs int64) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO scrollbacks (sha256, bytes, refcount, last_used_ts)
		VALUES (?, ?, 0, ?)
		ON CONFLICT(sha256) DO UPDATE SET last_used_ts = excluded.last_used_ts
	`, sha, bytes, lastUsedTs)
	if err != nil {
		return fmt.Errorf("upsert scrollback: %w", err)
	}
	return nil
}

// LinkEventScrollback links a scrollback to an event, bumping refcount in the
// same transaction.
func (s *Store) LinkEventScrollback(ctx context.Context, eventID int64, paneKey, sha string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO event_scrollbacks (event_id, pane_key, scrollback_sha) VALUES (?, ?, ?)`,
		eventID, paneKey, sha); err != nil {
		return fmt.Errorf("link event_scrollback: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE scrollbacks SET refcount = refcount + 1 WHERE sha256 = ?`, sha); err != nil {
		return fmt.Errorf("bump refcount: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SetMeta upserts a key/value into the meta table.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value
	`, key, value)
	if err != nil {
		return fmt.Errorf("set meta: %w", err)
	}
	return nil
}

// GetMeta returns the value for key, or "" if absent.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get meta: %w", err)
	}
	return v, nil
}

// ScrollbacksWithZeroRef returns the sha256 of every scrollback whose refcount
// has dropped to zero or less. Used by GC.
func (s *Store) ScrollbacksWithZeroRef(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sha256 FROM scrollbacks WHERE refcount <= 0`)
	if err != nil {
		return nil, fmt.Errorf("query orphans: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			return nil, fmt.Errorf("scan orphan sha: %w", err)
		}
		out = append(out, sha)
	}
	return out, rows.Err()
}

// DeleteScrollback removes the scrollback row by sha256.
func (s *Store) DeleteScrollback(ctx context.Context, sha string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM scrollbacks WHERE sha256 = ?`, sha)
	if err != nil {
		return fmt.Errorf("delete scrollback: %w", err)
	}
	return nil
}
