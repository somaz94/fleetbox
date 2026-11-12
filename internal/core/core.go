// Package core is the service layer between the transports (HTTP now, a CLI
// client later) and the cache, the poller, the providers and the store. Rules
// that are not about a wire format live here, so every transport answers the
// same way.
package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

// Service statuses, derived from counts and instance health.
const (
	StatusHealthy  = "healthy"
	StatusDegraded = "degraded"
	StatusDown     = "down"
	StatusIdle     = "idle"
)

// DefaultStopLimit bounds how much stop history a service detail carries.
const DefaultStopLimit = 20

// BuildInfo identifies the running binary.
type BuildInfo struct {
	Version, Commit, Date string
}

// StopReader is the part of the store core reads.
type StopReader interface {
	Stops(ctx context.Context, ref model.ServiceRef, limit int) ([]model.StopRecord, error)
}

// Refresher is the part of the poller core drives.
type Refresher interface {
	RefreshService(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error)
}

// Counts summarises a service's instances.
type Counts struct {
	Running, Pending, Stopped, Unhealthy int
}

// ServiceView is a service as transports present it.
type ServiceView struct {
	model.Service
	Status string
	Counts Counts
	Stale  bool
}

// DetailView is a service with its instances and recorded stops.
type DetailView struct {
	Service   ServiceView
	Instances []model.Instance
	Stops     []model.StopRecord
}

// Filter narrows a service listing. Empty fields match everything.
type Filter struct {
	Target, Provider, Status, Query string
}

type Core struct {
	cache     *inventory.Cache
	refresher Refresher
	providers map[string]provider.Provider
	stops     StopReader
	build     BuildInfo
	stopLimit int
}

func New(targets []inventory.Target, cache *inventory.Cache, refresher Refresher, stops StopReader, build BuildInfo) *Core {
	c := &Core{cache: cache, refresher: refresher, providers: map[string]provider.Provider{}, stops: stops, build: build, stopLimit: DefaultStopLimit}
	for _, t := range targets {
		c.providers[t.Config.Name] = t.Provider
	}
	return c
}

func (c *Core) Build() BuildInfo { return c.build }

func (c *Core) Targets() []inventory.TargetState { return c.cache.Targets() }

// Services lists services across targets, sorted by target then ref.
func (c *Core) Services(f Filter) []ServiceView {
	q := strings.ToLower(f.Query)
	out := []ServiceView{}
	for _, t := range c.cache.Targets() {
		if f.Target != "" && t.Name != f.Target {
			continue
		}
		if f.Provider != "" && t.Provider != f.Provider {
			continue
		}
		byService := groupInstances(t.Inventory.Instances)
		for _, s := range t.Inventory.Services {
			if q != "" && !strings.Contains(strings.ToLower(s.Ref.String()), q) {
				continue
			}
			v := view(s, byService[s.Ref], t.Stale())
			if f.Status != "" && v.Status != f.Status {
				continue
			}
			out = append(out, v)
		}
	}
	return out
}

// Service returns a service from the cache with its recorded stops.
func (c *Core) Service(ctx context.Context, ref model.ServiceRef) (DetailView, error) {
	t, ok := c.cache.Target(ref.Target)
	if !ok {
		return DetailView{}, fmt.Errorf("target %q: %w", ref.Target, provider.ErrNotFound)
	}
	d, ok := c.cache.Service(ref)
	if !ok {
		return DetailView{}, fmt.Errorf("service %s: %w", ref, provider.ErrNotFound)
	}
	return c.detail(ctx, d, t.Stale())
}

// Refresh reads a service fresh through the poller, which updates the cache.
func (c *Core) Refresh(ctx context.Context, ref model.ServiceRef) (DetailView, error) {
	d, err := c.refresher.RefreshService(ctx, ref)
	if err != nil {
		return DetailView{}, err
	}
	t, _ := c.cache.Target(ref.Target)
	return c.detail(ctx, d, t.Stale())
}

func (c *Core) detail(ctx context.Context, d model.ServiceDetail, stale bool) (DetailView, error) {
	stops, err := c.stops.Stops(ctx, d.Service.Ref, c.stopLimit)
	if err != nil {
		return DetailView{}, fmt.Errorf("read stop history: %w", err)
	}
	return DetailView{Service: view(d.Service, d.Instances, stale), Instances: d.Instances, Stops: stops}, nil
}

// Target returns a configured target's cached state.
func (c *Core) Target(name string) (inventory.TargetState, bool) { return c.cache.Target(name) }

// Restart restarts a service through its provider. Authorisation is the
// caller's job; core only refuses a provider without operations.
func (c *Core) Restart(ctx context.Context, ref model.ServiceRef) error {
	op, err := c.operator(ref.Target)
	if err != nil {
		return err
	}
	return op.Restart(ctx, ref)
}

// Scale sets a service's desired instance count through its provider.
func (c *Core) Scale(ctx context.Context, ref model.ServiceRef, desired int) error {
	op, err := c.operator(ref.Target)
	if err != nil {
		return err
	}
	return op.Scale(ctx, ref, desired)
}

func (c *Core) operator(target string) (provider.Operator, error) {
	p, err := c.provider(target)
	if err != nil {
		return nil, err
	}
	op, ok := p.(provider.Operator)
	if !ok {
		return nil, fmt.Errorf("target %q has no operations: %w", target, provider.ErrUnsupported)
	}
	return op, nil
}

// Events reads a service's timeline from its provider.
func (c *Core) Events(ctx context.Context, ref model.ServiceRef, since time.Time) ([]model.Event, error) {
	p, err := c.provider(ref.Target)
	if err != nil {
		return nil, err
	}
	return p.Events(ctx, ref, since)
}

// Logs opens an instance's output through its provider.
func (c *Core) Logs(ctx context.Context, ref model.InstanceRef, opts provider.LogOptions) (provider.LogStream, error) {
	p, err := c.provider(ref.Service.Target)
	if err != nil {
		return nil, err
	}
	return p.Logs(ctx, ref, opts)
}

func (c *Core) provider(target string) (provider.Provider, error) {
	p, ok := c.providers[target]
	if !ok {
		return nil, fmt.Errorf("target %q: %w", target, provider.ErrNotFound)
	}
	return p, nil
}

func groupInstances(ins []model.Instance) map[model.ServiceRef][]model.Instance {
	out := map[model.ServiceRef][]model.Instance{}
	for _, in := range ins {
		out[in.Ref.Service] = append(out[in.Ref.Service], in)
	}
	return out
}

func view(s model.Service, ins []model.Instance, stale bool) ServiceView {
	v := ServiceView{Service: s, Stale: stale}
	for _, in := range ins {
		switch in.Status {
		case model.StatusRunning:
			v.Counts.Running++
			if in.Health == model.HealthUnhealthy {
				v.Counts.Unhealthy++
			}
		case model.StatusPending:
			v.Counts.Pending++
		case model.StatusStopped:
			v.Counts.Stopped++
		}
	}
	v.Status = statusOf(s, v.Counts)
	return v
}

// statusOf prefers the provider's own counts and falls back to instances for
// providers with no desired count (Docker).
func statusOf(s model.Service, c Counts) string {
	running := c.Running
	if s.Running != nil {
		running = *s.Running
	}
	if s.Desired != nil {
		switch {
		case *s.Desired == 0:
			return StatusIdle
		case running == 0:
			return StatusDown
		case running < *s.Desired || c.Unhealthy > 0:
			return StatusDegraded
		}
		return StatusHealthy
	}
	switch {
	case running == 0:
		return StatusDown
	case c.Unhealthy > 0:
		return StatusDegraded
	}
	return StatusHealthy
}
