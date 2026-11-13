// Package config loads the fleetbox configuration file.
//
// Unknown keys are errors everywhere: a misspelled key must not be silently
// ignored. Provider-specific settings live under a
// key named after the provider and are decoded by the provider itself through
// DecodeStrict, so adding a provider never touches this package.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/somaz94/fleetbox/internal/model"
)

const (
	DefaultListen   = "127.0.0.1:8080"
	DefaultInterval = 30 * time.Second
	MinInterval     = 5 * time.Second
	DefaultDBPath   = "/var/lib/fleetbox/fleetbox.db"

	DefaultSessionTTL  = 12 * time.Hour
	MinSessionTTL      = 5 * time.Minute
	DefaultGroupsClaim = "groups"

	DefaultFindingsInterval = time.Minute
	MinFindingsInterval     = 10 * time.Second
	DefaultDriftAfter       = time.Hour
	DefaultStuckAfter       = 15 * time.Minute
	DefaultFailedStopWindow = 24 * time.Hour
	DefaultImageMaxAge      = 90 * 24 * time.Hour
)

// DefaultScopes are requested besides openid when auth.oidc.scopes is unset.
var DefaultScopes = []string{"profile", "email"}

// Roles a group can be granted on a target.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
)

var namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type Config struct {
	Server   Server   `yaml:"server"`
	Store    Store    `yaml:"store"`
	Auth     Auth     `yaml:"auth"`
	Targets  []Target `yaml:"targets"`
	Findings Findings `yaml:"findings"`
	Notify   Notify   `yaml:"notify"`
}

// Notify sends finding transitions out of fleetbox.
type Notify struct {
	Slack []SlackRoute `yaml:"slack"`
}

// SlackRoute sends the findings whose every target matches Targets, and whose
// kind is in Kinds (every kind when empty), to one incoming webhook. Name keys
// the route's delivery position, so renaming it starts afresh.
type SlackRoute struct {
	Name           string   `yaml:"name"`
	WebhookURLFile string   `yaml:"webhookURLFile"`
	Targets        []string `yaml:"targets"`
	Kinds          []string `yaml:"kinds"`
}

// Findings tunes the evaluator. Every field is optional, and zero means the
// default, so a delay cannot be set to nothing.
type Findings struct {
	Interval         time.Duration `yaml:"interval"`
	DriftAfter       time.Duration `yaml:"driftAfter"`
	StuckAfter       time.Duration `yaml:"stuckAfter"`
	FailedStopWindow time.Duration `yaml:"failedStopWindow"`
	ImageMaxAge      time.Duration `yaml:"imageMaxAge"`
	Disabled         []string      `yaml:"disabled"`
}

type Server struct {
	Listen    string `yaml:"listen"`
	PublicURL string `yaml:"publicURL"`
}

type Store struct {
	Path string `yaml:"path"`
}

type Auth struct {
	OIDC  *OIDC      `yaml:"oidc"`
	Proxy *Proxy     `yaml:"proxy"`
	Roles []RoleRule `yaml:"roles"`
}

type OIDC struct {
	Issuer   string `yaml:"issuer"`
	ClientID string `yaml:"clientID"`
	// ClientSecretFile is optional: a public client relies on PKCE alone.
	ClientSecretFile string        `yaml:"clientSecretFile"`
	Scopes           []string      `yaml:"scopes"`
	GroupsClaim      string        `yaml:"groupsClaim"`
	SessionTTL       time.Duration `yaml:"sessionTTL"`
}

// Proxy trusts identity headers set by an authenticating reverse proxy. The
// listen address must stay on loopback, so only a sidecar can set them.
type Proxy struct {
	UserHeader   string `yaml:"userHeader"`
	GroupsHeader string `yaml:"groupsHeader"`
}

type RoleRule struct {
	Group   string   `yaml:"group"`
	Targets []string `yaml:"targets"`
	Role    string   `yaml:"role"`
}

// Target is one configured provider endpoint. Spec holds the provider's own
// block (the value under the key equal to Provider), undecoded.
type Target struct {
	Name     string
	Provider string
	Env      string
	// Readonly refuses every operation on the target. It defaults to true:
	// Docker API access is root on the host, so operating is an opt-in.
	Readonly bool
	Interval time.Duration
	Spec     yaml.Node
}

type targetCommon struct {
	Name     string        `yaml:"name"`
	Provider string        `yaml:"provider"`
	Env      string        `yaml:"env"`
	Readonly bool          `yaml:"readonly"`
	Interval time.Duration `yaml:"interval"`
}

var commonKeys = map[string]bool{"name": true, "provider": true, "env": true, "readonly": true, "interval": true}

