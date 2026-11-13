package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/store"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// secretPath stands in for the token a real webhook URL carries in its path.
const secretPath = "/services/T000/B000/s3cr3t"

// slack is an incoming webhook that records what it is sent and can be told to fail.
type slack struct {
	mu     sync.Mutex
	texts  []string
	status int
	srv    *httptest.Server
}

func newSlack(t *testing.T) *slack {
	t.Helper()
	s := &slack{status: http.StatusOK}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var body struct{ Text string }
		if r.URL.Path != secretPath || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if s.status == http.StatusOK {
			s.texts = append(s.texts, body.Text)
		}
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *slack) sent() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

func (s *slack) fail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func webhookFile(t *testing.T, url string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "webhook")
	if err := os.WriteFile(f, []byte(url+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func openStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// open stores an open finding on the given targets and returns its id.
func open(t *testing.T, st *store.SQLite, key, kind string, targets ...string) int64 {
	t.Helper()
	ctx := context.Background()
	var refs []model.ServiceRef
	for _, tg := range targets {
		refs = append(refs, model.ServiceRef{Target: tg, Scope: "web", Name: "api"})
	}
	f := model.Finding{Key: key, Kind: kind, Rule: "r", Severity: model.SeverityWarning, Summary: key + " <&>", Services: refs,
		FirstSeen: t0, LastSeen: t0, OpenedAt: model.Ptr(t0)}
	if err := st.SaveFindings(ctx, store.FindingChanges{At: t0, Save: []store.FindingSave{{Finding: f, Opened: true}}}); err != nil {
		t.Fatal(err)
	}
	active, _ := st.ActiveFindings(ctx)
	for _, a := range active {
		if a.Key == key {
			return a.ID
		}
	}
	t.Fatalf("finding %s not stored", key)
	return 0
}

func newNotifier(t *testing.T, st Store, routes ...config.SlackRoute) (*Notifier, *bytes.Buffer, *time.Time) {
	t.Helper()
	var buf bytes.Buffer
	n, err := New(config.Notify{Slack: routes}, "https://fleetbox.example.com/", st, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := t0
	n.now = func() time.Time { return now }
	return n, &buf, &now
}

func TestDeliver(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx := context.Background()
	open(t, st, "before", model.FindingStuck, "prod")
	n, _, _ := newNotifier(t, st,
		config.SlackRoute{Name: "prod", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"prod*"}, Kinds: []string{model.FindingStuck, model.FindingDrift}})

	// A new route starts at the latest transition: nothing from before is replayed.
	n.Deliver(ctx)
	if got := sl.sent(); len(got) != 0 {
		t.Fatalf("replayed %v", got)
	}

	open(t, st, "stuck", model.FindingStuck, "prod")
	open(t, st, "hygiene", model.FindingHygiene, "prod")       // a kind the route does not take
	open(t, st, "dev", model.FindingStuck, "dev")              // a target it does not take
	open(t, st, "drift", model.FindingDrift, "prod", "dev")    // one target it does not take
	open(t, st, "both", model.FindingDrift, "prod", "prod-eu") // every target matches
	id := open(t, st, "gone", model.FindingStuck, "prod")
	if err := st.SaveFindings(ctx, store.FindingChanges{At: t0, Resolve: []int64{id}}); err != nil {
		t.Fatal(err)
	}
	n.Deliver(ctx)
	want := []string{
		"Opened stuck finding (warning): stuck &lt;&amp;&gt; on prod/web/api\n<https://fleetbox.example.com/findings?kind=stuck|Open in fleetbox>",
		"Opened drift finding (warning): both &lt;&amp;&gt; on prod/web/api, prod-eu/web/api\n<https://fleetbox.example.com/findings?kind=drift|Open in fleetbox>",
		"Opened stuck finding (warning): gone &lt;&amp;&gt; on prod/web/api\n<https://fleetbox.example.com/findings?kind=stuck|Open in fleetbox>",
		"Resolved stuck finding: gone &lt;&amp;&gt; on prod/web/api\n<https://fleetbox.example.com/findings?state=resolved&amp;kind=stuck|Open in fleetbox>",
	}
	if got := sl.sent(); strings.Join(got, "\n---\n") != strings.Join(want, "\n---\n") {
		t.Fatalf("sent:\n%s", strings.Join(got, "\n---\n"))
	}
	// Everything read, wanted or not, is behind the route now.
	last, _ := st.LastFindingEvent(ctx)
	if cursor, _, _ := st.NotifyCursor(ctx, "prod"); cursor != last {
		t.Fatalf("cursor %d, last event %d", cursor, last)
	}
	n.Deliver(ctx)
	if got := sl.sent(); len(got) != len(want) {
		t.Fatalf("sent twice: %d", len(got))
	}
}

func TestDeliverRetriesWithBackoff(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx := context.Background()
	n, logs, now := newNotifier(t, st, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
	n.Deliver(ctx) // sets the starting position
	open(t, st, "a", model.FindingStuck, "prod")

	sl.fail(http.StatusInternalServerError)
	r := n.routes[0]
	for _, want := range []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second} {
		n.Deliver(ctx)
		if r.backoff != want || !r.retryAt.Equal(now.Add(want)) {
			t.Fatalf("backoff %s retry at %s, want %s", r.backoff, r.retryAt, want)
		}
		n.Deliver(ctx) // still waiting: nothing is attempted
		*now = r.retryAt
	}
	if cursor, _, _ := st.NotifyCursor(ctx, "ops"); cursor != 0 {
		t.Fatalf("a failed delivery moved the position to %d", cursor)
	}
	r.backoff = 4 * time.Minute
	*now = now.Add(time.Hour)
	n.Deliver(ctx)
	if r.backoff != maxBackoff {
		t.Fatalf("backoff %s past the cap", r.backoff)
	}

	sl.fail(http.StatusOK)
	*now = now.Add(time.Hour)
	n.Deliver(ctx)
	if got := sl.sent(); len(got) != 1 || r.backoff != 0 || !r.retryAt.IsZero() {
		t.Fatalf("after recovery: sent %v, backoff %s", got, r.backoff)
	}
	if !strings.Contains(logs.String(), "slack answered 500") {
		t.Fatalf("failure not logged: %s", logs)
	}
}

// The webhook URL's path is a credential and must not surface in any error or log.
func TestWebhookURLStaysSecret(t *testing.T) {
	sl := newSlack(t)
	url := sl.srv.URL + secretPath
	sl.srv.Close() // connection refused from now on
	st := openStore(t)
	ctx := context.Background()
	n, logs, now := newNotifier(t, st, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, url), Targets: []string{"*"}})
	n.Deliver(ctx)
	open(t, st, "a", model.FindingStuck, "prod")
	*now = now.Add(time.Hour)
	n.Deliver(ctx)
	if !strings.Contains(logs.String(), "post to slack") || strings.Contains(logs.String(), "s3cr3t") {
		t.Fatalf("log = %s", logs)
	}
	if err := n.send(ctx, &route{url: "http://exa mple.com" + secretPath}, "x"); err == nil || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("bad URL error = %v", err)
	}
}

