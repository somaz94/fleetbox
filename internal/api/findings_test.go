package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/store"
)

// seedFindings stores an open stuck finding on prod, an open hygiene finding
// on prod, an open drift finding spanning prod and stage, and a resolved one.
func seedFindings(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	prod := model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}
	stage := model.ServiceRef{Target: "stage", Scope: "web", Name: "api"}
	f := func(key, kind, severity string, opened time.Time, refs ...model.ServiceRef) store.FindingSave {
		return store.FindingSave{Opened: true, Finding: model.Finding{Key: key, Kind: kind, Rule: "r", Severity: severity, Summary: key,
			Services: refs, Evidence: []model.Evidence{{Label: "env", Value: "sha256:a", Service: &refs[0]}, {Label: "note", Value: "x"}},
			FirstSeen: t0, LastSeen: opened, OpenedAt: &opened}}
	}
	if err := h.audit.SaveFindings(ctx, store.FindingChanges{At: t0, Save: []store.FindingSave{
		f("stuck", model.FindingStuck, model.SeverityWarning, t0.Add(time.Minute), prod),
		f("hygiene", model.FindingHygiene, model.SeverityInfo, t0.Add(2*time.Minute), prod),
		f("drift", model.FindingDrift, model.SeverityWarning, t0.Add(3*time.Minute), prod, stage),
		f("old", model.FindingWhy, model.SeverityWarning, t0, prod),
	}}); err != nil {
		t.Fatal(err)
	}
	active, _ := h.audit.ActiveFindings(ctx)
	if err := h.audit.SaveFindings(ctx, store.FindingChanges{At: t0.Add(time.Hour), Resolve: []int64{active[3].ID}}); err != nil {
		t.Fatal(err)
	}
}

func summaries(fs []Finding) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Summary)
	}
	return out
}

func TestListFindings(t *testing.T) {
	h := newHarnessWith(t, proxyAuth)
	seedFindings(t, h)
	list := func(query string, hdr http.Header) []string {
		return summaries(decode[[]Finding](t, h.doWith(t, "GET", "/api/v1/findings"+query, hdr)))
	}

	// The platform group sees every target; newest opening first.
	if got := list("", operator); !slices.Equal(got, []string{"drift", "hygiene", "stuck"}) {
		t.Fatalf("open = %v", got)
	}
	// Developers see prod only, so drift involving stage is hidden from them.
	if got := list("", viewer); !slices.Equal(got, []string{"hygiene", "stuck"}) {
		t.Fatalf("viewer = %v", got)
	}
	// Asking for a hidden target reads like asking for an unknown one.
	if got := list("?target=stage", viewer); len(got) != 0 {
		t.Fatalf("viewer stage = %v", got)
	}
	if got := list("", stranger); len(got) != 0 {
		t.Fatalf("stranger = %v", got)
	}
	for query, want := range map[string][]string{
		"?kind=hygiene":        {"hygiene"},
		"?severity=warning":    {"drift", "stuck"},
		"?target=stage":        {"drift"},
		"?target=nope":         {},
		"?state=resolved":      {"old"},
		"?state=open&kind=why": {},
	} {
		if got := list(query, operator); !slices.Equal(got, want) {
			t.Fatalf("%s = %v, want %v", query, got, want)
		}
	}

	d := decode[[]Finding](t, h.doWith(t, "GET", "/api/v1/findings?kind=drift", operator))[0]
	if len(d.Services) != 2 || d.Services[1].Target != "stage" || d.Evidence[0].Service == nil || d.Evidence[1].Service != nil ||
		d.ResolvedAt != nil || d.OpenedAt.IsZero() {
		t.Fatalf("drift = %+v", d)
	}
	if r := decode[[]Finding](t, h.doWith(t, "GET", "/api/v1/findings?state=resolved", operator))[0]; r.ResolvedAt == nil || r.OpenedAt.IsZero() {
		t.Fatalf("resolved = %+v", r)
	}

	for _, q := range []string{"?kind=nope", "?severity=loud", "?state=closed"} {
		wantError(t, h.doWith(t, "GET", "/api/v1/findings"+q, operator), http.StatusBadRequest, ErrorErrorCodeBadRequest)
	}
	h.audit.Close()
	wantError(t, h.doWith(t, "GET", "/api/v1/findings", operator), http.StatusInternalServerError, ErrorErrorCodeInternal)
}

// The resolved cap counts only what the caller asked for and may see.
func TestResolvedFindingsCapAfterFiltering(t *testing.T) {
	h := newHarnessWith(t, proxyAuth)
	ctx := context.Background()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	save := func(key, tgt string, at time.Time) {
		ref := model.ServiceRef{Target: tgt, Scope: "web", Name: "api"}
		f := model.Finding{Key: key, Kind: model.FindingStuck, Rule: "r", Severity: model.SeverityWarning, Summary: key,
			Services: []model.ServiceRef{ref}, FirstSeen: at, LastSeen: at, OpenedAt: &at}
		if err := h.audit.SaveFindings(ctx, store.FindingChanges{At: at, Save: []store.FindingSave{{Finding: f, Opened: true}}}); err != nil {
			t.Fatal(err)
		}
		active, _ := h.audit.ActiveFindings(ctx)
		if err := h.audit.SaveFindings(ctx, store.FindingChanges{At: at, Resolve: []int64{active[0].ID}}); err != nil {
			t.Fatal(err)
		}
	}
	save("old-prod", "prod", t0)
	for i := range resolvedFindings + 5 {
		save(fmt.Sprintf("stage-%d", i), "stage", t0.Add(time.Duration(i+1)*time.Minute))
	}
	if got := decode[[]Finding](t, h.doWith(t, "GET", "/api/v1/findings?state=resolved", viewer)); len(got) != 1 || got[0].Summary != "old-prod" {
		t.Fatalf("viewer resolved = %v", summaries(got))
	}
	if got := decode[[]Finding](t, h.doWith(t, "GET", "/api/v1/findings?state=resolved", operator)); len(got) != resolvedFindings || got[0].Summary != "stage-104" {
		t.Fatalf("operator resolved = %d, first %v", len(got), got[0].Summary)
	}
}
