package inventory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

const (
	DefaultDebounce       = time.Second
	DefaultRewatchBackoff = 10 * time.Second
	DefaultRetention      = 30 * 24 * time.Hour
	pruneEvery            = time.Hour
)

// StopRecorder is the part of the store the poller writes to.
type StopRecorder interface {
	RecordStops(ctx context.Context, recs []model.StopRecord) (int, error)
	PruneStops(ctx context.Context, before time.Time) (int, error)
}

// Target pairs a configured target with its provider.
type Target struct {
	Config   config.Target
	Provider provider.Provider
}

// Poller refreshes every target on its own interval, and on change
// notifications from providers that can push them.
type Poller struct {
	targets  map[string]Target
	cache    *Cache
	stops    StopRecorder
	log      *slog.Logger
	now      func() time.Time
	debounce time.Duration
	backoff  time.Duration
	keep     time.Duration
}

// NewPoller registers every target in the cache before the first refresh,
// so a target that never answers is still listed.
func NewPoller(targets []Target, cache *Cache, stops StopRecorder, log *slog.Logger) *Poller {
	p := &Poller{
		targets: map[string]Target{}, cache: cache, stops: stops, log: log, now: time.Now,
		debounce: DefaultDebounce, backoff: DefaultRewatchBackoff, keep: DefaultRetention,
	}
	for _, t := range targets {
		p.targets[t.Config.Name] = t
		cache.add(TargetState{
			Name: t.Config.Name, Provider: t.Provider.Kind(), Env: t.Config.Env,
			Readonly: t.Config.Readonly, Capabilities: t.Provider.Capabilities(),
		})
	}
	return p
}

// Run refreshes until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range p.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx, t)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.pruneLoop(ctx)
	}()
	wg.Wait()
}

func (p *Poller) loop(ctx context.Context, t Target) {
	trigger := make(chan struct{}, 1)
	if w, ok := t.Provider.(provider.Watcher); ok {
		go p.watch(ctx, t.Config.Name, w, trigger)
	}
	ticker := time.NewTicker(t.Config.Interval)
	defer ticker.Stop()
	p.Refresh(ctx, t.Config.Name)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-trigger:
			// Collapse a burst of changes (a compose up touches many containers) into one refresh.
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.debounce):
			}
			select {
			case <-trigger:
			default:
			}
		}
		p.Refresh(ctx, t.Config.Name)
	}
}

// watch keeps a provider watch open, re-opening it after a backoff whenever it
// fails or ends.
func (p *Poller) watch(ctx context.Context, name string, w provider.Watcher, trigger chan<- struct{}) {
	for {
		ch, err := w.Watch(ctx)
		if err != nil {
			p.log.Warn("watch failed", "target", name, "err", err)
		} else {
			for range ch {
				select {
				case trigger <- struct{}{}:
				default:
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.backoff):
		}
	}
}

// Refresh reads one target's inventory into the cache and records the stops
// it contains. A refresh is bounded by the target's interval.
func (p *Poller) Refresh(ctx context.Context, name string) error {
	t, ok := p.targets[name]
	if !ok {
		return fmt.Errorf("target %q: %w", name, provider.ErrNotFound)
	}
	rctx, cancel := context.WithTimeout(ctx, t.Config.Interval)
	defer cancel()
	inv, err := t.Provider.Inventory(rctx)
	at := p.now()
	if err != nil {
		if ctx.Err() == nil {
			p.cache.setFailure(name, err, at)
			p.log.Warn("refresh failed", "target", name, "err", err)
		}
		return err
	}
	p.cache.setSuccess(name, inv, at)
	p.recordStops(ctx, name, inv.Instances)
	return nil
}

// RefreshService reads one service fresh and updates the cache; a service the
// provider no longer has is removed.
func (p *Poller) RefreshService(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	t, ok := p.targets[ref.Target]
	if !ok {
		return model.ServiceDetail{}, fmt.Errorf("target %q: %w", ref.Target, provider.ErrNotFound)
	}
	d, err := t.Provider.Service(ctx, ref)
	if errors.Is(err, provider.ErrNotFound) {
		p.cache.removeService(ref)
		return model.ServiceDetail{}, err
	}
	if err != nil {
		return model.ServiceDetail{}, err
	}
	p.cache.replaceService(d)
	p.recordStops(ctx, ref.Target, d.Instances)
	return d, nil
}

func (p *Poller) recordStops(ctx context.Context, name string, ins []model.Instance) {
	var recs []model.StopRecord
	for _, in := range ins {
		if r, ok := model.StopRecordOf(in); ok {
			recs = append(recs, r)
		}
	}
	if _, err := p.stops.RecordStops(ctx, recs); err != nil {
		p.log.Error("record stops failed", "target", name, "err", err)
	}
}

func (p *Poller) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		p.prune(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (p *Poller) prune(ctx context.Context) {
	n, err := p.stops.PruneStops(ctx, p.now().Add(-p.keep))
	switch {
	case err != nil && ctx.Err() == nil:
		p.log.Error("prune stops failed", "err", err)
	case n > 0:
		p.log.Info("pruned stop records", "count", n)
	}
}
