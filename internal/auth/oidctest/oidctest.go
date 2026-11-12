// Package oidctest runs a minimal OpenID Connect issuer in-process, so login
// can be tested without a real identity provider. It implements discovery,
// an authorization endpoint that signs the configured user in at once, the
// code exchange with PKCE, and the JWKS.
package oidctest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// Behaviour is what the issuer does on the next requests.
type Behaviour struct {
	User   string
	Groups []string
	// Claims are added to the ID token, overriding the defaults.
	Claims map[string]any
	// Refuse answers the authorization request with error=access_denied.
	Refuse bool
	// WrongNonce signs the ID token with a nonce other than the one requested.
	WrongNonce bool
	// NoIDToken leaves id_token out of the token response.
	NoIDToken bool
	// TokenStatus, when set, fails the token request with that status.
	TokenStatus int
}

// Issuer is a running fake issuer.
type Issuer struct {
	URL          string
	ClientID     string
	ClientSecret string

	srv *httptest.Server
	key *rsa.PrivateKey

	mu    sync.Mutex
	b     Behaviour
	codes map[string]grant
}

type grant struct {
	clientID, redirect, challenge, nonce string
	b                                    Behaviour
}

const keyID = "test-key"

// New starts an issuer for one client; an empty secret makes it a public
// client. It stops when the test ends.
func New(t testing.TB, clientID, clientSecret string) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	i := &Issuer{ClientID: clientID, ClientSecret: clientSecret, key: key, codes: map[string]grant{},
		b: Behaviour{User: "alice", Groups: []string{"platform"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", i.discovery)
	mux.HandleFunc("GET /authorize", i.authorize)
	mux.HandleFunc("POST /token", i.token)
	mux.HandleFunc("GET /keys", i.keys)
	i.srv = httptest.NewServer(mux)
	i.URL = i.srv.URL
	t.Cleanup(i.srv.Close)
	return i
}

// Configure changes what the issuer does from now on.
func (i *Issuer) Configure(f func(b *Behaviour)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f(&i.b)
}

// Close stops the issuer early, to test an unreachable one.
func (i *Issuer) Close() { i.srv.Close() }

func (i *Issuer) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                i.URL,
		"authorization_endpoint":                i.URL + "/authorize",
		"token_endpoint":                        i.URL + "/token",
		"jwks_uri":                              i.URL + "/keys",
		"end_session_endpoint":                  i.URL + "/logout",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
	})
}

func (i *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || q.Get("client_id") != i.ClientID || q.Get("response_type") != "code" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "bad authorization request", http.StatusBadRequest)
		return
	}
	i.mu.Lock()
	b := i.b
	back := url.Values{"state": {q.Get("state")}}
	if b.Refuse {
		back.Set("error", "access_denied")
	} else {
		code := rand.Text()
		i.codes[code] = grant{clientID: i.ClientID, redirect: redirect.String(), challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), b: b}
		back.Set("code", code)
	}
	i.mu.Unlock()
	redirect.RawQuery = back.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok {
		id, secret = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
	}
	if id != i.ClientID || secret != i.ClientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}
	i.mu.Lock()
	code := r.PostForm.Get("code")
	g, found := i.codes[code]
	delete(i.codes, code)
	i.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != g.redirect ||
		base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}
	if g.b.TokenStatus != 0 {
		oauthError(w, g.b.TokenStatus, "server_error")
		return
	}
	resp := map[string]any{"access_token": rand.Text(), "token_type": "Bearer", "expires_in": 300}
	if !g.b.NoIDToken {
		nonce := g.nonce
		if g.b.WrongNonce {
			nonce = "not-the-requested-nonce"
		}
		now := time.Now()
		claims := map[string]any{
			"iss": i.URL, "sub": "sub-" + g.b.User, "aud": g.clientID, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
			"nonce": nonce, "preferred_username": g.b.User, "groups": g.b.Groups,
		}
		for k, v := range g.b.Claims {
			claims[k] = v
		}
		signed, err := i.sign(claims)
		if err != nil {
			oauthError(w, http.StatusInternalServerError, "server_error")
			return
		}
		resp["id_token"] = signed
	}
	writeJSON(w, http.StatusOK, resp)
}

func (i *Issuer) keys(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &i.key.PublicKey, KeyID: keyID, Algorithm: string(jose.RS256), Use: "sig"},
	}})
}

func (i *Issuer) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: i.key, KeyID: keyID}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func oauthError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
