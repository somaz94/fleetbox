package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
	"github.com/somaz94/fleetbox/internal/provider/providertest"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeContainer struct {
	summary containerSummary
	inspect containerJSON
	logs    []byte
}

type fakeDaemon struct {
	mu         sync.Mutex
	containers []fakeContainer
	images     map[string]imageJSON
	events     []eventMessage
	queries    map[string][]string // path -> raw queries
	posts      []string            // paths of POST requests, in order
	fail       map[string]int      // path prefix -> status to answer
}

func (d *fakeDaemon) record(r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.queries == nil {
		d.queries = map[string][]string{}
	}
	d.queries[r.URL.Path] = append(d.queries[r.URL.Path], r.URL.RawQuery)
	if r.Method == http.MethodPost {
		d.posts = append(d.posts, r.URL.Path)
	}
}

func (d *fakeDaemon) find(id string) (fakeContainer, bool) {
	for _, c := range d.containers {
		if strings.HasPrefix(c.summary.ID, id) || c.inspect.Name == "/"+id {
			return c, true
		}
	}
	return fakeContainer{}, false
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(apiError{Message: msg})
}

func (d *fakeDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		var out []containerSummary
		for _, c := range d.containers {
			out = append(out, c.summary)
		}
		json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /containers/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		c, ok := d.find(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "No such container")
			return
		}
		json.NewEncoder(w).Encode(c.inspect)
	})
	mux.HandleFunc("GET /images/{id}/json", func(w http.ResponseWriter, r *http.Request) {
		img, ok := d.images[r.PathValue("id")]
		if !ok {
			writeErr(w, http.StatusNotFound, "No such image")
			return
		}
		json.NewEncoder(w).Encode(img)
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		enc := json.NewEncoder(w)
		for _, e := range d.events {
			enc.Encode(e)
		}
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("until") == "" {
			<-r.Context().Done() // a live watch stays open
		}
	})
	mux.HandleFunc("POST /containers/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := d.find(r.PathValue("id")); !ok {
			writeErr(w, http.StatusNotFound, "No such container")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		c, ok := d.find(r.PathValue("id"))
		if !ok {
			writeErr(w, http.StatusNotFound, "No such container")
			return
		}
		w.Write(c.logs)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.record(r)
		for prefix, status := range d.fail {
			if strings.HasPrefix(r.URL.Path, prefix) {
				writeErr(w, status, "forced failure")
				return
			}
		}
		mux.ServeHTTP(w, r)
	})
}

type tcpDialer struct{ addr string }

func (d tcpDialer) DialContext(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "tcp", d.addr)
}

func frame(stream byte, s string) []byte {
	b := make([]byte, 8, 8+len(s))
	b[0] = stream
	binary.BigEndian.PutUint32(b[4:], uint32(len(s)))
	return append(b, s...)
}

func running(id, name, project, service, image string, labels map[string]string) fakeContainer {
	l := map[string]string{}
	if project != "" {
		l[labelProject], l[labelService] = project, service
	}
	for k, v := range labels {
		l[k] = v
	}
	c := fakeContainer{
		summary: containerSummary{ID: id, Names: []string{"/" + name}, Image: "example/" + name, ImageID: image, State: "running",
			Status: "Up 2 hours", Labels: l},
	}
	c.inspect.ID, c.inspect.Name = id, "/"+name
	c.inspect.State.Status = "running"
	c.inspect.State.StartedAt = t0.Format(time.RFC3339Nano)
	c.inspect.State.FinishedAt = "0001-01-01T00:00:00Z"
	c.inspect.Config.Labels = l
	return c
}

