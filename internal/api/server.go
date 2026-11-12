// Package api serves /api/v1. api.gen.go is generated from api/openapi.yaml
// (`make generate`); this file implements the generated strict interface on
// top of core, and logs.go serves the one streaming endpoint by hand.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/somaz94/fleetbox/internal/auth"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
	"github.com/somaz94/fleetbox/internal/store"
)

const (
	basePath     = "/api/v1"
	maxDesired   = 1000
	defaultAudit = 100
	maxAudit     = 500

	// An operation outlives a caller who hangs up, since the provider may
	// already have applied it, but not forever.
	operationTimeout = 5 * time.Minute
	auditTimeout     = 5 * time.Second
)

// forbidden refuses an operation the caller's role or the target does not allow.
type forbidden string

func (f forbidden) Error() string { return string(f) }

// AuditLog is the part of the store operations are recorded in.
type AuditLog interface {
	AppendAudit(ctx context.Context, e store.AuditEntry) (int64, error)
	FinishAudit(ctx context.Context, id int64, result, errText string) error
	Audit(ctx context.Context, q store.AuditQuery) ([]store.AuditEntry, error)
}

// Server implements StrictServerInterface.
type Server struct {
	core  *core.Core
	auth  *auth.Service
	audit AuditLog
	log   *slog.Logger
}

var _ StrictServerInterface = (*Server)(nil)

// NewHandler returns the /api/v1 handler. Errors of every kind, including
// unknown routes, malformed parameters and a missing identity, answer with the
// Error schema.
func NewHandler(c *core.Core, a *auth.Service, audit AuditLog, log *slog.Logger) http.Handler {
	s := &Server{core: c, auth: a, audit: audit, log: log}
	mux := http.NewServeMux()
	badRequest := func(w http.ResponseWriter, _ *http.Request, err error) {
		writeError(w, http.StatusBadRequest, ErrorErrorCodeBadRequest, err.Error())
	}
	strict := NewStrictHandlerWithOptions(s, nil, StrictHTTPServerOptions{
		RequestErrorHandlerFunc: badRequest,
		ResponseErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Error("api response failed", "path", r.URL.Path, "err", err)
			writeError(w, http.StatusInternalServerError, ErrorErrorCodeInternal, "internal error")
		},
	})
	HandlerWithOptions(strict, StdHTTPServerOptions{BaseURL: basePath, BaseRouter: mux, ErrorHandlerFunc: badRequest})
	mux.HandleFunc("GET "+basePath+"/services/{target}/{scope}/{service}/instances/{instance}/logs", s.streamLogs)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusNotFound, ErrorErrorCodeNotFound, "no such endpoint")
	})
	return s.guard(mux)
}

// guard identifies the caller of every request and refuses a state-changing
// one that fails the CSRF check.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := s.auth.Authenticate(r)
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeError(w, http.StatusUnauthorized, ErrorErrorCodeUnauthorized, err.Error())
			return
		}
		if err != nil {
			s.log.Error("authenticate", "err", err)
			writeError(w, http.StatusInternalServerError, ErrorErrorCodeInternal, "internal error")
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			if err := s.auth.CheckCSRF(r); err != nil {
				writeError(w, http.StatusForbidden, ErrorErrorCodeForbidden, err.Error())
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
	})
}

// visible reports whether the caller holds any role on target.
func (s *Server) visible(ctx context.Context, target string) bool {
	p, _ := auth.FromContext(ctx)
	return s.auth.Role(p, target) != ""
}

// hide answers for a target the caller may not see exactly as for one that
// does not exist, so names cannot be probed.
func (s *Server) hide(ctx context.Context, target string) error {
	if s.visible(ctx, target) {
		return nil
	}
	return fmt.Errorf("target %q: %w", target, provider.ErrNotFound)
}

func (s *Server) GetInfo(context.Context, GetInfoRequestObject) (GetInfoResponseObject, error) {
	b := s.core.Build()
	return GetInfo200JSONResponse{Version: b.Version, Commit: b.Commit, BuildDate: b.Date}, nil
}

