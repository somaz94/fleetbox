package findings

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/inventory"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/store"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

var cfg = config.Findings{Interval: time.Minute, DriftAfter: time.Hour, StuckAfter: 15 * time.Minute,
	FailedStopWindow: 24 * time.Hour, ImageMaxAge: 90 * 24 * time.Hour}

func ref(target, name string) model.ServiceRef {
	return model.ServiceRef{Target: target, Scope: "web", Name: name}
}

func target(name, provider, env string, svcs []model.Service, ins []model.Instance) inventory.TargetState {
	return inventory.TargetState{Name: name, Provider: provider, Env: env, RefreshedAt: model.Ptr(t0),
		Inventory: model.Inventory{Services: svcs, Instances: ins}}
}

func running(r model.ServiceRef, id string, cs ...model.Container) model.Instance {
	return model.Instance{Ref: model.InstanceRef{Service: r, ID: id}, Status: model.StatusRunning, Containers: cs}
}

func digest(d string) model.Container {
	return model.Container{Name: "app", Image: "example/app:1.0", Digest: d, HealthCheck: model.Ptr(true)}
}

func keys(obs []observation) []string {
	var out []string
	for _, o := range obs {
		out = append(out, o.key)
	}
	return out
}

func only(t *testing.T, obs []observation, key string) observation {
	t.Helper()
	if len(obs) != 1 || obs[0].key != key {
		t.Fatalf("observations = %v, want only %s", keys(obs), key)
	}
	return obs[0]
}

func TestDrift(t *testing.T) {
	prod, dev, stage := ref("prod", "api"), ref("dev", "api"), ref("stage", "api")
	labelled := model.Service{Ref: ref("dev", "backend"), Labels: map[string]string{appLabel: "api"}}
	targets := []inventory.TargetState{
		target("prod", "ecs", "prod", []model.Service{{Ref: prod}}, []model.Instance{running(prod, "1", digest("sha256:new")), running(prod, "2", digest("sha256:old"))}),
		target("dev", "ecs", "dev", []model.Service{{Ref: dev}, labelled}, []model.Instance{
			running(dev, "1", digest("sha256:new")), running(labelled.Ref, "2", digest("sha256:new"))}),
		// Another provider and a target without env never take part.
		target("host", "docker", "prod", []model.Service{{Ref: ref("host", "api")}}, []model.Instance{running(ref("host", "api"), "1", digest("sha256:x"))}),
		target("stage", "ecs", "", []model.Service{{Ref: stage}}, []model.Instance{running(stage, "1", digest("sha256:y"))}),
	}
	o := only(t, drift(input{targets: targets, cfg: cfg}, instancesBy(targets, true)), "drift/digest/ecs/api")
	if o.delay != time.Hour || o.summary != "api runs different images in dev, prod" || len(o.services) != 3 ||
		o.evidence[2].Label != "prod" || o.evidence[2].Value != "sha256:new, sha256:old" || *o.evidence[2].Service != prod {
		t.Fatalf("drift = %+v", o)
	}

	// The same images everywhere, or differences within one environment only, are not drift.
	same := []inventory.TargetState{
		target("prod", "ecs", "prod", []model.Service{{Ref: prod}}, []model.Instance{running(prod, "1", digest("sha256:a"))}),
		target("prod2", "ecs", "prod", []model.Service{{Ref: ref("prod2", "api")}}, []model.Instance{running(ref("prod2", "api"), "1", digest("sha256:b"))}),
		target("dev", "ecs", "dev", []model.Service{{Ref: dev}}, []model.Instance{running(dev, "1")}), // no digest known
	}
	if obs := drift(input{targets: same, cfg: cfg}, instancesBy(same, true)); len(obs) != 0 {
		t.Fatalf("unexpected drift %v", keys(obs))
	}
}

