package inventory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

var (
	t0      = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apiRef  = model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}
	jobRef  = model.ServiceRef{Target: "prod", Scope: "web", Name: "job"}
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type fakeProvider struct {
	mu      sync.Mutex
	inv     model.Inventory
	err     error
	calls   int
	details map[model.ServiceRef]model.ServiceDetail
	svcErr  error
}

func (f *fakeProvider) Kind() string { return "fake" }
func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Logs: true}
}
func (f *fakeProvider) Inventory(ctx context.Context) (model.Inventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if err := ctx.Err(); err != nil {
		return model.Inventory{}, err
	}
	return f.inv, f.err
}
func (f *fakeProvider) Service(_ context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	if f.svcErr != nil {
		return model.ServiceDetail{}, f.svcErr
	}
	d, ok := f.details[ref]
	if !ok {
		return model.ServiceDetail{}, provider.ErrNotFound
	}
	return d, nil
}
func (f *fakeProvider) Events(context.Context, model.ServiceRef, time.Time) ([]model.Event, error) {
	return nil, nil
}
func (f *fakeProvider) Logs(context.Context, model.InstanceRef, provider.LogOptions) (provider.LogStream, error) {
	return nil, provider.ErrUnsupported
}
func (f *fakeProvider) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type watchingProvider struct {
	*fakeProvider
	mu       sync.Mutex
	chans    chan chan struct{}
	watchErr error
	watches  int
}

func (w *watchingProvider) Watch(ctx context.Context) (<-chan struct{}, error) {
	w.mu.Lock()
	w.watches++
	err := w.watchErr
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	ch := make(chan struct{})
	w.chans <- ch
	return ch, nil
}

func (w *watchingProvider) watchCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.watches
}

type fakeStops struct {
	mu       sync.Mutex
	recs     []model.StopRecord
	err      error
	prunes   []time.Time
	pruneN   int
	pruneErr error
}

func (s *fakeStops) RecordStops(_ context.Context, recs []model.StopRecord) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return 0, s.err
	}
	s.recs = append(s.recs, recs...)
	return len(recs), nil
}

func (s *fakeStops) PruneStops(_ context.Context, before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunes = append(s.prunes, before)
	return s.pruneN, s.pruneErr
}

func (s *fakeStops) pruneCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.prunes)
}

func inventoryWith(refs ...model.ServiceRef) model.Inventory {
	stopped := t0
	inv := model.Inventory{}
	for _, r := range refs {
		inv.Services = append(inv.Services, model.Service{Ref: r, Provider: "fake"})
		inv.Instances = append(inv.Instances,
			model.Instance{Ref: model.InstanceRef{Service: r, ID: "run"}, Status: model.StatusRunning},
			model.Instance{Ref: model.InstanceRef{Service: r, ID: "stop"}, Status: model.StatusStopped, StoppedAt: &stopped, StopReason: "exited"},
		)
	}
	return inv
}

func newPoller(p provider.Provider, stops *fakeStops, interval time.Duration) (*Poller, *Cache) {
	cache := NewCache()
	pl := NewPoller([]Target{{Config: config.Target{Name: "prod", Env: "prod", Readonly: true, Interval: interval}, Provider: p}}, cache, stops, discard)
	pl.now = func() time.Time { return t0 }
	pl.debounce, pl.backoff = time.Millisecond, time.Millisecond
	return pl, cache
}

func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRegistrationBeforeFirstRefresh(t *testing.T) {
	_, cache := newPoller(&fakeProvider{}, &fakeStops{}, time.Minute)
	s, ok := cache.Target("prod")
	if !ok || s.Provider != "fake" || s.Env != "prod" || !s.Readonly || !s.Capabilities.Logs || s.RefreshedAt != nil || s.Stale() {
		t.Fatalf("state = %+v", s)
	}
	if _, ok := cache.Target("nope"); ok {
		t.Fatal("unknown target found")
	}
	if s.Endpoint != nil {
		t.Fatalf("a provider without Describer has endpoint %+v", s.Endpoint)
	}
}

type describedProvider struct{ fakeProvider }

func (*describedProvider) Endpoint() []model.Attribute {
	return []model.Attribute{{Name: "host", Value: "192.0.2.10:22"}}
}

// The endpoint is read once at registration and survives every refresh.
func TestEndpointKeptAcrossRefreshes(t *testing.T) {
	pl, cache := newPoller(&describedProvider{fakeProvider{inv: inventoryWith(apiRef)}}, &fakeStops{}, time.Minute)
	if err := pl.Refresh(context.Background(), "prod"); err != nil {
		t.Fatal(err)
	}
	s, _ := cache.Target("prod")
	if len(s.Endpoint) != 1 || s.Endpoint[0].Value != "192.0.2.10:22" || s.RefreshedAt == nil {
		t.Fatalf("state = %+v", s)
	}
}

