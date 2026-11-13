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
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/legacy"

	"github.com/somaz94/fleetbox/internal/auth"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
	"github.com/somaz94/fleetbox/internal/store"
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
	ops     bool // advertise restart and scale
	opCalls []string
	opErr   error
	onOp    func()
}

func (f *fakeProvider) Kind() string { return "ecs" }
func (f *fakeProvider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Logs: true, FollowLogs: true, Events: true, Restart: f.ops, Scale: f.ops}
}
func (f *fakeProvider) Restart(ctx context.Context, ref model.ServiceRef) error {
	return f.operate(ctx, "restart "+ref.String())
}
func (f *fakeProvider) Scale(ctx context.Context, ref model.ServiceRef, desired int) error {
	return f.operate(ctx, fmt.Sprintf("scale %s %d", ref, desired))
}

// operate fails like a real SDK call when the context is gone by the time the
// request would leave.
func (f *fakeProvider) operate(ctx context.Context, call string) error {
	f.mu.Lock()
	f.opCalls = append(f.opCalls, call)
	hook, err := f.onOp, f.opErr
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
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
	audit  *store.SQLite
	router routers.Router
}

// target describes the harness's one target, "prod".
type target struct {
	readonly bool
	ops      bool
}

// newHarness serves the API without authentication, as on loopback.
func newHarness(t *testing.T) *harness {
	t.Helper()
	return newHarnessWith(t, config.Auth{})
}

// proxyAuth identifies callers by X-User and X-Groups, which lets a test pick
// the caller per request.
var proxyAuth = config.Auth{
	Proxy: &config.Proxy{UserHeader: "X-User", GroupsHeader: "X-Groups"},
	Roles: []config.RoleRule{
		{Group: "platform", Targets: []string{"*"}, Role: config.RoleOperator},
		{Group: "developers", Targets: []string{"prod"}, Role: config.RoleViewer},
	},
}

func newHarnessWith(t *testing.T, a config.Auth) *harness {
	t.Helper()
	return newTargetHarness(t, a, target{readonly: true})
}

func newHarnessAuth(t *testing.T, authn *auth.Service) *harness {
	t.Helper()
	return newHarnessTarget(t, authn, target{readonly: true})
}

// newOpsHarness serves a target that can be operated.
func newOpsHarness(t *testing.T, a config.Auth) *harness {
	t.Helper()
	return newTargetHarness(t, a, target{ops: true})
}

