package auth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/auth/oidctest"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/store"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

var rules = []config.RoleRule{
	{Group: "platform", Targets: []string{"*"}, Role: config.RoleOperator},
	{Group: "developers", Targets: []string{"dev-*"}, Role: config.RoleOperator},
	{Group: "developers", Targets: []string{"*"}, Role: config.RoleViewer},
	{Group: "auditors", Targets: []string{"prod-*", "["}, Role: config.RoleViewer},
}

func TestNoneMode(t *testing.T) {
	s, err := New(&config.Config{}, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Authenticate(httptest.NewRequest("GET", "http://127.0.0.1:8080/", nil))
	if err != nil || p.User != "anonymous" || p.Groups == nil {
		t.Fatalf("principal = %+v, err %v", p, err)
	}
	for _, host := range []string{"localhost", "[::1]:8080"} {
		if _, err := s.Authenticate(httptest.NewRequest("GET", "http://"+host+"/", nil)); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
	}
	// A DNS-rebound page arrives under its own name.
	if _, err := s.Authenticate(httptest.NewRequest("GET", "http://attacker.example:8080/", nil)); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("non-loopback host: err = %v", err)
	}
	if s.Mode() != ModeNone || s.Role(p, "prod") != config.RoleOperator || s.Routes() != nil {
		t.Fatalf("mode %s role %s", s.Mode(), s.Role(p, "prod"))
	}
}

func TestProxyMode(t *testing.T) {
	cfg := &config.Config{Auth: config.Auth{Proxy: &config.Proxy{UserHeader: "X-User", GroupsHeader: "X-Groups"}, Roles: rules}}
	s, err := New(cfg, nil, discard)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/", nil)
	if _, err := s.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("no header: err = %v", err)
	}
	r.Header.Set("X-User", " alice ")
	r.Header.Add("X-Groups", "developers, ,auditors")
	r.Header.Add("X-Groups", "ops")
	p, err := s.Authenticate(r)
	if err != nil || p.User != "alice" || !slices.Equal(p.Groups, []string{"developers", "auditors", "ops"}) {
		t.Fatalf("principal = %+v, err %v", p, err)
	}
	if s.Mode() != ModeProxy || s.Routes() != nil {
		t.Fatal("proxy mode serves no auth routes")
	}

	noGroups, _ := New(&config.Config{Auth: config.Auth{Proxy: &config.Proxy{UserHeader: "X-User"}}}, nil, discard)
	if p, err := noGroups.Authenticate(r); err != nil || len(p.Groups) != 0 || p.Groups == nil {
		t.Fatalf("groups without a groups header = %+v, %v", p, err)
	}
}

func TestRole(t *testing.T) {
	s := &Service{mode: ModeProxy, rules: rules}
	tests := []struct {
		groups []string
		target string
		want   string
	}{
		{[]string{"platform"}, "prod-aws", config.RoleOperator},
		{[]string{"developers"}, "dev-aws", config.RoleOperator},
		{[]string{"developers"}, "prod-aws", config.RoleViewer},
		{[]string{"auditors"}, "prod-aws", config.RoleViewer},
		{[]string{"auditors"}, "dev-aws", ""},
		{[]string{"auditors", "developers"}, "dev-aws", config.RoleOperator},
		{[]string{"nobody"}, "prod-aws", ""},
		{nil, "prod-aws", ""},
	}
	for _, tt := range tests {
		if got := s.Role(Principal{Groups: tt.groups}, tt.target); got != tt.want {
			t.Errorf("Role(%v, %s) = %q, want %q", tt.groups, tt.target, got, tt.want)
		}
	}
}

