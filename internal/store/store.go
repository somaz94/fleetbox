// Package store persists what must outlive a provider's own memory: stop
// reasons (ECS forgets stopped tasks within hours) and the audit log.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo

	"github.com/somaz94/fleetbox/internal/model"
)

// Audit results.
const (
	ResultOK     = "ok"
	ResultDenied = "denied"
	ResultError  = "error"
)

// AuditEntry records one attempted operation, allowed or not.
type AuditEntry struct {
	ID      int64            `json:"id"`
	Time    time.Time        `json:"time"`
	User    string           `json:"user"`
	Service model.ServiceRef `json:"service"`
	Action  string           `json:"action"`
	Params  string           `json:"params,omitempty"`
	Result  string           `json:"result"`
	Error   string           `json:"error,omitempty"`
}

// Store is what the rest of the system needs from persistence. SQLite
// implements it; a Postgres implementation can replace it for HA.
type Store interface {
	RecordStops(ctx context.Context, recs []model.StopRecord) (int, error)
	Stops(ctx context.Context, ref model.ServiceRef, limit int) ([]model.StopRecord, error)
	PruneStops(ctx context.Context, before time.Time) (int, error)
	AppendAudit(ctx context.Context, e AuditEntry) error
	Audit(ctx context.Context, limit int) ([]AuditEntry, error)
	Close() error
}

// migrations are applied in order; PRAGMA user_version records how many ran.
var migrations = []string{
	`CREATE TABLE stops (
		target      TEXT    NOT NULL,
		scope       TEXT    NOT NULL,
		service     TEXT    NOT NULL,
		instance_id TEXT    NOT NULL,
		stopped_at  INTEGER NOT NULL,
		reason      TEXT    NOT NULL DEFAULT '',
		exit_code   INTEGER,
		image       TEXT    NOT NULL DEFAULT '',
		digest      TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (target, scope, service, instance_id)
	);
	CREATE INDEX stops_by_service ON stops (target, scope, service, stopped_at DESC);
	CREATE TABLE audit (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		at      INTEGER NOT NULL,
		user    TEXT    NOT NULL,
		target  TEXT    NOT NULL,
		scope   TEXT    NOT NULL,
		service TEXT    NOT NULL,
		action  TEXT    NOT NULL,
		params  TEXT    NOT NULL DEFAULT '',
		result  TEXT    NOT NULL,
		error   TEXT    NOT NULL DEFAULT ''
	);`,
}

// SQLite is the single-replica Store.
type SQLite struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*SQLite, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	// One connection serialises writers; the load here is a few writes per poll.
	db.SetMaxOpenConns(1)
	s := &SQLite{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLite) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("store: schema version %d is newer than this build (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		// BEGIN and COMMIT travel in the same script so the schema change and
		// its version bump cannot land apart.
		script := fmt.Sprintf("BEGIN;\n%s;\nPRAGMA user_version = %d;\nCOMMIT;", migrations[i], i+1)
		if _, err := s.db.ExecContext(ctx, script); err != nil {
			s.db.ExecContext(ctx, "ROLLBACK")
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	return nil
}

// RecordStops stores stop records, ignoring ones already stored. It returns
// how many were new.
func (s *SQLite) RecordStops(ctx context.Context, recs []model.StopRecord) (int, error) {
	if len(recs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO stops
		(target, scope, service, instance_id, stopped_at, reason, exit_code, image, digest)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	added := 0
	for _, r := range recs {
		ref := r.Instance.Service
		var code any
		if r.ExitCode != nil {
			code = *r.ExitCode
		}
		res, err := stmt.ExecContext(ctx, ref.Target, ref.Scope, ref.Name, r.Instance.ID,
			r.StoppedAt.UnixMilli(), r.Reason, code, r.Image, r.Digest)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		added += int(n)
	}
	return added, tx.Commit()
}

// Stops returns a service's stop records, newest first.
func (s *SQLite) Stops(ctx context.Context, ref model.ServiceRef, limit int) ([]model.StopRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT instance_id, stopped_at, reason, exit_code, image, digest
		FROM stops WHERE target = ? AND scope = ? AND service = ?
		ORDER BY stopped_at DESC, instance_id LIMIT ?`, ref.Target, ref.Scope, ref.Name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.StopRecord{}
	for rows.Next() {
		var (
			r    model.StopRecord
			at   int64
			code sql.NullInt64
		)
		if err := rows.Scan(&r.Instance.ID, &at, &r.Reason, &code, &r.Image, &r.Digest); err != nil {
			return nil, err
		}
		r.Instance.Service = ref
		r.StoppedAt = time.UnixMilli(at).UTC()
		if code.Valid {
			r.ExitCode = model.Ptr(int(code.Int64))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneStops deletes records of stops before the given time.
func (s *SQLite) PruneStops(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM stops WHERE stopped_at < ?", before.UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *SQLite) AppendAudit(ctx context.Context, e AuditEntry) error {
	if e.User == "" || e.Action == "" || e.Result == "" {
		return errors.New("store: audit entry needs user, action and result")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO audit (at, user, target, scope, service, action, params, result, error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, e.Time.UnixMilli(), e.User, e.Service.Target, e.Service.Scope, e.Service.Name,
		e.Action, e.Params, e.Result, e.Error)
	return err
}

// Audit returns the newest entries first.
func (s *SQLite) Audit(ctx context.Context, limit int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, user, target, scope, service, action, params, result, error
		FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var (
			e  AuditEntry
			at int64
		)
		if err := rows.Scan(&e.ID, &at, &e.User, &e.Service.Target, &e.Service.Scope, &e.Service.Name,
			&e.Action, &e.Params, &e.Result, &e.Error); err != nil {
			return nil, err
		}
		e.Time = time.UnixMilli(at).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *SQLite) Close() error { return s.db.Close() }