// UnmarshalYAML splits the common keys from the provider block and rejects
// anything else.
func (t *Target) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: target must be a mapping", node.Line)
	}
	common := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	var extra []*yaml.Node // key, value pairs
	for i := 0; i+1 < len(node.Content); i += 2 {
		k, v := node.Content[i], node.Content[i+1]
		if commonKeys[k.Value] {
			common.Content = append(common.Content, k, v)
		} else {
			extra = append(extra, k, v)
		}
	}
	c := targetCommon{Readonly: true}
	if err := DecodeStrict(common, &c); err != nil {
		return err
	}
	*t = Target{Name: c.Name, Provider: c.Provider, Env: c.Env, Readonly: c.Readonly, Interval: c.Interval}
	for i := 0; i < len(extra); i += 2 {
		k, v := extra[i], extra[i+1]
		if c.Provider == "" || k.Value != c.Provider {
			return fmt.Errorf("line %d: target %q: unknown key %q", k.Line, c.Name, k.Value)
		}
		t.Spec = *v
	}
	return nil
}

// DecodeStrict decodes node into out, failing on any key out does not declare.
// yaml.Node.Decode has no strict mode, so the node is re-encoded and decoded
// through a strict decoder.
func DecodeStrict(node *yaml.Node, out any) error {
	if node == nil || node.Kind == 0 {
		return nil
	}
	b, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Load reads and validates the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

// Parse decodes and validates a configuration document, applying defaults.
func Parse(b []byte) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Config
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, err
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = DefaultListen
	}
	if c.Store.Path == "" {
		c.Store.Path = DefaultDBPath
	}
	if o := c.Auth.OIDC; o != nil {
		if o.Scopes == nil {
			o.Scopes = DefaultScopes
		}
		if o.GroupsClaim == "" {
			o.GroupsClaim = DefaultGroupsClaim
		}
		if o.SessionTTL == 0 {
			o.SessionTTL = DefaultSessionTTL
		}
	}
	for i := range c.Targets {
		if c.Targets[i].Interval == 0 {
			c.Targets[i].Interval = DefaultInterval
		}
	}
	f := &c.Findings
	for _, d := range []struct {
		v   *time.Duration
		def time.Duration
	}{
		{&f.Interval, DefaultFindingsInterval},
		{&f.DriftAfter, DefaultDriftAfter},
		{&f.StuckAfter, DefaultStuckAfter},
		{&f.FailedStopWindow, DefaultFailedStopWindow},
		{&f.ImageMaxAge, DefaultImageMaxAge},
	} {
		if *d.v == 0 {
			*d.v = d.def
		}
	}
}

// Validate checks everything that does not need a provider to answer.
func (c *Config) Validate() error {
	var errs []error
	if len(c.Targets) == 0 {
		errs = append(errs, errors.New("targets: at least one target is required"))
	}
	seen := map[string]bool{}
	for i, t := range c.Targets {
		where := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			where = fmt.Sprintf("target %q", t.Name)
		}
		switch {
		case t.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		case !namePattern.MatchString(t.Name):
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits and dashes", where))
		case seen[t.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name", where))
		}
		seen[t.Name] = true
		if t.Provider == "" {
			errs = append(errs, fmt.Errorf("%s: provider is required", where))
		} else if t.Spec.Kind == 0 {
			errs = append(errs, fmt.Errorf("%s: a %q block is required", where, t.Provider))
		}
		if t.Interval < MinInterval {
			errs = append(errs, fmt.Errorf("%s: interval must be at least %s", where, MinInterval))
		}
	}
	for i, r := range c.Auth.Roles {
		where := fmt.Sprintf("auth.roles[%d]", i)
		if r.Group == "" {
			errs = append(errs, fmt.Errorf("%s: group is required", where))
		}
		if r.Role != RoleViewer && r.Role != RoleOperator {
			errs = append(errs, fmt.Errorf("%s: role must be %q or %q", where, RoleViewer, RoleOperator))
		}
		if len(r.Targets) == 0 {
			errs = append(errs, fmt.Errorf("%s: targets is required", where))
		}
		errs = append(errs, checkGlobs(where, r.Targets)...)
	}
	if o := c.Auth.OIDC; o != nil {
		if o.Issuer == "" || o.ClientID == "" {
			errs = append(errs, errors.New("auth.oidc: issuer and clientID are required"))
		}
		if o.SessionTTL < MinSessionTTL {
			errs = append(errs, fmt.Errorf("auth.oidc.sessionTTL must be at least %s", MinSessionTTL))
		}
		if err := checkPublicURL(c.Server.PublicURL); err != nil {
			errs = append(errs, fmt.Errorf("server.publicURL: %w (auth.oidc derives its redirect URL from it)", err))
		} else if u, _ := url.Parse(c.Server.PublicURL); u.Scheme == "http" {
			if ok, _ := IsLoopback(net.JoinHostPort(u.Hostname(), "0")); !ok {
				errs = append(errs, errors.New("server.publicURL must be https with auth.oidc: the session cookie would travel in clear"))
			}
		}
	}
	if p := c.Auth.Proxy; p != nil {
		if c.Auth.OIDC != nil {
			errs = append(errs, errors.New("auth: oidc and proxy are mutually exclusive"))
		}
		if p.UserHeader == "" {
			errs = append(errs, errors.New("auth.proxy.userHeader is required"))
		}
		if err := checkPublicURL(c.Server.PublicURL); err != nil {
			errs = append(errs, fmt.Errorf("server.publicURL: %w (auth.proxy checks the browser's Origin against it)", err))
		}
	}
	errs = append(errs, c.Findings.validate()...)
	errs = append(errs, c.Notify.validate()...)
	if loopback, err := IsLoopback(c.Server.Listen); err != nil {
		errs = append(errs, fmt.Errorf("server.listen: %w", err))
	} else if !loopback && c.Auth.OIDC == nil {
		errs = append(errs, fmt.Errorf("server.listen %q is reachable beyond this host: configure auth.oidc or bind to 127.0.0.1", c.Server.Listen))
	}
	return errors.Join(errs...)
}

