package core

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

var (
	t0      = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apiRef  = model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}
	idleRef = model.ServiceRef{Target: "prod", Scope: "web", Name: "idle"}
	webRef  = model.ServiceRef{Target: "host", Scope: "shop", Name: "web"}
)

type fakeProvider struct {
	kind    string
	inv     model.Inventory
	err     error
	events  []model.Event
	evErr   error
	details map[model.ServiceRef]model.ServiceDetail
}

func (f *fakeProvider) Kind() string { return f.kind }
func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Events: true}
}
func (f *fakeProvider) Inventory(context.Context) (model.Inventory, error) {
	return f.inv, f.err
}
func (f *fakeProvider) Service(_ context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	d, ok := f.details[ref]
	if !ok {
		return model.ServiceDetail{}, provider.ErrNotFound
	}
	return d, nil
}
func (f *fakeProvider) Events(context.Context, model.ServiceRef, time.Time) ([]model.Event, error) {
	return f.events, f.evErr
}
func (f *fakeProvider) Logs(_ context.Context, ref model.InstanceRef, _ provider.LogOptions) (provider.LogStream, error) {
	return &provider.SliceStream{Lines: []model.LogLine{{Text: ref.ID}}}, nil
}

type fakeStops struct {
	recs []model.StopRecord
	err  error
}

func (s *fakeStops) Stops(context.Context, model.ServiceRef, int) ([]model.StopRecord, error) {
	return s.recs, s.err
}
func (s *fakeStops) RecordStops(context.Context, []model.StopRecord) (int, error) { return 0, nil }
func (s *fakeStops) PruneStops(context.Context, time.Time) (int, error)           { return 0, nil }

func inst(ref model.ServiceRef, id, status, health string) model.Instance {
	return model.Instance{Ref: model.InstanceRef{Service: ref, ID: id}, Status: status, Health: health}
}