func (s *Server) GetMe(ctx context.Context, _ GetMeRequestObject) (GetMeResponseObject, error) {
	p, _ := auth.FromContext(ctx)
	groups := p.Groups
	if groups == nil {
		groups = []string{}
	}
	out := GetMe200JSONResponse{User: p.User, Groups: groups, Mode: MeMode(s.auth.Mode()), Targets: []TargetRole{}}
	for _, t := range s.core.Targets() {
		if r := s.auth.Role(p, t.Name); r != "" {
			out.Targets = append(out.Targets, TargetRole{Name: t.Name, Role: Role(r)})
		}
	}
	return out, nil
}

func (s *Server) ListTargets(ctx context.Context, _ ListTargetsRequestObject) (ListTargetsResponseObject, error) {
	out := ListTargets200JSONResponse{}
	for _, t := range s.core.Targets() {
		if s.visible(ctx, t.Name) {
			out = append(out, toTarget(t))
		}
	}
	return out, nil
}

func (s *Server) ListServices(ctx context.Context, req ListServicesRequestObject) (ListServicesResponseObject, error) {
	p := req.Params
	if p.Status != nil && !p.Status.Valid() {
		return ListServices400JSONResponse(errorBody(ErrorErrorCodeBadRequest, "unknown status "+string(*p.Status))), nil
	}
	f := core.Filter{Target: deref(p.Target), Provider: deref(p.Provider), Query: deref(p.Q)}
	if p.Status != nil {
		f.Status = string(*p.Status)
	}
	out := ListServices200JSONResponse{}
	for _, v := range s.core.Services(f) {
		if s.visible(ctx, v.Ref.Target) {
			out = append(out, toService(v))
		}
	}
	return out, nil
}

func (s *Server) GetService(ctx context.Context, req GetServiceRequestObject) (GetServiceResponseObject, error) {
	var d core.DetailView
	err := s.hide(ctx, req.Target)
	if err == nil {
		d, err = s.core.Service(ctx, model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service})
	}
	if err != nil {
		e, status := s.classify(err)
		if status == http.StatusNotFound {
			return GetService404JSONResponse(e), nil
		}
		return GetService500JSONResponse(e), nil
	}
	return GetService200JSONResponse(toDetail(d)), nil
}

func (s *Server) RefreshService(ctx context.Context, req RefreshServiceRequestObject) (RefreshServiceResponseObject, error) {
	var d core.DetailView
	err := s.hide(ctx, req.Target)
	if err == nil {
		d, err = s.core.Refresh(ctx, model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service})
	}
	if err != nil {
		e, status := s.classify(err)
		switch status {
		case http.StatusNotFound:
			return RefreshService404JSONResponse(e), nil
		case http.StatusBadGateway:
			return RefreshService502JSONResponse(e), nil
		case http.StatusGatewayTimeout:
			return RefreshService504JSONResponse(e), nil
		}
		return RefreshService500JSONResponse(e), nil
	}
	return RefreshService200JSONResponse(toDetail(d)), nil
}

func (s *Server) ListServiceEvents(ctx context.Context, req ListServiceEventsRequestObject) (ListServiceEventsResponseObject, error) {
	ref := model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service}
	var since time.Time
	if req.Params.Since != nil {
		since = *req.Params.Since
	}
	var evs []model.Event
	err := s.hide(ctx, req.Target)
	if err == nil {
		evs, err = s.core.Events(ctx, ref, since)
	}
	if err != nil {
		e, status := s.classify(err)
		switch status {
		case http.StatusNotFound:
			return ListServiceEvents404JSONResponse(e), nil
		case http.StatusNotImplemented:
			return ListServiceEvents501JSONResponse(e), nil
		case http.StatusBadGateway:
			return ListServiceEvents502JSONResponse(e), nil
		case http.StatusGatewayTimeout:
			return ListServiceEvents504JSONResponse(e), nil
		}
		return ListServiceEvents500JSONResponse(e), nil
	}
	out := ListServiceEvents200JSONResponse{}
	for _, ev := range evs {
		out = append(out, Event{Time: ev.Time, Instance: ptr(ev.Instance), Severity: Severity(ev.Severity), Message: ev.Message})
	}
	return out, nil
}