func newDaemon() *fakeDaemon {
	web1 := running("aaaaaaaaaa01ffff", "shop-web-1", "shop", "web", "sha256:img1", map[string]string{"fleetbox.io/app": "web"})
	web1.summary.Ports = []struct {
		PrivatePort int    `json:"PrivatePort"`
		PublicPort  int    `json:"PublicPort"`
		Type        string `json:"Type"`
	}{{PrivatePort: 8080, PublicPort: 80, Type: "tcp"}, {PrivatePort: 8080, PublicPort: 80, Type: "tcp"}, {PrivatePort: 9090, Type: "tcp"}}
	web1.inspect.State.Health = &struct {
		Status string `json:"Status"`
		Log    []struct {
			ExitCode int    `json:"ExitCode"`
			Output   string `json:"Output"`
		} `json:"Log"`
	}{Status: "healthy"}
	web1.logs = append(append(append(
		frame(1, "2026-01-01T00:00:00.000000001Z hello\n2026-01-01T00:00:01Z wor"),
		frame(1, "ld\n")...),
		frame(2, "2026-01-01T00:00:02Z oops")...),
		frame(1, "no timestamp\n")...)

	web2 := running("aaaaaaaaaa02ffff", "shop-web-2", "shop", "web", "sha256:img1", nil)
	web2.inspect.RestartCount = 3
	web2.inspect.Config.Healthcheck = &struct {
		Test []string `json:"Test"`
	}{Test: []string{"CMD-SHELL", "curl -f http://localhost/"}}

	worker := running("bbbbbbbbbb01ffff", "shop-worker-1", "shop", "worker", "sha256:gone", nil)
	worker.summary.State, worker.summary.Status = "exited", "Exited (137) 5 minutes ago"
	worker.inspect.State.Status, worker.inspect.State.ExitCode, worker.inspect.State.OOMKilled = "exited", 137, true
	worker.inspect.State.FinishedAt = t0.Add(time.Hour).Format(time.RFC3339Nano)
	worker.inspect.State.Error = "task failed"
	worker.inspect.State.Health = &struct {
		Status string `json:"Status"`
		Log    []struct {
			ExitCode int    `json:"ExitCode"`
			Output   string `json:"Output"`
		} `json:"Log"`
	}{Status: "unhealthy", Log: []struct {
		ExitCode int    `json:"ExitCode"`
		Output   string `json:"Output"`
	}{{ExitCode: 1, Output: "curl: connection refused\n"}}}

	api1 := running("cccccccccc01ffff", "shop-api-1", "shop", "api", "sha256:img1", nil)
	api1.inspect.Config.Healthcheck = &struct {
		Test []string `json:"Test"`
	}{Test: []string{"NONE"}}
	api2 := running("cccccccccc02ffff", "shop-api-2", "shop", "api", "sha256:img4", nil)

	redis := running("dddddddddd01ffff", "redis", "", "", "sha256:local", nil)
	redis.inspect.Config.Tty = true
	redis.logs = []byte("2026-01-01T00:00:00Z ready\r\npartial")

	return &fakeDaemon{
		containers: []fakeContainer{web1, web2, worker, api1, api2, redis},
		images: map[string]imageJSON{
			"sha256:img1":  {RepoDigests: []string{"example/web@sha256:d1"}, Created: "2025-09-22T10:11:12.123456789Z"},
			"sha256:img4":  {RepoDigests: []string{"example/web@sha256:d4"}, Created: "1970-01-01T00:00:00Z"},
			"sha256:local": {},
		},
		events: []eventMessage{
			event("aaaaaaaaaa01ffff", "shop-web-1", "start", t0, nil),
			event("aaaaaaaaaa01ffff", "shop-web-1", "exec_start: sh", t0.Add(time.Second), nil),
			event("aaaaaaaaaa01ffff", "shop-web-1", "health_status: unhealthy", t0.Add(2*time.Second), nil),
			event("aaaaaaaaaa01ffff", "shop-web-1", "die", t0.Add(3*time.Second), map[string]string{"exitCode": "1"}),
			event("aaaaaaaaaa01ffff", "shop-web-1", "die", t0.Add(4*time.Second), map[string]string{"exitCode": "0"}),
			event("aaaaaaaaaa01ffff", "shop-web-1", "oom", t0.Add(5*time.Second), nil),
		},
	}
}

func event(id, name, action string, at time.Time, attrs map[string]string) eventMessage {
	m := eventMessage{Type: "container", Action: action, TimeNano: at.UnixNano()}
	m.Actor.ID = id
	m.Actor.Attributes = map[string]string{"name": name}
	for k, v := range attrs {
		m.Actor.Attributes[k] = v
	}
	return m
}

func serve(t *testing.T, d *fakeDaemon) *Provider {
	t.Helper()
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	p := NewWithDialer("host1", tcpDialer{addr: strings.TrimPrefix(srv.URL, "http://")})
	p.now = func() time.Time { return t0.Add(time.Hour) }
	return p
}