func setup(t *testing.T) (*Core, *fakeProvider, *fakeProvider, *fakeStops) {
	t.Helper()
	ecs := &fakeProvider{kind: "ecs", inv: model.Inventory{
		Services: []model.Service{
			{Ref: apiRef, Provider: "ecs", Desired: model.Ptr(2), Running: model.Ptr(2)},
			{Ref: idleRef, Provider: "ecs", Desired: model.Ptr(0), Running: model.Ptr(0)},
		},
		Instances: []model.Instance{
			inst(apiRef, "a", model.StatusRunning, model.HealthHealthy),
			inst(apiRef, "b", model.StatusRunning, model.HealthUnhealthy),
			inst(apiRef, "c", model.StatusStopped, ""),
			inst(apiRef, "d", model.StatusPending, ""),
		},
	}, details: map[model.ServiceRef]model.ServiceDetail{
		apiRef: {Service: model.Service{Ref: apiRef, Provider: "ecs", Desired: model.Ptr(2), Running: model.Ptr(2)},
			Instances: []model.Instance{inst(apiRef, "a", model.StatusRunning, "")}},
	}}
	docker := &fakeProvider{kind: "docker", inv: model.Inventory{
		Services:  []model.Service{{Ref: webRef, Provider: "docker", Running: model.Ptr(1)}},
		Instances: []model.Instance{inst(webRef, "w", model.StatusRunning, "")},
	}}
	targets := []inventory.Target{
		{Config: config.Target{Name: "prod", Interval: time.Minute}, Provider: ecs},
		{Config: config.Target{Name: "host", Interval: time.Minute}, Provider: docker},
	}
	stops := &fakeStops{recs: []model.StopRecord{{Instance: model.InstanceRef{Service: apiRef, ID: "c"}, StoppedAt: t0}}}
	cache := inventory.NewCache()
	poller := inventory.NewPoller(targets, cache, stops, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, n := range []string{"prod", "host"} {
		if err := poller.Refresh(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	return New(targets, cache, poller, stops, BuildInfo{Version: "v0.1.0"}), ecs, docker, stops
}

func TestServices(t *testing.T) {
	c, _, docker, _ := setup(t)
	all := c.Services(Filter{})
	if len(all) != 3 || all[0].Ref != webRef || all[1].Ref != apiRef {
		t.Fatalf("order = %+v", all)
	}
	api := all[1]
	if api.Status != StatusDegraded || api.Counts != (Counts{Running: 2, Pending: 1, Stopped: 1, Unhealthy: 1}) || api.Stale {
		t.Fatalf("api = %+v", api)
	}
	if all[2].Status != StatusIdle {
		t.Fatalf("idle = %+v", all[2])
	}

	cases := []struct {
		f    Filter
		want int
	}{
		{Filter{Target: "prod"}, 2},
		{Filter{Provider: "docker"}, 1},
		{Filter{Status: StatusIdle}, 1},
		{Filter{Query: "SHOP/W"}, 1},
		{Filter{Target: "nope"}, 0},
	}
	for _, tt := range cases {
		if got := len(c.Services(tt.f)); got != tt.want {
			t.Fatalf("Services(%+v) = %d, want %d", tt.f, got, tt.want)
		}
	}

	// A target whose refresh failed marks its services stale.
	docker.err = errors.New("refused")
	c.refresher.(*inventory.Poller).Refresh(context.Background(), "host")
	if got := c.Services(Filter{Target: "host"}); len(got) != 1 || !got[0].Stale {
		t.Fatalf("stale = %+v", got)
	}
}

func TestStatusOf(t *testing.T) {
	tests := []struct {
		name string
		s    model.Service
		c    Counts
		want string
	}{
		{"idle", model.Service{Desired: model.Ptr(0)}, Counts{}, StatusIdle},
		{"down", model.Service{Desired: model.Ptr(2), Running: model.Ptr(0)}, Counts{}, StatusDown},
		{"short", model.Service{Desired: model.Ptr(2), Running: model.Ptr(1)}, Counts{Running: 1}, StatusDegraded},
		{"healthy", model.Service{Desired: model.Ptr(2), Running: model.Ptr(2)}, Counts{Running: 2}, StatusHealthy},
		{"counts from instances", model.Service{Desired: model.Ptr(1)}, Counts{Running: 1}, StatusHealthy},
		{"docker down", model.Service{}, Counts{Stopped: 2}, StatusDown},
		{"docker unhealthy", model.Service{}, Counts{Running: 1, Unhealthy: 1}, StatusDegraded},
		{"docker healthy", model.Service{Running: model.Ptr(1)}, Counts{Running: 1}, StatusHealthy},
	}
	for _, tt := range tests {
		if got := statusOf(tt.s, tt.c); got != tt.want {
			t.Fatalf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestStoppedSince(t *testing.T) {
	early, late := t0, t0.Add(time.Hour)
	stopped := func(at *time.Time) model.Instance { return model.Instance{Status: model.StatusStopped, StoppedAt: at} }
	tests := []struct {
		name string
		s    model.Service
		ins  []model.Instance
		want *time.Time
	}{
		{"newest stop", model.Service{}, []model.Instance{stopped(&early), stopped(&late), stopped(nil)}, &late},
		{"something runs", model.Service{}, []model.Instance{stopped(&late), {Status: model.StatusRunning}}, nil},
		{"something pending", model.Service{}, []model.Instance{stopped(&late), {Status: model.StatusPending}}, nil},
		{"provider counts a running task", model.Service{Running: model.Ptr(1)}, []model.Instance{stopped(&late)}, nil},
		{"provider counts a pending task", model.Service{Running: model.Ptr(0), Pending: model.Ptr(1)}, []model.Instance{stopped(&late)}, nil},
		{"no stop time known", model.Service{}, []model.Instance{stopped(nil)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := view(tt.s, tt.ins, false).StoppedSince
			if (got == nil) != (tt.want == nil) || got != nil && !got.Equal(*tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	ref := model.ServiceRef{Target: "host-b", Scope: "_standalone", Name: "redis"}
	for q, want := range map[string]bool{"host-b/_standalone/redis": true, "host-b/redis": true, "redis": true, "host-b/web": false} {
		if got := matches(ref, q); got != want {
			t.Fatalf("matches(%q) = %v", q, got)
		}
	}
}

func TestService(t *testing.T) {
	c, _, _, stops := setup(t)
	ctx := context.Background()
	d, err := c.Service(ctx, apiRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Instances) != 4 || len(d.Stops) != 1 || d.Service.Status != StatusDegraded {
		t.Fatalf("detail = %+v", d)
	}
	for _, ref := range []model.ServiceRef{{Target: "nope", Scope: "x", Name: "y"}, {Target: "prod", Scope: "web", Name: "nope"}} {
		if _, err := c.Service(ctx, ref); !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("%v: err = %v", ref, err)
		}
	}

	r, err := c.Refresh(ctx, apiRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Instances) != 1 || r.Service.Status != StatusHealthy {
		t.Fatalf("refresh = %+v", r)
	}
	if _, err := c.Refresh(ctx, idleRef); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	stops.err = errors.New("db locked")
	if _, err := c.Service(ctx, apiRef); err == nil {
		t.Fatal("store failure not returned")
	}
}

func TestEventsAndLogs(t *testing.T) {
	c, ecs, _, _ := setup(t)
	ctx := context.Background()
	ecs.events = []model.Event{{Time: t0, Service: apiRef, Severity: model.SeverityInfo, Message: "ok"}}
	if evs, err := c.Events(ctx, apiRef, time.Time{}); err != nil || len(evs) != 1 {
		t.Fatalf("events = %v, %v", evs, err)
	}
	if _, err := c.Events(ctx, model.ServiceRef{Target: "nope"}, time.Time{}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	s, err := c.Logs(ctx, model.InstanceRef{Service: apiRef, ID: "a"}, provider.LogOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s.Recv(); l.Text != "a" {
		t.Fatalf("line = %+v", l)
	}
	if _, err := c.Logs(ctx, model.InstanceRef{Service: model.ServiceRef{Target: "nope"}}, provider.LogOptions{}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if c.Build().Version != "v0.1.0" || len(c.Targets()) != 2 {
		t.Fatal("build or targets wrong")
	}
	if ts, ok := c.Target("host"); !ok || ts.Provider != "docker" {
		t.Fatalf("target = %+v, %v", ts, ok)
	}
}

type operatingProvider struct {
	fakeProvider
	calls []string
	err   error
}

func (o *operatingProvider) Restart(_ context.Context, ref model.ServiceRef) error {
	o.calls = append(o.calls, "restart "+ref.String())
	return o.err
}

func (o *operatingProvider) Scale(_ context.Context, ref model.ServiceRef, desired int) error {
	o.calls = append(o.calls, fmt.Sprintf("scale %s %d", ref, desired))
	return o.err
}

func TestOperations(t *testing.T) {
	op := &operatingProvider{fakeProvider: fakeProvider{kind: "ecs"}}
	targets := []inventory.Target{
		{Config: config.Target{Name: "prod"}, Provider: op},
		{Config: config.Target{Name: "host"}, Provider: &fakeProvider{kind: "docker"}},
	}
	c := New(targets, inventory.NewCache(), nil, &fakeStops{}, BuildInfo{})
	ctx := context.Background()
	if err := c.Restart(ctx, apiRef); err != nil {
		t.Fatal(err)
	}
	if err := c.Scale(ctx, apiRef, 3); err != nil {
		t.Fatal(err)
	}
	if strings.Join(op.calls, ";") != "restart prod/web/api;scale prod/web/api 3" {
		t.Fatalf("calls = %q", op.calls)
	}
	op.err = provider.ErrUnreachable
	if err := c.Restart(ctx, apiRef); !errors.Is(err, provider.ErrUnreachable) {
		t.Fatalf("provider error: %v", err)
	}
	if err := c.Scale(ctx, webRef, 1); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("provider without operations: %v", err)
	}
	if err := c.Restart(ctx, model.ServiceRef{Target: "nope"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
	if err := c.Scale(ctx, model.ServiceRef{Target: "nope"}, 1); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown target: %v", err)
	}
}
