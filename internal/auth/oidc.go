package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/store"
)

const (
	loginCookie  = "fleetbox_login"
	loginTTL     = 10 * time.Minute
	idpTimeout   = 15 * time.Second
	callbackPath = "/auth/callback"
)

// loginState is what the callback needs from the login that started it. It
// travels sealed in the login cookie, so an unauthenticated client starting
// logins costs the server no memory.
type loginState struct {
	State    string `json:"s"`
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
	Next     string `json:"x"`
	Expires  int64  `json:"e"`
}

type oidcFlow struct {
	cfg       *config.OIDC
	secret    string
	redirect  string
	home      string
	secure    bool
	loginName string
	sealer    cipher.AEAD // keyed at start-up: a restart abandons logins in progress
	client    *http.Client
	svc       *Service
	log       *slog.Logger

	// discoverSem serialises discovery and, unlike a mutex, lets a waiter
	// give up when its request ends. It guards provider and endSession.
	discoverSem chan struct{}
	provider    *oidc.Provider
	endSession  string
}

func newOIDCFlow(cfg *config.Config, svc *Service, log *slog.Logger) (*oidcFlow, error) {
	o := cfg.Auth.OIDC
	key := make([]byte, 32)
	rand.Read(key)
	// Neither fails for a 32-byte AES key.
	block, _ := aes.NewCipher(key)
	sealer, _ := cipher.NewGCMWithRandomNonce(block)
	f := &oidcFlow{
		cfg: o, svc: svc, log: log, sealer: sealer,
		redirect:    svc.origin + callbackPath,
		home:        svc.origin + "/",
		secure:      svc.secure,
		loginName:   svc.cookieName(loginCookie),
		client:      &http.Client{Timeout: idpTimeout},
		discoverSem: make(chan struct{}, 1),
	}
	if o.ClientSecretFile != "" {
		b, err := os.ReadFile(o.ClientSecretFile)
		if err != nil {
			return nil, fmt.Errorf("auth.oidc.clientSecretFile: %w", err)
		}
		f.secret = strings.TrimSpace(string(b))
	}
	return f, nil
}

func (f *oidcFlow) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/login", f.login)
	mux.HandleFunc("GET "+callbackPath, f.callback)
	mux.HandleFunc("POST /auth/logout", f.logout)
	return mux
}

// idpContext routes go-oidc and oauth2 through the client with a timeout;
// go-oidc's background key fetch would otherwise wait on a stuck issuer forever.
func (f *oidcFlow) idpContext(ctx context.Context) context.Context {
	return context.WithValue(oidc.ClientContext(ctx, f.client), oauth2.HTTPClient, f.client)
}