func TestRefresh(t *testing.T) {
	fp := &fakeProvider{inv: inventoryWith(apiRef)}
	stops := &fakeStops{}
	pl, cache := newPoller(fp, stops, time.Minute)
	ctx := context.Background()

	if err := pl.Refresh(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	s, _ := cache.Target("prod")
	if s.RefreshedAt == nil || len(s.Inventory.Services) != 1 || s.Stale() {
		t.Fatalf("state = %+v", s)
	}
	if len(stops.recs) != 1 || stops.recs[0].Instance.ID != "stop" {
		t.Fatalf("stops = %+v", stops.recs)
	}

	// A failure keeps the last good inventory and says why.
	fp.err = errors.New("dial tcp: refused")
	if err := pl.Refresh(ctx, "prod"); err == nil {
		t.Fatal("failure not returned")
	}
	s, _ = cache.Target("prod")
	if !s.Stale() || s.Error != "dial tcp: refused" || len(s.Inventory.Services) != 1 {
		t.Fatalf("stale state = %+v", s)
	}
	if got := cache.Targets(); len(got) != 1 || !got[0].Stale() {
		t.Fatalf("targets = %+v", got)
	}

	fp.err = nil
	pl.Refresh(ctx, "prod")
	if s, _ := cache.Target("prod"); s.Stale() {
		t.Fatal("success did not clear the error")
	}

	// Shutting down is not a target failure.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := pl.Refresh(cctx, "prod"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if s, _ := cache.Target("prod"); s.Stale() {
		t.Fatal("cancellation recorded as a failure")
	}

	if err := pl.Refresh(ctx, "nope"); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	stops.err = errors.New("disk full")
	if err := pl.Refresh(ctx, "prod"); err != nil {
		t.Fatalf("a store failure must not fail the refresh: %v", err)
	}
}

func TestRefreshService(t *testing.T) {
	newRun := t0.Add(time.Hour)
	fresh := model.ServiceDetail{
		Service:   model.Service{Ref: apiRef, Provider: "fake", Revision: "v2"},
		Instances: []model.Instance{{Ref: model.InstanceRef{Service: apiRef, ID: "new"}, Status: model.StatusRunning, StartedAt: &newRun}},
	}
	job := model.ServiceDetail{Service: model.Service{Ref: jobRef, Provider: "fake"}, Instances: []model.Instance{}}
	fp := &fakeProvider{
		inv:     inventoryWith(apiRef, model.ServiceRef{Target: "prod", Scope: "web", Name: "zzz"}),
		details: map[model.ServiceRef]model.ServiceDetail{apiRef: fresh, jobRef: job},
	}
	pl, cache := newPoller(fp, &fakeStops{}, time.Minute)
	ctx := context.Background()
	pl.Refresh(ctx, "prod")
	before, _ := cache.Target("prod")

	if _, err := pl.RefreshService(ctx, apiRef); err != nil {
		t.Fatal(err)
	}
	d, ok := cache.Service(apiRef)
	if !ok || d.Service.Revision != "v2" || len(d.Instances) != 1 || d.Instances[0].Ref.ID != "new" {
		t.Fatalf("detail = %+v", d)
	}
	if len(before.Inventory.Instances) != 4 || before.Inventory.Services[0].Revision != "" {
		t.Fatal("a copy handed out earlier was mutated")
	}

	// A service the poll has not seen yet is inserted in order.
	if _, err := pl.RefreshService(ctx, jobRef); err != nil {
		t.Fatal(err)
	}
	s, _ := cache.Target("prod")
	if len(s.Inventory.Services) != 3 || s.Inventory.Services[1].Ref != jobRef {
		t.Fatalf("services = %+v", s.Inventory.Services)
	}

	// A service the provider no longer has leaves the cache.
	delete(fp.details, apiRef)
	if _, err := pl.RefreshService(ctx, apiRef); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := cache.Service(apiRef); ok {
		t.Fatal("removed service still cached")
	}
	if _, ok := cache.Service(model.ServiceRef{Target: "nope"}); ok {
		t.Fatal("unknown target found")
	}

	fp.svcErr = errors.New("throttled")
	if _, err := pl.RefreshService(ctx, jobRef); err == nil {
		t.Fatal("provider error not returned")
	}
	if _, err := pl.RefreshService(ctx, model.ServiceRef{Target: "nope"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}

	// Writes for an unknown target are ignored, not a panic.
	cache.replaceService(model.ServiceDetail{Service: model.Service{Ref: model.ServiceRef{Target: "nope"}}})
	cache.removeService(model.ServiceRef{Target: "nope"})
	cache.setSuccess("nope", model.Inventory{}, t0)
	cache.setFailure("nope", errors.New("x"), t0)
}

func TestRunTicks(t *testing.T) {
	fp := &fakeProvider{inv: inventoryWith(apiRef)}
	stops := &fakeStops{pruneN: 3}
	pl, _ := newPoller(fp, stops, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pl.Run(ctx); close(done) }()
	eventually(t, func() bool { return fp.callCount() >= 3 })
	eventually(t, func() bool { return stops.pruneCount() >= 1 })
	cancel()
	<-done
	if got := stops.prunes[0]; !got.Equal(t0.Add(-DefaultRetention)) {
		t.Fatalf("pruned before %s", got)
	}
}

func TestRunWatches(t *testing.T) {
	wp := &watchingProvider{fakeProvider: &fakeProvider{inv: inventoryWith(apiRef)}, chans: make(chan chan struct{}, 4)}
	pl, _ := newPoller(wp, &fakeStops{pruneErr: errors.New("locked")}, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { pl.Run(ctx); close(done) }()

	eventually(t, func() bool { return wp.callCount() == 1 })
	ch := <-wp.chans
	ch <- struct{}{}
	eventually(t, func() bool { return wp.callCount() == 2 })

	// A watch that ends is reopened.
	close(ch)
	ch2 := <-wp.chans
	ch2 <- struct{}{}
	eventually(t, func() bool { return wp.callCount() == 3 })

	// A watch that cannot open is retried.
	wp.mu.Lock()
	wp.watchErr = errors.New("daemon restarting")
	wp.mu.Unlock()
	close(ch2)
	n := wp.watchCount()
	eventually(t, func() bool { return wp.watchCount() > n+1 })

	cancel()
	<-done
}
