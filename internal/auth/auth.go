// Package auth identifies the caller of a request and decides what it may see.
//
// Three modes, chosen by configuration: none (loopback only, everyone is an
// operator), oidc (the server runs the login and keeps the session) and proxy
// (an authenticating sidecar sets identity headers). Authorisation is the same
// in every mode: OIDC or proxy groups mapped to target globs.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/store"
)

// Modes.
const (
	ModeNone  = "none"
	ModeOIDC  = "oidc"
	ModeProxy = "proxy"
)

// CSRFHeader must be present on every state-changing request. A browser adds
// a custom header to a cross-origin request only after a CORS preflight,
// which fleetbox never grants.
const CSRFHeader = "X-Fleetbox-CSRF"

// SessionCookie holds the session token in oidc mode. Over https it carries
// the __Host- prefix.
const SessionCookie = "fleetbox_session"

// ErrUnauthenticated means the request carries no valid identity.
var ErrUnauthenticated = errors.New("authentication required")

// Principal is the caller of a request.
type Principal struct {
	User string
	// Subject is the OIDC sub claim, which survives a rename of User. It is
	// empty in the other modes.
	Subject string
	Groups  []string
}

// Sessions is the persistence oidc mode needs.
type Sessions interface {
	CreateSession(ctx context.Context, s store.Session) error
	Session(ctx context.Context, id string, now time.Time) (store.Session, error)
	DeleteSession(ctx context.Context, id string) error
	PruneSessions(ctx context.Context, now time.Time) (int, error)
}

// Service authenticates requests and maps principals to roles.
type Service struct {
	mode          string
	origin        string // expected Origin of state-changing requests; derived from the request when empty
	secure        bool
	sessionCookie string
	rules         []config.RoleRule
	proxy         *config.Proxy
	oidc          *oidcFlow
	sessions      Sessions
	now           func() time.Time
}

// New builds the service for the configured mode. It reads the OIDC client
// secret but does not contact the issuer: discovery happens on the first login,
// so an issuer outage never stops the server or signed-in users.
func New(cfg *config.Config, sessions Sessions, log *slog.Logger) (*Service, error) {
	s := &Service{mode: ModeNone, rules: cfg.Auth.Roles, sessions: sessions, now: time.Now}
	if cfg.Server.PublicURL != "" {
		u, err := url.Parse(cfg.Server.PublicURL)
		if err != nil {
			return nil, fmt.Errorf("server.publicURL: %w", err)
		}
		s.origin, s.secure = origin(u), u.Scheme == "https"
	}
	s.sessionCookie = s.cookieName(SessionCookie)
	switch {
	case cfg.Auth.OIDC != nil:
		f, err := newOIDCFlow(cfg, s, log)
		if err != nil {
			return nil, err
		}
		s.mode, s.oidc = ModeOIDC, f
	case cfg.Auth.Proxy != nil:
		s.mode, s.proxy = ModeProxy, cfg.Auth.Proxy
	}
	return s, nil
}

// Mode reports which of ModeNone, ModeOIDC and ModeProxy is active.
func (s *Service) Mode() string { return s.mode }

// Authenticate identifies the caller. It returns ErrUnauthenticated when the
// request carries no valid identity, and any other error when one could not be
// checked.
func (s *Service) Authenticate(r *http.Request) (Principal, error) {
	switch s.mode {
	case ModeProxy:
		user := strings.TrimSpace(r.Header.Get(s.proxy.UserHeader))
		if user == "" {
			return Principal{}, ErrUnauthenticated
		}
		p := Principal{User: user, Groups: []string{}}
		if s.proxy.GroupsHeader != "" {
			for _, v := range r.Header.Values(s.proxy.GroupsHeader) {
				for _, g := range strings.Split(v, ",") {
					if g = strings.TrimSpace(g); g != "" {
						p.Groups = append(p.Groups, g)
					}
				}
			}
		}
		return p, nil
	case ModeOIDC:
		c, err := r.Cookie(s.sessionCookie)
		if err != nil || c.Value == "" {
			return Principal{}, ErrUnauthenticated
		}
		sess, err := s.sessions.Session(r.Context(), hashToken(c.Value), s.now())
		if errors.Is(err, store.ErrNotFound) {
			return Principal{}, ErrUnauthenticated
		}
		if err != nil {
			return Principal{}, fmt.Errorf("read session: %w", err)
		}
		return Principal{User: sess.User, Subject: sess.Subject, Groups: sess.Groups}, nil
	default:
		// A DNS-rebound page reaches loopback under its own host name, so its
		// Origin and Host agree; only a loopback Host proves a local caller.
		host := r.Host
		if _, _, err := net.SplitHostPort(host); err != nil {
			host += ":80"
		}
		if ok, _ := config.IsLoopback(host); !ok {
			return Principal{}, fmt.Errorf("%w: host %q is not a loopback name", ErrUnauthenticated, r.Host)
		}
		return Principal{User: "anonymous", Groups: []string{}}, nil
	}
}

// cookieName adds the __Host- prefix to a Secure cookie, which the browser
// then refuses to accept from a sibling subdomain.
func (s *Service) cookieName(base string) string {
	if s.secure {
		return "__Host-" + base
	}
	return base
}

// origin is the form a browser sends in the Origin header: lowercase host,
// no default port.
func origin(u *url.URL) string {
	host, port := strings.ToLower(u.Hostname()), u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return u.Scheme + "://" + host
}

// Role returns the most permissive role any rule grants p on target, or ""
// when none does. Without authentication everyone is an operator: that mode
// only ever listens on loopback.
func (s *Service) Role(p Principal, target string) string {
	if s.mode == ModeNone {
		return config.RoleOperator
	}
	best := ""
	for _, r := range s.rules {
		if best == config.RoleOperator {
			break
		}
		if !slices.Contains(p.Groups, r.Group) || !config.MatchTarget(r.Targets, target) {
			continue
		}
		if r.Role == config.RoleOperator || best == "" {
			best = r.Role
		}
	}
	return best
}

// CheckCSRF accepts a state-changing request only with the CSRF header and an
// Origin equal to the server's own.
func (s *Service) CheckCSRF(r *http.Request) error {
	if r.Header.Get(CSRFHeader) == "" {
		return fmt.Errorf("missing %s header", CSRFHeader)
	}
	want := s.origin
	if want == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		want = scheme + "://" + r.Host
	}
	if got := r.Header.Get("Origin"); got != want {
		return fmt.Errorf("origin %q does not match %q", got, want)
	}
	return nil
}

// Routes serves /auth/* in oidc mode; it is nil in the other modes.
func (s *Service) Routes() http.Handler {
	if s.oidc == nil {
		return nil
	}
	return s.oidc.routes()
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal WithPrincipal stored, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// hashToken is the session id stored for a cookie value.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
