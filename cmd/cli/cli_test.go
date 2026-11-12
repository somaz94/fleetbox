package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somaz94/fleetbox/internal/auth/oidctest"
	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

// testProvider stands in for a real platform so no test reaches a network.
type testProvider struct{}

func (testProvider) Kind() string                        { return "test" }
func (testProvider) Capabilities() provider.Capabilities { return provider.Capabilities{} }
func (testProvider) Inventory(context.Context) (model.Inventory, error) {
	return model.Inventory{}, nil
}
func (testProvider) Service(context.Context, model.ServiceRef) (model.ServiceDetail, error) {
	return model.ServiceDetail{}, provider.ErrNotFound
}
func (testProvider) Events(context.Context, model.ServiceRef, time.Time) ([]model.Event, error) {
	return nil, nil
}
func (testProvider) Logs(context.Context, model.InstanceRef, provider.LogOptions) (provider.LogStream, error) {
	return nil, provider.ErrUnsupported
}

func init() {
	provider.Register("test", func(t config.Target) (provider.Provider, error) {
		var spec struct {
			Fail bool `yaml:"fail"`
		}
		if err := config.DecodeStrict(&t.Spec, &spec); err != nil {
			return nil, err
		}
		if spec.Fail {
			return nil, io.ErrUnexpectedEOF
		}
		return testProvider{}, nil
	})
}

func writeConfig(t *testing.T, targets string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "data", "fleetbox.db")
	doc := "store:\n  path: " + db + "\ntargets:\n" + targets
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, db
}

const goodTargets = "  - {name: a, provider: test, test: {}}\n  - {name: b, provider: test, test: {}}\n"

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestVersion(t *testing.T) {
	out, err := run(t, "version")
	if err != nil || !strings.HasPrefix(out, "fleetbox dev") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
}

func TestCheck(t *testing.T) {
	cfg, _ := writeConfig(t, goodTargets)
	out, err := run(t, "check", "--config", cfg)
	if err != nil || out != "config OK: 2 targets (test=2)\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}

	bad, _ := writeConfig(t, "  - {name: a, provider: test, test: {fail: true}}\n")
	if _, err := run(t, "check", "-c", bad); err == nil || !strings.Contains(err.Error(), `target "a"`) {
		t.Fatalf("err = %v", err)
	}
	unknown, _ := writeConfig(t, "  - {name: a, provider: nope, nope: {}}\n")
	if _, err := run(t, "check", "-c", unknown); err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Fatalf("err = %v", err)
	}
	if _, err := run(t, "check", "-c", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing config accepted")
	}
}

func TestServe(t *testing.T) {
	path, db := writeConfig(t, goodTargets)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, "error") }()

	base := "http://" + ln.Addr().String()
	for _, p := range []string{"/healthz", "/readyz"} {
		var resp *http.Response
		deadline := time.Now().Add(5 * time.Second)
		for {
			resp, err = http.Get(base + p)
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || string(body) != "ok\n" {
			t.Fatalf("%s = %d %q", p, resp.StatusCode, body)
		}
	}
	resp, err := http.Get(base + "/api/v1/targets")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `"name":"a"`) || !strings.Contains(string(body), `"name":"b"`) {
		t.Fatalf("targets = %d %s", resp.StatusCode, body)
	}
	if _, err := os.Stat(db); err != nil {
		t.Fatalf("store not created: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// startServe runs serve on an ephemeral port until the test ends.
func startServe(t *testing.T, cfg *config.Config, ln net.Listener) string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, ln, "error") }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(15 * time.Second):
			t.Error("serve did not stop")
		}
	})
	base := "http://" + ln.Addr().String()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			return base
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeOIDC(t *testing.T) {
	idp := oidctest.New(t, "fleetbox", "s3cret")
	idp.Configure(func(b *oidctest.Behaviour) { b.User, b.Groups = "alice", []string{"developers"} })
	dir := t.TempDir()
	secret := filepath.Join(dir, "client-secret")
	os.WriteFile(secret, []byte("s3cret"), 0o600)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	doc := fmt.Sprintf(`server: {publicURL: %q}
store: {path: %q}
auth:
  oidc: {issuer: %q, clientID: fleetbox, clientSecretFile: %q}
  roles:
    - {group: developers, targets: ["a"], role: viewer}
targets:
`, base, filepath.Join(dir, "fleetbox.db"), idp.URL, secret) + goodTargets
	cfg, err := config.Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	startServe(t, cfg, ln)

	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar}
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := browser.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/api/v1/targets"); code != http.StatusUnauthorized || !strings.Contains(body, `"unauthorized"`) {
		t.Fatalf("targets before login = %d %s", code, body)
	}
	// The login lands on "/", which the console will serve; until then it is a 404.
	if code, _ := get("/auth/login"); code != http.StatusNotFound {
		t.Fatalf("login = %d", code)
	}
	if code, body := get("/api/v1/me"); code != http.StatusOK || !strings.Contains(body, `"user":"alice"`) ||
		!strings.Contains(body, `"targets":[{"name":"a","role":"viewer"}]`) {
		t.Fatalf("me = %d %s", code, body)
	}
	if _, body := get("/api/v1/targets"); !strings.Contains(body, `"name":"a"`) || strings.Contains(body, `"name":"b"`) {
		t.Fatalf("targets = %s", body)
	}
	if code, _ := get("/healthz"); code != http.StatusOK {
		t.Fatal("probes must not need a session")
	}
}

func TestServeErrors(t *testing.T) {
	listen := func() net.Listener {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		return ln
	}
	path, _ := writeConfig(t, goodTargets)
	cfg, _ := config.Load(path)
	if err := serve(context.Background(), cfg, listen(), "loud"); err == nil || !strings.Contains(err.Error(), "log level") {
		t.Fatalf("err = %v", err)
	}

	// An unreadable OIDC client secret stops the server before it listens.
	withOIDC := *cfg
	withOIDC.Auth.OIDC = &config.OIDC{Issuer: "https://sso.example.com", ClientID: "fleetbox",
		ClientSecretFile: filepath.Join(t.TempDir(), "missing")}
	if err := serve(context.Background(), &withOIDC, listen(), "info"); err == nil || !strings.Contains(err.Error(), "clientSecretFile") {
		t.Fatalf("err = %v", err)
	}

	bad, _ := writeConfig(t, "  - {name: a, provider: test, test: {fail: true}}\n")
	badCfg, _ := config.Load(bad)
	if err := serve(context.Background(), badCfg, listen(), "info"); err == nil {
		t.Fatal("provider failure not returned")
	}

	blocked := *cfg
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o600)
	blocked.Store.Path = filepath.Join(file, "x.db")
	if err := serve(context.Background(), &blocked, listen(), "info"); err == nil {
		t.Fatal("store failure not returned")
	}

	// A listener that fails ends serve with that error.
	ln := listen()
	ln.Close()
	if err := serve(context.Background(), cfg, ln, "info"); err == nil || !strings.Contains(err.Error(), "http server") {
		t.Fatalf("err = %v", err)
	}

	if _, err := run(t, "serve", "-c", filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("missing config accepted")
	}
	inUse := listen()
	defer inUse.Close()
	cfgInUse, _ := writeConfig(t, goodTargets)
	b, _ := os.ReadFile(cfgInUse)
	os.WriteFile(cfgInUse, append([]byte("server: {listen: \""+inUse.Addr().String()+"\"}\n"), b...), 0o600)
	if _, err := run(t, "serve", "-c", cfgInUse); err == nil {
		t.Fatal("binding a used port succeeded")
	}
}
