package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
)

// FindingSave writes one finding; a zero ID inserts it. Opened records the
// open transition along with it.
type FindingSave struct {
	Finding model.Finding
	Opened  bool
}

// FindingChanges is one evaluation's outcome, written in one transaction.
// Resolve closes open findings and records the transition. Retire closes open
// findings of a kind switched off, and Drop deletes pending ones that went
// away before opening; nobody is told about either.
type FindingChanges struct {
	At      time.Time
	Save    []FindingSave
	Resolve []int64
	Retire  []int64
	Drop    []int64
}

// Finding states a query selects.
const (
	StateOpen     = "open"
	StateResolved = "resolved"
)

// FindingQuery selects open or resolved findings; retention bounds both.
type FindingQuery struct {
	State string
}

// StopsSince returns every stop record at or after since, newest first.
func (s *SQLite) StopsSince(ctx context.Context, since time.Time) ([]model.StopRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target, scope, service, instance_id, stopped_at, reason, exit_code, image, digest
		FROM stops WHERE stopped_at >= ? ORDER BY stopped_at DESC, instance_id`, since.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.StopRecord{}
	for rows.Next() {
		var (
			r    model.StopRecord
			ref  = &r.Instance.Service
			at   int64
			code sql.NullInt64
		)
		if err := rows.Scan(&ref.Target, &ref.Scope, &ref.Name, &r.Instance.ID, &at, &r.Reason, &code, &r.Image, &r.Digest); err != nil {
			return nil, err
		}
		r.StoppedAt = time.UnixMilli(at).UTC()
		if code.Valid {
			r.ExitCode = model.Ptr(int(code.Int64))
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const findingColumns = `f.id, f.key, f.kind, f.rule, f.severity, f.summary, f.services, f.evidence, f.first_seen, f.last_seen, f.opened_at, f.resolved_at`

// ActiveFindings returns every pending and open finding.
func (s *SQLite) ActiveFindings(ctx context.Context) ([]model.Finding, error) {
	return s.findings(ctx, `SELECT `+findingColumns+` FROM findings f WHERE resolved_at IS NULL ORDER BY id`)
}

// Findings returns open findings, most recently opened first, or resolved
// ones, most recently resolved first.
func (s *SQLite) Findings(ctx context.Context, q FindingQuery) ([]model.Finding, error) {
	switch q.State {
	case StateOpen:
		return s.findings(ctx, `SELECT `+findingColumns+` FROM findings f
			WHERE opened_at IS NOT NULL AND resolved_at IS NULL ORDER BY opened_at DESC, id DESC`)
	case StateResolved:
		return s.findings(ctx, `SELECT `+findingColumns+` FROM findings f
			WHERE opened_at IS NOT NULL AND resolved_at IS NOT NULL ORDER BY resolved_at DESC, id DESC`)
	}
	return nil, fmt.Errorf("store: unknown finding state %q", q.State)
}

func (s *SQLite) findings(ctx context.Context, query string, args ...any) ([]model.Finding, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Finding{}
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// FindingEvents returns transitions after the given event id, oldest first.
// Each carries its finding as stored now, with the summary, severity and
// services it had at the transition, so a route matches what the message says.
func (s *SQLite) FindingEvents(ctx context.Context, after int64, limit int) ([]model.FindingEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT e.id, e.transition, e.at, e.severity, e.summary, e.services, `+findingColumns+`
		FROM finding_events e JOIN findings f ON f.id = e.finding WHERE e.id > ? ORDER BY e.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.FindingEvent{}
	for rows.Next() {
		var (
			ev                          model.FindingEvent
			at                          int64
			severity, summary, services string
		)
		f, err := scanFinding(rows, &ev.ID, &ev.Transition, &at, &severity, &summary, &services)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(services), &f.Services); err != nil {
			return nil, fmt.Errorf("store: event %d services: %w", ev.ID, err)
		}
		ev.Time = time.UnixMilli(at).UTC()
		f.Severity, f.Summary = severity, summary
		ev.Finding = f
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LastFindingEvent returns the id of the latest transition, or 0 when none is kept.
func (s *SQLite) LastFindingEvent(ctx context.Context) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM finding_events`).Scan(&id)
	return id, err
}

// NotifyCursor returns the last transition a route delivered, and false for a
// route that has never delivered.
func (s *SQLite) NotifyCursor(ctx context.Context, route string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT event FROM notify_cursors WHERE route = ?`, route).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// SetNotifyCursor records the last transition a route delivered.
func (s *SQLite) SetNotifyCursor(ctx context.Context, route string, event int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO notify_cursors (route, event) VALUES (?, ?)
		ON CONFLICT (route) DO UPDATE SET event = excluded.event`, route, event)
	return err
}

