// Package provider defines the contract every platform adapter implements and
// the registry that builds adapters from configuration.
//
// A provider instance is bound to one target. Adapters register a Factory under
// their kind in init(); the server imports them for that side effect only.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
)

// Errors the rest of the system branches on. Adapters wrap their own errors
// with these so "target unreachable" stays distinct from "not found".
var (
	ErrNotFound    = errors.New("not found")
	ErrUnreachable = errors.New("target unreachable")
	ErrUnsupported = errors.New("not supported by this provider")
)

// Capabilities says which optional features an adapter offers. The console
// does not render a control for a missing capability, and the API refuses it.
type Capabilities struct {
	Logs       bool `json:"logs"`
	FollowLogs bool `json:"followLogs"`
	Events     bool `json:"events"`
	Restart    bool `json:"restart"`
	Scale      bool `json:"scale"`
}

// LogOptions selects which part of an instance's output to read.
type LogOptions struct {
	Container string    // empty means the instance's first container with logs
	Since     time.Time // zero means no lower bound
	Tail      int       // 0 means the adapter's default
	Follow    bool
}

// LogStream yields lines until io.EOF (or, when following, until its context ends).
type LogStream interface {
	Recv() (model.LogLine, error)
	Close() error
}

// Provider is the read contract. Every method must honour context cancellation
// and return output sorted as documented, so callers never re-sort.
type Provider interface {
	Kind() string
	Capabilities() Capabilities
	// Inventory returns every service and instance of the target, services
	// sorted by ref and instances sorted by ref.
	Inventory(ctx context.Context) (model.Inventory, error)
	// Service reads one service fresh, bypassing any cache.
	Service(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error)
	// Events returns the service's timeline since the given time, newest first.
	Events(ctx context.Context, ref model.ServiceRef, since time.Time) ([]model.Event, error)
	Logs(ctx context.Context, ref model.InstanceRef, opts LogOptions) (LogStream, error)
}

// Operator is implemented by adapters that can change what runs.
type Operator interface {
	Restart(ctx context.Context, ref model.ServiceRef) error
	Scale(ctx context.Context, ref model.ServiceRef, desired int) error
}

// Watcher is implemented by adapters that can push change notifications. Each
// value received means "something changed, refresh"; the channel closes when
// the watch ends.
type Watcher interface {
	Watch(ctx context.Context) (<-chan struct{}, error)
}

// Describer is optional: what the target points at, for the console. It must
// never include a credential or the path to one.
type Describer interface {
	Endpoint() []model.Attribute
}

// Factory builds an adapter for one target.
type Factory func(t config.Target) (Provider, error)

var (
	mu       sync.RWMutex
	registry = map[string]Factory{}
)

// Register makes a provider kind available. It panics on a duplicate, which
// can only be a programming error.
func Register(kind string, f Factory) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("provider %q registered twice", kind))
	}
	registry[kind] = f
}

// New builds the adapter for t.
func New(t config.Target) (Provider, error) {
	mu.RLock()
	f, ok := registry[t.Provider]
	mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("target %q: unknown provider %q (known: %v)", t.Name, t.Provider, Kinds())
	}
	p, err := f(t)
	if err != nil {
		return nil, fmt.Errorf("target %q: %w", t.Name, err)
	}
	return p, nil
}

// Kinds lists the registered provider kinds, sorted.
func Kinds() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// SliceStream is a LogStream over lines already in memory.
type SliceStream struct {
	Lines []model.LogLine
	next  int
}

func (s *SliceStream) Recv() (model.LogLine, error) {
	if s.next >= len(s.Lines) {
		return model.LogLine{}, io.EOF
	}
	s.next++
	return s.Lines[s.next-1], nil
}

func (s *SliceStream) Close() error { return nil }