func TestNew(t *testing.T) {
	dir := t.TempDir()
	for _, tt := range []struct{ name, content, want string }{
		{"not a URL", "s3cr3t-token", "does not hold an http(s) URL"},
		{"no host", "https://" + secretPath, "does not hold an http(s) URL"},
		{"wrong scheme", "ftp://hooks.example.com" + secretPath, "does not hold an http(s) URL"},
		{"clear text", "http://hooks.example.com" + secretPath, "must be https"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-"))
			if err := os.WriteFile(f, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := New(config.Notify{Slack: []config.SlackRoute{{Name: "ops", WebhookURLFile: f}}}, "", nil, slog.New(slog.DiscardHandler))
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "s3cr3t") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := New(config.Notify{Slack: []config.SlackRoute{{Name: "ops", WebhookURLFile: filepath.Join(dir, "missing")}}}, "", nil, nil); err == nil ||
		!strings.Contains(err.Error(), `notify.slack "ops": read webhookURLFile`) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestMessage(t *testing.T) {
	var refs []model.ServiceRef
	for i := range 5 {
		refs = append(refs, model.ServiceRef{Target: fmt.Sprintf("t%d", i), Scope: "s", Name: "n"})
	}
	ev := model.FindingEvent{Transition: model.TransitionOpened, Finding: model.Finding{Kind: model.FindingDrift, Severity: model.SeverityWarning,
		Summary: "api runs different images", Services: refs}}
	if got := message(ev, ""); got != "Opened drift finding (warning): api runs different images on t0/s/n, t1/s/n, t2/s/n and 2 more" {
		t.Fatalf("message = %q", got)
	}
}

func TestDeliverPagesThroughBacklog(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx := context.Background()
	n, _, _ := newNotifier(t, st, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
	n.Deliver(ctx)
	for i := range batch + 3 {
		open(t, st, fmt.Sprintf("f%02d", i), model.FindingStuck, "prod")
	}
	n.Deliver(ctx)
	if got := sl.sent(); len(got) != batch+3 || !strings.Contains(got[batch+2], "f52") {
		t.Fatalf("sent %d", len(got))
	}
}

// failingStore fails the named step.
type failingStore struct {
	Store
	op string
}

func (f failingStore) FindingEvents(ctx context.Context, after int64, limit int) ([]model.FindingEvent, error) {
	if f.op == "events" {
		return nil, fmt.Errorf("boom")
	}
	return f.Store.FindingEvents(ctx, after, limit)
}

func (f failingStore) LastFindingEvent(ctx context.Context) (int64, error) {
	if f.op == "last" {
		return 0, fmt.Errorf("boom")
	}
	return f.Store.LastFindingEvent(ctx)
}

func (f failingStore) NotifyCursor(ctx context.Context, route string) (int64, bool, error) {
	if f.op == "cursor" {
		return 0, false, fmt.Errorf("boom")
	}
	return f.Store.NotifyCursor(ctx, route)
}

func (f failingStore) SetNotifyCursor(ctx context.Context, route string, event int64) error {
	if f.op == "set" && event > 0 {
		return fmt.Errorf("boom")
	}
	return f.Store.SetNotifyCursor(ctx, route, event)
}

func TestDeliverStoreErrors(t *testing.T) {
	sl := newSlack(t)
	for op, want := range map[string]string{"cursor": "read position", "last": "read latest transition", "events": "read transitions", "set": "record position"} {
		t.Run(op, func(t *testing.T) {
			st := openStore(t)
			ctx := context.Background()
			if op != "last" {
				if err := st.SetNotifyCursor(ctx, "ops", 0); err != nil {
					t.Fatal(err)
				}
			}
			open(t, st, "a", model.FindingStuck, "prod")
			n, logs, _ := newNotifier(t, failingStore{Store: st, op: op},
				config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
			n.Deliver(ctx)
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("log = %s", logs)
			}
		})
	}
}

func TestRun(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := st.SetNotifyCursor(ctx, "ops", 0); err != nil {
		t.Fatal(err)
	}
	open(t, st, "a", model.FindingStuck, "prod")
	n, _, _ := newNotifier(t, st, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
	n.every = 10 * time.Millisecond
	done := make(chan struct{})
	go func() {
		n.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for len(sl.sent()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("nothing delivered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	// Without routes Run returns at once, and a cancelled delivery is not counted as a failure.
	none, _, _ := newNotifier(t, st)
	none.Run(context.Background())
	n.routes[0].backoff = 0
	n.Deliver(ctx)
	if n.routes[0].backoff != 0 {
		t.Fatalf("a cancelled delivery backed off %s", n.routes[0].backoff)
	}
}

// flakyCursor fails the next cursor write once.
type flakyCursor struct {
	Store
	fail bool
}

func (f *flakyCursor) SetNotifyCursor(ctx context.Context, route string, event int64) error {
	if f.fail && event > 0 {
		f.fail = false
		return fmt.Errorf("database is locked")
	}
	return f.Store.SetNotifyCursor(ctx, route, event)
}

// A message Slack accepted is not sent again because its position failed to record.
func TestDeliverSurvivesACursorWriteFailure(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx := context.Background()
	if err := st.SetNotifyCursor(ctx, "ops", 0); err != nil {
		t.Fatal(err)
	}
	open(t, st, "a", model.FindingStuck, "prod")
	fs := &flakyCursor{Store: st, fail: true}
	n, logs, now := newNotifier(t, fs, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
	n.Deliver(ctx)
	*now = now.Add(time.Hour)
	n.Deliver(ctx)
	if got := sl.sent(); len(got) != 1 || !strings.Contains(logs.String(), "record position") {
		t.Fatalf("sent %d, log %s", len(got), logs)
	}
	if cursor, _, _ := st.NotifyCursor(ctx, "ops"); cursor == 0 {
		t.Fatal("the position was never recorded")
	}
}

// A webhook that redirects is a failure: following it would leak the URL as Referer.
func TestRedirectIsNotFollowed(t *testing.T) {
	var reached bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
	defer other.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer redirect.Close()
	n, _, _ := newNotifier(t, nil)
	err := n.send(context.Background(), &route{url: redirect.URL + secretPath}, "x")
	if err == nil || !strings.Contains(err.Error(), "302") || strings.Contains(err.Error(), "s3cr3t") || reached {
		t.Fatalf("err = %v, other server reached %v", err, reached)
	}
}

func TestSlackErrorBodyIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("no_service\n"))
	}))
	defer srv.Close()
	n, _, _ := newNotifier(t, nil)
	if err := n.send(context.Background(), &route{url: srv.URL + secretPath}, "x"); err == nil || !strings.HasSuffix(err.Error(), "404 Not Found: no_service") {
		t.Fatalf("err = %v", err)
	}
}

// A position that could not be recorded is recorded on the next pass, even with nothing new to send.
func TestPendingPositionIsRecordedLater(t *testing.T) {
	sl := newSlack(t)
	st := openStore(t)
	ctx := context.Background()
	if err := st.SetNotifyCursor(ctx, "ops", 0); err != nil {
		t.Fatal(err)
	}
	open(t, st, "a", model.FindingStuck, "prod")
	fs := &flakyCursor{Store: st, fail: true}
	n, _, now := newNotifier(t, fs, config.SlackRoute{Name: "ops", WebhookURLFile: webhookFile(t, sl.srv.URL+secretPath), Targets: []string{"*"}})
	n.Deliver(ctx)
	fs.fail = true // the catch-up write fails too
	*now = now.Add(time.Hour)
	n.Deliver(ctx)
	if cursor, _, _ := st.NotifyCursor(ctx, "ops"); cursor != 0 || len(sl.sent()) != 1 {
		t.Fatalf("cursor %d, sent %d", cursor, len(sl.sent()))
	}
}