// discover fetches the issuer's metadata once; a failure is retried on the
// next login.
func (f *oidcFlow) discover(ctx context.Context) (*oidc.Provider, error) {
	ctx, cancel := context.WithTimeout(ctx, idpTimeout)
	defer cancel()
	select {
	case f.discoverSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-f.discoverSem }()
	if f.provider != nil {
		return f.provider, nil
	}
	p, err := oidc.NewProvider(f.idpContext(ctx), f.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	var meta struct {
		EndSession string `json:"end_session_endpoint"`
	}
	_ = p.Claims(&meta) // NewProvider already decoded this document
	f.provider, f.endSession = p, meta.EndSession
	return p, nil
}

func (f *oidcFlow) oauth2Config(p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{
		ClientID: f.cfg.ClientID, ClientSecret: f.secret, Endpoint: p.Endpoint(), RedirectURL: f.redirect,
		Scopes: append([]string{oidc.ScopeOpenID}, f.cfg.Scopes...),
	}
}

func (f *oidcFlow) login(w http.ResponseWriter, r *http.Request) {
	p, err := f.discover(r.Context())
	if err != nil {
		f.log.Error("oidc discovery failed", "issuer", f.cfg.Issuer, "err", err)
		http.Error(w, "the identity provider is unreachable", http.StatusBadGateway)
		return
	}
	ls := loginState{State: randomToken(), Verifier: oauth2.GenerateVerifier(), Nonce: randomToken(),
		Next: safeNext(r.URL.Query().Get("next")), Expires: f.svc.now().Add(loginTTL).Unix()}
	// The cookie binds the callback to this browser, so a link carrying
	// someone else's state cannot sign the victim into the attacker's account.
	f.setLoginCookie(w, f.seal(ls), int(loginTTL.Seconds()))
	u := f.oauth2Config(p).AuthCodeURL(ls.State, oauth2.S256ChallengeOption(ls.Verifier), oidc.Nonce(ls.Nonce))
	http.Redirect(w, r, u, http.StatusFound)
}

func (f *oidcFlow) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ls, ok := f.openLogin(r)
	f.setLoginCookie(w, "", -1)
	if e := q.Get("error"); e != "" {
		f.log.Warn("oidc login refused by the identity provider", "error", truncate(e), "description", truncate(q.Get("error_description")))
		http.Error(w, "the identity provider refused the login", http.StatusUnauthorized)
		return
	}
	if !ok || q.Get("state") == "" || ls.State != q.Get("state") {
		http.Error(w, "login state missing, expired or from another browser: start again from /auth/login", http.StatusBadRequest)
		return
	}
	if q.Get("code") == "" {
		http.Error(w, "the callback carries no code", http.StatusBadRequest)
		return
	}

	p, err := f.discover(r.Context())
	if err != nil {
		f.log.Error("oidc discovery failed", "issuer", f.cfg.Issuer, "err", err)
		http.Error(w, "the identity provider is unreachable", http.StatusBadGateway)
		return
	}
	ctx, cancel := context.WithTimeout(f.idpContext(r.Context()), idpTimeout)
	defer cancel()
	tok, err := f.oauth2Config(p).Exchange(ctx, q.Get("code"), oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		f.log.Error("oidc code exchange failed", "err", err)
		http.Error(w, "the identity provider did not accept the login", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		f.log.Error("oidc token response has no id_token")
		http.Error(w, "the identity provider returned no ID token", http.StatusBadGateway)
		return
	}
	var claims map[string]any
	idt, err := p.Verifier(&oidc.Config{ClientID: f.cfg.ClientID, Now: f.svc.now}).Verify(ctx, raw)
	if err == nil && idt.Nonce != ls.Nonce {
		err = errors.New("nonce mismatch")
	}
	if err == nil {
		err = idt.Claims(&claims)
	}
	if err != nil {
		f.log.Warn("oidc ID token rejected", "err", err)
		http.Error(w, "the ID token was not valid", http.StatusUnauthorized)
		return
	}
	user := firstString(claims, "preferred_username", "email")
	if user == "" {
		user = idt.Subject
	}

	now := f.svc.now()
	if n, err := f.svc.sessions.PruneSessions(r.Context(), now); err != nil {
		f.log.Warn("prune sessions", "err", err)
	} else if n > 0 {
		f.log.Debug("pruned expired sessions", "count", n)
	}
	if old, err := r.Cookie(f.svc.sessionCookie); err == nil {
		if err := f.svc.sessions.DeleteSession(r.Context(), hashToken(old.Value)); err != nil {
			f.log.Warn("delete previous session", "err", err)
		}
	}
	token := randomToken()
	sess := store.Session{ID: hashToken(token), User: user, Groups: stringList(claims[f.cfg.GroupsClaim]),
		CreatedAt: now, ExpiresAt: now.Add(f.cfg.SessionTTL)}
	if err := f.svc.sessions.CreateSession(r.Context(), sess); err != nil {
		f.log.Error("create session", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	f.log.Info("signed in", "user", user, "groups", sess.Groups)
	f.setSessionCookie(w, token, int(f.cfg.SessionTTL.Seconds()))
	http.Redirect(w, r, ls.Next, http.StatusSeeOther)
}

// logout ends the session and answers with where the browser should go next:
// the issuer's end-session endpoint when it has one, so the issuer's own
// session ends too.
func (f *oidcFlow) logout(w http.ResponseWriter, r *http.Request) {
	if err := f.svc.CheckCSRF(r); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(f.svc.sessionCookie); err == nil {
		if err := f.svc.sessions.DeleteSession(r.Context(), hashToken(c.Value)); err != nil {
			f.log.Error("delete session", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}
	f.setSessionCookie(w, "", -1)
	redirect := f.home
	if _, err := f.discover(r.Context()); err != nil {
		f.log.Warn("oidc discovery failed during logout", "err", err)
	} else if f.endSession != "" {
		v := url.Values{"client_id": {f.cfg.ClientID}, "post_logout_redirect_uri": {f.home}}
		redirect = f.endSession + "?" + v.Encode()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"redirect": redirect})
}

// Both cookies use Path=/ so that, when Secure, they can carry the __Host-
// prefix, which stops a sibling subdomain from planting either one.
func (f *oidcFlow) setLoginCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: f.loginName, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: f.secure, SameSite: http.SameSiteLaxMode})
}

func (f *oidcFlow) setSessionCookie(w http.ResponseWriter, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: f.svc.sessionCookie, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: f.secure, SameSite: http.SameSiteLaxMode})
}

func (f *oidcFlow) seal(ls loginState) string {
	b, _ := json.Marshal(ls) // strings and an int: cannot fail
	return base64.RawURLEncoding.EncodeToString(f.sealer.Seal(nil, nil, b, []byte(f.loginName)))
}

// openLogin reads the login cookie; it fails on a missing, forged or expired one.
func (f *oidcFlow) openLogin(r *http.Request) (loginState, bool) {
	c, err := r.Cookie(f.loginName)
	if err != nil {
		return loginState{}, false
	}
	sealed, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return loginState{}, false
	}
	b, err := f.sealer.Open(nil, nil, sealed, []byte(f.loginName))
	if err != nil {
		return loginState{}, false
	}
	var ls loginState
	if json.Unmarshal(b, &ls) != nil || f.svc.now().Unix() > ls.Expires {
		return loginState{}, false
	}
	return ls, true
}

// safeNext keeps the post-login redirect on this server. Backslashes are
// refused anywhere: http.Redirect cleans the path, so "/x/../\evil" would
// become "/\evil", which browsers follow off-site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.ContainsRune(next, '\\') {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b) // never fails; see crypto/rand.Read
	return base64.RawURLEncoding.EncodeToString(b)
}

// truncate bounds what an issuer redirect, which anyone can forge, puts in the log.
func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

func firstString(claims map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := claims[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// stringList reads a groups claim, which issuers send as a list or, for a
// single group, sometimes as a string.
func stringList(v any) []string {
	out := []string{}
	switch v := v.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}
