package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
)

var (
	t0  = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ref = model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}
)

func open(t *testing.T) (*SQLite, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nested", "fleetbox.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func stop(id string, at time.Time, code *int) model.StopRecord {
	return model.StopRecord{Instance: model.InstanceRef{Service: ref, ID: id}, StoppedAt: at, Reason: "exited", ExitCode: code, Image: "img", Digest: "sha256:a"}
}

func TestStops(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	n, err := s.RecordStops(ctx, []model.StopRecord{stop("a", t0, model.Ptr(137)), stop("b", t0.Add(time.Minute), nil)})
	if err != nil || n != 2 {
		t.Fatalf("RecordStops = %d, %v", n, err)
	}
	// The poller sees the same stopped task on every refresh; it is stored once.
	n, err = s.RecordStops(ctx, []model.StopRecord{stop("a", t0, model.Ptr(137)), stop("c", t0.Add(2*time.Minute), model.Ptr(0))})
	if err != nil || n != 1 {
		t.Fatalf("RecordStops again = %d, %v", n, err)
	}
	if n, err := s.RecordStops(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty = %d, %v", n, err)
	}

	got, err := s.Stops(ctx, ref, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Instance.ID != "c" || got[2].Instance.ID != "a" {
		t.Fatalf("order = %+v", got)
	}
	if *got[2].ExitCode != 137 || got[1].ExitCode != nil || *got[0].ExitCode != 0 {
		t.Fatal("exit codes must round-trip, absent staying absent")
	}
	if !got[2].StoppedAt.Equal(t0) || got[2].Instance.Service != ref || got[2].Digest != "sha256:a" {
		t.Fatalf("record = %+v", got[2])
	}
	if got, _ := s.Stops(ctx, ref, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %d", len(got))
	}
	if got, _ := s.Stops(ctx, model.ServiceRef{Target: "prod", Scope: "web", Name: "other"}, 10); len(got) != 0 {
		t.Fatalf("other service = %+v", got)
	}

	pruned, err := s.PruneStops(ctx, t0.Add(90*time.Second))
	if err != nil || pruned != 2 {
		t.Fatalf("PruneStops = %d, %v", pruned, err)
	}
	if got, _ := s.Stops(ctx, ref, 10); len(got) != 1 {
		t.Fatalf("after prune = %+v", got)
	}
}

func TestAudit(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for i, res := range []string{ResultOK, ResultDenied} {
		e := AuditEntry{Time: t0.Add(time.Duration(i) * time.Second), User: "alice", Service: ref, Action: "restart", Result: res}
		if res == ResultDenied {
			e.Error = "target is read-only"
		}
		if err := s.AppendAudit(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendAudit(ctx, AuditEntry{User: "alice"}); err == nil {
		t.Fatal("incomplete entry accepted")
	}
	got, err := s.Audit(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Result != ResultDenied || got[0].Error == "" || got[1].Service != ref || !got[1].Time.Equal(t0) {
		t.Fatalf("audit = %+v", got)
	}
}

func TestSessions(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, sess := range []Session{
		{ID: "live", User: "alice", Groups: []string{"platform", "dev"}, CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)},
		{ID: "old", User: "bob", CreatedAt: t0.Add(-2 * time.Hour), ExpiresAt: t0.Add(-time.Hour)},
	} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CreateSession(ctx, Session{ID: "x"}); err == nil {
		t.Fatal("session without a user accepted")
	}
	if err := s.CreateSession(ctx, Session{ID: "live", User: "mallory"}); err == nil {
		t.Fatal("duplicate session id accepted")
	}
	got, err := s.Session(ctx, "live", t0)
	if err != nil {
		t.Fatal(err)
	}
	if got.User != "alice" || len(got.Groups) != 2 || got.Groups[1] != "dev" || !got.CreatedAt.Equal(t0) || !got.ExpiresAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("session = %+v", got)
	}
	if _, err := s.Session(ctx, "live", t0.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session: err = %v", err)
	}
	if _, err := s.Session(ctx, "nope", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: err = %v", err)
	}
	if old, err := s.Session(ctx, "old", t0.Add(-90*time.Minute)); err != nil || old.Groups == nil {
		t.Fatalf("session without groups = %+v, %v", old, err)
	}
	if n, err := s.PruneSessions(ctx, t0); err != nil || n != 1 {
		t.Fatalf("pruned %d, err %v", n, err)
	}
	if err := s.DeleteSession(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "live", t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session: err = %v", err)
	}

	if _, err := s.db.Exec(`INSERT INTO sessions VALUES ('bad', 'eve', 'not json', 0, ?)`, t0.Add(time.Hour).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Session(ctx, "bad", t0); err == nil || !strings.Contains(err.Error(), "session groups") {
		t.Fatalf("corrupt groups: err = %v", err)
	}
}

func TestReopenKeepsDataAndSchema(t *testing.T) {
	s, path := open(t)
	ctx := context.Background()
	if _, err := s.RecordStops(ctx, []model.StopRecord{stop("a", t0, nil)}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, _ := s2.Stops(ctx, ref, 10); len(got) != 1 {
		t.Fatalf("data lost on reopen: %+v", got)
	}
}

func TestOpenErrors(t *testing.T) {
	s, path := open(t)
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(context.Background(), path); err == nil || !strings.Contains(err.Error(), "newer than this build") {
		t.Fatalf("err = %v", err)
	}

	// A failing migration is reported and leaves the version where it was.
	orig := migrations
	t.Cleanup(func() { migrations = orig })
	migrations = append(append([]string{}, orig...), "CREATE TABLE broken (")
	fresh := filepath.Join(t.TempDir(), "m.db")
	if _, err := Open(context.Background(), fresh); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("migration %d", len(orig)+1)) {
		t.Fatalf("err = %v", err)
	}
	migrations = orig
	s3, err := Open(context.Background(), fresh)
	if err != nil {
		t.Fatalf("reopen after a failed migration: %v", err)
	}
	s3.Close()

	if _, err := Open(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "schema version") {
		t.Fatalf("opening a directory: err = %v", err)
	}

	blocker := filepath.Join(t.TempDir(), "file")
	if s, err := Open(context.Background(), blocker); err != nil {
		t.Fatal(err)
	} else {
		s.Close()
	}
	if _, err := Open(context.Background(), filepath.Join(blocker, "under-a-file.db")); err == nil {
		t.Fatal("opening below a regular file succeeded")
	}
}

func TestClosedStoreErrors(t *testing.T) {
	s, _ := open(t)
	s.Close()
	ctx := context.Background()
	if _, err := s.RecordStops(ctx, []model.StopRecord{stop("a", t0, nil)}); err == nil {
		t.Fatal("RecordStops on a closed store")
	}
	if _, err := s.Stops(ctx, ref, 1); err == nil {
		t.Fatal("Stops on a closed store")
	}
	if _, err := s.PruneStops(ctx, t0); err == nil {
		t.Fatal("PruneStops on a closed store")
	}
	if _, err := s.Audit(ctx, 1); err == nil {
		t.Fatal("Audit on a closed store")
	}
	if err := s.CreateSession(ctx, Session{ID: "a", User: "u"}); err == nil {
		t.Fatal("CreateSession on a closed store")
	}
	if _, err := s.Session(ctx, "a", t0); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("Session on a closed store: %v", err)
	}
	if _, err := s.PruneSessions(ctx, t0); err == nil {
		t.Fatal("PruneSessions on a closed store")
	}
}
