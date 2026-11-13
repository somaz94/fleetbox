package findings

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
)

// appLabel names the app a service belongs to across environments; without
// it, services match by name.
const appLabel = "fleetbox.io/app"

// ruleRestarting is the one rule judged from memory rather than from data.
const ruleRestarting = "restarting"

// observation is a condition that holds right now.
type observation struct {
	key, kind, rule, severity, summary string
	services                           []model.ServiceRef
	evidence                           []model.Evidence
	since                              *time.Time    // when the data says it began; otherwise when first seen
	delay                              time.Duration // how long it must last before it opens
}

// input is what one evaluation looks at: only targets that can be judged.
type input struct {
	targets []inventory.TargetState
	stops   []model.StopRecord
	rising  map[model.InstanceRef]restartMemo
	off     map[string]bool // kinds switched off
	now     time.Time
	cfg     config.Findings
}

func detect(in input) []observation {
	running := instancesBy(in.targets, true)
	var out []observation
	if !in.off[model.FindingDrift] {
		out = append(out, drift(in, running)...)
	}
	if !in.off[model.FindingStuck] {
		out = append(out, stuck(in, running, instancesBy(in.targets, false))...)
	}
	if !in.off[model.FindingWhy] {
		out = append(out, why(in)...)
	}
	if !in.off[model.FindingHygiene] {
		out = append(out, hygiene(in, running)...)
	}
	return out
}

func instancesBy(targets []inventory.TargetState, runningOnly bool) map[model.ServiceRef][]model.Instance {
	out := map[model.ServiceRef][]model.Instance{}
	for _, t := range targets {
		for _, in := range t.Inventory.Instances {
			if !runningOnly || in.Status == model.StatusRunning {
				out[in.Ref.Service] = append(out[in.Ref.Service], in)
			}
		}
	}
	return out
}

func services(targets []inventory.TargetState) []model.Service {
	var out []model.Service
	for _, t := range targets {
		out = append(out, t.Inventory.Services...)
	}
	return out
}

func drift(in input, running map[model.ServiceRef][]model.Instance) []observation {
	type member struct {
		ref     model.ServiceRef
		env     string
		digests string
	}
	type group struct {
		provider, app string
		members       []member
	}
	groups := map[string]*group{}
	for _, t := range in.targets {
		if t.Env == "" {
			continue
		}
		for _, s := range t.Inventory.Services {
			ds := digests(running[s.Ref])
			if ds == "" {
				continue
			}
			app := s.Labels[appLabel]
			if app == "" {
				app = s.Ref.Name
			}
			k := t.Provider + "/" + app
			if groups[k] == nil {
				groups[k] = &group{provider: t.Provider, app: app}
			}
			groups[k].members = append(groups[k].members, member{s.Ref, t.Env, ds})
		}
	}
	var out []observation
	gks := make([]string, 0, len(groups))
	for k := range groups {
		gks = append(gks, k)
	}
	slices.Sort(gks) // one tick's openings, and so its messages, come out in a stable order
	for _, k := range gks {
		g := groups[k]
		ms := g.members
		differ := false
		for i := range ms {
			for j := i + 1; j < len(ms); j++ {
				differ = differ || ms[i].env != ms[j].env && ms[i].digests != ms[j].digests
			}
		}
		if !differ {
			continue
		}
		sort.Slice(ms, func(i, j int) bool {
			if ms[i].env != ms[j].env {
				return ms[i].env < ms[j].env
			}
			return ms[i].ref.String() < ms[j].ref.String()
		})
		o := observation{key: "drift/digest/" + k, kind: model.FindingDrift, rule: "digest", severity: model.SeverityWarning,
			delay: in.cfg.DriftAfter}
		var envs []string
		for _, m := range ms {
			if !slices.Contains(envs, m.env) {
				envs = append(envs, m.env)
			}
			o.services = append(o.services, m.ref)
			o.evidence = append(o.evidence, model.Evidence{Label: m.env, Value: m.digests, Service: model.Ptr(m.ref)})
		}
		o.summary = fmt.Sprintf("%s runs different images in %s", g.app, strings.Join(envs, ", "))
		out = append(out, o)
	}
	return out
}

