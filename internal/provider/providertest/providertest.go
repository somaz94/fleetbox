// Package providertest is the conformance suite every provider adapter must
// pass, run against the adapter's fake SDK client. It pins the guarantees the
// cache, the API and the console rely on, so a new provider cannot quietly
// break them.
package providertest

import (
	"context"
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

// Fixture tells the suite what the adapter's fake holds.
type Fixture struct {
	Target string
	// Known is a service the fake holds, with at least one instance.
	Known model.ServiceRef
	// KnownInstance is an instance of Known whose logs the fake can serve.
	KnownInstance model.InstanceRef
	// Missing is a service the fake does not hold.
	Missing model.ServiceRef
}

// Run executes the suite. newProvider must return a fresh adapter on each call.
func Run(t *testing.T, newProvider func(t *testing.T) provider.Provider, fx Fixture) {
	t.Helper()

	t.Run("kind", func(t *testing.T) {
		if newProvider(t).Kind() == "" {
			t.Fatal("Kind is empty")
		}
	})

	t.Run("inventory", func(t *testing.T) {
		inv, err := newProvider(t).Inventory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		checkInventory(t, fx, inv)
	})

	t.Run("inventory honours cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := newProvider(t).Inventory(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})

	t.Run("service", func(t *testing.T) {
		d, err := newProvider(t).Service(context.Background(), fx.Known)
		if err != nil {
			t.Fatal(err)
		}
		if d.Service.Ref != fx.Known {
			t.Fatalf("ref = %v, want %v", d.Service.Ref, fx.Known)
		}
		if len(d.Instances) == 0 {
			t.Fatal("known service has no instances")
		}
		for _, in := range d.Instances {
			if in.Ref.Service != fx.Known {
				t.Fatalf("instance %v belongs to another service", in.Ref)
			}
		}
		checkSortedInstances(t, d.Instances)
	})

	t.Run("missing service is ErrNotFound", func(t *testing.T) {
		_, err := newProvider(t).Service(context.Background(), fx.Missing)
		if !errors.Is(err, provider.ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("events", func(t *testing.T) {
		p := newProvider(t)
		evs, err := p.Events(context.Background(), fx.Known, time.Time{})
		if !p.Capabilities().Events {
			if !errors.Is(err, provider.ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		for i, e := range evs {
			if e.Service != fx.Known {
				t.Fatalf("event %d belongs to %v", i, e.Service)
			}
			if e.Severity != model.SeverityInfo && e.Severity != model.SeverityWarning {
				t.Fatalf("event %d severity %q", i, e.Severity)
			}
			if i > 0 && e.Time.After(evs[i-1].Time) {
				t.Fatalf("events not newest first at %d", i)
			}
		}
	})

	t.Run("logs", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Logs(context.Background(), fx.KnownInstance, provider.LogOptions{Tail: 10})
		if !p.Capabilities().Logs {
			if !errors.Is(err, provider.ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for i := 0; ; i++ {
			if i > 10000 {
				t.Fatal("non-follow stream did not end")
			}
			if _, err := s.Recv(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
		}
	})

	t.Run("operations", func(t *testing.T) {
		p := newProvider(t)
		caps := p.Capabilities()
		op, ok := p.(provider.Operator)
		if !ok {
			if caps.Restart || caps.Scale {
				t.Fatal("restart or scale advertised without implementing provider.Operator")
			}
			return
		}
		checkOperation(t, fx, "restart", caps.Restart, op.Restart)
		checkOperation(t, fx, "scale", caps.Scale, func(ctx context.Context, ref model.ServiceRef) error {
			return op.Scale(ctx, ref, 1)
		})
	})
}

// checkOperation pins the error kinds the API maps to 501, 404 and a cancelled
// request. A provider may meet them in a read before its mutating call, so the
// call itself is covered by the provider's own tests.
func checkOperation(t *testing.T, fx Fixture, name string, capable bool, run func(context.Context, model.ServiceRef) error) {
	t.Helper()
	ctx := context.Background()
	if !capable {
		if err := run(ctx, fx.Known); !errors.Is(err, provider.ErrUnsupported) {
			t.Fatalf("%s without the capability: err = %v, want ErrUnsupported", name, err)
		}
		return
	}
	if err := run(ctx, fx.Known); err != nil {
		t.Fatalf("%s %s: %v", name, fx.Known, err)
	}
	if err := run(ctx, fx.Missing); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("%s %s: err = %v, want ErrNotFound", name, fx.Missing, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := run(cancelled, fx.Known); !errors.Is(err, context.Canceled) {
		t.Fatalf("%s with a cancelled context: err = %v, want context.Canceled", name, err)
	}
}

func checkInventory(t *testing.T, fx Fixture, inv model.Inventory) {
	t.Helper()
	services := map[model.ServiceRef]bool{}
	for i, s := range inv.Services {
		if s.Ref.Target != fx.Target || s.Ref.Scope == "" || s.Ref.Name == "" {
			t.Fatalf("service %d has an incomplete ref %v", i, s.Ref)
		}
		if s.Provider == "" {
			t.Fatalf("service %v has no provider", s.Ref)
		}
		if services[s.Ref] {
			t.Fatalf("duplicate service %v", s.Ref)
		}
		services[s.Ref] = true
	}
	if !sort.SliceIsSorted(inv.Services, func(i, j int) bool {
		return inv.Services[i].Ref.String() < inv.Services[j].Ref.String()
	}) {
		t.Fatal("services not sorted by ref")
	}
	if !services[fx.Known] {
		t.Fatalf("known service %v missing from inventory", fx.Known)
	}
	ids := map[model.InstanceRef]bool{}
	for _, in := range inv.Instances {
		if !services[in.Ref.Service] {
			t.Fatalf("instance %v belongs to no listed service", in.Ref)
		}
		if in.Ref.ID == "" || ids[in.Ref] {
			t.Fatalf("instance id empty or duplicate: %v", in.Ref)
		}
		ids[in.Ref] = true
		switch in.Status {
		case model.StatusRunning, model.StatusPending, model.StatusStopped, model.StatusUnknown:
		default:
			t.Fatalf("instance %v has non-normalised status %q", in.Ref, in.Status)
		}
		for _, c := range in.Containers {
			if c.ImageCreated != nil && c.ImageCreated.IsZero() {
				t.Fatalf("instance %v container %q reports a zero image time instead of none", in.Ref, c.Name)
			}
		}
	}
	checkSortedInstances(t, inv.Instances)
}

func checkSortedInstances(t *testing.T, ins []model.Instance) {
	t.Helper()
	if !sort.SliceIsSorted(ins, func(i, j int) bool { return ins[i].Ref.String() < ins[j].Ref.String() }) {
		t.Fatal("instances not sorted by ref")
	}
}
