// Package notify announces finding transitions to Slack incoming webhooks. It
// reads the transitions the evaluator stored, in order, and keeps each route's
// position in the store, so a failed delivery is retried and a restart picks
// up where it stopped. A webhook URL is a credential: it never reaches a log,
// an error or the store.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
)

// Store is where transitions and delivery positions are kept.
type Store interface {
	FindingEvents(ctx context.Context, after int64, limit int) ([]model.FindingEvent, error)
	LastFindingEvent(ctx context.Context) (int64, error)
	NotifyCursor(ctx context.Context, route string) (int64, bool, error)
	SetNotifyCursor(ctx context.Context, route string, event int64) error
}

const (
	pollEvery   = 10 * time.Second
	batch       = 50
	minBackoff  = pollEvery // a retry is only looked at on a tick
	maxBackoff  = 5 * time.Minute
	sendTimeout = 10 * time.Second
	maxServices = 3 // listed in a message before "and N more"
)

type route struct {
	name    string
	url     string
	targets []string
	kinds   map[string]bool // empty means every kind
	done    int64           // last transition handled; ahead of the stored position when recording it failed
	backoff time.Duration
	retryAt time.Time
}

// Notifier delivers transitions to every configured route.
type Notifier struct {
	routes    []*route
	st        Store
	client    *http.Client
	publicURL string
	log       *slog.Logger
	now       func() time.Time
	every     time.Duration
}

// New reads each route's webhook URL. An unreadable one fails start-up, as an
// OIDC client secret does, rather than leaving a channel silently unfed.
func New(cfg config.Notify, publicURL string, st Store, log *slog.Logger) (*Notifier, error) {
	n := &Notifier{st: st, publicURL: strings.TrimRight(publicURL, "/"), log: log, now: time.Now, every: pollEvery,
		client: &http.Client{Timeout: sendTimeout,
			// A redirect would hand the webhook URL on as Referer and turn the POST into a bodiless GET.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, rc := range cfg.Slack {
		b, err := os.ReadFile(rc.WebhookURLFile)
		if err != nil {
			return nil, fmt.Errorf("notify.slack %q: read webhookURLFile: %w", rc.Name, err)
		}
		raw := strings.TrimSpace(string(b))
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return nil, fmt.Errorf("notify.slack %q: %s does not hold an http(s) URL", rc.Name, rc.WebhookURLFile)
		}
		if loopback, _ := config.IsLoopback(net.JoinHostPort(u.Hostname(), "0")); u.Scheme == "http" && !loopback {
			return nil, fmt.Errorf("notify.slack %q: the URL in %s must be https, it is a credential", rc.Name, rc.WebhookURLFile)
		}
		r := &route{name: rc.Name, url: raw, targets: rc.Targets, kinds: map[string]bool{}}
		for _, k := range rc.Kinds {
			r.kinds[k] = true
		}
		n.routes = append(n.routes, r)
	}
	return n, nil
}

// Run delivers on every tick until ctx ends; without routes it returns at once.
func (n *Notifier) Run(ctx context.Context) {
	if len(n.routes) == 0 {
		return
	}
	n.Deliver(ctx)
	t := time.NewTicker(n.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n.Deliver(ctx)
		}
	}
}

// Deliver gives every route not waiting out a failure one pass.
func (n *Notifier) Deliver(ctx context.Context) {
	for _, r := range n.routes {
		if n.now().Before(r.retryAt) {
			continue
		}
		if err := n.deliver(ctx, r); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.backoff = min(max(2*r.backoff, minBackoff), maxBackoff)
			r.retryAt = n.now().Add(r.backoff)
			n.log.Warn("slack delivery failed", "route", r.name, "retry_in", r.backoff, "err", err)
			continue
		}
		r.backoff, r.retryAt = 0, time.Time{}
	}
}

// deliver sends everything after the route's position, moving the position
// past each transition only once it is sent or not wanted. A route seen for
// the first time starts at the latest transition rather than replaying history.
func (n *Notifier) deliver(ctx context.Context, r *route) error {
	cursor, ok, err := n.st.NotifyCursor(ctx, r.name)
	if err != nil {
		return fmt.Errorf("read position: %w", err)
	}
	if !ok {
		last, err := n.st.LastFindingEvent(ctx)
		if err != nil {
			return fmt.Errorf("read latest transition: %w", err)
		}
		return n.st.SetNotifyCursor(ctx, r.name, last)
	}
	if r.done > cursor {
		cursor = r.done
		if err := n.st.SetNotifyCursor(ctx, r.name, cursor); err != nil {
			return fmt.Errorf("record position: %w", err)
		}
	}
	for {
		events, err := n.st.FindingEvents(ctx, cursor, batch)
		if err != nil {
			return fmt.Errorf("read transitions: %w", err)
		}
		for _, ev := range events {
			if r.wants(ev.Finding) {
				if err := n.send(ctx, r, message(ev, n.publicURL)); err != nil {
					return err
				}
			}
			cursor = ev.ID
			r.done = cursor
			if err := n.st.SetNotifyCursor(ctx, r.name, cursor); err != nil {
				return fmt.Errorf("record position: %w", err)
			}
		}
		if len(events) < batch {
			return nil
		}
	}
}

func (r *route) wants(f model.Finding) bool {
	if len(r.kinds) > 0 && !r.kinds[f.Kind] {
		return false
	}
	targets := f.Targets()
	for _, t := range targets {
		if !config.MatchTarget(r.targets, t) {
			return false
		}
	}
	return len(targets) > 0
}

func (n *Notifier) send(ctx context.Context, r *route, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url, bytes.NewReader(body))
	if err != nil {
		return errors.New("build the request") // the parse error would quote the URL
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		// url.Error quotes the URL, whose path is the webhook's secret.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("post to slack: %w", err)
	}
	// Slack's error body ("invalid_token", "channel_is_archived") says what is wrong and never echoes the URL.
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack answered %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// message writes a transition as Slack text, with a link to the console when
// fleetbox knows its own address.
func message(ev model.FindingEvent, publicURL string) string {
	f := ev.Finding
	var refs []string
	for _, s := range f.Services[:min(len(f.Services), maxServices)] {
		refs = append(refs, s.String())
	}
	on := strings.Join(refs, ", ")
	if extra := len(f.Services) - maxServices; extra > 0 {
		on += fmt.Sprintf(" and %d more", extra)
	}
	var text, link string
	if ev.Transition == model.TransitionResolved {
		text = fmt.Sprintf("Resolved %s finding: %s on %s", f.Kind, escape(f.Summary), escape(on))
		link = publicURL + "/findings?state=resolved&kind=" + f.Kind
	} else {
		text = fmt.Sprintf("Opened %s finding (%s): %s on %s", f.Kind, f.Severity, escape(f.Summary), escape(on))
		link = publicURL + "/findings?kind=" + f.Kind
	}
	if publicURL != "" {
		text += "\n<" + escape(link) + "|Open in fleetbox>"
	}
	return text
}

// escape keeps Slack from reading text as markup.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}
