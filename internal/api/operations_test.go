package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/somaz94/fleetbox/internal/auth"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/provider"
)

const (
	restartPath = "/api/v1/services/prod/web/api/restart"
	scalePath   = "/api/v1/services/prod/web/api/scale"
)

// caller is a proxy-mode identity making a same-origin request.
func caller(user, groups string) http.Header {
	h := http.Header{"X-User": {user}, "X-Groups": {groups}, "Origin": {"http://127.0.0.1:8080"}}
	h.Set(auth.CSRFHeader, "1")
	return h
}

var (
	operator = caller("olga", "platform")
	viewer   = caller("vic", "developers")
	stranger = caller("mallory", "contractors")
)

// results lists the audit log as "user action result error", newest first.
func results(t *testing.T, h *harness, hdr http.Header) []string {
	t.Helper()
	var out []string
	for _, e := range decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit", hdr)).Entries {
		out = append(out, strings.TrimSpace(strings.Join([]string{e.User, string(e.Action), string(e.Result), deref(e.Error)}, " ")))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestOperations(t *testing.T) {
	h := newOpsHarness(t, proxyAuth)

	d := decode[ServiceDetail](t, h.doWith(t, "POST", restartPath, operator))
	if d.Service.Ref.Name != "api" || len(d.Instances) != 1 {
		t.Fatalf("detail = %+v", d)
	}
	if rec := h.send(t, "POST", scalePath, operator, `{"desired":3}`); rec.Code != http.StatusOK {
		t.Fatalf("scale = %d: %s", rec.Code, rec.Body)
	}
	if got := strings.Join(h.fp.opCalls, ";"); got != "restart prod/web/api;scale prod/web/api 3" {
		t.Fatalf("calls = %q", got)
	}

	for _, body := range []string{`{"desired":-1}`, `{"desired":1001}`, `{`} {
		wantError(t, h.send(t, "POST", scalePath, operator, body), http.StatusBadRequest, ErrorErrorCodeBadRequest)
	}
	wantError(t, h.doWith(t, "POST", restartPath, viewer), http.StatusForbidden, ErrorErrorCodeForbidden)
	wantError(t, h.doWith(t, "POST", restartPath, stranger), http.StatusNotFound, ErrorErrorCodeNotFound)
	// An unknown target is refused without an audit entry: there is nothing to protect.
	wantError(t, h.doWith(t, "POST", "/api/v1/services/nope/web/api/restart", operator), http.StatusNotFound, ErrorErrorCodeNotFound)

	h.fp.opErr = provider.ErrUnreachable
	wantError(t, h.doWith(t, "POST", restartPath, operator), http.StatusBadGateway, ErrorErrorCodeUnreachable)
	h.fp.opErr = errors.New("dial 192.0.2.1: secret detail")
	e := wantError(t, h.send(t, "POST", scalePath, operator, `{"desired":0}`), http.StatusInternalServerError, ErrorErrorCodeInternal)
	if strings.Contains(e.Error.Message, "secret") {
		t.Fatal("an internal error leaked its text")
	}

	// A failed read after a successful operation answers from the cache.
	h.fp.opErr, h.fp.svcErr = nil, provider.ErrUnreachable
	if d := decode[ServiceDetail](t, h.doWith(t, "POST", restartPath, operator)); d.Service.Ref.Name != "api" || len(d.Instances) != 1 {
		t.Fatalf("detail after a failing read = %+v", d)
	}

	want := []string{
		"olga restart ok",
		"olga scale error internal: internal error",
		"olga restart error unreachable: target unreachable",
		"mallory restart denied not_found: target \"prod\": not found",
		"vic restart denied forbidden: restart needs the operator role on target \"prod\"",
		"olga scale ok",
		"olga restart ok",
	}
	if got := results(t, h, operator); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit:\n%s", strings.Join(got, "\n"))
	}
	page := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit?limit=2", operator))
	if len(page.Entries) != 2 || page.Next == nil {
		t.Fatalf("page = %+v", page)
	}
	next := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit?limit=5&before="+itoa(*page.Next), operator))
	if len(next.Entries) != 5 || next.Next == nil || next.Entries[0].Id >= *page.Next {
		t.Fatalf("next page = %+v", next)
	}
	if last := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit?before="+itoa(next.Entries[4].Id), operator)); len(last.Entries) != 0 || last.Next != nil {
		t.Fatalf("last page = %+v", last)
	}
	scaled := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit?target=prod&limit=6", operator)).Entries[5]
	if scaled.Params == nil || *scaled.Params != `{"desired":3}` || scaled.Subject != nil || scaled.Service.Target != "prod" {
		t.Fatalf("scale entry = %+v", scaled)
	}
	if got := results(t, h, viewer); len(got) != len(want) {
		t.Fatalf("a viewer sees %d entries", len(got))
	}
	if got := decode[AuditPage](t, h.doWith(t, "GET", "/api/v1/audit?target=nope", operator)); len(got.Entries) != 0 {
		t.Fatalf("unknown target = %+v", got)
	}
	for _, q := range []string{"limit=0", "limit=501", "before=0"} {
		wantError(t, h.doWith(t, "GET", "/api/v1/audit?"+q, operator), http.StatusBadRequest, ErrorErrorCodeBadRequest)
	}
}