// scanFinding reads findingColumns, after any leading columns given.
func scanFinding(rows *sql.Rows, lead ...any) (model.Finding, error) {
	var (
		f                  model.Finding
		services, evidence string
		first, last        int64
		opened, resolved   sql.NullInt64
	)
	dest := append(lead, &f.ID, &f.Key, &f.Kind, &f.Rule, &f.Severity, &f.Summary, &services, &evidence,
		&first, &last, &opened, &resolved)
	if err := rows.Scan(dest...); err != nil {
		return f, err
	}
	if err := json.Unmarshal([]byte(services), &f.Services); err != nil {
		return f, fmt.Errorf("store: finding %d services: %w", f.ID, err)
	}
	if err := json.Unmarshal([]byte(evidence), &f.Evidence); err != nil {
		return f, fmt.Errorf("store: finding %d evidence: %w", f.ID, err)
	}
	f.FirstSeen, f.LastSeen = time.UnixMilli(first).UTC(), time.UnixMilli(last).UTC()
	f.OpenedAt, f.ResolvedAt = timeOf(opened), timeOf(resolved)
	return f, nil
}

// SaveFindings writes an evaluation's outcome and its transitions together, so
// a transition is recorded exactly when the state it announces is.
func (s *SQLite) SaveFindings(ctx context.Context, c FindingChanges) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	at := c.At.UnixMilli()
	event := func(id int64, transition string) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO finding_events (finding, transition, at, severity, summary, services)
			SELECT id, ?, ?, severity, summary, services FROM findings WHERE id = ?`, transition, at, id)
		return err
	}
	for _, sv := range c.Save {
		f := sv.Finding
		services, err := json.Marshal(f.Services)
		if err != nil {
			return err
		}
		evidence, err := json.Marshal(f.Evidence)
		if err != nil {
			return err
		}
		id := f.ID
		if id == 0 {
			res, err := tx.ExecContext(ctx, `INSERT INTO findings
				(key, kind, rule, severity, summary, services, evidence, first_seen, last_seen, opened_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				f.Key, f.Kind, f.Rule, f.Severity, f.Summary, services, evidence,
				f.FirstSeen.UnixMilli(), f.LastSeen.UnixMilli(), millis(f.OpenedAt))
			if err != nil {
				return err
			}
			if id, err = res.LastInsertId(); err != nil {
				return err
			}
		} else {
			res, err := tx.ExecContext(ctx, `UPDATE findings SET severity = ?, summary = ?, services = ?,
				evidence = ?, last_seen = ?, opened_at = ? WHERE id = ? AND resolved_at IS NULL`,
				f.Severity, f.Summary, services, evidence, f.LastSeen.UnixMilli(), millis(f.OpenedAt), id)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return fmt.Errorf("store: finding %d is no longer active", id)
			}
		}
		if sv.Opened {
			if err := event(id, model.TransitionOpened); err != nil {
				return err
			}
		}
	}
	closeOpen := func(id int64) (bool, error) {
		res, err := tx.ExecContext(ctx, `UPDATE findings SET resolved_at = ? WHERE id = ? AND resolved_at IS NULL`, at, id)
		if err != nil {
			return false, err
		}
		n, _ := res.RowsAffected()
		return n == 1, nil
	}
	for _, id := range c.Resolve {
		closed, err := closeOpen(id)
		if err != nil {
			return err
		}
		if closed {
			if err := event(id, model.TransitionResolved); err != nil {
				return err
			}
		}
	}
	for _, id := range c.Retire {
		if _, err := closeOpen(id); err != nil {
			return err
		}
	}
	for _, id := range c.Drop {
		if _, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE id = ? AND opened_at IS NULL`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PruneFindings deletes findings resolved before the given time, and
// transitions recorded before it.
func (s *SQLite) PruneFindings(ctx context.Context, before time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	b := before.UnixMilli()
	if _, err := tx.ExecContext(ctx, `DELETE FROM finding_events WHERE at < ?
		OR finding IN (SELECT id FROM findings WHERE resolved_at < ?)`, b, b); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE resolved_at < ?`, b)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), tx.Commit()
}

func millis(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func timeOf(v sql.NullInt64) *time.Time {
	if !v.Valid {
		return nil
	}
	t := time.UnixMilli(v.Int64).UTC()
	return &t
}