func TestStuck(t *testing.T) {
	r := ref("prod", "api")
	started := t0.Add(-time.Hour)
	svc := model.Service{Ref: r, Desired: model.Ptr(3), Running: model.Ptr(1), Rollout: &model.Rollout{State: "IN_PROGRESS", StartedAt: &started}}
	sick := running(r, "a")
	sick.Health = model.HealthUnhealthy
	targets := []inventory.TargetState{target("prod", "ecs", "", []model.Service{svc}, []model.Instance{sick, running(r, "b")})}
	rising := map[model.InstanceRef]restartMemo{{Service: r, ID: "b"}: {count: 4, rose: t0.Add(-time.Minute)}}

	obs := stuck(input{targets: targets, rising: rising, now: t0, cfg: cfg}, instancesBy(targets, true), instancesBy(targets, false))
	want := []string{"stuck/rollout/prod/web/api", "stuck/below-desired/prod/web/api", "stuck/unhealthy/prod/web/api", "stuck/restarting/prod/web/api"}
	if got := keys(obs); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("stuck = %v", got)
	}
	if !obs[0].since.Equal(started) || obs[1].summary != "runs 1 of 3 desired" || obs[2].summary != "1 running instance is unhealthy" ||
		obs[3].evidence[0].Value != "b (4 restarts)" || obs[3].delay != cfg.StuckAfter {
		t.Fatalf("stuck = %+v", obs)
	}

	// A finished rollout, a full service and a restart that stopped rising are fine.
	svc.Rollout.State, svc.Running = "COMPLETED", model.Ptr(3)
	rising[model.InstanceRef{Service: r, ID: "b"}] = restartMemo{count: 4, rose: t0.Add(-time.Hour)}
	healthy := []inventory.TargetState{target("prod", "ecs", "", []model.Service{svc}, []model.Instance{running(r, "a"), running(r, "b")})}
	if obs := stuck(input{targets: healthy, rising: rising, now: t0, cfg: cfg}, instancesBy(healthy, true), instancesBy(healthy, false)); len(obs) != 0 {
		t.Fatalf("unexpected %v", keys(obs))
	}
}

func TestTrackRestarts(t *testing.T) {
	r := ref("host", "web")
	inst := func(id string, restarts int, status string) model.Instance {
		in := running(r, id)
		in.Restarts, in.Status = model.Ptr(restarts), status
		return in
	}
	ts := func(ins ...model.Instance) []inventory.TargetState {
		return []inventory.TargetState{target("host", "docker", "", nil, ins)}
	}
	a := model.InstanceRef{Service: r, ID: "a"}
	known := map[string]bool{"host": true}
	m := trackRestarts(map[model.InstanceRef]restartMemo{}, ts(inst("a", 2, model.StatusRunning)), known, t0)
	if m[a] != (restartMemo{count: 2}) {
		t.Fatalf("baseline = %+v", m[a])
	}
	m = trackRestarts(m, ts(inst("a", 3, model.StatusPending)), known, t0.Add(time.Minute))
	if !m[a].rose.Equal(t0.Add(time.Minute)) || m[a].count != 3 {
		t.Fatalf("after a rise = %+v", m[a])
	}
	m = trackRestarts(m, ts(inst("a", 3, model.StatusRunning)), known, t0.Add(2*time.Minute))
	if !m[a].rose.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a steady count moved the rise: %+v", m[a])
	}
	// A configured target that cannot be judged keeps its memory; a removed
	// target, or a gone or stopped instance, is forgotten.
	if kept := trackRestarts(m, nil, known, t0); kept[a] != m[a] {
		t.Fatalf("unjudged target forgot %+v", kept)
	}
	if removed := trackRestarts(m, nil, map[string]bool{}, t0); len(removed) != 0 {
		t.Fatalf("removed target kept: %+v", removed)
	}
	if gone := trackRestarts(m, ts(inst("a", 3, model.StatusStopped)), known, t0); len(gone) != 0 {
		t.Fatalf("stopped instance kept: %+v", gone)
	}
}

func TestWhy(t *testing.T) {
	r, other := ref("prod", "api"), ref("gone", "api")
	stopAt := func(i int, code *int, reason string) model.StopRecord {
		return model.StopRecord{Instance: model.InstanceRef{Service: r, ID: "i"}, StoppedAt: t0.Add(-time.Duration(i) * time.Minute),
			ExitCode: code, Reason: reason, Image: "example/app:1.0"}
	}
	stops := []model.StopRecord{
		stopAt(0, model.Ptr(1), "exited with code 1"),
		stopAt(1, model.Ptr(143), "Scaling activity initiated by deployment"), // a stop signal
		stopAt(2, model.Ptr(137), "exited with code 137; OOMKilled"),
		stopAt(3, model.Ptr(137), "Essential container exited; app: OutOfMemoryError: Container killed"),
		stopAt(4, model.Ptr(0), "exited with code 0"),
		stopAt(5, nil, "Task stopped by user"),
		stopAt(6, model.Ptr(2), ""), stopAt(7, model.Ptr(3), ""), stopAt(8, model.Ptr(4), ""),
		stopAt(9, model.Ptr(130), "interrupted"), stopAt(10, model.Ptr(137), "killed after the grace period"),
		{Instance: model.InstanceRef{Service: other, ID: "x"}, ExitCode: model.Ptr(1)}, // its target cannot be judged
	}
	targets := []inventory.TargetState{target("prod", "ecs", "", nil, nil)}
	o := only(t, why(input{targets: targets, stops: stops, cfg: cfg}), "why/failed-stop/prod/web/api")
	if o.summary != "6 failed stops in the last 1d" || len(o.evidence) != maxStops || o.delay != 0 ||
		o.evidence[0].Value != "exit 1: exited with code 1 (example/app:1.0)" || o.evidence[0].Label != t0.Format(time.RFC3339) {
		t.Fatalf("why = %+v", o)
	}
	reasonOnly := []model.StopRecord{{Instance: model.InstanceRef{Service: r, ID: "i"}, StoppedAt: t0, Reason: "OOMKilled"}}
	if o := only(t, why(input{targets: targets, stops: reasonOnly, cfg: cfg}), o.key); o.evidence[0].Value != "OOMKilled" || o.summary != "1 failed stop in the last 1d" {
		t.Fatalf("why = %+v", o)
	}
}