// operation is one audited request.
type operation struct {
	action  string
	params  string
	allowed func(provider.Capabilities) bool
	run     func(ctx context.Context) error
}

// operate checks an operation request and runs it. A hidden target is refused
// before anything else so it answers exactly like a missing one. Every request
// on a configured target is audited, and the entry is written before the
// provider is called: an operation that cannot be audited is not performed.
func (s *Server) operate(ctx context.Context, ref model.ServiceRef, op operation) (core.DetailView, error) {
	t, ok := s.core.Target(ref.Target)
	if !ok {
		return core.DetailView{}, fmt.Errorf("target %q: %w", ref.Target, provider.ErrNotFound)
	}
	p, _ := auth.FromContext(ctx)
	entry := store.AuditEntry{Time: time.Now(), User: p.User, Subject: p.Subject, Service: ref, Action: op.action, Params: op.params}
	var refusal error
	switch role := s.auth.Role(p, ref.Target); {
	case role == "":
		refusal = fmt.Errorf("target %q: %w", ref.Target, provider.ErrNotFound)
	case role != config.RoleOperator:
		refusal = forbidden(fmt.Sprintf("%s needs the operator role on target %q", op.action, ref.Target))
	case t.Readonly:
		refusal = forbidden(fmt.Sprintf("target %q is read-only", ref.Target))
	case !op.allowed(t.Capabilities):
		refusal = fmt.Errorf("the %s provider cannot %s: %w", t.Provider, op.action, provider.ErrUnsupported)
	}
	if refusal != nil {
		entry.Result, entry.Error = store.ResultDenied, auditText(refusal)
		if _, err := s.audit.AppendAudit(ctx, entry); err != nil {
			s.log.Error("audit a refused operation", "service", ref.String(), "action", op.action, "err", err)
		}
		return core.DetailView{}, refusal
	}

	prior, priorErr := s.core.Service(ctx, ref)
	entry.Result = store.ResultPending
	id, err := s.audit.AppendAudit(ctx, entry)
	if err != nil {
		return core.DetailView{}, fmt.Errorf("audit before %s: %w", op.action, err)
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operationTimeout)
	runErr := op.run(runCtx)
	cancel()
	result, text := store.ResultOK, ""
	if runErr != nil {
		result, text = store.ResultError, auditText(runErr)
	}
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditTimeout)
	if err := s.audit.FinishAudit(auditCtx, id, result, text); err != nil {
		s.log.Error("audit the outcome of an operation", "id", id, "result", result, "err", err)
	}
	cancel()
	if runErr != nil {
		return core.DetailView{}, runErr
	}
	// The operation went through, so it never answers as a failure: a failed
	// read falls back to the cache, then to the service as it was before.
	d, err := s.core.Refresh(ctx, ref)
	if err == nil {
		return d, nil
	}
	s.log.Warn("read after operation", "service", ref.String(), "err", err)
	if d, err := s.core.Service(ctx, ref); err == nil {
		return d, nil
	}
	if priorErr != nil {
		return core.DetailView{}, priorErr
	}
	return prior, nil
}

// auditText is what the audit log keeps of an error: its code and the message
// a caller would see, never an internal error's text.
func auditText(err error) string {
	e, _ := errorFor(err)
	return string(e.Error.Code) + ": " + e.Error.Message
}

func (s *Server) RestartService(ctx context.Context, req RestartServiceRequestObject) (RestartServiceResponseObject, error) {
	ref := model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service}
	d, err := s.operate(ctx, ref, operation{
		action:  "restart",
		allowed: func(c provider.Capabilities) bool { return c.Restart },
		run:     func(ctx context.Context) error { return s.core.Restart(ctx, ref) },
	})
	if err != nil {
		e, status := s.classify(err)
		switch status {
		case http.StatusForbidden:
			return RestartService403JSONResponse(e), nil
		case http.StatusNotFound:
			return RestartService404JSONResponse(e), nil
		case http.StatusNotImplemented:
			return RestartService501JSONResponse(e), nil
		case http.StatusBadGateway:
			return RestartService502JSONResponse(e), nil
		case http.StatusGatewayTimeout:
			return RestartService504JSONResponse(e), nil
		}
		return RestartService500JSONResponse(e), nil
	}
	return RestartService200JSONResponse(toDetail(d)), nil
}

