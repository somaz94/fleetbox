package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

var (
	t0      = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	apiRef  = model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}
	discard = slog.New(slog.NewTextHandler(io.Discard, nil))
)

type fakeProvider struct {
	mu      sync.Mutex
	inv     model.Inventory
	detail  model.ServiceDetail
	svcErr  error
	events  []model.Event
	evErr   error
	logs    provider.LogStream
	logsErr error
}

func (f *fakeProvider) Kind() string { return "ecs" }
func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Logs: true, FollowLogs: true, Events: true}
}
func (f *fakeProvider) Inventory(context.Context) (model.Inventory, error) { return f.inv, nil }
func (f *fakeProvider) Service(_ context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.svcErr != nil {
		return model.ServiceDetail{}, f.svcErr
	}
	if ref != apiRef {
		return model.ServiceDetail{}, provider.ErrNotFound
	}
	return f.detail, nil
}
func (f *fakeProvider) Events(context.Context, model.ServiceRef, time.Time) ([]model.Event, error) {
	return f.events, f.evErr
}
func (f *fakeProvider) Logs(context.Context, model.InstanceRef, provider.LogOptions) (provider.LogStream, error) {
	return f.logs, f.logsErr
}

type fakeStops struct{ err error }

func (s *fakeStops) Stops(_ context.Context, ref model.ServiceRef, _ int) ([]model.StopRecord, error) {
	if s.err != nil {
		return nil, s.err
	}
	return []model.StopRecord{{Instance: model.InstanceRef{Service: ref, ID: "old"}, StoppedAt: t0, Reason: "OOM", ExitCode: model.Ptr(137)}}, nil
}
func (s *fakeStops) RecordStops(context.Context, []model.StopRecord) (int, error) { return 0, nil }
func (s *fakeStops) PruneStops(context.Context, time.Time) (int, error)           { return 0, nil }

func fixture() model.Inventory {
	started := t0
	return model.Inventory{
		Services: []model.Service{{
			Ref: apiRef, Provider: "ecs", Desired: model.Ptr(2), Running: model.Ptr(1), Pending: model.Ptr(0),
			Revision: "api:7", Rollout: &model.Rollout{State: "IN_PROGRESS", StartedAt: &started},
			Labels: map[string]string{"fleetbox.io/app": "api"},
		}},
		Instances: []model.Instance{{
			Ref: model.InstanceRef{Service: apiRef, ID: "t1"}, Status: model.StatusRunning, Health: model.HealthHealthy,
			Revision: "api:7", StartedAt: &started, Restarts: model.Ptr(0),
			Containers: []model.Container{{Name: "app", Image: "example/api:1", Digest: "sha256:a", Status: "running",
				Health: model.HealthHealthy, ExitCode: model.Ptr(0),
				Ports: []model.Port{{Container: 8080, Host: model.Ptr(80), Protocol: "tcp"}, {Container: 9090}}}},
		}},
	}
}

type harness struct {
	h      http.Handler
	fp     *fakeProvider
	stops  *fakeStops
	router routers.Router
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fp := &fakeProvider{inv: fixture()}
	fp.detail = model.ServiceDetail{Service: fp.inv.Services[0], Instances: fp.inv.Instances}
	stops := &fakeStops{}
	targets := []inventory.Target{{Config: config.Target{Name: "prod", Env: "prod", Readonly: true, Interval: time.Minute}, Provider: fp}}
	cache := inventory.NewCache()
	poller := inventory.NewPoller(targets, cache, stops, discard)
	if err := poller.Refresh(context.Background(), "prod"); err != nil {
		t.Fatal(err)
	}
	c := core.New(targets, cache, poller, stops, core.BuildInfo{Version: "v0.1.0", Commit: "abc", Date: "today"})

	spec, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	spec.Servers = openapi3.Servers{{URL: "http://fleetbox.test/api/v1"}}
	router, err := legacy.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{h: NewHandler(c, discard), fp: fp, stops: stops, router: router}
}

// do sends a request and validates every JSON response against the spec,
// including that the status code is one the operation documents.
func (h *harness) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://fleetbox.test"+target, nil)
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, req)
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		return rec
	}
	route, params, err := h.router.FindRoute(req)
	if err != nil {
		var e Error
		if json.Unmarshal(rec.Body.Bytes(), &e) != nil || e.Error.Code == "" {
			t.Fatalf("%s %s: unrouted response is not an Error: %s", method, target, rec.Body)
		}
		return rec
	}
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route},
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(context.Background(), in); err != nil {
		t.Fatalf("%s %s: response violates the spec: %v\n%s", method, target, err, rec.Body)
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return v
}

