package model

import (
	"sort"
	"time"
)

// Finding kinds.
const (
	FindingDrift   = "drift"
	FindingStuck   = "stuck"
	FindingWhy     = "why"
	FindingHygiene = "hygiene"
)

// FindingKinds lists every kind, in the order the console shows them.
var FindingKinds = []string{FindingDrift, FindingStuck, FindingWhy, FindingHygiene}

// Finding transitions, the only moments a finding is announced.
const (
	TransitionOpened   = "opened"
	TransitionResolved = "resolved"
)

// Evidence is one fact a finding was raised on.
type Evidence struct {
	Label   string      `json:"label"`
	Value   string      `json:"value"`
	Service *ServiceRef `json:"service,omitempty"`
}

// Finding is a condition fleetbox keeps track of. It is pending while OpenedAt
// is nil, open once set, and resolved once ResolvedAt is set. Key names the
// condition; a condition that returns after resolving is a new finding with
// the same key.
type Finding struct {
	ID         int64
	Key        string
	Kind       string
	Rule       string
	Severity   string
	Summary    string
	Services   []ServiceRef
	Evidence   []Evidence
	FirstSeen  time.Time
	LastSeen   time.Time
	OpenedAt   *time.Time
	ResolvedAt *time.Time
}

// Targets returns the targets the finding involves, sorted and without repeats.
func (f Finding) Targets() []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range f.Services {
		if !seen[s.Target] {
			seen[s.Target] = true
			out = append(out, s.Target)
		}
	}
	sort.Strings(out)
	return out
}

// FindingEvent is one open or resolve transition. Its finding is as stored
// now, except the summary, severity and services, which are as they were at
// the transition.
type FindingEvent struct {
	ID         int64
	Transition string
	Time       time.Time
	Finding    Finding
}