func (s *Server) ScaleService(ctx context.Context, req ScaleServiceRequestObject) (ScaleServiceResponseObject, error) {
	if req.Body == nil || req.Body.Desired < 0 || req.Body.Desired > maxDesired {
		return ScaleService400JSONResponse(errorBody(ErrorErrorCodeBadRequest, fmt.Sprintf("desired must be between 0 and %d", maxDesired))), nil
	}
	ref := model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service}
	desired := req.Body.Desired
	d, err := s.operate(ctx, ref, operation{
		action:  "scale",
		params:  fmt.Sprintf(`{"desired":%d}`, desired),
		allowed: func(c provider.Capabilities) bool { return c.Scale },
		run:     func(ctx context.Context) error { return s.core.Scale(ctx, ref, desired) },
	})
	if err != nil {
		e, status := s.classify(err)
		switch status {
		case http.StatusForbidden:
			return ScaleService403JSONResponse(e), nil
		case http.StatusNotFound:
			return ScaleService404JSONResponse(e), nil
		case http.StatusNotImplemented:
			return ScaleService501JSONResponse(e), nil
		case http.StatusBadGateway:
			return ScaleService502JSONResponse(e), nil
		case http.StatusGatewayTimeout:
			return ScaleService504JSONResponse(e), nil
		}
		return ScaleService500JSONResponse(e), nil
	}
	return ScaleService200JSONResponse(toDetail(d)), nil
}

func (s *Server) ListAudit(ctx context.Context, req ListAuditRequestObject) (ListAuditResponseObject, error) {
	q := store.AuditQuery{Limit: defaultAudit}
	if l := req.Params.Limit; l != nil {
		if *l < 1 || *l > maxAudit {
			return ListAudit400JSONResponse(errorBody(ErrorErrorCodeBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxAudit))), nil
		}
		q.Limit = *l
	}
	if b := req.Params.Before; b != nil {
		if *b < 1 {
			return ListAudit400JSONResponse(errorBody(ErrorErrorCodeBadRequest, "before must be positive")), nil
		}
		q.Before = *b
	}
	for _, t := range s.core.Targets() {
		if (req.Params.Target == nil || *req.Params.Target == t.Name) && s.visible(ctx, t.Name) {
			q.Targets = append(q.Targets, t.Name)
		}
	}
	entries, err := s.audit.Audit(ctx, q)
	if err != nil {
		e, _ := s.classify(err)
		return ListAudit500JSONResponse(e), nil
	}
	out := ListAudit200JSONResponse{Entries: []AuditEntry{}}
	for _, e := range entries {
		out.Entries = append(out.Entries, AuditEntry{
			Id: e.ID, Time: e.Time, User: e.User, Subject: ptr(e.Subject),
			Service: ServiceRef{Target: e.Service.Target, Scope: e.Service.Scope, Name: e.Service.Name},
			Action:  AuditEntryAction(e.Action), Params: ptr(e.Params), Result: AuditEntryResult(e.Result), Error: ptr(e.Error),
		})
	}
	if len(entries) == q.Limit {
		out.Next = &entries[len(entries)-1].ID
	}
	return out, nil
}

// classify maps an error to its wire code and status. Internal errors are
// logged and answered without their text.
func (s *Server) classify(err error) (Error, int) {
	e, status := errorFor(err)
	if status == http.StatusInternalServerError {
		s.log.Error("api request failed", "err", err)
	}
	return e, status
}