func wantError(t *testing.T, rec *httptest.ResponseRecorder, status int, code ErrorErrorCode) Error {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d: %s", rec.Code, status, rec.Body)
	}
	e := decode[Error](t, rec)
	if e.Error.Code != code {
		t.Fatalf("code = %q, want %q", e.Error.Code, code)
	}
	return e
}

func TestInfoAndTargets(t *testing.T) {
	h := newHarness(t)
	info := decode[Info](t, h.do(t, "GET", "/api/v1/info"))
	if info.Version != "v0.1.0" || info.Commit != "abc" {
		t.Fatalf("info = %+v", info)
	}
	targets := decode[[]Target](t, h.do(t, "GET", "/api/v1/targets"))
	if len(targets) != 1 {
		t.Fatalf("targets = %+v", targets)
	}
	tg := targets[0]
	if tg.Name != "prod" || !tg.Readonly || *tg.Env != "prod" || tg.ServiceCount != 1 || tg.InstanceCount != 1 || tg.Stale || tg.Error != nil || !tg.Capabilities.FollowLogs {
		t.Fatalf("target = %+v", tg)
	}
}

func TestListServices(t *testing.T) {
	h := newHarness(t)
	svcs := decode[[]Service](t, h.do(t, "GET", "/api/v1/services"))
	if len(svcs) != 1 {
		t.Fatalf("services = %+v", svcs)
	}
	s := svcs[0]
	if s.Status != ServiceStatusDegraded || *s.Desired != 2 || *s.Revision != "api:7" || s.Rollout.State != "IN_PROGRESS" ||
		(*s.Labels)["fleetbox.io/app"] != "api" || s.Instances.Running != 1 {
		t.Fatalf("service = %+v", s)
	}
	for q, want := range map[string]int{
		"?status=degraded": 1, "?status=healthy": 0, "?target=prod&provider=ecs": 1, "?q=WEB/API": 1, "?q=nothing": 0,
	} {
		if got := decode[[]Service](t, h.do(t, "GET", "/api/v1/services"+q)); len(got) != want {
			t.Fatalf("%s: %d services, want %d", q, len(got), want)
		}
	}
	wantError(t, h.do(t, "GET", "/api/v1/services?status=bogus"), http.StatusBadRequest, ErrorErrorCodeBadRequest)
}

func TestGetService(t *testing.T) {
	h := newHarness(t)
	d := decode[ServiceDetail](t, h.do(t, "GET", "/api/v1/services/prod/web/api"))
	if len(d.Instances) != 1 || len(d.Stops) != 1 || d.Stops[0].InstanceId != "old" || *d.Stops[0].ExitCode != 137 {
		t.Fatalf("detail = %+v", d)
	}
	in := d.Instances[0]
	c := in.Containers[0]
	if in.Id != "t1" || *in.Health != HealthHealthy || in.StoppedAt != nil || *c.Digest != "sha256:a" || *c.ExitCode != 0 ||
		len(*c.Ports) != 2 || *(*c.Ports)[0].Host != 80 || (*c.Ports)[1].Host != nil || (*c.Ports)[1].Protocol != nil {
		t.Fatalf("instance = %+v", in)
	}
	// Absent fields stay absent on the wire rather than appearing as zero values.
	raw := decode[struct {
		Instances []map[string]json.RawMessage `json:"instances"`
	}](t, h.do(t, "GET", "/api/v1/services/prod/web/api"))
	for _, key := range []string{"stoppedAt", "stopReason"} {
		if _, ok := raw.Instances[0][key]; ok {
			t.Fatalf("absent %s was serialised", key)
		}
	}

	wantError(t, h.do(t, "GET", "/api/v1/services/prod/web/nope"), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.do(t, "GET", "/api/v1/services/nope/web/api"), http.StatusNotFound, ErrorErrorCodeNotFound)
	h.stops.err = errors.New("database is locked")
	e := wantError(t, h.do(t, "GET", "/api/v1/services/prod/web/api"), http.StatusInternalServerError, ErrorErrorCodeInternal)
	if strings.Contains(e.Error.Message, "locked") {
		t.Fatal("an internal error leaked its text")
	}
}