func TestCheckCSRF(t *testing.T) {
	derived := &Service{}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8080/api/v1/x", nil)
	if err := derived.CheckCSRF(r); err == nil || !strings.Contains(err.Error(), CSRFHeader) {
		t.Fatalf("no header: err = %v", err)
	}
	r.Header.Set(CSRFHeader, "1")
	if err := derived.CheckCSRF(r); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("no origin: err = %v", err)
	}
	r.Header.Set("Origin", "http://127.0.0.1:8080")
	if err := derived.CheckCSRF(r); err != nil {
		t.Fatalf("same origin: %v", err)
	}
	r.TLS = &tls.ConnectionState{}
	if err := derived.CheckCSRF(r); err == nil {
		t.Fatal("an http origin passed for a TLS request")
	}

	s, _ := New(&config.Config{Server: config.Server{PublicURL: "https://Fleetbox.Example.com:443/"}}, nil, discard)
	r = httptest.NewRequest("POST", "http://127.0.0.1/api/v1/x", nil)
	r.Header.Set(CSRFHeader, "1")
	r.Header.Set("Origin", "https://fleetbox.example.com")
	if err := s.CheckCSRF(r); err != nil {
		t.Fatalf("publicURL origin: %v", err)
	}
	r.Header.Set("Origin", "https://evil.example.com")
	if err := s.CheckCSRF(r); err == nil {
		t.Fatal("foreign origin accepted")
	}
}

func TestOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://Fleetbox.Example.com:443/": "https://fleetbox.example.com",
		"http://fleetbox.example.com:80":    "http://fleetbox.example.com",
		"https://fleetbox.example.com:8443": "https://fleetbox.example.com:8443",
		"http://[::1]:8080":                 "http://[::1]:8080",
		"http://[::1]":                      "http://[::1]",
	} {
		if got := origin(mustURL(t, in)); got != want {
			t.Errorf("origin(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("principal in an empty context")
	}
	p, ok := FromContext(WithPrincipal(context.Background(), Principal{User: "alice"}))
	if !ok || p.User != "alice" {
		t.Fatalf("principal = %+v", p)
	}
}

// oidcEnv is fleetbox's auth routes behind a test server, a fake issuer and a
// browser with a cookie jar that records each redirect it follows.
type oidcEnv struct {
	t       *testing.T
	idp     *oidctest.Issuer
	svc     *Service
	st      *store.SQLite
	srv     *httptest.Server
	browser *http.Client
	hops    []*http.Response
	skew    atomic.Int64 // added to the service's clock
}

// newOIDCEnv starts the environment; an empty secret makes fleetbox a public client.
func newOIDCEnv(t *testing.T, secret string, mutate func(*config.OIDC)) *oidcEnv {
	t.Helper()
	e := &oidcEnv{t: t, idp: oidctest.New(t, "fleetbox", secret)}
	secretFile := ""
	if secret != "" {
		secretFile = filepath.Join(t.TempDir(), "secret")
		if err := os.WriteFile(secretFile, []byte(secret+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Unstarted, so the URL is known before the handler exists.
	e.srv = httptest.NewUnstartedServer(nil)
	t.Cleanup(e.srv.Close)
	srvURL := "http://" + e.srv.Listener.Addr().String()

	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e.st = st
	o := &config.OIDC{Issuer: e.idp.URL, ClientID: "fleetbox", ClientSecretFile: secretFile, Scopes: config.DefaultScopes,
		GroupsClaim: config.DefaultGroupsClaim, SessionTTL: time.Hour}
	if mutate != nil {
		mutate(o)
	}
	cfg := &config.Config{Server: config.Server{PublicURL: srvURL}, Auth: config.Auth{OIDC: o, Roles: rules}}
	e.svc, err = New(cfg, st, discard)
	if err != nil {
		t.Fatal(err)
	}
	e.svc.now = func() time.Time { return time.Now().Add(time.Duration(e.skew.Load())) }
	mux := http.NewServeMux()
	mux.Handle("/auth/", e.svc.Routes())
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		p, err := e.svc.Authenticate(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		fmt.Fprintf(w, "%s %s", p.User, strings.Join(p.Groups, ","))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "page ", r.URL.RequestURI()) })
	e.srv.Config.Handler = mux
	e.srv.Start()

	jar, _ := cookiejar.New(nil)
	e.browser = &http.Client{Jar: jar, CheckRedirect: func(req *http.Request, _ []*http.Request) error {
		e.hops = append(e.hops, req.Response)
		return nil
	}}
	return e
}

func (e *oidcEnv) get(path string) (int, string) {
	e.t.Helper()
	e.hops = nil
	resp, err := e.browser.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *oidcEnv) logout(headers map[string]string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/auth/logout", nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := e.browser.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *oidcEnv) cookie(name string) *http.Cookie {
	for _, r := range e.hops {
		for _, c := range r.Cookies() {
			if c.Name == name {
				return c
			}
		}
	}
	return nil
}

func TestOIDCLogin(t *testing.T) {
	e := newOIDCEnv(t, "s3cret", nil)
	if code, _ := e.get("/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("whoami before login = %d", code)
	}
	code, body := e.get("/auth/login?next=/services%3Fq%3Dapi")
	if code != http.StatusOK || body != "page /services?q=api" {
		t.Fatalf("after login: %d %q", code, body)
	}
	auth, err := url.Parse(e.hops[0].Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := auth.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("nonce") == "" || q.Get("scope") != "openid profile email" ||
		q.Get("redirect_uri") != e.srv.URL+"/auth/callback" {
		t.Fatalf("authorization request = %s", auth)
	}
	c := e.cookie(SessionCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Secure || c.Path != "/" || c.MaxAge != 3600 {
		t.Fatalf("session cookie = %+v", c)
	}
	if code, body := e.get("/whoami"); code != http.StatusOK || body != "alice platform" {
		t.Fatalf("whoami = %d %q", code, body)
	}
	// Only a hash of the cookie is stored.
	if _, err := e.st.Session(context.Background(), c.Value, time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("raw token found in the store: %v", err)
	}

	// Signing in again replaces the session rather than adding one.
	e.idp.Configure(func(b *oidctest.Behaviour) { b.User, b.Groups = "bob", []string{"developers"} })
	if code, body := e.get("/auth/login?next=//evil.example.com"); code != http.StatusOK || body != "page /" {
		t.Fatalf("unsafe next: %d %q", code, body)
	}
	if _, err := e.st.Session(context.Background(), hashToken(c.Value), time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old session survived a new login: %v", err)
	}
	if _, body := e.get("/whoami"); body != "bob developers" {
		t.Fatalf("whoami = %q", body)
	}

	e.skew.Store(int64(2 * time.Hour))
	if code, _ := e.get("/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: %d", code)
	}
	e.skew.Store(0)

	// Logout needs the CSRF header and the server's origin.
	if code, _ := e.logout(nil); code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d", code)
	}
	code, body = e.logout(map[string]string{CSRFHeader: "1", "Origin": e.srv.URL})
	var out struct{ Redirect string }
	if json.Unmarshal([]byte(body), &out); code != http.StatusOK || !strings.HasPrefix(out.Redirect, e.idp.URL+"/logout?") ||
		!strings.Contains(out.Redirect, "post_logout_redirect_uri="+url.QueryEscape(e.srv.URL+"/")) {
		t.Fatalf("logout = %d %s", code, body)
	}
	if code, _ := e.get("/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("session survived logout: %d", code)
	}
}

func TestOIDCClaims(t *testing.T) {
	// A public client: PKCE alone protects the code.
	e := newOIDCEnv(t, "", func(o *config.OIDC) { o.GroupsClaim = "roles" })
	e.idp.Configure(func(b *oidctest.Behaviour) {
		b.Claims = map[string]any{"preferred_username": "", "email": "carol@example.com", "roles": "auditors"}
	})
	e.get("/auth/login")
	if _, body := e.get("/whoami"); body != "carol@example.com auditors" {
		t.Fatalf("whoami = %q", body)
	}
	e.idp.Configure(func(b *oidctest.Behaviour) {
		b.Claims = map[string]any{"preferred_username": nil, "roles": []any{"a", 1, ""}}
	})
	e.get("/auth/login")
	if _, body := e.get("/whoami"); body != "sub-alice a" {
		t.Fatalf("whoami = %q", body)
	}
}

func TestOIDCFailures(t *testing.T) {
	tests := []struct {
		name string
		b    func(*oidctest.Behaviour)
		want int
	}{
		{"refused", func(b *oidctest.Behaviour) { b.Refuse = true }, http.StatusUnauthorized},
		{"wrong nonce", func(b *oidctest.Behaviour) { b.WrongNonce = true }, http.StatusUnauthorized},
		{"no id token", func(b *oidctest.Behaviour) { b.NoIDToken = true }, http.StatusBadGateway},
		{"token error", func(b *oidctest.Behaviour) { b.TokenStatus = http.StatusInternalServerError }, http.StatusBadGateway},
		{"expired token", func(b *oidctest.Behaviour) { b.Claims = map[string]any{"exp": time.Now().Add(-time.Hour).Unix()} }, http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newOIDCEnv(t, "s3cret", nil)
			e.idp.Configure(tt.b)
			if code, _ := e.get("/auth/login"); code != tt.want {
				t.Fatalf("status = %d, want %d", code, tt.want)
			}
			if code, _ := e.get("/whoami"); code != http.StatusUnauthorized {
				t.Fatal("a failed login left a session")
			}
		})
	}
}

func TestOIDCCallbackState(t *testing.T) {
	e := newOIDCEnv(t, "s3cret", nil)
	if code, _ := e.get("/auth/callback?state=x&code=y"); code != http.StatusBadRequest {
		t.Fatalf("callback without a login cookie = %d", code)
	}
	base := mustURL(t, e.srv.URL)
	for _, v := range []string{"not base64!", "bm90IHNlYWxlZA"} {
		e.browser.Jar.SetCookies(base, []*http.Cookie{{Name: loginCookie, Value: v, Path: "/"}})
		if code, _ := e.get("/auth/callback?state=x&code=y"); code != http.StatusBadRequest {
			t.Fatalf("forged login cookie %q = %d", v, code)
		}
	}
	e.browser.Jar.SetCookies(base, []*http.Cookie{{Name: loginCookie, Path: "/",
		Value: e.svc.oidc.seal(loginState{State: "x", Next: "/", Expires: time.Now().Add(time.Minute).Unix()})}})
	if code, _ := e.get("/auth/callback?state=x"); code != http.StatusBadRequest {
		t.Fatalf("callback without a code = %d", code)
	}
	// Stop at the issuer's redirect, then replay the callback once it expired.
	e.browser.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(req.URL.Path, "/auth/callback") {
			e.hops = append(e.hops, req.Response)
			return http.ErrUseLastResponse
		}
		return nil
	}
	e.get("/auth/login")
	callback := e.hops[0].Header.Get("Location")
	e.skew.Store(int64(loginTTL + time.Minute))
	resp, err := e.browser.Get(callback)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expired login = %d", resp.StatusCode)
	}
	e.skew.Store(0)
	resp, _ = e.browser.Get(callback)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed state = %d", resp.StatusCode)
	}
}