func (f Findings) validate() []error {
	var errs []error
	if f.Interval < MinFindingsInterval {
		errs = append(errs, fmt.Errorf("findings.interval must be at least %s", MinFindingsInterval))
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{{"driftAfter", f.DriftAfter}, {"stuckAfter", f.StuckAfter}, {"failedStopWindow", f.FailedStopWindow}, {"imageMaxAge", f.ImageMaxAge}} {
		if d.v < 0 {
			errs = append(errs, fmt.Errorf("findings.%s must not be negative", d.name))
		}
	}
	return append(errs, checkKinds("findings.disabled", f.Disabled)...)
}

func (n Notify) validate() []error {
	var errs []error
	seen := map[string]bool{}
	for i, r := range n.Slack {
		where := fmt.Sprintf("notify.slack[%d]", i)
		switch {
		case r.Name == "":
			errs = append(errs, fmt.Errorf("%s: name is required", where))
		case !namePattern.MatchString(r.Name):
			errs = append(errs, fmt.Errorf("%s: name must be lowercase letters, digits and dashes", where))
		case seen[r.Name]:
			errs = append(errs, fmt.Errorf("%s: duplicate name %q", where, r.Name))
		}
		seen[r.Name] = true
		if r.WebhookURLFile == "" {
			errs = append(errs, fmt.Errorf("%s: webhookURLFile is required", where))
		}
		// Required, like a role's: which targets a channel hears about is a decision.
		if len(r.Targets) == 0 {
			errs = append(errs, fmt.Errorf("%s: targets is required", where))
		}
		errs = append(errs, checkGlobs(where, r.Targets)...)
		errs = append(errs, checkKinds(where+".kinds", r.Kinds)...)
	}
	return errs
}

// MatchTarget reports whether target matches any of the globs, as roles and
// notification routes both read them.
func MatchTarget(globs []string, target string) bool {
	for _, g := range globs {
		if ok, _ := path.Match(g, target); ok {
			return true
		}
	}
	return false
}

func checkGlobs(where string, globs []string) []error {
	var errs []error
	for _, g := range globs {
		if _, err := path.Match(g, ""); err != nil {
			errs = append(errs, fmt.Errorf("%s: bad target pattern %q", where, g))
		}
	}
	return errs
}

func checkKinds(where string, kinds []string) []error {
	var errs []error
	for _, k := range kinds {
		if !slices.Contains(model.FindingKinds, k) {
			errs = append(errs, fmt.Errorf("%s: unknown kind %q, want one of %s", where, k, strings.Join(model.FindingKinds, ", ")))
		}
	}
	return errs
}

// checkPublicURL accepts an absolute http(s) URL of the server root.
func checkPublicURL(s string) error {
	if s == "" {
		return errors.New("required")
	}
	u, err := url.Parse(s)
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%q must be an absolute http or https URL", s)
	}
	if (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%q must be the server root, without a path", s)
	}
	return nil
}

// IsLoopback reports whether a listen address only accepts local connections.
// An empty host (":8080") listens on every interface.
func IsLoopback(listen string) (bool, error) {
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return false, err
	}
	if host == "localhost" {
		return true, nil
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback(), nil
}