func TestRefreshService(t *testing.T) {
	h := newHarness(t)
	if rec := h.do(t, "POST", "/api/v1/services/prod/web/api/refresh"); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// The JSON catch-all answers a wrong method too, so every API error keeps one shape.
	wantError(t, h.do(t, "GET", "/api/v1/services/prod/web/api/refresh"), http.StatusNotFound, ErrorErrorCodeNotFound)
	cases := []struct {
		err    error
		status int
		code   ErrorErrorCode
	}{
		{provider.ErrNotFound, http.StatusNotFound, ErrorErrorCodeNotFound},
		{fmt.Errorf("x: %w", provider.ErrUnreachable), http.StatusBadGateway, ErrorErrorCodeUnreachable},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, ErrorErrorCodeTimeout},
		{errors.New("boom"), http.StatusInternalServerError, ErrorErrorCodeInternal},
	}
	for _, tt := range cases {
		h.fp.mu.Lock()
		h.fp.svcErr = tt.err
		h.fp.mu.Unlock()
		wantError(t, h.do(t, "POST", "/api/v1/services/prod/web/api/refresh"), tt.status, tt.code)
	}
}

func TestListServiceEvents(t *testing.T) {
	h := newHarness(t)
	h.fp.events = []model.Event{
		{Time: t0.Add(time.Minute), Service: apiRef, Instance: "t1", Severity: model.SeverityWarning, Message: "stopped"},
		{Time: t0, Service: apiRef, Severity: model.SeverityInfo, Message: "steady"},
	}
	evs := decode[[]Event](t, h.do(t, "GET", "/api/v1/services/prod/web/api/events?since=2026-01-01T00:00:00Z"))
	if len(evs) != 2 || *evs[0].Instance != "t1" || evs[1].Instance != nil || evs[0].Severity != SeverityWarning {
		t.Fatalf("events = %+v", evs)
	}
	wantError(t, h.do(t, "GET", "/api/v1/services/prod/web/api/events?since=yesterday"), http.StatusBadRequest, ErrorErrorCodeBadRequest)

	cases := []struct {
		err    error
		status int
		code   ErrorErrorCode
	}{
		{provider.ErrNotFound, http.StatusNotFound, ErrorErrorCodeNotFound},
		{provider.ErrUnsupported, http.StatusNotImplemented, ErrorErrorCodeUnsupported},
		{provider.ErrUnreachable, http.StatusBadGateway, ErrorErrorCodeUnreachable},
		{context.DeadlineExceeded, http.StatusGatewayTimeout, ErrorErrorCodeTimeout},
		{errors.New("boom"), http.StatusInternalServerError, ErrorErrorCodeInternal},
	}
	for _, tt := range cases {
		h.fp.evErr = tt.err
		wantError(t, h.do(t, "GET", "/api/v1/services/prod/web/api/events"), tt.status, tt.code)
	}
}

func TestUnknownEndpoint(t *testing.T) {
	h := newHarness(t)
	wantError(t, h.do(t, "GET", "/api/v1/nope"), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.do(t, "GET", "/api/v2/info"), http.StatusNotFound, ErrorErrorCodeNotFound)
}

// sseEvent is one parsed server-sent event.
type sseEvent struct{ name, data string }

func readEvents(t *testing.T, r io.Reader) ([]sseEvent, int) {
	t.Helper()
	var (
		out   []sseEvent
		cur   sseEvent
		pings int
	)
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": ping":
			pings++
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "" && cur.name != "":
			out = append(out, cur)
			cur = sseEvent{}
		}
	}
	return out, pings
}