// digests is the sorted set of image digests a service's running instances
// use, or empty when none is known.
func digests(ins []model.Instance) string {
	var ds []string
	for _, in := range ins {
		for _, c := range in.Containers {
			if c.Digest != "" && !slices.Contains(ds, c.Digest) {
				ds = append(ds, c.Digest)
			}
		}
	}
	sort.Strings(ds)
	return strings.Join(ds, ", ")
}

func stuck(in input, running, all map[model.ServiceRef][]model.Instance) []observation {
	var out []observation
	for _, s := range services(in.targets) {
		ref := s.Ref
		base := observation{kind: model.FindingStuck, severity: model.SeverityWarning, services: []model.ServiceRef{ref}, delay: in.cfg.StuckAfter}
		if r := s.Rollout; r != nil && r.State == "IN_PROGRESS" && r.StartedAt != nil {
			o := base
			o.key, o.rule, o.since = "stuck/rollout/"+ref.String(), "rollout", r.StartedAt
			o.summary = "rollout has not finished"
			o.evidence = []model.Evidence{{Label: "started", Value: r.StartedAt.UTC().Format(time.RFC3339)}}
			out = append(out, o)
		}
		if s.Desired != nil && s.Running != nil && *s.Running < *s.Desired {
			o := base
			o.key, o.rule = "stuck/below-desired/"+ref.String(), "below-desired"
			o.summary = fmt.Sprintf("runs %d of %d desired", *s.Running, *s.Desired)
			o.evidence = []model.Evidence{{Label: "desired", Value: fmt.Sprint(*s.Desired)}, {Label: "running", Value: fmt.Sprint(*s.Running)}}
			out = append(out, o)
		}
		var unhealthy, restarting []model.Evidence
		for _, i := range running[ref] {
			if i.Health == model.HealthUnhealthy {
				unhealthy = append(unhealthy, model.Evidence{Label: "instance", Value: i.Ref.ID})
			}
		}
		for _, i := range all[ref] {
			if m, ok := in.rising[i.Ref]; ok && !m.rose.IsZero() && in.now.Sub(m.rose) < in.cfg.StuckAfter {
				restarting = append(restarting, model.Evidence{Label: "instance", Value: fmt.Sprintf("%s (%d restarts)", i.Ref.ID, m.count)})
			}
		}
		if len(unhealthy) > 0 {
			o := base
			o.key, o.rule, o.evidence = "stuck/unhealthy/"+ref.String(), "unhealthy", unhealthy
			o.summary = fmt.Sprintf("%d running %s unhealthy", len(unhealthy), plural(len(unhealthy), "instance is", "instances are"))
			out = append(out, o)
		}
		if len(restarting) > 0 {
			o := base
			o.key, o.rule, o.evidence = "stuck/restarting/"+ref.String(), ruleRestarting, restarting
			o.summary = "keeps restarting"
			out = append(out, o)
		}
	}
	return out
}

// restartMemo is what the evaluator remembers of an instance's restart count
// between evaluations: the count, and when it last rose.
type restartMemo struct {
	count int
	rose  time.Time
}

// trackRestarts carries the memory forward. Instances on configured targets
// that cannot be judged keep their entry; others are forgotten once gone. A
// first sighting only sets the baseline.
func trackRestarts(prev map[model.InstanceRef]restartMemo, targets []inventory.TargetState, known map[string]bool, now time.Time) map[model.InstanceRef]restartMemo {
	judged := map[string]bool{}
	next := map[model.InstanceRef]restartMemo{}
	for _, t := range targets {
		judged[t.Name] = true
		for _, in := range t.Inventory.Instances {
			if in.Restarts == nil || in.Status == model.StatusStopped {
				continue
			}
			m, ok := prev[in.Ref]
			if ok && *in.Restarts > m.count {
				m.rose = now
			}
			m.count = *in.Restarts
			next[in.Ref] = m
		}
	}
	for ref, m := range prev {
		if t := ref.Service.Target; known[t] && !judged[t] {
			next[ref] = m
		}
	}
	return next
}

func failed(r model.StopRecord) bool {
	if strings.Contains(r.Reason, model.ReasonOOMKilled) || strings.Contains(r.Reason, "OutOfMemory") { // the latter is ECS's text
		return true
	}
	return r.ExitCode != nil && *r.ExitCode != 0 && !model.SignalExit(*r.ExitCode)
}

// maxStops bounds the evidence of a why finding.
const maxStops = 5

