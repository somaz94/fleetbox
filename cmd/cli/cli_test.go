package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
