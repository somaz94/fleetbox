// Package api serves /api/v1. api.gen.go is generated from api/openapi.yaml
// (`make generate`); this file implements the generated strict interface on
// top of core, and logs.go serves the one streaming endpoint by hand.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/somaz94/fleetbox/internal/core"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

const basePath = "/api/v1"

// Server implements StrictServerInterface.
type Server struct {
	core *core.Core
	log  *slog.Logger
}

var _ StrictServerInterface = (*Server)(nil)

// NewHandler returns the /api/v1 handler. Errors of every kind, including
// unknown routes and malformed parameters, answer with the Error schema.
func NewHandler(c *core.Core, log *slog.Logger) http.Handler {
	s := &Server{core: c, log: log}
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
	return mux
}

func (s *Server) GetInfo(context.Context, GetInfoRequestObject) (GetInfoResponseObject, error) {
	b := s.core.Build()
	return GetInfo200JSONResponse{Version: b.Version, Commit: b.Commit, BuildDate: b.Date}, nil
}

func (s *Server) ListTargets(context.Context, ListTargetsRequestObject) (ListTargetsResponseObject, error) {
	out := ListTargets200JSONResponse{}
	for _, t := range s.core.Targets() {
		out = append(out, toTarget(t))
	}
	return out, nil
}

func (s *Server) ListServices(_ context.Context, req ListServicesRequestObject) (ListServicesResponseObject, error) {
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
		out = append(out, toService(v))
	}
	return out, nil
}

func (s *Server) GetService(ctx context.Context, req GetServiceRequestObject) (GetServiceResponseObject, error) {
	d, err := s.core.Service(ctx, model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service})
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
	d, err := s.core.Refresh(ctx, model.ServiceRef{Target: req.Target, Scope: req.Scope, Name: req.Service})
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
	evs, err := s.core.Events(ctx, ref, since)
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

// classify maps an error to its wire code and status. Internal errors are
// logged and answered without their text.
func (s *Server) classify(err error) (Error, int) {
	switch {
	case errors.Is(err, provider.ErrNotFound):
		return errorBody(ErrorErrorCodeNotFound, err.Error()), http.StatusNotFound
	case errors.Is(err, provider.ErrUnsupported):
		return errorBody(ErrorErrorCodeUnsupported, err.Error()), http.StatusNotImplemented
	case errors.Is(err, provider.ErrUnreachable):
		return errorBody(ErrorErrorCodeUnreachable, err.Error()), http.StatusBadGateway
	case errors.Is(err, context.DeadlineExceeded):
		return errorBody(ErrorErrorCodeTimeout, "the provider did not answer in time"), http.StatusGatewayTimeout
	default:
		s.log.Error("api request failed", "err", err)
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