func errorFor(err error) (Error, int) {
	var f forbidden
	switch {
	case errors.As(err, &f):
		return errorBody(ErrorErrorCodeForbidden, err.Error()), http.StatusForbidden
	case errors.Is(err, provider.ErrNotFound):
		return errorBody(ErrorErrorCodeNotFound, err.Error()), http.StatusNotFound
	case errors.Is(err, provider.ErrUnsupported):
		return errorBody(ErrorErrorCodeUnsupported, err.Error()), http.StatusNotImplemented
	case errors.Is(err, provider.ErrUnreachable):
		return errorBody(ErrorErrorCodeUnreachable, err.Error()), http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded):
		return errorBody(ErrorErrorCodeTimeout, "the provider did not answer in time"), http.StatusGatewayTimeout
	default:
		return errorBody(ErrorErrorCodeInternal, "internal error"), http.StatusInternalServerError
	}
}

func errorBody(code ErrorErrorCode, msg string) Error {
	var e Error
	e.Error.Code, e.Error.Message = code, msg
	return e
}

func writeError(w http.ResponseWriter, status int, code ErrorErrorCode, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody(code, msg))
}

func toTarget(t inventory.TargetState) Target {
	c := t.Capabilities
	return Target{
		Name: t.Name, Provider: t.Provider, Env: ptr(t.Env), Readonly: t.Readonly,
		Capabilities: Capabilities{Logs: c.Logs, FollowLogs: c.FollowLogs, Events: c.Events, Restart: c.Restart, Scale: c.Scale},
		RefreshedAt:  t.RefreshedAt, AttemptedAt: t.AttemptedAt, Stale: t.Stale(), Error: ptr(t.Error),
		ServiceCount: len(t.Inventory.Services), InstanceCount: len(t.Inventory.Instances),
	}
}

func toService(v core.ServiceView) Service {
	s := Service{
		Ref:      ServiceRef{Target: v.Ref.Target, Scope: v.Ref.Scope, Name: v.Ref.Name},
		Provider: v.Provider, Status: ServiceStatus(v.Status), Stale: v.Stale,
		Desired: v.Desired, Running: v.Running, Pending: v.Pending, Revision: ptr(v.Revision),
		Instances: InstanceCounts{Running: v.Counts.Running, Pending: v.Counts.Pending, Stopped: v.Counts.Stopped, Unhealthy: v.Counts.Unhealthy},
	}
	if v.Rollout != nil {
		s.Rollout = &Rollout{State: v.Rollout.State, StartedAt: v.Rollout.StartedAt}
	}
	if len(v.Labels) > 0 {
		labels := v.Labels
		s.Labels = &labels
	}
	return s
}

func toDetail(d core.DetailView) ServiceDetail {
	out := ServiceDetail{Service: toService(d.Service), Instances: []Instance{}, Stops: []StopRecord{}}
	for _, in := range d.Instances {
		out.Instances = append(out.Instances, toInstance(in))
	}
	for _, r := range d.Stops {
		out.Stops = append(out.Stops, StopRecord{InstanceId: r.Instance.ID, StoppedAt: r.StoppedAt, Reason: ptr(r.Reason),
			ExitCode: r.ExitCode, Image: ptr(r.Image), Digest: ptr(r.Digest)})
	}
	return out
}

func toInstance(in model.Instance) Instance {
	out := Instance{
		Id: in.Ref.ID, Status: InstanceStatus(in.Status), Health: health(in.Health), Revision: ptr(in.Revision),
		StartedAt: in.StartedAt, StoppedAt: in.StoppedAt, StopReason: ptr(in.StopReason), Restarts: in.Restarts,
		Containers: []Container{},
	}
	for _, c := range in.Containers {
		mc := Container{Name: c.Name, Image: ptr(c.Image), Digest: ptr(c.Digest), Status: ptr(c.Status),
			Health: health(c.Health), ExitCode: c.ExitCode, Reason: ptr(c.Reason)}
		if len(c.Ports) > 0 {
			ports := make([]Port, 0, len(c.Ports))
			for _, p := range c.Ports {
				ports = append(ports, Port{Container: p.Container, Host: p.Host, Protocol: ptr(p.Protocol)})
			}
			mc.Ports = &ports
		}
		out.Containers = append(out.Containers, mc)
	}
	return out
}

// ptr maps the empty string to an absent field, matching the model's rule
// that missing data is absent rather than zero.
func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func health(h string) *Health {
	if h == "" {
		return nil
	}
	v := Health(h)
	return &v
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
