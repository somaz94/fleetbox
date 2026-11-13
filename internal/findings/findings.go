// Package findings raises what no single console shows: image drift across
// environments, services stuck short of their goal, why things stopped, and
// image hygiene. It reads only the cache and the store's stop history, never a
// provider, and keeps each finding's state in the store so a restart neither
// forgets how long a condition has lasted nor announces it twice.
package findings

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/store"
)

// Source is the cache the evaluator reads.
type Source interface {
	Targets() []inventory.TargetState
}

// Store is the persistence the evaluator needs.
type Store interface {
	StopsSince(ctx context.Context, since time.Time) ([]model.StopRecord, error)
	ActiveFindings(ctx context.Context) ([]model.Finding, error)
	SaveFindings(ctx context.Context, c store.FindingChanges) error
	PruneFindings(ctx context.Context, before time.Time) (int, error)
}

const pruneEvery = time.Hour

// Evaluator runs the rules on an interval and records what changed.
type Evaluator struct {
	cfg  config.Findings
	src  Source
	st   Store
	log  *slog.Logger
	now  func() time.Time
	keep time.Duration

	restarts  map[model.InstanceRef]restartMemo
	started   time.Time
	lastPrune time.Time
}

// New builds an evaluator; cfg must have its defaults applied.
func New(cfg config.Findings, src Source, st Store, log *slog.Logger) *Evaluator {
	return &Evaluator{cfg: cfg, src: src, st: st, log: log, now: time.Now, keep: inventory.DefaultRetention,
		restarts: map[model.InstanceRef]restartMemo{}}
}

// Run evaluates on every tick until ctx ends. The first evaluation waits one
// interval so the poller can fill the cache; a target not yet refreshed is skipped.
func (e *Evaluator) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := e.Evaluate(ctx); err != nil && ctx.Err() == nil {
				e.log.Error("evaluate findings", "err", err)
			}
		}
	}
}

// Evaluate runs the rules once and records the outcome.
func (e *Evaluator) Evaluate(ctx context.Context) error {
	now := e.now()
	if e.started.IsZero() {
		e.started = now
	}
	var judged []inventory.TargetState
	known, ok := map[string]bool{}, map[string]bool{}
	for _, t := range e.src.Targets() {
		known[t.Name] = true
		// A target never refreshed, or whose last refresh failed, says nothing new.
		if t.RefreshedAt != nil && !t.Stale() {
			judged = append(judged, t)
			ok[t.Name] = true
		}
	}
	e.restarts = trackRestarts(e.restarts, judged, known, now)
	stops, err := e.st.StopsSince(ctx, now.Add(-e.cfg.FailedStopWindow))
	if err != nil {
		return fmt.Errorf("read stops: %w", err)
	}
	active, err := e.st.ActiveFindings(ctx)
	if err != nil {
		return fmt.Errorf("read findings: %w", err)
	}
	off := map[string]bool{}
	for _, k := range e.cfg.Disabled {
		off[k] = true
	}
	// Restart counts live only in memory: for one stuckAfter after start, an
	// instance that stopped restarting cannot be told from one not yet seen to.
	warming := now.Sub(e.started) < e.cfg.StuckAfter
	c := reconcile(active, detect(input{targets: judged, stops: stops, rising: e.restarts, now: now, cfg: e.cfg}), policy{
		// A target no longer configured will never answer again, so its findings resolve.
		judged:  func(t string) bool { return ok[t] || !known[t] },
		hold:    func(f model.Finding) bool { return warming && f.Kind == model.FindingStuck && f.Rule == ruleRestarting },
		retired: func(f model.Finding) bool { return off[f.Kind] },
	}, now)
	if err := e.st.SaveFindings(ctx, c); err != nil {
		return fmt.Errorf("save findings: %w", err)
	}
	e.logTransitions(active, c)
	if now.Sub(e.lastPrune) >= pruneEvery {
		if _, err := e.st.PruneFindings(ctx, now.Add(-e.keep)); err != nil {
			return fmt.Errorf("prune findings: %w", err)
		}
		e.lastPrune = now
	}
	return nil
}

func (e *Evaluator) logTransitions(active []model.Finding, c store.FindingChanges) {
	for _, sv := range c.Save {
		if sv.Opened {
			e.log.Info("finding opened", "finding", sv.Finding.Key, "summary", sv.Finding.Summary)
		}
	}
	keys := map[int64]string{}
	for _, f := range active {
		keys[f.ID] = f.Key
	}
	for _, id := range c.Resolve {
		e.log.Info("finding resolved", "finding", keys[id])
	}
}

// policy is what reconcile asks about findings that were not observed.
type policy struct {
	judged  func(target string) bool   // the target's data is current
	hold    func(f model.Finding) bool // keep it as it is for now
	retired func(f model.Finding) bool // its kind was switched off
}

// reconcile turns what holds now into changes to the stored findings. A
// finding on a target that cannot be judged is left exactly as it is, so an
// unreachable target neither resolves nor rewrites what was found on it.
func reconcile(active []model.Finding, obs []observation, p policy, now time.Time) store.FindingChanges {
	c := store.FindingChanges{At: now}
	frozen := func(f model.Finding) bool {
		for _, t := range f.Targets() {
			if !p.judged(t) {
				return true
			}
		}
		return false
	}
	byKey := map[string]model.Finding{}
	for _, f := range active {
		byKey[f.Key] = f
	}
	seen := map[string]bool{}
	for _, o := range obs {
		if seen[o.key] {
			continue // a second insert would break the active key's uniqueness
		}
		seen[o.key] = true
		f, ok := byKey[o.key]
		if ok && frozen(f) {
			continue
		}
		if !ok {
			first := now
			if o.since != nil && o.since.Before(now) {
				first = *o.since
			}
			f = model.Finding{Key: o.key, Kind: o.kind, Rule: o.rule, FirstSeen: first}
		}
		f.Severity, f.Summary, f.Services, f.Evidence, f.LastSeen = o.severity, o.summary, o.services, o.evidence, now
		opened := f.OpenedAt == nil && now.Sub(f.FirstSeen) >= o.delay
		if opened {
			at := now
			f.OpenedAt = &at
		}
		c.Save = append(c.Save, store.FindingSave{Finding: f, Opened: opened})
	}
	for _, f := range active {
		if seen[f.Key] || frozen(f) {
			continue
		}
		switch {
		case p.retired(f) && f.OpenedAt != nil:
			c.Retire = append(c.Retire, f.ID)
		case p.hold(f) && !p.retired(f):
		case f.OpenedAt != nil:
			c.Resolve = append(c.Resolve, f.ID)
		default:
			c.Drop = append(c.Drop, f.ID) // went away before anyone was told
		}
	}
	return c
}
