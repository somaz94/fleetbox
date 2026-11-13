package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
)

func finding(key string, opened *time.Time) model.Finding {
	return model.Finding{Key: key, Kind: model.FindingStuck, Rule: "below-desired", Severity: model.SeverityWarning,
		Summary: "runs 1 of 2 desired", Services: []model.ServiceRef{ref},
		Evidence:  []model.Evidence{{Label: "desired", Value: "2", Service: &ref}},
		FirstSeen: t0, LastSeen: t0, OpenedAt: opened}
}

// transitions lists the recorded events as "key transition".
func transitions(t *testing.T, s *SQLite) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT f.key, e.transition FROM finding_events e JOIN findings f ON f.id = e.finding ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k, tr string
		if err := rows.Scan(&k, &tr); err != nil {
			t.Fatal(err)
		}
		out = append(out, k+" "+tr)
	}
	return out
}

func TestFindingLifecycle(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	// A pending finding and an open one are stored; only the open one is announced.
	if err := s.SaveFindings(ctx, FindingChanges{At: t0, Save: []FindingSave{
		{Finding: finding("pending", nil)},
		{Finding: finding("open", model.Ptr(t0)), Opened: true},
	}}); err != nil {
		t.Fatal(err)
	}
	active, err := s.ActiveFindings(ctx)
	if err != nil || len(active) != 2 {
		t.Fatalf("active = %+v, %v", active, err)
	}
	pending, open := active[0], active[1]
	if pending.OpenedAt != nil || !open.OpenedAt.Equal(t0) || open.Evidence[0].Service == nil || *open.Evidence[0].Service != ref ||
		open.Services[0] != ref || !open.FirstSeen.Equal(t0) {
		t.Fatalf("stored = %+v / %+v", pending, open)
	}
	if got, _ := s.Findings(ctx, FindingQuery{State: StateOpen}); len(got) != 1 || got[0].Key != "open" {
		t.Fatalf("open findings = %+v", got)
	}

	// The pending one opens, the open one is updated, then resolved.
	t1 := t0.Add(time.Hour)
	pending.OpenedAt, pending.LastSeen = &t1, t1
	open.Summary, open.LastSeen = "runs 0 of 2 desired", t1
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Save: []FindingSave{{Finding: pending, Opened: true}, {Finding: open}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Resolve: []int64{open.ID, open.ID}}); err != nil {
		t.Fatal(err)
	}
	resolved, err := s.Findings(ctx, FindingQuery{State: StateResolved})
	if err != nil || len(resolved) != 1 || resolved[0].Summary != "runs 0 of 2 desired" || !resolved[0].ResolvedAt.Equal(t1) {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}

	// A condition that returns is a new finding under the same key.
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Save: []FindingSave{{Finding: finding("open", nil)}}}); err != nil {
		t.Fatal(err)
	}
	active, _ = s.ActiveFindings(ctx)
	if len(active) != 2 || active[1].Key != "open" || active[1].ID == open.ID {
		t.Fatalf("active after return = %+v", active)
	}
	// Dropping removes a pending finding without a word; an open one survives a drop.
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Drop: []int64{active[1].ID, active[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if active, _ = s.ActiveFindings(ctx); len(active) != 1 || active[0].Key != "pending" {
		t.Fatalf("active after drop = %+v", active)
	}

	want := []string{"open opened", "pending opened", "open resolved"}
	if got := transitions(t, s); len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("transitions = %v", got)
	}

	// An active key is unique.
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Save: []FindingSave{{Finding: finding("pending", nil)}}}); err == nil {
		t.Fatal("a second active finding with the same key was stored")
	}
	if _, err := s.Findings(ctx, FindingQuery{State: "bogus"}); err == nil {
		t.Fatal("unknown state accepted")
	}
}

