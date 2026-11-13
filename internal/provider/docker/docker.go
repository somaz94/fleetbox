// Package docker adapts a plain Docker host to the provider contract. A target
// is one host; compose projects are scopes and compose services are services.
// A container outside compose is a service of its own under StandaloneScope.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

const (
	Kind = "docker"

	StandaloneScope = "_standalone"
	TransportSSH    = "ssh"

	labelProject = "com.docker.compose.project"
	labelService = "com.docker.compose.service"
	labelOneOff  = "com.docker.compose.oneoff"
	labelPrefix  = "fleetbox.io/"

	shortIDLen      = 12
	defaultTail     = 200
	defaultLookback = 24 * time.Hour
)

func init() { provider.Register(Kind, New) }

// Config is the target's `docker:` block.
type Config struct {
	Transport      string `yaml:"transport"`
	Host           string `yaml:"host"`
	Port           int    `yaml:"port"`
	User           string `yaml:"user"`
	KeyFile        string `yaml:"keyFile"`
	KnownHostsFile string `yaml:"knownHostsFile"`
	DockerPath     string `yaml:"dockerPath"`
}

// Provider is the Docker adapter for one host.
type Provider struct {
	target string
	engine *engine
	now    func() time.Time

	endpoint []model.Attribute

	mu     sync.Mutex
	images map[string]imageInfo // by image ID; image IDs are content addresses
}

// New builds the adapter from the target's `docker:` block.
func New(t config.Target) (provider.Provider, error) {
	var c Config
	if err := config.DecodeStrict(&t.Spec, &c); err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	switch c.Transport {
	case "", TransportSSH:
		tr, err := newSSHTransport(c)
		if err != nil {
			return nil, fmt.Errorf("docker: %w", err)
		}
		p := NewWithDialer(t.Name, tr)
		p.endpoint = endpointOf(c, tr.addr)
		return p, nil
	default:
		return nil, fmt.Errorf("docker: unknown transport %q", c.Transport)
	}
}

// NewWithDialer builds the adapter over any transport; tests dial a fake daemon.
func NewWithDialer(target string, d Dialer) *Provider {
	return &Provider{target: target, engine: newEngine(d), now: time.Now, images: map[string]imageInfo{}}
}

func (p *Provider) Kind() string { return Kind }

// Endpoint names the host, never the user or key it signs in with.
func (p *Provider) Endpoint() []model.Attribute { return slices.Clone(p.endpoint) }

func endpointOf(c Config, addr string) []model.Attribute {
	out := []model.Attribute{{Name: "transport", Value: TransportSSH}, {Name: "host", Value: addr}}
	if c.DockerPath != "" {
		out = append(out, model.Attribute{Name: "dockerPath", Value: c.DockerPath})
	}
	return out
}

func (p *Provider) Capabilities() provider.Capabilities {
	return provider.Capabilities{Logs: true, FollowLogs: true, Events: true, Restart: true}
}

