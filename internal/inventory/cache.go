// Package inventory keeps the latest view of every target in memory and the
// poller that refreshes it. Reads are served from here and never wait on a
// provider.
package inventory

import (
	"sort"
	"sync"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

// TargetState is one target as of its last refresh. A failed refresh keeps
// the previous inventory and records the error, so a target that goes away
// shows stale data with a reason instead of disappearing.
type TargetState struct {
	Name         string                `json:"name"`
	Provider     string                `json:"provider"`
	Env          string                `json:"env,omitempty"`
	Readonly     bool                  `json:"readonly"`
	Capabilities provider.Capabilities `json:"capabilities"`
	Endpoint     []model.Attribute     `json:"endpoint,omitempty"`
	RefreshedAt  *time.Time            `json:"refreshedAt,omitempty"`
	AttemptedAt  *time.Time            `json:"attemptedAt,omitempty"`
	Error        string                `json:"error,omitempty"`
	Inventory    model.Inventory       `json:"-"`
}

// Stale reports whether the last refresh attempt failed.
func (s TargetState) Stale() bool { return s.Error != "" }

// Cache holds one TargetState per target. Stored slices are never mutated in
// place, only replaced, so the shallow copies handed to readers stay valid.
type Cache struct {
	mu      sync.RWMutex
	targets map[string]*TargetState
}

func NewCache() *Cache { return &Cache{targets: map[string]*TargetState{}} }

// add registers a target before its first refresh.
func (c *Cache) add(s TargetState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.targets[s.Name] = &s
}

// Targets returns every target, sorted by name.
func (c *Cache) Targets() []TargetState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]TargetState, 0, len(c.targets))
	for _, s := range c.targets {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (c *Cache) Target(name string) (TargetState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	s, ok := c.targets[name]
	if !ok {
		return TargetState{}, false
	}
	return *s, true
}

// Service returns one service and its instances from the cache.
func (c *Cache) Service(ref model.ServiceRef) (model.ServiceDetail, bool) {
	s, ok := c.Target(ref.Target)
	if !ok {
		return model.ServiceDetail{}, false
	}
	for _, svc := range s.Inventory.Services {
		if svc.Ref == ref {
			d := model.ServiceDetail{Service: svc, Instances: []model.Instance{}}
			for _, in := range s.Inventory.Instances {
				if in.Ref.Service == ref {
					d.Instances = append(d.Instances, in)
				}
			}
			return d, true
		}
	}
	return model.ServiceDetail{}, false
}

func (c *Cache) setSuccess(name string, inv model.Inventory, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.targets[name]; ok {
		s.Inventory, s.Error = inv, ""
		s.RefreshedAt, s.AttemptedAt = &at, &at
	}
}

func (c *Cache) setFailure(name string, err error, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.targets[name]; ok {
		s.Error, s.AttemptedAt = err.Error(), &at
	}
}

// replaceService swaps one service's entry and instances for a fresh read,
// leaving the rest of the target untouched.
func (c *Cache) replaceService(d model.ServiceDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.targets[d.Service.Ref.Target]
	if !ok {
		return
	}
	ref := d.Service.Ref
	inv := model.Inventory{Services: make([]model.Service, 0, len(s.Inventory.Services)+1)}
	replaced := false
	for _, svc := range s.Inventory.Services {
		if svc.Ref == ref {
			svc, replaced = d.Service, true
		}
		inv.Services = append(inv.Services, svc)
	}
	if !replaced {
		inv.Services = append(inv.Services, d.Service)
		sort.Slice(inv.Services, func(i, j int) bool { return inv.Services[i].Ref.String() < inv.Services[j].Ref.String() })
	}
	for _, in := range s.Inventory.Instances {
		if in.Ref.Service != ref {
			inv.Instances = append(inv.Instances, in)
		}
	}
	inv.Instances = append(inv.Instances, d.Instances...)
	sort.Slice(inv.Instances, func(i, j int) bool { return inv.Instances[i].Ref.String() < inv.Instances[j].Ref.String() })
	s.Inventory = inv
}

// removeService drops a service the provider no longer has.
func (c *Cache) removeService(ref model.ServiceRef) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.targets[ref.Target]
	if !ok {
		return
	}
	inv := model.Inventory{Services: []model.Service{}, Instances: []model.Instance{}}
	for _, svc := range s.Inventory.Services {
		if svc.Ref != ref {
			inv.Services = append(inv.Services, svc)
		}
	}
	for _, in := range s.Inventory.Instances {
		if in.Ref.Service != ref {
			inv.Instances = append(inv.Instances, in)
		}
	}
	s.Inventory = inv
}
