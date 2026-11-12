package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const valid = `
server:
  publicURL: https://fleetbox.example.com
auth:
  oidc:
    issuer: https://sso.example.com/realms/example
    clientID: fleetbox
  roles:
    - group: platform
      targets: ["*"]
      role: operator
targets:
  - name: prod-aws
    provider: ecs
    env: prod
    readonly: true
    ecs:
      region: ap-northeast-2
  - name: build-host-1
    provider: docker
    interval: 1m
    docker:
      host: 192.0.2.10
`

func TestParseValid(t *testing.T) {
	c, err := Parse([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if c.Server.Listen != DefaultListen || c.Store.Path != DefaultDBPath {
		t.Fatalf("defaults not applied: %+v %+v", c.Server, c.Store)
	}
	if len(c.Targets) != 2 {
		t.Fatalf("targets = %d", len(c.Targets))
	}
	prod := c.Targets[0]
	if prod.Name != "prod-aws" || prod.Provider != "ecs" || prod.Env != "prod" || !prod.Readonly || prod.Interval != DefaultInterval {
		t.Fatalf("unexpected target %+v", prod)
	}
	var spec struct {
		Region string `yaml:"region"`
	}
	if err := DecodeStrict(&prod.Spec, &spec); err != nil || spec.Region != "ap-northeast-2" {
		t.Fatalf("spec = %+v, err %v", spec, err)
	}
	if c.Targets[1].Interval != time.Minute {
		t.Fatalf("interval = %s", c.Targets[1].Interval)
	}
	o := c.Auth.OIDC
	if o.GroupsClaim != DefaultGroupsClaim || o.SessionTTL != DefaultSessionTTL || len(o.Scopes) != len(DefaultScopes) {
		t.Fatalf("oidc defaults not applied: %+v", o)
	}
}

func TestParseOIDCAndProxy(t *testing.T) {
	doc := "server: {publicURL: 'https://fleetbox.example.com/'}\nauth:\n  oidc:\n    {issuer: https://sso.example.com, clientID: fleetbox, scopes: [], groupsClaim: roles, sessionTTL: 1h}\n" +
		"targets:\n  - {name: a, provider: ecs, ecs: {}}"
	c, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(oidcDoc("http://127.0.0.1:8080"))); err != nil {
		t.Fatalf("http on loopback: %v", err)
	}
	// An explicit empty list asks for openid alone and is kept.
	if o := c.Auth.OIDC; o.Scopes == nil || len(o.Scopes) != 0 || o.GroupsClaim != "roles" || o.SessionTTL != time.Hour {
		t.Fatalf("oidc = %+v", o)
	}
	proxy := "server: {publicURL: 'https://fleetbox.example.com'}\nauth:\n  proxy: {userHeader: X-Forwarded-User, groupsHeader: X-Forwarded-Groups}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}"
	c, err = Parse([]byte(proxy))
	if err != nil || c.Auth.Proxy.UserHeader != "X-Forwarded-User" {
		t.Fatalf("proxy = %+v, err %v", c, err)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{name: "empty", doc: "", want: "config is empty"},
		{name: "unknown top-level key", doc: "servr: {}\ntargets: []", want: "servr"},
		{name: "misspelled readonly", doc: "targets:\n  - name: a\n    provider: ecs\n    readOnly: true\n    ecs: {region: x}", want: `unknown key "readOnly"`},
		{name: "block for another provider", doc: "targets:\n  - name: a\n    provider: ecs\n    docker: {host: x}", want: `unknown key "docker"`},
		{name: "no provider block", doc: "targets:\n  - name: a\n    provider: ecs", want: `a "ecs" block is required`},
		{name: "no provider", doc: "targets:\n  - name: a", want: "provider is required"},
		{name: "no targets", doc: "server: {}", want: "at least one target"},
		{name: "bad name", doc: "targets:\n  - name: Prod_AWS\n    provider: ecs\n    ecs: {}", want: "lowercase"},
		{name: "missing name", doc: "targets:\n  - provider: ecs\n    ecs: {}", want: "name is required"},
		{name: "duplicate", doc: "targets:\n  - {name: a, provider: ecs, ecs: {}}\n  - {name: a, provider: ecs, ecs: {}}", want: "duplicate name"},
		{name: "short interval", doc: "targets:\n  - {name: a, provider: ecs, interval: 1s, ecs: {}}", want: "interval must be at least"},
		{name: "bad interval", doc: "targets:\n  - {name: a, provider: ecs, interval: soon, ecs: {}}", want: "soon"},
		{name: "target not a mapping", doc: "targets:\n  - a", want: "must be a mapping"},
		{name: "bad role", doc: "auth:\n  roles:\n    - {group: g, targets: ['*'], role: admin}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "role must be"},
		{name: "role without group or targets", doc: "auth:\n  roles:\n    - {role: viewer}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "group is required"},
		{name: "bad pattern", doc: "auth:\n  roles:\n    - {group: g, targets: ['['], role: viewer}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "bad target pattern"},
		{name: "oidc incomplete", doc: "auth:\n  oidc: {issuer: https://sso.example.com}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "issuer and clientID"},
		{name: "oidc without publicURL", doc: oidcDoc(""), want: "server.publicURL: required"},
		{name: "relative publicURL", doc: oidcDoc("fleetbox.example.com"), want: "absolute http or https"},
		{name: "publicURL with a path", doc: oidcDoc("https://example.com/fleetbox"), want: "without a path"},
		{name: "publicURL unparsable", doc: oidcDoc("https://exa mple.com"), want: "server.publicURL"},
		{name: "short session", doc: "server: {publicURL: 'https://fleetbox.example.com'}\nauth:\n  oidc: {issuer: https://sso.example.com, clientID: c, sessionTTL: 1m}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "sessionTTL must be at least"},
		{name: "oidc and proxy", doc: "server: {publicURL: 'https://fleetbox.example.com'}\nauth:\n  oidc: {issuer: https://sso.example.com, clientID: c}\n  proxy: {userHeader: X-User}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "mutually exclusive"},
		{name: "proxy without header", doc: "auth:\n  proxy: {groupsHeader: X-Groups}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "userHeader is required"},
		{name: "proxy without publicURL", doc: "auth:\n  proxy: {userHeader: X-User}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "checks the browser's Origin"},
		{name: "oidc over plain http", doc: oidcDoc("http://fleetbox.example.com"), want: "must be https"},
		{name: "proxy beyond loopback", doc: "server: {listen: ':8080'}\nauth:\n  proxy: {userHeader: X-User}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "reachable beyond this host"},
		{name: "every interface without auth", doc: "server: {listen: ':8080'}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "reachable beyond this host"},
		{name: "pod address without auth", doc: "server: {listen: '192.0.2.10:8080'}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "reachable beyond this host"},
		{name: "bad listen", doc: "server: {listen: '8080'}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}", want: "server.listen"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func oidcDoc(publicURL string) string {
	return "server: {publicURL: '" + publicURL + "'}\nauth:\n  oidc: {issuer: https://sso.example.com, clientID: fleetbox}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}"
}

func TestDecodeStrict(t *testing.T) {
	var out struct {
		A string `yaml:"a"`
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte("a: x\nb: y"), &n); err != nil {
		t.Fatal(err)
	}
	if err := DecodeStrict(n.Content[0], &out); err == nil || !strings.Contains(err.Error(), "field b not found") {
		t.Fatalf("err = %v", err)
	}
	if err := DecodeStrict(nil, &out); err != nil {
		t.Fatalf("nil node: %v", err)
	}
	if err := DecodeStrict(&yaml.Node{}, &out); err != nil {
		t.Fatalf("empty node: %v", err)
	}
}

func TestIsLoopback(t *testing.T) {
	for listen, want := range map[string]bool{
		"127.0.0.1:8080": true, "[::1]:8080": true, "localhost:8080": true,
		":8080": false, "0.0.0.0:8080": false, "192.0.2.10:8080": false, "fleetbox.example.com:8080": false,
	} {
		got, err := IsLoopback(listen)
		if err != nil || got != want {
			t.Fatalf("IsLoopback(%q) = %v, %v; want %v", listen, got, err, want)
		}
	}
	doc := "server: {listen: ':8080', publicURL: 'https://fleetbox.example.com'}\nauth:\n  oidc: {issuer: https://sso.example.com, clientID: fleetbox}\ntargets:\n  - {name: a, provider: ecs, ecs: {}}"
	if _, err := Parse([]byte(doc)); err != nil {
		t.Fatalf("every interface with OIDC: %v", err)
	}
}

// The shipped example must stay loadable as the schema changes.
func TestExampleConfig(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "examples", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) == 0 {
		t.Fatal("example has no targets")
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(valid), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("want error for a missing file")
	}
}