var webRef = model.ServiceRef{Target: "host1", Scope: "shop", Name: "web"}

func TestConformance(t *testing.T) {
	providertest.Run(t, func(t *testing.T) provider.Provider { return serve(t, newDaemon()) }, providertest.Fixture{
		Target:        "host1",
		Known:         webRef,
		KnownInstance: model.InstanceRef{Service: webRef, ID: "aaaaaaaaaa01"},
		Missing:       model.ServiceRef{Target: "host1", Scope: "shop", Name: "nope"},
	})
}

func TestInventory(t *testing.T) {
	d := newDaemon()
	p := serve(t, d)
	inv, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	svcs := map[string]model.Service{}
	for _, s := range inv.Services {
		svcs[s.Ref.String()] = s
	}
	if len(svcs) != 4 {
		t.Fatalf("services = %v", svcs)
	}
	web := svcs["host1/shop/web"]
	if *web.Running != 2 || web.Revision != "sha256:d1" || web.Desired != nil || web.Labels["fleetbox.io/app"] != "web" {
		t.Fatalf("web = %+v", web)
	}
	if api := svcs["host1/shop/api"]; api.Revision != "" || *api.Running != 2 {
		t.Fatalf("mixed digests must leave revision absent: %+v", api)
	}
	if worker := svcs["host1/shop/worker"]; *worker.Running != 0 || worker.Revision != "" {
		t.Fatalf("worker = %+v", worker)
	}
	if redis := svcs["host1/_standalone/redis"]; redis.Revision != "" {
		t.Fatalf("locally built image must have no digest: %+v", redis)
	}

	byID := map[string]model.Instance{}
	for _, in := range inv.Instances {
		byID[in.Ref.ID] = in
	}
	w1 := byID["aaaaaaaaaa01"]
	if w1.Status != model.StatusRunning || w1.Health != model.HealthHealthy || !w1.StartedAt.Equal(t0) || w1.StoppedAt != nil || *w1.Restarts != 0 {
		t.Fatalf("web-1 = %+v", w1)
	}
	c := w1.Containers[0]
	if c.Name != "shop-web-1" || c.Digest != "sha256:d1" || len(c.Ports) != 2 || *c.Ports[0].Host != 80 || c.Ports[1].Host != nil || c.ExitCode != nil {
		t.Fatalf("web-1 container = %+v", c)
	}
	if !c.ImageCreated.Equal(time.Date(2025, 9, 22, 10, 11, 12, 123456789, time.UTC)) || !*c.HealthCheck {
		t.Fatalf("web-1 image time and health check = %v, %v", c.ImageCreated, c.HealthCheck)
	}
	// A health check comes from the container's configuration even before it
	// has run, and NONE switches an inherited one off.
	for id, want := range map[string]bool{"aaaaaaaaaa02": true, "cccccccccc01": false, "dddddddddd01": false} {
		if got := byID[id].Containers[0].HealthCheck; got == nil || *got != want {
			t.Fatalf("%s health check = %v, want %v", id, got, want)
		}
	}
	for _, id := range []string{"dddddddddd01", "bbbbbbbbbb01", "cccccccccc02"} {
		if got := byID[id].Containers[0].ImageCreated; got != nil {
			t.Fatalf("%s image time = %v; none, removed or epoch-stamped images must report none", id, got)
		}
	}
	wk := byID["bbbbbbbbbb01"]
	if wk.Status != model.StatusStopped || *wk.Containers[0].ExitCode != 137 || !wk.StoppedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("worker = %+v", wk)
	}
	for _, want := range []string{"exited with code 137", "OOMKilled", "task failed", "connection refused"} {
		if !strings.Contains(wk.StopReason, want) {
			t.Fatalf("stop reason %q lacks %q", wk.StopReason, want)
		}
	}
	if rec, ok := model.StopRecordOf(wk); !ok || *rec.ExitCode != 137 {
		t.Fatalf("stop record = %+v, %v", rec, ok)
	}

	// Image digests are resolved once per image ID.
	if _, err := p.Inventory(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(d.queries["/images/sha256:img1/json"]); n != 1 {
		t.Fatalf("image inspected %d times", n)
	}
}