func why(in input) []observation {
	judged := map[string]bool{}
	for _, t := range in.targets {
		judged[t.Name] = true
	}
	byService := map[model.ServiceRef][]model.StopRecord{}
	var refs []model.ServiceRef
	for _, r := range in.stops { // newest first
		ref := r.Instance.Service
		if !judged[ref.Target] || !failed(r) {
			continue
		}
		if byService[ref] == nil {
			refs = append(refs, ref)
		}
		byService[ref] = append(byService[ref], r)
	}
	var out []observation
	for _, ref := range refs {
		recs := byService[ref]
		o := observation{key: "why/failed-stop/" + ref.String(), kind: model.FindingWhy, rule: "failed-stop",
			severity: model.SeverityWarning, services: []model.ServiceRef{ref},
			summary: fmt.Sprintf("%d failed %s in the last %s", len(recs), plural(len(recs), "stop", "stops"), span(in.cfg.FailedStopWindow))}
		for _, r := range recs[:min(len(recs), maxStops)] {
			v := r.Reason
			if r.ExitCode != nil {
				v = fmt.Sprintf("exit %d: %s", *r.ExitCode, r.Reason)
			}
			if r.Image != "" {
				v += " (" + r.Image + ")"
			}
			o.evidence = append(o.evidence, model.Evidence{Label: r.StoppedAt.UTC().Format(time.RFC3339), Value: v})
		}
		out = append(out, o)
	}
	return out
}

func hygiene(in input, running map[model.ServiceRef][]model.Instance) []observation {
	var out []observation
	for _, s := range services(in.targets) {
		ins := running[s.Ref]
		if len(ins) == 0 {
			continue
		}
		var latest, untagged, old, names []string
		checked, known := false, true
		for _, i := range ins {
			for _, c := range i.Containers {
				switch imageTag(c.Image) {
				case tagLatest:
					latest = appendNew(latest, c.Image)
				case tagNone:
					untagged = appendNew(untagged, c.Image)
				}
				if c.ImageCreated != nil && in.now.Sub(*c.ImageCreated) > in.cfg.ImageMaxAge {
					old = appendNew(old, c.Image+" built "+c.ImageCreated.UTC().Format(time.DateOnly))
				}
				names = appendNew(names, c.Name)
				switch {
				case c.HealthCheck == nil:
					known = false
				case *c.HealthCheck:
					checked = true
				}
			}
		}
		add := func(rule, summary, label string, values []string) {
			o := observation{key: "hygiene/" + rule + "/" + s.Ref.String(), kind: model.FindingHygiene, rule: rule,
				severity: model.SeverityInfo, services: []model.ServiceRef{s.Ref}, summary: summary}
			for _, v := range values {
				o.evidence = append(o.evidence, model.Evidence{Label: label, Value: v})
			}
			out = append(out, o)
		}
		if len(latest) > 0 {
			add("latest-tag", "runs images tagged latest", "image", latest)
		}
		if len(untagged) > 0 {
			add("untagged", "runs images with neither tag nor digest", "image", untagged)
		}
		if len(old) > 0 {
			add("old-image", "runs images built more than "+span(in.cfg.ImageMaxAge)+" ago", "image", old)
		}
		if known && !checked && len(names) > 0 {
			add("no-healthcheck", "no container has a health check", "container", names)
		}
	}
	return out
}

type tagKind int

const (
	tagOK tagKind = iota
	tagLatest
	tagNone
)

// bareImageID matches an image referenced by ID, as Docker reports a container
// whose tag has since moved to another image.
var bareImageID = regexp.MustCompile(`^(sha256:)?[0-9a-f]{12,64}$`)

// imageTag classifies an image reference: pinned by digest or carrying a tag
// other than latest is fine; no tag means latest, as the registry reads it.
func imageTag(ref string) tagKind {
	switch {
	case ref == "" || bareImageID.MatchString(ref):
		return tagNone
	case strings.Contains(ref, "@"):
		return tagOK
	}
	name := ref[strings.LastIndex(ref, "/")+1:]
	tag := ""
	if i := strings.LastIndex(name, ":"); i >= 0 {
		tag = name[i+1:]
	}
	if tag == "" || tag == "latest" {
		return tagLatest
	}
	return tagOK
}

func appendNew(s []string, v string) []string {
	if slices.Contains(s, v) {
		return s
	}
	return append(s, v)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// span writes a configured duration the way people say it: 24h as 1d.
func span(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return d.String()
}
