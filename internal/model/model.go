// Package model holds the provider-neutral types every provider maps onto.
//
// A field a provider cannot supply is left nil or empty, never zero: the
// console must be able to tell "no data" from "zero".
package model

import (
	"fmt"
	"strings"
	"time"
)

// Instance status values, normalised across providers.
const (
	StatusRunning = "running"
	StatusPending = "pending"
	StatusStopped = "stopped"
	StatusUnknown = "unknown"
)

// Health values. An instance without a health check carries no health at all.
const (
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
	HealthStarting  = "starting"
)

// Event severities.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
)

// ServiceRef addresses a service as target/scope/name. Scope is part of the key
// because ECS service names are unique only within a cluster.
type ServiceRef struct {
	Target string `json:"target"`
	Scope  string `json:"scope"`
	Name   string `json:"name"`
}

func (r ServiceRef) String() string { return r.Target + "/" + r.Scope + "/" + r.Name }

// ParseServiceRef parses the target/scope/name form produced by String.
func ParseServiceRef(s string) (ServiceRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return ServiceRef{}, fmt.Errorf("service ref %q: want target/scope/name", s)
	}
	return ServiceRef{Target: parts[0], Scope: parts[1], Name: parts[2]}, nil
}

// InstanceRef addresses one instance (an ECS task, a Docker container).
type InstanceRef struct {
	Service ServiceRef `json:"service"`
	ID      string     `json:"id"`
}

func (r InstanceRef) String() string { return r.Service.String() + "/" + r.ID }

// Rollout describes the deployment currently in effect.
type Rollout struct {
	State     string     `json:"state"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

// Service is one deployable unit: an ECS service, a compose service.
type Service struct {
	Ref      ServiceRef        `json:"ref"`
	Provider string            `json:"provider"`
	Desired  *int              `json:"desired,omitempty"`
	Running  *int              `json:"running,omitempty"`
	Pending  *int              `json:"pending,omitempty"`
	Revision string            `json:"revision,omitempty"`
	Rollout  *Rollout          `json:"rollout,omitempty"`
	Labels   map[string]string `json:"labels,omitempty"`
}

// Port is one published port.
type Port struct {
	Container int    `json:"container"`
	Host      *int   `json:"host,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
}

// Container is one container inside an instance. A Docker instance has exactly one.
type Container struct {
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
	Digest   string `json:"digest,omitempty"`
	Status   string `json:"status,omitempty"`
	Health   string `json:"health,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Ports    []Port `json:"ports,omitempty"`
	// ImageCreated is when the image was built, if the provider can tell.
	ImageCreated *time.Time `json:"imageCreated,omitempty"`
	// HealthCheck reports whether the container defines one; nil when unknown.
	HealthCheck *bool `json:"healthCheck,omitempty"`
}

// Instance is one running or recently stopped copy of a service.
type Instance struct {
	Ref        InstanceRef `json:"ref"`
	Status     string      `json:"status"`
	Health     string      `json:"health,omitempty"`
	Revision   string      `json:"revision,omitempty"`
	StartedAt  *time.Time  `json:"startedAt,omitempty"`
	StoppedAt  *time.Time  `json:"stoppedAt,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
	Restarts   *int        `json:"restarts,omitempty"`
	Containers []Container `json:"containers"`
}

// Inventory is everything one target runs, as of one refresh.
type Inventory struct {
	Services  []Service  `json:"services"`
	Instances []Instance `json:"instances"`
}

// ServiceDetail is one service with its instances, read fresh from the provider.
type ServiceDetail struct {
	Service   Service    `json:"service"`
	Instances []Instance `json:"instances"`
}

// Event is one entry of a service's timeline.
type Event struct {
	Time     time.Time  `json:"time"`
	Service  ServiceRef `json:"service"`
	Instance string     `json:"instance,omitempty"`
	Severity string     `json:"severity"`
	Message  string     `json:"message"`
}

// LogLine is one line of container output.
type LogLine struct {
	Time   *time.Time `json:"time,omitempty"`
	Stream string     `json:"stream,omitempty"`
	Text   string     `json:"text"`
}

// StopRecord is kept by the store so a stop reason outlives the provider's own
// retention (ECS forgets stopped tasks within hours).
type StopRecord struct {
	Instance  InstanceRef `json:"instance"`
	StoppedAt time.Time   `json:"stoppedAt"`
	Reason    string      `json:"reason,omitempty"`
	ExitCode  *int        `json:"exitCode,omitempty"`
	Image     string      `json:"image,omitempty"`
	Digest    string      `json:"digest,omitempty"`
}

// SignalExit reports whether a container ended because it was stopped on
// purpose (SIGINT, SIGKILL after the grace period, SIGTERM) rather than failing.
func SignalExit(code int) bool { return code == 130 || code == 137 || code == 143 }

// StopRecordOf returns the record for a stopped instance, or false when the
// instance is not stopped or carries no stop time. Container reasons the
// instance's own reason lacks are appended: ECS reports an out-of-memory kill
// only on the container.
func StopRecordOf(in Instance) (StopRecord, bool) {
	if in.Status != StatusStopped || in.StoppedAt == nil {
		return StopRecord{}, false
	}
	rec := StopRecord{Instance: in.Ref, StoppedAt: *in.StoppedAt, Reason: in.StopReason}
	for _, c := range in.Containers {
		if rec.Image == "" {
			rec.Image, rec.Digest = c.Image, c.Digest
		}
		if c.Reason != "" && !strings.Contains(rec.Reason, c.Reason) {
			if rec.Reason != "" {
				rec.Reason += "; "
			}
			rec.Reason += c.Name + ": " + c.Reason
		}
		// A sidecar stopped by a signal must not hide the code the app failed with.
		if c.ExitCode != nil && *c.ExitCode != 0 && (rec.ExitCode == nil || SignalExit(*rec.ExitCode) && !SignalExit(*c.ExitCode)) {
			code := *c.ExitCode
			rec.ExitCode = &code
		}
	}
	if rec.ExitCode == nil && len(in.Containers) > 0 && in.Containers[0].ExitCode != nil {
		code := *in.Containers[0].ExitCode
		rec.ExitCode = &code
	}
	return rec, true
}

// Ptr returns a pointer to v, for the optional fields above.
func Ptr[T any](v T) *T { return &v }