func TestPruneFindings(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.SaveFindings(ctx, FindingChanges{At: t0, Save: []FindingSave{
		{Finding: finding("old", model.Ptr(t0)), Opened: true},
		{Finding: finding("recent", model.Ptr(t0)), Opened: true},
		{Finding: finding("active", model.Ptr(t0)), Opened: true},
	}}); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ActiveFindings(ctx)
	if err := s.SaveFindings(ctx, FindingChanges{At: t0.Add(time.Hour), Resolve: []int64{active[0].ID}}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveFindings(ctx, FindingChanges{At: t0.Add(3 * time.Hour), Resolve: []int64{active[1].ID}}); err != nil {
		t.Fatal(err)
	}
	n, err := s.PruneFindings(ctx, t0.Add(2*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	left, _ := s.Findings(ctx, FindingQuery{State: StateResolved})
	if len(left) != 1 || left[0].Key != "recent" {
		t.Fatalf("resolved left = %+v", left)
	}
	// Every transition before the cut goes, open findings' included.
	if got := transitions(t, s); len(got) != 1 || got[0] != "recent resolved" {
		t.Fatalf("transitions left = %v", got)
	}
	if got, _ := s.Findings(ctx, FindingQuery{State: StateOpen}); len(got) != 1 || got[0].Key != "active" {
		t.Fatalf("open left = %+v", got)
	}
}

func TestStopsSince(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	other := model.StopRecord{Instance: model.InstanceRef{Service: model.ServiceRef{Target: "dev", Scope: "s", Name: "n"}, ID: "x"},
		StoppedAt: t0.Add(2 * time.Hour), Reason: "OOMKilled"}
	if _, err := s.RecordStops(ctx, []model.StopRecord{stop("a", t0, model.Ptr(1)), stop("b", t0.Add(time.Hour), nil), other}); err != nil {
		t.Fatal(err)
	}
	got, err := s.StopsSince(ctx, t0.Add(time.Hour))
	if err != nil || len(got) != 2 {
		t.Fatalf("stops = %+v, %v", got, err)
	}
	if got[0].Instance != other.Instance || got[0].ExitCode != nil || got[1].Instance.ID != "b" || got[1].Instance.Service != ref ||
		!got[1].StoppedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("stops = %+v", got)
	}
}

func TestStoreErrorsOnClosedDatabase(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	ctx := context.Background()
	if _, err := s.StopsSince(ctx, t0); err == nil {
		t.Fatal("StopsSince on a closed store")
	}
	if _, err := s.ActiveFindings(ctx); err == nil {
		t.Fatal("ActiveFindings on a closed store")
	}
	if err := s.SaveFindings(ctx, FindingChanges{At: t0}); err == nil {
		t.Fatal("SaveFindings on a closed store")
	}
	if _, err := s.PruneFindings(ctx, t0); err == nil {
		t.Fatal("PruneFindings on a closed store")
	}
}

func TestCorruptFindingRows(t *testing.T) {
	for _, col := range []string{"services", "evidence"} {
		s, _ := open(t)
		ctx := context.Background()
		if err := s.SaveFindings(ctx, FindingChanges{At: t0, Save: []FindingSave{{Finding: finding("k", nil)}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE findings SET ` + col + ` = '{'`); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ActiveFindings(ctx); err == nil {
			t.Fatalf("corrupt %s was read", col)
		}
	}
}

func TestSaveFindingsRollsBack(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	// The second save collides on the active key, so neither it nor the
	// first one's transition may land.
	err := s.SaveFindings(ctx, FindingChanges{At: t0, Save: []FindingSave{
		{Finding: finding("k", model.Ptr(t0)), Opened: true},
		{Finding: finding("k", nil)},
	}})
	if err == nil {
		t.Fatal("colliding keys saved")
	}
	if active, _ := s.ActiveFindings(ctx); len(active) != 0 || len(transitions(t, s)) != 0 {
		t.Fatalf("a failed save left %+v", active)
	}
}

func TestFindingEventsAndRetire(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.SaveFindings(ctx, FindingChanges{At: t0, Save: []FindingSave{
		{Finding: finding("a", model.Ptr(t0)), Opened: true},
		{Finding: finding("b", model.Ptr(t0)), Opened: true},
	}}); err != nil {
		t.Fatal(err)
	}
	active, _ := s.ActiveFindings(ctx)
	a, b := active[0], active[1]
	a.Summary, a.LastSeen = "runs 0 of 2 desired", t0.Add(time.Minute)
	t1 := t0.Add(time.Hour)
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Save: []FindingSave{{Finding: a}}, Resolve: []int64{a.ID}, Retire: []int64{b.ID}}); err != nil {
		t.Fatal(err)
	}
	if active, _ := s.ActiveFindings(ctx); len(active) != 0 {
		t.Fatalf("retired finding still active: %+v", active)
	}

	// Events come oldest first, each with the summary it had then; a retired
	// finding was never announced as resolved.
	events, err := s.FindingEvents(ctx, 0, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("events = %+v, %v", events, err)
	}
	opened, resolved := events[0], events[2]
	if opened.Finding.Key != "a" || opened.Transition != model.TransitionOpened || opened.Finding.Summary != "runs 1 of 2 desired" ||
		!opened.Time.Equal(t0) || resolved.Transition != model.TransitionResolved || resolved.Finding.Summary != "runs 0 of 2 desired" ||
		!resolved.Time.Equal(t1) || !resolved.Finding.ResolvedAt.Equal(t1) {
		t.Fatalf("events = %+v", events)
	}
	if later, _ := s.FindingEvents(ctx, opened.ID, 1); len(later) != 1 || later[0].Finding.Key != "b" {
		t.Fatalf("after %d = %+v", opened.ID, later)
	}

	// A resolved finding cannot be written again.
	if err := s.SaveFindings(ctx, FindingChanges{At: t1, Save: []FindingSave{{Finding: a, Opened: true}}}); err == nil {
		t.Fatal("a resolved finding was updated")
	}
	s.Close()
	if _, err := s.FindingEvents(ctx, 0, 1); err == nil {
		t.Fatal("FindingEvents on a closed store")
	}
}