func TestOperationRefusals(t *testing.T) {
	// Read-only and without the capability: the read-only refusal comes first,
	// and a viewer is refused for the role before the target is considered.
	readonly := newHarnessWith(t, proxyAuth)
	wantError(t, readonly.doWith(t, "POST", restartPath, viewer), http.StatusForbidden, ErrorErrorCodeForbidden)
	wantError(t, readonly.doWith(t, "POST", restartPath, operator), http.StatusForbidden, ErrorErrorCodeForbidden)

	authn, err := auth.New(&config.Config{Auth: proxyAuth}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	incapable := newHarnessTarget(t, authn, target{})
	wantError(t, incapable.send(t, "POST", scalePath, operator, `{"desired":2}`), http.StatusNotImplemented, ErrorErrorCodeUnsupported)

	for _, h := range []*harness{readonly, incapable} {
		if len(h.fp.opCalls) != 0 {
			t.Fatalf("the provider was called: %q", h.fp.opCalls)
		}
	}
	want := []string{`olga restart denied forbidden: target "prod" is read-only`, `vic restart denied forbidden: restart needs the operator role on target "prod"`}
	if got := results(t, readonly, operator); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit = %q", got)
	}
	if got := results(t, incapable, operator); len(got) != 1 || got[0] != "olga scale denied unsupported: the ecs provider cannot scale: not supported by this provider" {
		t.Fatalf("audit = %q", got)
	}
}

func TestOperationWithoutAuth(t *testing.T) {
	h := newOpsHarness(t, config.Auth{})
	if rec := h.do(t, "POST", restartPath); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d: %s", rec.Code, rec.Body)
	}
	if got := results(t, h, nil); len(got) != 1 || got[0] != "anonymous restart ok" {
		t.Fatalf("audit = %q", got)
	}
}

func TestAuditFailClosed(t *testing.T) {
	h := newOpsHarness(t, proxyAuth)
	// The outcome cannot be written once the store goes away mid-operation;
	// the operation has run, so it still answers with the service.
	h.fp.onOp = func() { h.audit.Close() }
	if rec := h.doWith(t, "POST", restartPath, operator); rec.Code != http.StatusOK {
		t.Fatalf("restart = %d: %s", rec.Code, rec.Body)
	}

	h.fp.onOp, h.fp.opCalls = nil, nil
	wantError(t, h.doWith(t, "POST", restartPath, operator), http.StatusInternalServerError, ErrorErrorCodeInternal)
	if len(h.fp.opCalls) != 0 {
		t.Fatal("an operation ran without an audit entry")
	}
	// A refusal is still a refusal when it cannot be recorded.
	wantError(t, h.doWith(t, "POST", restartPath, viewer), http.StatusForbidden, ErrorErrorCodeForbidden)
	wantError(t, h.doWith(t, "GET", "/api/v1/audit", operator), http.StatusInternalServerError, ErrorErrorCodeInternal)
}

// Both operations answer every documented status the same way.
func TestOperationStatuses(t *testing.T) {
	authn, err := auth.New(&config.Config{Auth: proxyAuth}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	ops := newOpsHarness(t, proxyAuth)
	incapable := newHarnessTarget(t, authn, target{})
	for _, path := range []string{restartPath, scalePath} {
		send := func(h *harness, hdr http.Header) *httptest.ResponseRecorder {
			return h.send(t, "POST", path, hdr, `{"desired":1}`)
		}
		wantError(t, send(ops, viewer), http.StatusForbidden, ErrorErrorCodeForbidden)
		wantError(t, send(ops, stranger), http.StatusNotFound, ErrorErrorCodeNotFound)
		wantError(t, send(incapable, operator), http.StatusNotImplemented, ErrorErrorCodeUnsupported)
		for err, want := range map[error]struct {
			status int
			code   ErrorErrorCode
		}{
			provider.ErrUnreachable:  {http.StatusBadGateway, ErrorErrorCodeUnreachable},
			context.DeadlineExceeded: {http.StatusGatewayTimeout, ErrorErrorCodeTimeout},
			errors.New("boom"):       {http.StatusInternalServerError, ErrorErrorCodeInternal},
		} {
			ops.fp.opErr = err
			wantError(t, send(ops, operator), want.status, want.code)
		}
		ops.fp.opErr = nil
	}
}

// A caller who hangs up mid-operation neither aborts the provider call nor
// leaves the entry pending.
func TestOperationOutlivesCaller(t *testing.T) {
	h := newOpsHarness(t, proxyAuth)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.fp.onOp = cancel
	req := httptest.NewRequest("POST", "http://127.0.0.1:8080"+restartPath, nil).WithContext(ctx)
	req.Header = operator.Clone()
	h.h.ServeHTTP(httptest.NewRecorder(), req)
	if got := results(t, h, operator); len(got) != 1 || got[0] != "olga restart ok" {
		t.Fatalf("audit = %q", got)
	}
}

func TestOperationFallbacks(t *testing.T) {
	h := newOpsHarness(t, proxyAuth)
	// A read that finds the service gone drops it from the cache; the answer is
	// the service as it was before the operation.
	h.fp.svcErr = provider.ErrNotFound
	if d := decode[ServiceDetail](t, h.send(t, "POST", scalePath, operator, `{"desired":0}`)); d.Service.Ref.Name != "api" {
		t.Fatalf("detail = %+v", d)
	}
	// A service never seen has nothing to fall back on.
	h.fp.svcErr = nil
	wantError(t, h.doWith(t, "POST", "/api/v1/services/prod/web/other/restart", operator), http.StatusNotFound, ErrorErrorCodeNotFound)
}