func TestOIDCUnreachable(t *testing.T) {
	e := newOIDCEnv(t, "s3cret", nil)
	e.idp.Close()
	if code, _ := e.get("/auth/login"); code != http.StatusBadGateway {
		t.Fatalf("login with the issuer down = %d", code)
	}
	// A callback whose discovery fails, as when the issuer went down mid-login.
	sealed := e.svc.oidc.seal(loginState{State: "st", Next: "/", Expires: time.Now().Add(time.Minute).Unix()})
	e.browser.Jar.SetCookies(mustURL(t, e.srv.URL), []*http.Cookie{{Name: loginCookie, Value: sealed, Path: "/"}})
	if code, _ := e.get("/auth/callback?state=st&code=c"); code != http.StatusBadGateway {
		t.Fatalf("callback with the issuer down = %d", code)
	}
	code, body := e.logout(map[string]string{CSRFHeader: "1", "Origin": e.srv.URL})
	if code != http.StatusOK || !strings.Contains(body, `"redirect":"`+e.srv.URL+`/"`) {
		t.Fatalf("logout with the issuer down = %d %s", code, body)
	}
}

func TestOIDCStoreFailures(t *testing.T) {
	e := newOIDCEnv(t, "s3cret", nil)
	e.get("/auth/login")
	e.st.Close()
	if code, _ := e.get("/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("whoami = %d", code)
	}
	req := httptest.NewRequest("GET", "/", nil)
	for _, c := range e.browser.Jar.Cookies(mustURL(t, e.srv.URL)) {
		req.AddCookie(c)
	}
	if _, err := e.svc.Authenticate(req); err == nil || errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("a store failure must not read as signed out: %v", err)
	}
	if code, _ := e.get("/auth/login"); code != http.StatusInternalServerError {
		t.Fatalf("login with the store down = %d", code)
	}
	if code, _ := e.logout(map[string]string{CSRFHeader: "1", "Origin": e.srv.URL}); code != http.StatusInternalServerError {
		t.Fatalf("logout with the store down = %d", code)
	}
}