func TestInventoryRaces(t *testing.T) {
	d := newDaemon()
	// Listed, then removed before inspect: skipped, not an error.
	d.containers = append(d.containers, fakeContainer{summary: containerSummary{ID: "eeeeeeeeee01ffff", Names: []string{"/gone"}, State: "exited"}})
	d.containers[len(d.containers)-1].inspect.Name = "/still-listed"
	gone := d.containers[len(d.containers)-1]
	d.containers = d.containers[:len(d.containers)-1]
	h := d.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/containers/json" {
			var out []containerSummary
			for _, c := range append(d.containers, gone) {
				out = append(out, c.summary)
			}
			json.NewEncoder(w).Encode(out)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	p := NewWithDialer("host1", tcpDialer{addr: strings.TrimPrefix(srv.URL, "http://")})
	inv, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range inv.Instances {
		if in.Ref.ID == "eeeeeeeeee01" {
			t.Fatal("removed container listed")
		}
	}
}

func TestInventoryErrors(t *testing.T) {
	for _, path := range []string{"/containers/json", "/containers/aaaaaaaaaa01ffff/json", "/images/"} {
		t.Run(path, func(t *testing.T) {
			d := newDaemon()
			d.fail = map[string]int{path: http.StatusInternalServerError}
			_, err := serve(t, d).Inventory(context.Background())
			if err == nil || !strings.Contains(err.Error(), "forced failure") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	t.Run("bad json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{")) }))
		defer srv.Close()
		p := NewWithDialer("host1", tcpDialer{addr: strings.TrimPrefix(srv.URL, "http://")})
		if _, err := p.Inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := ln.Addr().String()
		ln.Close()
		p := NewWithDialer("host1", tcpDialer{addr: addr})
		if _, err := p.Inventory(context.Background()); !errors.Is(err, provider.ErrUnreachable) {
			t.Fatalf("err = %v, want ErrUnreachable", err)
		}
	})
}

func TestService(t *testing.T) {
	p := serve(t, newDaemon())
	d, err := p.Service(context.Background(), webRef)
	if err != nil {
		t.Fatal(err)
	}
	if d.Service.Ref != webRef || len(d.Instances) != 2 {
		t.Fatalf("detail = %+v", d)
	}
	redis := model.ServiceRef{Target: "host1", Scope: StandaloneScope, Name: "redis"}
	if d, err := p.Service(context.Background(), redis); err != nil || len(d.Instances) != 1 {
		t.Fatalf("standalone: %+v, %v", d, err)
	}

	dm := newDaemon()
	dm.fail = map[string]int{"/containers/json": http.StatusInternalServerError}
	if _, err := serve(t, dm).Service(context.Background(), webRef); err == nil {
		t.Fatal("list failure not returned")
	}
	dm = newDaemon()
	dm.fail = map[string]int{"/images/": http.StatusInternalServerError}
	if _, err := serve(t, dm).Service(context.Background(), webRef); err == nil {
		t.Fatal("build failure not returned")
	}
}