// Restart restarts the service's containers one after another, so a service
// with several replicas keeps some of them up, and stops at the first failure.
// A container removed since the listing is skipped; with none left the service
// is not found.
// Like `docker compose restart` it skips one-off containers from
// `docker compose run`: starting an exited one would run its command again.
func (p *Provider) Restart(ctx context.Context, ref model.ServiceRef) error {
	summaries, err := p.engine.listContainers(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for _, s := range summaries {
		if p.serviceRef(s.Labels, containerName(s.Names)) == ref && s.Labels[labelOneOff] != "True" {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("service %s: %w", ref, provider.ErrNotFound)
	}
	restarted := 0
	for i, id := range ids {
		err := p.engine.restartContainer(ctx, id)
		if errors.Is(err, provider.ErrNotFound) {
			continue // removed or recreated since the list
		}
		if err != nil {
			return fmt.Errorf("restart container %s (%d of %d): %w", shortID(id), i+1, len(ids), err)
		}
		restarted++
	}
	if restarted == 0 {
		return fmt.Errorf("service %s: %w", ref, provider.ErrNotFound)
	}
	return nil
}

// Scale is not supported: a compose service's replica count lives in a
// compose file on the host, which fleetbox does not edit.
func (p *Provider) Scale(context.Context, model.ServiceRef, int) error {
	return fmt.Errorf("docker: scale: %w", provider.ErrUnsupported)
}

func (p *Provider) Inventory(ctx context.Context) (model.Inventory, error) {
	summaries, err := p.engine.listContainers(ctx)
	if err != nil {
		return model.Inventory{}, err
	}
	return p.build(ctx, summaries, nil)
}

func (p *Provider) Service(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	summaries, err := p.engine.listContainers(ctx)
	if err != nil {
		return model.ServiceDetail{}, err
	}
	inv, err := p.build(ctx, summaries, &ref)
	if err != nil {
		return model.ServiceDetail{}, err
	}
	if len(inv.Services) == 0 {
		return model.ServiceDetail{}, fmt.Errorf("service %s: %w", ref, provider.ErrNotFound)
	}
	return model.ServiceDetail{Service: inv.Services[0], Instances: inv.Instances}, nil
}

// build turns container summaries into services and instances, keeping only
// the service named by only when it is set.
func (p *Provider) build(ctx context.Context, summaries []containerSummary, only *model.ServiceRef) (model.Inventory, error) {
	inv := model.Inventory{Services: []model.Service{}, Instances: []model.Instance{}}
	services := map[model.ServiceRef]*model.Service{}
	digests := map[model.ServiceRef]map[string]bool{}
	for _, s := range summaries {
		ref := p.serviceRef(s.Labels, containerName(s.Names))
		if only != nil && ref != *only {
			continue
		}
		cj, err := p.engine.inspectContainer(ctx, s.ID)
		if errors.Is(err, provider.ErrNotFound) {
			continue // removed between list and inspect
		}
		if err != nil {
			return model.Inventory{}, err
		}
		img, err := p.image(ctx, s.ImageID)
		if err != nil {
			return model.Inventory{}, err
		}
		in := mapContainer(ref, s, cj, img)
		inv.Instances = append(inv.Instances, in)

		svc, ok := services[ref]
		if !ok {
			svc = &model.Service{Ref: ref, Provider: Kind, Running: model.Ptr(0)}
			services[ref] = svc
			digests[ref] = map[string]bool{}
		}
		if in.Status == model.StatusRunning {
			*svc.Running++
			digests[ref][img.digest] = true
		}
		for k, v := range s.Labels {
			if strings.HasPrefix(k, labelPrefix) {
				if svc.Labels == nil {
					svc.Labels = map[string]string{}
				}
				svc.Labels[k] = v
			}
		}
	}
	for ref, svc := range services {
		// One digest across the running containers is the revision; a mix has none.
		if ds := digests[ref]; len(ds) == 1 {
			for d := range ds {
				svc.Revision = d
			}
		}
		inv.Services = append(inv.Services, *svc)
	}
	sort.Slice(inv.Services, func(i, j int) bool { return inv.Services[i].Ref.String() < inv.Services[j].Ref.String() })
	sort.Slice(inv.Instances, func(i, j int) bool { return inv.Instances[i].Ref.String() < inv.Instances[j].Ref.String() })
	return inv, nil
}

func (p *Provider) serviceRef(labels map[string]string, name string) model.ServiceRef {
	project, service := labels[labelProject], labels[labelService]
	if project != "" && service != "" {
		return model.ServiceRef{Target: p.target, Scope: project, Name: service}
	}
	return model.ServiceRef{Target: p.target, Scope: StandaloneScope, Name: name}
}

// imageInfo is what fleetbox keeps of an image: its repository digest, absent
// for a locally built image, and when it was built.
type imageInfo struct {
	digest  string
	created *time.Time
}

// image resolves an image ID once; an image removed while a stopped container
// still references it is reported with nothing known.
func (p *Provider) image(ctx context.Context, imageID string) (imageInfo, error) {
	if imageID == "" {
		return imageInfo{}, nil
	}
	p.mu.Lock()
	info, ok := p.images[imageID]
	p.mu.Unlock()
	if ok {
		return info, nil
	}
	img, err := p.engine.inspectImage(ctx, imageID)
	if errors.Is(err, provider.ErrNotFound) {
		return imageInfo{}, nil
	}
	if err != nil {
		return imageInfo{}, err
	}
	if len(img.RepoDigests) > 0 {
		if _, after, ok := strings.Cut(img.RepoDigests[0], "@"); ok {
			info.digest = after
		}
	}
	// Reproducible builders (ko, Bazel, nix) stamp the Unix epoch, which says nothing about age.
	if c := parseTime(img.Created); c != nil && c.Unix() > 0 {
		info.created = c
	}
	p.mu.Lock()
	p.images[imageID] = info
	p.mu.Unlock()
	return info, nil
}

// hasHealthCheck reads the container's own configuration, which already
// carries a HEALTHCHECK inherited from its image; NONE switches one off.
func hasHealthCheck(cj containerJSON) bool {
	if cj.State.Health != nil {
		return true
	}
	h := cj.Config.Healthcheck
	return h != nil && len(h.Test) > 0 && h.Test[0] != "NONE"
}

func mapContainer(ref model.ServiceRef, s containerSummary, cj containerJSON, img imageInfo) model.Instance {
	name := containerName(s.Names)
	c := model.Container{Name: name, Image: s.Image, Digest: img.digest, Status: s.State,
		ImageCreated: img.created, HealthCheck: model.Ptr(hasHealthCheck(cj))}
	in := model.Instance{
		Ref:       model.InstanceRef{Service: ref, ID: shortID(s.ID)},
		Status:    statusOf(s.State),
		Revision:  img.digest,
		StartedAt: parseTime(cj.State.StartedAt),
		Restarts:  model.Ptr(cj.RestartCount),
	}
	addDetail(&in, cj)
	if h := cj.State.Health; h != nil {
		in.Health = healthOf(h.Status)
		c.Health = in.Health
	}
	// With IPv6 on, the daemon lists a published port once per address family.
	seen := map[[3]any]bool{}
	for _, pt := range s.Ports {
		k := [3]any{pt.PrivatePort, pt.PublicPort, pt.Type}
		if seen[k] {
			continue
		}
		seen[k] = true
		port := model.Port{Container: pt.PrivatePort, Protocol: pt.Type}
		if pt.PublicPort != 0 {
			port.Host = model.Ptr(pt.PublicPort)
		}
		c.Ports = append(c.Ports, port)
	}
	if in.Status == model.StatusStopped {
		in.StoppedAt = parseTime(cj.State.FinishedAt)
		c.ExitCode = model.Ptr(cj.State.ExitCode)
		in.StopReason = stopReason(cj)
		c.Reason = in.StopReason
	}
	in.Containers = []model.Container{c}
	return in
}

// addDetail fills the inspect-only fields. A limit of zero means none was set.
func addDetail(in *model.Instance, cj containerJSON) {
	in.CreatedAt = parseTime(cj.Created)
	hc := cj.HostConfig
	switch rp := hc.RestartPolicy; {
	case rp.Name == "":
		in.RestartPolicy = "no"
	case rp.Name == "on-failure" && rp.MaximumRetryCount > 0:
		in.RestartPolicy = fmt.Sprintf("on-failure:%d", rp.MaximumRetryCount)
	default:
		in.RestartPolicy = rp.Name
	}
	switch {
	case hc.NanoCPUs > 0:
		in.CPU = model.Ptr(float64(hc.NanoCPUs) / 1e9)
	case hc.CPUQuota > 0:
		period := hc.CPUPeriod
		if period <= 0 {
			period = 100000 // the kernel's CFS period when only --cpu-quota is set
		}
		in.CPU = model.Ptr(float64(hc.CPUQuota) / float64(period))
	}
	if hc.Memory > 0 {
		in.MemoryMiB = model.Ptr(max(1, int(hc.Memory>>20)))
	}
	for _, m := range cj.Mounts {
		src := m.Source
		switch {
		case m.Type == "volume" && m.Name != "":
			src = m.Name
		case strings.HasPrefix(m.Destination, "/run/secrets/"):
			src = "" // where a compose secret lives on the host is not shown
		}
		in.Mounts = append(in.Mounts, model.Mount{Type: m.Type, Source: src, Destination: m.Destination, ReadOnly: !m.RW})
	}
	for _, name := range slices.Sorted(maps.Keys(cj.NetworkSettings.Networks)) {
		if ip := cj.NetworkSettings.Networks[name].IPAddress; ip != "" {
			in.Addresses = append(in.Addresses, model.Address{Network: name, IP: ip})
		}
	}
}

func stopReason(cj containerJSON) string {
	parts := []string{fmt.Sprintf("exited with code %d", cj.State.ExitCode)}
	if cj.State.OOMKilled {
		parts = append(parts, model.ReasonOOMKilled)
	}
	if cj.State.Error != "" {
		parts = append(parts, cj.State.Error)
	}
	if h := cj.State.Health; h != nil && h.Status == "unhealthy" && len(h.Log) > 0 {
		parts = append(parts, "last health check: "+strings.TrimSpace(h.Log[len(h.Log)-1].Output))
	}
	return strings.Join(parts, "; ")
}

// Events reads the daemon's stored container events for the service. The
// daemon keeps a bounded in-memory log, so the zero since means the last day.
func (p *Provider) Events(ctx context.Context, ref model.ServiceRef, since time.Time) ([]model.Event, error) {
	now := p.now()
	if since.IsZero() {
		since = now.Add(-defaultLookback)
	}
	filters := map[string][]string{"label": {labelProject + "=" + ref.Scope, labelService + "=" + ref.Name}}
	if ref.Scope == StandaloneScope {
		filters = map[string][]string{"container": {ref.Name}}
	}
	dec, body, err := p.engine.events(ctx, since, now, filters)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	out := []model.Event{}
	for {
		var m eventMessage
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("docker events: %w", err)
		}
		if e, ok := mapEvent(ref, m); ok {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

func mapEvent(ref model.ServiceRef, m eventMessage) (model.Event, bool) {
	action, _, _ := strings.Cut(m.Action, ":") // "health_status: unhealthy"
	switch action {
	case "create", "start", "restart", "stop", "die", "kill", "oom", "destroy", "pause", "unpause", "health_status":
	default:
		return model.Event{}, false // exec_*, attach, resize and the like are noise here
	}
	name := m.Actor.Attributes["name"]
	msg := "container " + name + " " + strings.TrimSpace(m.Action)
	sev := model.SeverityInfo
	switch {
	case action == "die":
		code := m.Actor.Attributes["exitCode"]
		msg += " (exit code " + code + ")"
		if code != "0" {
			sev = model.SeverityWarning
		}
	case action == "oom", action == "kill", m.Action == "health_status: unhealthy":
		sev = model.SeverityWarning
	}
	return model.Event{
		Time: time.Unix(0, m.TimeNano).UTC(), Service: ref, Instance: shortID(m.Actor.ID), Severity: sev, Message: msg,
	}, true
}

func (p *Provider) Logs(ctx context.Context, ref model.InstanceRef, opts provider.LogOptions) (provider.LogStream, error) {
	cj, err := p.engine.inspectContainer(ctx, ref.ID)
	if err != nil {
		return nil, err
	}
	if got := p.serviceRef(cj.Config.Labels, strings.TrimPrefix(cj.Name, "/")); got != ref.Service {
		return nil, fmt.Errorf("container %s belongs to %s: %w", ref.ID, got, provider.ErrNotFound)
	}
	tail := opts.Tail
	if tail <= 0 {
		tail = defaultTail
	}
	body, err := p.engine.logs(ctx, cj.ID, opts, tail)
	if err != nil {
		return nil, err
	}
	return newLogStream(body, cj.Config.Tty), nil
}

// Watch streams container events and signals a refresh for each one. The
// channel closes when the stream ends; the caller re-watches.
func (p *Provider) Watch(ctx context.Context) (<-chan struct{}, error) {
	dec, body, err := p.engine.events(ctx, time.Time{}, time.Time{}, nil)
	if err != nil {
		return nil, err
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		defer body.Close()
		for {
			var m eventMessage
			if err := dec.Decode(&m); err != nil {
				return
			}
			if _, ok := mapEvent(model.ServiceRef{}, m); !ok {
				continue
			}
			select {
			case ch <- struct{}{}:
			default: // a refresh is already pending
			}
		}
	}()
	return ch, nil
}

func statusOf(state string) string {
	switch state {
	case "running":
		return model.StatusRunning
	case "created", "restarting":
		return model.StatusPending
	case "exited", "dead", "removing":
		return model.StatusStopped
	default:
		return model.StatusUnknown
	}
}

func healthOf(s string) string {
	switch s {
	case "healthy":
		return model.HealthHealthy
	case "unhealthy":
		return model.HealthUnhealthy
	case "starting":
		return model.HealthStarting
	default:
		return ""
	}
}

// parseTime reads a daemon timestamp; the daemon writes the zero time for
// "never", which is reported as absent.
func parseTime(s string) *time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.Year() <= 1 {
		return nil
	}
	t = t.UTC()
	return &t
}

func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func shortID(id string) string {
	if len(id) > shortIDLen {
		return id[:shortIDLen]
	}
	return id
}