func TestStreamLogs(t *testing.T) {
	h := newHarness(t)
	lineSchema := mustSchema(t, "LogLine")
	errSchema := mustSchema(t, "Error")
	ts := t0
	path := "/api/v1/services/prod/web/api/instances/t1/logs"

	h.fp.logs = &provider.SliceStream{Lines: []model.LogLine{{Time: &ts, Stream: "stderr", Text: "boom"}, {Text: "plain"}}}
	rec := h.do(t, "GET", path+"?tail=10&since=2026-01-01T00:00:00Z&follow=false&container=app")
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status = %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	evs, _ := readEvents(t, rec.Body)
	if len(evs) != 3 || evs[0].name != "line" || evs[2].name != "end" {
		t.Fatalf("events = %+v", evs)
	}
	for _, e := range evs[:2] {
		validateJSON(t, lineSchema, e.data)
	}
	if !strings.Contains(evs[0].data, `"stream":"stderr"`) || strings.Contains(evs[1].data, "stream") || strings.Contains(evs[1].data, "time") {
		t.Fatalf("lines = %+v", evs[:2])
	}

	h.fp.logs = &failingStream{lines: []model.LogLine{{Text: "first"}}, err: errors.New("connection reset")}
	evs, _ = readEvents(t, h.do(t, "GET", path).Body)
	if len(evs) != 2 || evs[1].name != "error" {
		t.Fatalf("events = %+v", evs)
	}
	validateJSON(t, errSchema, evs[1].data)

	for _, q := range []string{"?tail=0", "?tail=5001", "?tail=x", "?since=yesterday", "?follow=maybe"} {
		wantError(t, h.do(t, "GET", path+q), http.StatusBadRequest, ErrorErrorCodeBadRequest)
	}
	h.fp.logsErr = provider.ErrUnsupported
	wantError(t, h.do(t, "GET", path), http.StatusNotImplemented, ErrorErrorCodeUnsupported)
	h.fp.logsErr = provider.ErrNotFound
	wantError(t, h.do(t, "GET", path), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.do(t, "GET", "/api/v1/services/nope/web/api/instances/t1/logs"), http.StatusNotFound, ErrorErrorCodeNotFound)

	// A writer that cannot flush cannot stream.
	nf := &noFlush{header: http.Header{}}
	h.h.ServeHTTP(nf, httptest.NewRequest("GET", path, nil))
	if nf.status != http.StatusInternalServerError {
		t.Fatalf("status = %d", nf.status)
	}
}

func TestStreamLogsFollow(t *testing.T) {
	old := heartbeatInterval
	heartbeatInterval = 5 * time.Millisecond
	t.Cleanup(func() { heartbeatInterval = old })

	h := newHarness(t)
	blocking := &blockingStream{first: model.LogLine{Text: "hello"}, closed: make(chan struct{})}
	h.fp.logs = blocking
	srv := httptest.NewServer(h.h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/v1/services/prod/web/api/instances/t1/logs?follow=true", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(resp.Body)
	sawLine, pings := false, 0
	for sc.Scan() && pings < 2 {
		switch line := sc.Text(); {
		case strings.HasPrefix(line, "data: ") && strings.Contains(line, "hello"):
			sawLine = true
		case line == ": ping":
			pings++
		}
	}
	if !sawLine || pings < 2 {
		t.Fatalf("line seen %v, pings %d", sawLine, pings)
	}
	cancel()
	resp.Body.Close()
	select {
	case <-blocking.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("stream not closed after the client left")
	}
}

type failingStream struct {
	lines []model.LogLine
	err   error
}

func (s *failingStream) Recv() (model.LogLine, error) {
	if len(s.lines) == 0 {
		return model.LogLine{}, s.err
	}
	l := s.lines[0]
	s.lines = s.lines[1:]
	return l, nil
}
func (s *failingStream) Close() error { return nil }

// blockingStream yields one line, then blocks like a followed stream with no
// new output until it is closed.
type blockingStream struct {
	first  model.LogLine
	sent   bool
	closed chan struct{}
	once   sync.Once
}

func (s *blockingStream) Recv() (model.LogLine, error) {
	if !s.sent {
		s.sent = true
		return s.first, nil
	}
	<-s.closed
	return model.LogLine{}, context.Canceled
}
func (s *blockingStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

type noFlush struct {
	header http.Header
	status int
}

func (w *noFlush) Header() http.Header         { return w.header }
func (w *noFlush) Write(b []byte) (int, error) { return len(b), nil }
func (w *noFlush) WriteHeader(s int)           { w.status = s }

func mustSchema(t *testing.T, name string) *openapi3.Schema {
	t.Helper()
	spec, err := GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	ref, ok := spec.Components.Schemas[name]
	if !ok {
		t.Fatalf("schema %s missing", name)
	}
	return ref.Value
}

func validateJSON(t *testing.T, s *openapi3.Schema, data string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatalf("not JSON: %s", data)
	}
	if err := s.VisitJSON(v); err != nil {
		t.Fatalf("%s violates the schema: %v", data, err)
	}
}