func TestEvents(t *testing.T) {
	d := newDaemon()
	p := serve(t, d)
	evs, err := p.Events(context.Background(), webRef, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("events = %+v", evs)
	}
	want := []struct {
		sev, msg string
	}{
		{model.SeverityWarning, "container shop-web-1 oom"},
		{model.SeverityInfo, "container shop-web-1 die (exit code 0)"},
		{model.SeverityWarning, "container shop-web-1 die (exit code 1)"},
		{model.SeverityWarning, "container shop-web-1 health_status: unhealthy"},
		{model.SeverityInfo, "container shop-web-1 start"},
	}
	for i, w := range want {
		if evs[i].Severity != w.sev || evs[i].Message != w.msg || evs[i].Instance != "aaaaaaaaaa01" {
			t.Fatalf("event %d = %+v, want %+v", i, evs[i], w)
		}
	}
	q := d.queries["/events"][0]
	for _, part := range []string{"since=" + itoa(t0.Add(-23*time.Hour).Unix()), "until=" + itoa(t0.Add(time.Hour).Unix()), "com.docker.compose.service%3Dweb"} {
		if !strings.Contains(q, part) {
			t.Fatalf("query %q lacks %q", q, part)
		}
	}

	if _, err := p.Events(context.Background(), model.ServiceRef{Target: "host1", Scope: StandaloneScope, Name: "redis"}, t0); err != nil {
		t.Fatal(err)
	}
	if q := d.queries["/events"][1]; !strings.Contains(q, "redis") || !strings.Contains(q, "since="+itoa(t0.Unix())) {
		t.Fatalf("standalone query = %q", q)
	}

	d.fail = map[string]int{"/events": http.StatusInternalServerError}
	if _, err := p.Events(context.Background(), webRef, time.Time{}); err == nil {
		t.Fatal("events failure not returned")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{bad")) }))
	defer bad.Close()
	bp := NewWithDialer("host1", tcpDialer{addr: strings.TrimPrefix(bad.URL, "http://")})
	if _, err := bp.Events(context.Background(), webRef, t0); err == nil || !strings.Contains(err.Error(), "docker events") {
		t.Fatalf("err = %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func readAll(t *testing.T, s provider.LogStream) []model.LogLine {
	t.Helper()
	var out []model.LogLine
	for {
		l, err := s.Recv()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
}

func TestLogs(t *testing.T) {
	d := newDaemon()
	p := serve(t, d)

	s, err := p.Logs(context.Background(), model.InstanceRef{Service: webRef, ID: "aaaaaaaaaa01"}, provider.LogOptions{Since: t0, Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	lines := readAll(t, s)
	s.Close()
	if len(lines) != 4 {
		t.Fatalf("lines = %+v", lines)
	}
	if lines[0].Text != "hello" || lines[0].Stream != "stdout" || lines[0].Time.Nanosecond() != 1 {
		t.Fatalf("line 0 = %+v", lines[0])
	}
	if lines[1].Text != "world" || lines[2].Text != "no timestamp" || lines[2].Time != nil {
		t.Fatalf("lines = %+v", lines)
	}
	if lines[3].Text != "oops" || lines[3].Stream != "stderr" {
		t.Fatalf("flushed partial = %+v", lines[3])
	}
	q := d.queries["/containers/aaaaaaaaaa01ffff/logs"][0]
	for _, part := range []string{"follow=true", "tail=200", "since=" + itoa(t0.Unix()), "timestamps=1"} {
		if !strings.Contains(q, part) {
			t.Fatalf("query %q lacks %q", q, part)
		}
	}

	redis := model.InstanceRef{Service: model.ServiceRef{Target: "host1", Scope: StandaloneScope, Name: "redis"}, ID: "dddddddddd01"}
	s, err = p.Logs(context.Background(), redis, provider.LogOptions{Tail: 5})
	if err != nil {
		t.Fatal(err)
	}
	lines = readAll(t, s)
	if len(lines) != 2 || lines[0].Text != "ready" || lines[1].Text != "partial" || lines[1].Stream != "stdout" {
		t.Fatalf("tty lines = %+v", lines)
	}

	wrongSvc := model.InstanceRef{Service: model.ServiceRef{Target: "host1", Scope: "shop", Name: "api"}, ID: "aaaaaaaaaa01"}
	if _, err := p.Logs(context.Background(), wrongSvc, provider.LogOptions{}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if _, err := p.Logs(context.Background(), model.InstanceRef{Service: webRef, ID: "ffffffffffff"}, provider.LogOptions{}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	d.fail = map[string]int{"/containers/aaaaaaaaaa01ffff/logs": http.StatusInternalServerError}
	if _, err := p.Logs(context.Background(), model.InstanceRef{Service: webRef, ID: "aaaaaaaaaa01"}, provider.LogOptions{}); err == nil {
		t.Fatal("logs failure not returned")
	}
}

func TestLogStreamTruncatedFrame(t *testing.T) {
	full := frame(1, "2026-01-01T00:00:00Z complete\n")
	for _, body := range [][]byte{append(full, 1, 0, 0), append(full, frame(1, "cut short")[:12]...)} {
		s := newLogStream(io.NopCloser(strings.NewReader(string(body))), false)
		lines := readAll(t, s)
		if len(lines) != 1 || lines[0].Text != "complete" {
			t.Fatalf("lines = %+v", lines)
		}
	}
	s := newLogStream(io.NopCloser(errReader{}), false)
	if _, err := s.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("err = %v", err)
	}
	s = newLogStream(io.NopCloser(io.MultiReader(strings.NewReader(string(frame(1, "x")[:8])), errReader{})), false)
	if _, err := s.Recv(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("payload err = %v", err)
	}
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("reset") }

func TestWatch(t *testing.T) {
	p := serve(t, newDaemon())
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := p.Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("no change signalled")
	}
	cancel()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				goto closed
			}
		case <-deadline:
			t.Fatal("watch channel not closed after cancel")
		}
	}
closed:
	d := newDaemon()
	d.fail = map[string]int{"/events": http.StatusInternalServerError}
	if _, err := serve(t, d).Watch(context.Background()); err == nil {
		t.Fatal("watch failure not returned")
	}
}

func TestInstanceDetail(t *testing.T) {
	tests := []struct {
		name, inspect string
		check         func(t *testing.T, in model.Instance)
	}{
		{name: "everything set", inspect: `{"Created": "2024-03-01T10:00:00.5Z",
			"HostConfig": {"RestartPolicy": {"Name": "on-failure", "MaximumRetryCount": 5}, "NanoCpus": 1500000000, "Memory": 536870912},
			"Mounts": [{"Type": "bind", "Source": "/data/repo", "Destination": "/srv", "RW": true},
			           {"Type": "volume", "Name": "pgdata", "Source": "/var/lib/docker/volumes/pgdata/_data", "Destination": "/var/lib/postgresql", "RW": false}],
			"NetworkSettings": {"Networks": {"lan": {"IPAddress": "192.0.2.6"}, "bridge": {"IPAddress": "172.17.0.2"}, "none": {"IPAddress": ""}}}}`,
			check: func(t *testing.T, in model.Instance) {
				if in.CreatedAt == nil || !in.CreatedAt.Equal(time.Date(2024, 3, 1, 10, 0, 0, 5e8, time.UTC)) {
					t.Fatalf("created = %v", in.CreatedAt)
				}
				if in.RestartPolicy != "on-failure:5" || *in.CPU != 1.5 || *in.MemoryMiB != 512 {
					t.Fatalf("policy %q cpu %v memory %v", in.RestartPolicy, *in.CPU, *in.MemoryMiB)
				}
				want := []model.Mount{{Type: "bind", Source: "/data/repo", Destination: "/srv"},
					{Type: "volume", Source: "pgdata", Destination: "/var/lib/postgresql", ReadOnly: true}}
				if !slices.Equal(in.Mounts, want) {
					t.Fatalf("mounts = %+v", in.Mounts)
				}
				if !slices.Equal(in.Addresses, []model.Address{{Network: "bridge", IP: "172.17.0.2"}, {Network: "lan", IP: "192.0.2.6"}}) {
					t.Fatalf("addresses = %+v", in.Addresses)
				}
			}},
		{name: "unlimited and unnamed policy", inspect: `{"HostConfig": {"RestartPolicy": {"Name": ""}}}`,
			check: func(t *testing.T, in model.Instance) {
				if in.RestartPolicy != "no" || in.CPU != nil || in.MemoryMiB != nil || in.Mounts != nil || in.Addresses != nil {
					t.Fatalf("instance = %+v", in)
				}
			}},
		{name: "cpu from quota", inspect: `{"HostConfig": {"RestartPolicy": {"Name": "unless-stopped"}, "CpuQuota": 50000, "CpuPeriod": 200000, "Memory": 1024}}`,
			check: func(t *testing.T, in model.Instance) {
				if in.RestartPolicy != "unless-stopped" || *in.CPU != 0.25 || *in.MemoryMiB != 1 {
					t.Fatalf("policy %q cpu %v memory %v", in.RestartPolicy, *in.CPU, *in.MemoryMiB)
				}
			}},
		{name: "quota alone uses the kernel period", inspect: `{"HostConfig": {"RestartPolicy": {"Name": "on-failure"}, "CpuQuota": 50000}}`,
			check: func(t *testing.T, in model.Instance) {
				if in.RestartPolicy != "on-failure" || *in.CPU != 0.5 {
					t.Fatalf("policy %q cpu %v", in.RestartPolicy, *in.CPU)
				}
			}},
		{name: "secret sources hidden, unnamed volume keeps its path", inspect: `{"Mounts": [
			{"Type": "bind", "Source": "/home/deploy/db_password.txt", "Destination": "/run/secrets/db_password"},
			{"Type": "volume", "Source": "/var/lib/docker/volumes/x/_data", "Destination": "/cache", "RW": true}]}`,
			check: func(t *testing.T, in model.Instance) {
				want := []model.Mount{{Type: "bind", Destination: "/run/secrets/db_password", ReadOnly: true},
					{Type: "volume", Source: "/var/lib/docker/volumes/x/_data", Destination: "/cache"}}
				if !slices.Equal(in.Mounts, want) {
					t.Fatalf("mounts = %+v", in.Mounts)
				}
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cj containerJSON
			if err := json.Unmarshal([]byte(tt.inspect), &cj); err != nil {
				t.Fatal(err)
			}
			var in model.Instance
			addDetail(&in, cj)
			tt.check(t, in)
		})
	}
}

func TestMappings(t *testing.T) {
	for in, want := range map[string]string{
		"running": model.StatusRunning, "created": model.StatusPending, "restarting": model.StatusPending,
		"exited": model.StatusStopped, "dead": model.StatusStopped, "paused": model.StatusUnknown,
	} {
		if got := statusOf(in); got != want {
			t.Fatalf("statusOf(%q) = %q", in, got)
		}
	}
	for in, want := range map[string]string{"healthy": model.HealthHealthy, "unhealthy": model.HealthUnhealthy, "starting": model.HealthStarting, "none": ""} {
		if got := healthOf(in); got != want {
			t.Fatalf("healthOf(%q) = %q", in, got)
		}
	}
	if parseTime("0001-01-01T00:00:00Z") != nil || parseTime("garbage") != nil {
		t.Fatal("zero or bad time must be absent")
	}
	if containerName(nil) != "" || shortID("abc") != "abc" {
		t.Fatal("helpers broken")
	}
	if _, ok := mapEvent(webRef, eventMessage{Action: "attach"}); ok {
		t.Fatal("noise event kept")
	}
}

func TestRestart(t *testing.T) {
	d := newDaemon()
	migrate := running("eeeeeeeeee01ffff", "shop-web-run-1", "shop", "web", "sha256:img1", map[string]string{labelOneOff: "True"})
	migrate.summary.State = "exited"
	d.containers = append(d.containers, migrate)
	p := serve(t, d)
	ctx := context.Background()
	if err := p.Restart(ctx, webRef); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(d.posts, " "); got != "/containers/aaaaaaaaaa01ffff/restart /containers/aaaaaaaaaa02ffff/restart" {
		t.Fatalf("posts = %s", got)
	}
	standalone := model.ServiceRef{Target: "host1", Scope: StandaloneScope, Name: "redis"}
	if err := p.Restart(ctx, standalone); err != nil || d.posts[len(d.posts)-1] != "/containers/dddddddddd01ffff/restart" {
		t.Fatalf("standalone restart: %v, posts %v", err, d.posts)
	}
	if err := p.Restart(ctx, model.ServiceRef{Target: "host1", Scope: "shop", Name: "nope"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown service: %v", err)
	}

	// A container recreated since the list is skipped; any other failure stops the rest.
	d.posts = nil
	d.fail = map[string]int{"/containers/aaaaaaaaaa01ffff/restart": http.StatusNotFound}
	if err := p.Restart(ctx, webRef); err != nil || len(d.posts) != 2 {
		t.Fatalf("vanished container: %v, posts %v", err, d.posts)
	}
	d.fail = map[string]int{"/containers/aaaaaaaaaa01ffff/restart": http.StatusNotFound, "/containers/aaaaaaaaaa02ffff/restart": http.StatusNotFound}
	if err := p.Restart(ctx, webRef); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("every container vanished: %v", err)
	}
	d.posts = nil
	d.fail = map[string]int{"/containers/aaaaaaaaaa01ffff/restart": http.StatusInternalServerError}
	err := p.Restart(ctx, webRef)
	if err == nil || !strings.Contains(err.Error(), "aaaaaaaaaa01 (1 of 2)") || len(d.posts) != 1 {
		t.Fatalf("failing first container: %v, posts %v", err, d.posts)
	}
	d.fail = map[string]int{"/containers/json": http.StatusInternalServerError}
	if err := p.Restart(ctx, webRef); err == nil {
		t.Fatal("restart with a failing list")
	}
	if err := p.Scale(ctx, webRef, 2); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("scale: %v", err)
	}
}