func TestHygiene(t *testing.T) {
	r := ref("host", "web")
	old := t0.Add(-100 * 24 * time.Hour)
	containers := []model.Container{
		{Name: "app", Image: "example/app:latest", ImageCreated: &old, HealthCheck: model.Ptr(false)},
		{Name: "cache", Image: "redis", HealthCheck: model.Ptr(false)},
		{Name: "orphan", Image: "sha256:" + strings.Repeat("a", 64), HealthCheck: model.Ptr(false)},
		{Name: "pinned", Image: "example/app@sha256:abc", ImageCreated: model.Ptr(t0), HealthCheck: model.Ptr(false)},
	}
	stopped := running(r, "z", model.Container{Name: "late", Image: "example/x:latest"})
	stopped.Status = model.StatusStopped
	targets := []inventory.TargetState{target("host", "docker", "", []model.Service{{Ref: r}, {Ref: ref("host", "idle")}},
		[]model.Instance{running(r, "a", containers...), stopped})}
	obs := hygiene(input{targets: targets, now: t0, cfg: cfg}, instancesBy(targets, true))
	want := "hygiene/latest-tag/host/web/web hygiene/untagged/host/web/web hygiene/old-image/host/web/web hygiene/no-healthcheck/host/web/web"
	if got := strings.Join(keys(obs), " "); got != want {
		t.Fatalf("hygiene = %s", got)
	}
	if len(obs[0].evidence) != 2 || obs[0].evidence[1].Value != "redis" || obs[0].severity != model.SeverityInfo ||
		obs[2].summary != "runs images built more than 90d ago" || obs[2].evidence[0].Value != "example/app:latest built 2025-09-23" ||
		len(obs[3].evidence) != 4 {
		t.Fatalf("hygiene = %+v", obs)
	}

	// One container with a health check, or one whose check is unknown, settles it.
	for _, hc := range []*bool{model.Ptr(true), nil} {
		cs := []model.Container{{Name: "app", Image: "example/app:1.0", HealthCheck: model.Ptr(false)}, {Name: "side", Image: "example/side:1.0", HealthCheck: hc}}
		ts := []inventory.TargetState{target("host", "docker", "", []model.Service{{Ref: r}}, []model.Instance{running(r, "a", cs...)})}
		if obs := hygiene(input{targets: ts, now: t0, cfg: cfg}, instancesBy(ts, true)); len(obs) != 0 {
			t.Fatalf("health check %v: unexpected %v", hc, keys(obs))
		}
	}
}

func TestImageTag(t *testing.T) {
	for ref, want := range map[string]tagKind{
		"example/app:1.2":                       tagOK,
		"registry.example.com:5000/app:2":       tagOK,
		"registry.example.com:5000/app":         tagLatest,
		"example/app@sha256:abc":                tagOK,
		"redis":                                 tagLatest,
		"redis:latest":                          tagLatest,
		"0123456789ab":                          tagNone,
		"sha256:" + strings.Repeat("f", 64):     tagNone,
		"":                                      tagNone,
		"123456789012.dkr.ecr.example/app:v1.0": tagOK,
	} {
		if got := imageTag(ref); got != want {
			t.Errorf("imageTag(%q) = %v, want %v", ref, got, want)
		}
	}
}