func newTargetHarness(t *testing.T, a config.Auth, tg target) *harness {
	t.Helper()
	authn, err := auth.New(&config.Config{Auth: a}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	return newHarnessTarget(t, authn, tg)
}

func newHarnessTarget(t *testing.T, authn *auth.Service, tg target) *harness {
	t.Helper()
	fp := &fakeProvider{inv: fixture(), ops: tg.ops}
	fp.detail = model.ServiceDetail{Service: fp.inv.Services[0], Instances: fp.inv.Instances}
	stops := &fakeStops{}
	audit, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "audit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { audit.Close() })
	targets := []inventory.Target{{Config: config.Target{Name: "prod", Env: "prod", Readonly: tg.readonly, Interval: time.Minute}, Provider: fp}}
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
	spec.Servers = openapi3.Servers{{URL: "http://127.0.0.1:8080/api/v1"}}
	router, err := legacy.NewRouter(spec)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{h: NewHandler(c, authn, audit, discard), fp: fp, stops: stops, audit: audit, router: router}
}

// do sends a request as a browser on the console would, CSRF header and
// Origin included, and validates the response against the spec.
func (h *harness) do(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	hdr := http.Header{}
	if method != http.MethodGet {
		hdr.Set(auth.CSRFHeader, "1")
		hdr.Set("Origin", "http://127.0.0.1:8080")
	}
	return h.doWith(t, method, target, hdr)
}

// doWith sends a request with exactly the given headers and validates every
// JSON response against the spec, including that the status code is one the
// operation documents.
func (h *harness) doWith(t *testing.T, method, target string, hdr http.Header) *httptest.ResponseRecorder {
	t.Helper()
	return h.send(t, method, target, hdr, "")
}

// send is doWith with a JSON request body.
func (h *harness) send(t *testing.T, method, target string, hdr http.Header, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "http://127.0.0.1:8080"+target, strings.NewReader(body))
	for k, v := range hdr {
		req.Header[k] = v
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

func TestMe(t *testing.T) {
	me := decode[Me](t, newHarness(t).do(t, "GET", "/api/v1/me"))
	if me.User != "anonymous" || me.Mode != MeModeNone || me.Groups == nil || len(me.Targets) != 1 || me.Targets[0].Role != RoleOperator {
		t.Fatalf("me = %+v", me)
	}

	h := newHarnessWith(t, proxyAuth)
	as := func(groups string) Me {
		return decode[Me](t, h.doWith(t, "GET", "/api/v1/me", http.Header{"X-User": {"alice"}, "X-Groups": {groups}}))
	}
	if me := as("developers"); me.User != "alice" || me.Mode != MeModeProxy || len(me.Groups) != 1 ||
		len(me.Targets) != 1 || me.Targets[0] != (TargetRole{Name: "prod", Role: RoleViewer}) {
		t.Fatalf("me = %+v", me)
	}
	if me := as("developers,platform"); me.Targets[0].Role != RoleOperator {
		t.Fatalf("me = %+v", me)
	}
	if me := as(""); me.Targets == nil || len(me.Targets) != 0 {
		t.Fatalf("me = %+v", me)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarnessWith(t, proxyAuth)
	for _, tt := range []struct{ method, path string }{
		{"GET", "/api/v1/info"}, {"GET", "/api/v1/me"}, {"GET", "/api/v1/targets"}, {"GET", "/api/v1/services"},
		{"GET", "/api/v1/services/prod/web/api"}, {"POST", "/api/v1/services/prod/web/api/refresh"},
		{"GET", "/api/v1/services/prod/web/api/events"}, {"GET", "/api/v1/services/prod/web/api/instances/t1/logs"},
		{"POST", "/api/v1/services/prod/web/api/restart"}, {"POST", "/api/v1/services/prod/web/api/scale"},
		{"GET", "/api/v1/audit"}, {"GET", "/api/v1/nope"},
	} {
		wantError(t, h.do(t, tt.method, tt.path), http.StatusUnauthorized, ErrorErrorCodeUnauthorized)
	}
}

func TestHiddenTargets(t *testing.T) {
	h := newHarnessWith(t, proxyAuth)
	h.fp.logs = &provider.SliceStream{Lines: []model.LogLine{{Text: "x"}}}
	stranger := http.Header{"X-User": {"mallory"}, "X-Groups": {"contractors"}}
	if got := decode[[]Target](t, h.doWith(t, "GET", "/api/v1/targets", stranger)); len(got) != 0 {
		t.Fatalf("targets = %+v", got)
	}
	if got := decode[[]Service](t, h.doWith(t, "GET", "/api/v1/services", stranger)); len(got) != 0 {
		t.Fatalf("services = %+v", got)
	}
	// A hidden target reads exactly like an unknown one.
	unknown := wantError(t, h.doWith(t, "GET", "/api/v1/services/nope/web/api", stranger), http.StatusNotFound, ErrorErrorCodeNotFound)
	hidden := wantError(t, h.doWith(t, "GET", "/api/v1/services/prod/web/api", stranger), http.StatusNotFound, ErrorErrorCodeNotFound)
	if strings.ReplaceAll(hidden.Error.Message, "prod", "nope") != unknown.Error.Message {
		t.Fatalf("hidden %q, unknown %q", hidden.Error.Message, unknown.Error.Message)
	}
	post := stranger.Clone()
	post.Set(auth.CSRFHeader, "1")
	post.Set("Origin", "http://127.0.0.1:8080")
	wantError(t, h.doWith(t, "POST", "/api/v1/services/prod/web/api/refresh", post), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.doWith(t, "POST", "/api/v1/services/prod/web/api/restart", post), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.send(t, "POST", "/api/v1/services/prod/web/api/scale", post, `{"desired":1}`), http.StatusNotFound, ErrorErrorCodeNotFound)
	if got := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit", stranger)); len(got.Entries) != 0 {
		t.Fatalf("audit = %+v", got)
	}
	wantError(t, h.doWith(t, "GET", "/api/v1/services/prod/web/api/events", stranger), http.StatusNotFound, ErrorErrorCodeNotFound)
	wantError(t, h.doWith(t, "GET", "/api/v1/services/prod/web/api/instances/t1/logs", stranger), http.StatusNotFound, ErrorErrorCodeNotFound)

	viewer := http.Header{"X-User": {"alice"}, "X-Groups": {"developers"}}
	if got := decode[[]Service](t, h.doWith(t, "GET", "/api/v1/services", viewer)); len(got) != 1 {
		t.Fatalf("services = %+v", got)
	}
	if rec := h.doWith(t, "GET", "/api/v1/services/prod/web/api/instances/t1/logs", viewer); rec.Code != http.StatusOK {
		t.Fatalf("logs = %d", rec.Code)
	}
}

func TestCSRF(t *testing.T) {
	h := newHarness(t)
	path := "/api/v1/services/prod/web/api/refresh"
	wantError(t, h.doWith(t, "POST", path, nil), http.StatusForbidden, ErrorErrorCodeForbidden)
	wantError(t, h.doWith(t, "POST", path, http.Header{auth.CSRFHeader: {"1"}, "Origin": {"https://evil.example.com"}}),
		http.StatusForbidden, ErrorErrorCodeForbidden)
	if rec := h.do(t, "POST", path); rec.Code != http.StatusOK {
		t.Fatalf("same-origin refresh = %d: %s", rec.Code, rec.Body)
	}
	wantError(t, h.doWith(t, "POST", "/api/v1/services/prod/web/api/restart", nil), http.StatusForbidden, ErrorErrorCodeForbidden)
}

func TestSessionStoreFailure(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	cfg := &config.Config{Server: config.Server{PublicURL: "http://127.0.0.1:8080"},
		Auth: config.Auth{OIDC: &config.OIDC{Issuer: "https://sso.example.com", ClientID: "fleetbox", SessionTTL: time.Hour}}}
	authn, err := auth.New(cfg, st, discard)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessAuth(t, authn)
	wantError(t, h.do(t, "GET", "/api/v1/info"), http.StatusUnauthorized, ErrorErrorCodeUnauthorized)
	cookie := http.Header{"Cookie": {auth.SessionCookie + "=token"}}
	e := wantError(t, h.doWith(t, "GET", "/api/v1/info", cookie), http.StatusInternalServerError, ErrorErrorCodeInternal)
	if strings.Contains(e.Error.Message, "closed") {
		t.Fatal("an internal error leaked its text")
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
	h.h.ServeHTTP(nf, httptest.NewRequest("GET", "http://127.0.0.1:8080"+path, nil))
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
