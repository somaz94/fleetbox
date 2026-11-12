package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
)

type stub struct{}

func (stub) Kind() string               { return "stub" }
func (stub) Capabilities() Capabilities { return Capabilities{} }
func (stub) Inventory(context.Context) (model.Inventory, error) {
	return model.Inventory{}, nil
}
func (stub) Service(context.Context, model.ServiceRef) (model.ServiceDetail, error) {
	return model.ServiceDetail{}, ErrNotFound
}
func (stub) Events(context.Context, model.ServiceRef, time.Time) ([]model.Event, error) {
	return nil, nil
}
func (stub) Logs(context.Context, model.InstanceRef, LogOptions) (LogStream, error) {
	return nil, ErrUnsupported
}

func TestRegistry(t *testing.T) {
	Register("stub-ok", func(config.Target) (Provider, error) { return stub{}, nil })
	Register("stub-bad", func(config.Target) (Provider, error) { return nil, errors.New("boom") })

	if p, err := New(config.Target{Name: "a", Provider: "stub-ok"}); err != nil || p.Kind() != "stub" {
		t.Fatalf("New = %v, %v", p, err)
	}
	if _, err := New(config.Target{Name: "a", Provider: "stub-bad"}); err == nil || !strings.Contains(err.Error(), `target "a": boom`) {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(config.Target{Name: "a", Provider: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v", err)
	}
	kinds := Kinds()
	if len(kinds) < 2 || kinds[0] > kinds[len(kinds)-1] {
		t.Fatalf("kinds not sorted: %v", kinds)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("duplicate Register did not panic")
		}
	}()
	Register("stub-ok", func(config.Target) (Provider, error) { return stub{}, nil })
}

func TestSliceStream(t *testing.T) {
	s := &SliceStream{Lines: []model.LogLine{{Text: "a"}, {Text: "b"}}}
	for _, want := range []string{"a", "b"} {
		l, err := s.Recv()
		if err != nil || l.Text != want {
			t.Fatalf("Recv = %v, %v", l, err)
		}
	}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want EOF", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