func TestSpan(t *testing.T) {
	for d, want := range map[time.Duration]string{24 * time.Hour: "1d", 3 * time.Hour: "3h", 90 * time.Minute: "1h30m0s"} {
		if got := span(d); got != want {
			t.Errorf("span(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestDetectHonoursDisabled(t *testing.T) {
	r := ref("prod", "api")
	targets := []inventory.TargetState{target("prod", "ecs", "", []model.Service{{Ref: r, Desired: model.Ptr(2), Running: model.Ptr(1)}},
		[]model.Instance{running(r, "a", model.Container{Name: "app", Image: "example/app:latest"})})}
	stops := []model.StopRecord{{Instance: model.InstanceRef{Service: r, ID: "a"}, StoppedAt: t0, ExitCode: model.Ptr(1)}}
	all := detect(input{targets: targets, stops: stops, now: t0, cfg: cfg})
	if len(all) != 3 {
		t.Fatalf("all = %v", keys(all))
	}
	off := map[string]bool{}
	for _, k := range model.FindingKinds {
		off[k] = true
	}
	if got := detect(input{targets: targets, stops: stops, now: t0, cfg: cfg, off: off}); len(got) != 0 {
		t.Fatalf("disabled kinds still found %v", keys(got))
	}
}

func TestReconcile(t *testing.T) {
	p := policy{
		judged:  func(t string) bool { return t != "down" },
		hold:    func(f model.Finding) bool { return f.Rule == "held" },
		retired: func(f model.Finding) bool { return f.Kind == model.FindingHygiene },
	}
	obs := func(key, tgt string, delay time.Duration, since *time.Time) observation {
		return observation{key: key, kind: model.FindingStuck, rule: "r", severity: model.SeverityWarning, summary: key,
			services: []model.ServiceRef{ref(tgt, "api")}, delay: delay, since: since}
	}
	long, future := t0.Add(-time.Hour), t0.Add(time.Hour)

	c := reconcile(nil, []observation{obs("now", "prod", 0, nil), obs("later", "prod", time.Hour, nil),
		obs("since", "prod", time.Hour, &long), obs("future", "prod", time.Hour, &future), obs("now", "prod", 0, nil)}, p, t0)
	if len(c.Save) != 4 || !c.Save[0].Opened || c.Save[1].Opened || !c.Save[2].Opened || !c.Save[2].Finding.FirstSeen.Equal(long) ||
		c.Save[1].Finding.OpenedAt != nil || !c.Save[0].Finding.OpenedAt.Equal(t0) || !c.Save[3].Finding.FirstSeen.Equal(t0) {
		t.Fatalf("first pass = %+v", c)
	}

	opened := t0
	on := func(tgts ...string) []model.ServiceRef {
		var out []model.ServiceRef
		for _, tg := range tgts {
			out = append(out, ref(tg, "api"))
		}
		return out
	}
	active := []model.Finding{
		{ID: 1, Key: "pending", FirstSeen: t0.Add(-2 * time.Hour), Services: on("prod")},
		{ID: 2, Key: "open", FirstSeen: t0, OpenedAt: &opened, Services: on("prod")},
		{ID: 3, Key: "gone-open", FirstSeen: t0, OpenedAt: &opened, Services: on("prod")},
		{ID: 4, Key: "gone-pending", FirstSeen: t0, Services: on("prod")},
		{ID: 5, Key: "frozen", FirstSeen: t0, OpenedAt: &opened, Services: on("prod", "down")},
		{ID: 6, Key: "frozen-seen", FirstSeen: t0, Services: on("down")},
		{ID: 7, Key: "held", Rule: "held", FirstSeen: t0, OpenedAt: &opened, Services: on("prod")},
		{ID: 8, Key: "retired-open", Kind: model.FindingHygiene, FirstSeen: t0, OpenedAt: &opened, Services: on("prod")},
		{ID: 9, Key: "retired-pending", Kind: model.FindingHygiene, Rule: "held", FirstSeen: t0, Services: on("prod")},
	}
	c = reconcile(active, []observation{obs("pending", "prod", time.Hour, nil), obs("open", "prod", 0, nil), obs("frozen-seen", "prod", 0, nil)}, p, t0)
	if len(c.Save) != 2 || c.Save[0].Finding.ID != 1 || !c.Save[0].Opened || c.Save[1].Finding.ID != 2 || c.Save[1].Opened ||
		c.Save[1].Finding.OpenedAt != &opened {
		t.Fatalf("saves = %+v", c.Save)
	}
	if fmt.Sprint(c.Resolve, c.Retire, c.Drop) != "[3] [8] [4 9]" {
		t.Fatalf("resolve %v retire %v drop %v", c.Resolve, c.Retire, c.Drop)
	}
}

// fakeSource is a cache whose targets a test replaces between evaluations.
type fakeSource struct {
	mu      sync.Mutex
	targets []inventory.TargetState
}

func (f *fakeSource) Targets() []inventory.TargetState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.targets
}

func (f *fakeSource) set(ts ...inventory.TargetState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = ts
}

func newEvaluator(t *testing.T) (*Evaluator, *fakeSource, *store.SQLite, *time.Time) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	src := &fakeSource{}
	e := New(cfg, src, st, slog.New(slog.DiscardHandler))
	now := t0
	e.now = func() time.Time { return now }
	return e, src, st, &now
}

func TestEvaluate(t *testing.T) {
	e, src, st, now := newEvaluator(t)
	ctx := context.Background()
	r := ref("prod", "api")
	short := target("prod", "ecs", "", []model.Service{{Ref: r, Desired: model.Ptr(2), Running: model.Ptr(1)}}, nil)
	full := target("prod", "ecs", "", []model.Service{{Ref: r, Desired: model.Ptr(2), Running: model.Ptr(2)}}, nil)
	open := func() []model.Finding {
		got, err := st.Findings(ctx, store.FindingQuery{State: store.StateOpen})
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	src.set(short)
	if err := e.Evaluate(ctx); err != nil || len(open()) != 0 {
		t.Fatalf("a fresh shortfall must wait: %v, %+v", err, open())
	}
	// Unreachable for a while: the pending finding is neither dropped nor opened.
	stale := short
	stale.Error = "dial tcp: timeout"
	src.set(stale)
	*now = now.Add(20 * time.Minute)
	if err := e.Evaluate(ctx); err != nil || len(open()) != 0 {
		t.Fatalf("stale target judged: %v, %+v", err, open())
	}
	src.set(short)
	if err := e.Evaluate(ctx); err != nil || len(open()) != 1 || open()[0].Summary != "runs 1 of 2 desired" {
		t.Fatalf("shortfall past stuckAfter: %v, %+v", err, open())
	}
	src.set(full)
	*now = now.Add(time.Minute)
	if err := e.Evaluate(ctx); err != nil || len(open()) != 0 {
		t.Fatalf("recovered service still open: %v, %+v", err, open())
	}
	resolved, _ := st.Findings(ctx, store.FindingQuery{State: store.StateResolved})
	if len(resolved) != 1 || !resolved[0].ResolvedAt.Equal(*now) {
		t.Fatalf("resolved = %+v", resolved)
	}

	events, err := st.FindingEvents(ctx, 0, 10)
	if err != nil || len(events) != 2 || events[0].Transition != model.TransitionOpened || events[1].Transition != model.TransitionResolved ||
		events[0].Finding.Key != "stuck/below-desired/prod/web/api" || events[0].Finding.Summary != "runs 1 of 2 desired" {
		t.Fatalf("events = %+v, %v", events, err)
	}

	// Resolved findings age out once the store's retention passes.
	*now = now.Add(e.keep + 2*time.Hour)
	if err := e.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if resolved, _ = st.Findings(ctx, store.FindingQuery{State: store.StateResolved}); len(resolved) != 0 {
		t.Fatalf("not pruned: %+v", resolved)
	}
}

func TestEvaluateStoreErrors(t *testing.T) {
	for _, op := range []string{"StopsSince", "ActiveFindings", "SaveFindings", "PruneFindings"} {
		e, src, st, _ := newEvaluator(t)
		src.set(target("prod", "ecs", "", nil, nil))
		e.st = failing{Store: st, op: op}
		if err := e.Evaluate(context.Background()); err == nil {
			t.Fatalf("%s failure not reported", op)
		}
	}
}

type failing struct {
	Store
	op string
}

func (f failing) fail(op string) error {
	if op == f.op {
		return errors.New("boom")
	}
	return nil
}

func (f failing) StopsSince(ctx context.Context, since time.Time) ([]model.StopRecord, error) {
	if err := f.fail("StopsSince"); err != nil {
		return nil, err
	}
	return f.Store.StopsSince(ctx, since)
}

func (f failing) ActiveFindings(ctx context.Context) ([]model.Finding, error) {
	if err := f.fail("ActiveFindings"); err != nil {
		return nil, err
	}
	return f.Store.ActiveFindings(ctx)
}

func (f failing) SaveFindings(ctx context.Context, c store.FindingChanges) error {
	if err := f.fail("SaveFindings"); err != nil {
		return err
	}
	return f.Store.SaveFindings(ctx, c)
}

func (f failing) PruneFindings(ctx context.Context, before time.Time) (int, error) {
	if err := f.fail("PruneFindings"); err != nil {
		return 0, err
	}
	return f.Store.PruneFindings(ctx, before)
}

func TestRun(t *testing.T) {
	e, src, st, _ := newEvaluator(t)
	var buf bytes.Buffer
	var mu sync.Mutex
	e.log = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	e.cfg.Interval = 10 * time.Millisecond
	e.st = failing{Store: st, op: "StopsSince"}
	src.set(target("prod", "ecs", "", nil, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		logged := strings.Contains(buf.String(), "evaluate findings")
		mu.Unlock()
		if logged {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a failed evaluation was not logged")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestEvaluateAcrossRestart(t *testing.T) {
	e, src, st, now := newEvaluator(t)
	ctx := context.Background()
	r := ref("host", "web")
	crashing := func(n int) inventory.TargetState {
		in := running(r, "a")
		in.Restarts = model.Ptr(n)
		return target("host", "docker", "", []model.Service{{Ref: r}}, []model.Instance{in})
	}
	for i := 0; i <= 16; i++ { // a restart every minute for 16 minutes
		src.set(crashing(i))
		if err := e.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	if got, _ := st.Findings(ctx, store.FindingQuery{State: store.StateOpen}); len(got) != 1 || got[0].Rule != ruleRestarting {
		t.Fatalf("crash loop not open: %+v", got)
	}

	// A new evaluator over the same store sees no rise on its first look, and
	// must neither resolve the finding nor announce it again.
	e2 := New(cfg, src, st, slog.New(slog.DiscardHandler))
	e2.now = e.now
	for i := 17; i <= 20; i++ {
		src.set(crashing(i))
		if err := e2.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
	}
	events, _ := st.FindingEvents(ctx, 0, 10)
	if len(events) != 1 || events[0].Transition != model.TransitionOpened {
		t.Fatalf("events across a restart = %+v", events)
	}
}

func TestEvaluateLoneRestart(t *testing.T) {
	e, src, st, now := newEvaluator(t)
	ctx := context.Background()
	r := ref("host", "web")
	in := running(r, "a")
	for _, n := range []int{0, 1, 1, 1, 1} { // one restart, then quiet, ticked across stuckAfter
		in.Restarts = model.Ptr(n)
		src.set(target("host", "docker", "", []model.Service{{Ref: r}}, []model.Instance{in}))
		if err := e.Evaluate(ctx); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(cfg.StuckAfter / 2)
	}
	if got, _ := st.Findings(ctx, store.FindingQuery{State: store.StateOpen}); len(got) != 0 {
		t.Fatalf("a lone restart opened %+v", got)
	}
}

func TestEvaluateRemovedTargetAndDisabledKind(t *testing.T) {
	e, src, st, now := newEvaluator(t)
	ctx := context.Background()
	gone, img := ref("gone", "api"), ref("host", "web")
	src.set(
		target("gone", "ecs", "", []model.Service{{Ref: gone, Desired: model.Ptr(1), Running: model.Ptr(0)}}, nil),
		target("host", "docker", "", []model.Service{{Ref: img}}, []model.Instance{running(img, "a", model.Container{Name: "app", Image: "example/app:latest", HealthCheck: model.Ptr(true)})}),
	)
	*now = now.Add(-time.Hour)
	if err := e.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Hour)
	if err := e.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Findings(ctx, store.FindingQuery{State: store.StateOpen}); len(got) != 2 {
		t.Fatalf("open = %+v", got)
	}

	// The target leaves the configuration and hygiene is switched off.
	src.set(target("host", "docker", "", []model.Service{{Ref: img}}, nil))
	e.cfg.Disabled = []string{model.FindingHygiene}
	if err := e.Evaluate(ctx); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Findings(ctx, store.FindingQuery{State: store.StateOpen}); len(got) != 0 {
		t.Fatalf("still open = %+v", got)
	}
	var announced []string
	events, _ := st.FindingEvents(ctx, 0, 10)
	for _, ev := range events {
		announced = append(announced, ev.Transition+" "+ev.Finding.Rule)
	}
	if strings.Join(announced, ", ") != "opened latest-tag, opened below-desired, resolved below-desired" {
		t.Fatalf("announced = %v", announced)
	}
}