func TestNewErrors(t *testing.T) {
	cfg := &config.Config{Server: config.Server{PublicURL: "https://fleetbox.example.com"},
		Auth: config.Auth{OIDC: &config.OIDC{Issuer: "https://sso.example.com", ClientID: "c", ClientSecretFile: filepath.Join(t.TempDir(), "missing")}}}
	if _, err := New(cfg, nil, discard); err == nil || !strings.Contains(err.Error(), "clientSecretFile") {
		t.Fatalf("err = %v", err)
	}
	cfg.Auth.OIDC.ClientSecretFile = ""
	s, err := New(cfg, nil, discard)
	if err != nil || s.Mode() != ModeOIDC || !s.oidc.secure || s.Routes() == nil {
		t.Fatalf("service = %+v, err %v", s, err)
	}
	if s.sessionCookie != "__Host-"+SessionCookie || s.oidc.loginName != "__Host-"+loginCookie {
		t.Fatalf("cookies %s, %s", s.sessionCookie, s.oidc.loginName)
	}
	cfg.Server.PublicURL = "https://exa mple.com"
	if _, err := New(cfg, nil, discard); err == nil || !strings.Contains(err.Error(), "publicURL") {
		t.Fatalf("err = %v", err)
	}
}

func TestOIDCHousekeeping(t *testing.T) {
	e := newOIDCEnv(t, "s3cret", nil)
	stale := store.Session{ID: "stale", User: "old", CreatedAt: time.Now().Add(-2 * time.Hour), ExpiresAt: time.Now().Add(-time.Hour)}
	if err := e.st.CreateSession(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	e.get("/auth/login")
	if _, err := e.st.Session(context.Background(), "stale", stale.CreatedAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session not pruned at sign-in: %v", err)
	}

	// A login waiting behind a stuck discovery gives up with its request.
	f := e.svc.oidc
	f.discoverSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.discover(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	<-f.discoverSem
}

func TestTruncate(t *testing.T) {
	if got := truncate(strings.Repeat("x", 300)); len(got) != 200 {
		t.Fatalf("len = %d", len(got))
	}
	if truncate("short") != "short" {
		t.Fatal("short string changed")
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/services":            "/services",
		"/a?b=c":               "/a?b=c",
		"//evil.example":       "/",
		"/\\evil.example":      "/",
		"/x/../\\evil.example": "/",
		"/./\\evil.example":    "/",
		"https://evil.test":    "/",
		"services":             "/",
		"/%zz":                 "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
