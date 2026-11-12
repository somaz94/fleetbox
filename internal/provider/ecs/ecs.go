// Package ecs adapts Amazon ECS to the provider contract. One target is one
// account (through an assumed role) and one region; a scope is a cluster.
package ecs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
)

const (
	Kind = "ecs"

	describeServicesBatch = 10
	describeTasksBatch    = 100
	defaultTail           = 200
	defaultFollowInterval = 2 * time.Second
	sessionName           = "fleetbox"
)

func init() { provider.Register(Kind, New) }

// Config is the target's `ecs:` block.
type Config struct {
	Region         string   `yaml:"region"`
	ReadRoleArn    string   `yaml:"readRoleArn"`
	OperateRoleArn string   `yaml:"operateRoleArn"`
	Clusters       []string `yaml:"clusters"`
}

// API is the subset of the ECS client this adapter calls.
type API interface {
	ListClusters(context.Context, *ecs.ListClustersInput, ...func(*ecs.Options)) (*ecs.ListClustersOutput, error)
	ListServices(context.Context, *ecs.ListServicesInput, ...func(*ecs.Options)) (*ecs.ListServicesOutput, error)
	DescribeServices(context.Context, *ecs.DescribeServicesInput, ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error)
	ListTasks(context.Context, *ecs.ListTasksInput, ...func(*ecs.Options)) (*ecs.ListTasksOutput, error)
	DescribeTasks(context.Context, *ecs.DescribeTasksInput, ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error)
	DescribeTaskDefinition(context.Context, *ecs.DescribeTaskDefinitionInput, ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error)
}

// OperateAPI is the call made with the target's operate role.
type OperateAPI interface {
	UpdateService(context.Context, *ecs.UpdateServiceInput, ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error)
}

// LogsAPI is the subset of the CloudWatch Logs client this adapter calls.
type LogsAPI interface {
	GetLogEvents(context.Context, *cloudwatchlogs.GetLogEventsInput, ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error)
}

// Provider is the ECS adapter for one target.
type Provider struct {
	target         string
	cfg            Config
	api            API
	operate        OperateAPI // nil without operateRoleArn
	logs           func(region string) LogsAPI
	followInterval time.Duration
	taskDefs       sync.Map // task definition ARN -> *types.TaskDefinition; immutable once registered
}

// New builds the adapter from the target's `ecs:` block. Credentials come from
// the SDK default chain; ReadRoleArn and OperateRoleArn are each assumed from
// that identity, never from one another.
func New(t config.Target) (provider.Provider, error) {
	var c Config
	if err := config.DecodeStrict(&t.Spec, &c); err != nil {
		return nil, fmt.Errorf("ecs: %w", err)
	}
	if c.Region == "" {
		return nil, errors.New("ecs: region is required")
	}
	base, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(c.Region))
	if err != nil {
		return nil, fmt.Errorf("ecs: load AWS config: %w", err)
	}
	identity := base.Copy()
	assume := func(arn string) aws.CredentialsProvider {
		return aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(identity), arn,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = sessionName }))
	}
	if c.ReadRoleArn != "" {
		base.Credentials = assume(c.ReadRoleArn)
	}
	var mu sync.Mutex
	clients := map[string]LogsAPI{}
	logs := func(region string) LogsAPI {
		mu.Lock()
		defer mu.Unlock()
		if cl, ok := clients[region]; ok {
			return cl
		}
		rc := base.Copy()
		rc.Region = region
		cl := cloudwatchlogs.NewFromConfig(rc)
		clients[region] = cl
		return cl
	}
	p := NewWithClients(t.Name, c, ecs.NewFromConfig(base), logs)
	if c.OperateRoleArn != "" {
		oc := identity
		oc.Credentials = assume(c.OperateRoleArn)
		p.WithOperateClient(ecs.NewFromConfig(oc))
	}
	return p, nil
}

// NewWithClients builds the adapter around given clients; tests pass fakes.
func NewWithClients(target string, c Config, api API, logs func(region string) LogsAPI) *Provider {
	return &Provider{target: target, cfg: c, api: api, logs: logs, followInterval: defaultFollowInterval}
}

// WithOperateClient enables restart and scale through op, a client holding
// the operate role.
func (p *Provider) WithOperateClient(op OperateAPI) *Provider {
	p.operate = op
	return p
}

func (p *Provider) Kind() string { return Kind }

func (p *Provider) Capabilities() provider.Capabilities {
	ops := p.operate != nil
	return provider.Capabilities{Logs: true, FollowLogs: true, Events: true, Restart: ops, Scale: ops}
}

// Restart starts a new deployment of the service with its current task
// definition, which replaces every task.
func (p *Provider) Restart(ctx context.Context, ref model.ServiceRef) error {
	return p.update(ctx, ref, &ecs.UpdateServiceInput{ForceNewDeployment: true})
}

// Scale sets the service's desired task count.
func (p *Provider) Scale(ctx context.Context, ref model.ServiceRef, desired int) error {
	if desired < 0 || desired > math.MaxInt32 {
		return fmt.Errorf("ecs: desired count %d out of range", desired)
	}
	return p.update(ctx, ref, &ecs.UpdateServiceInput{DesiredCount: aws.Int32(int32(desired))})
}

func (p *Provider) update(ctx context.Context, ref model.ServiceRef, in *ecs.UpdateServiceInput) error {
	if p.operate == nil {
		return fmt.Errorf("target %q has no operateRoleArn: %w", p.target, provider.ErrUnsupported)
	}
	in.Cluster, in.Service = aws.String(ref.Scope), aws.String(ref.Name)
	if _, err := p.operate.UpdateService(ctx, in); err != nil {
		return p.classify(err)
	}
	return nil
}

// Inventory lists every cluster's services, then its running and recently
// stopped tasks in two calls per cluster, grouping tasks by their
// "service:<name>" group instead of listing tasks per service.
func (p *Provider) Inventory(ctx context.Context) (model.Inventory, error) {
	clusters, err := p.clusters(ctx)
	if err != nil {
		return model.Inventory{}, err
	}
	var inv model.Inventory
	for _, cluster := range clusters {
		svcs, err := p.describeAllServices(ctx, cluster)
		if err != nil {
			return model.Inventory{}, err
		}
		known := map[string]bool{}
		for _, s := range svcs {
			ms := p.mapService(cluster, s)
			known[ms.Ref.Name] = true
			inv.Services = append(inv.Services, ms)
		}
		tasks, err := p.tasks(ctx, cluster, "")
		if err != nil {
			return model.Inventory{}, err
		}
		for _, task := range tasks {
			name, ok := serviceOfTask(task)
			if !ok || !known[name] {
				continue // standalone RunTask tasks have no service
			}
			inv.Instances = append(inv.Instances, p.mapTask(cluster, name, task))
		}
	}
	sortInventory(&inv)
	return inv, nil
}

func (p *Provider) Service(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error) {
	svc, err := p.describeService(ctx, ref)
	if err != nil {
		return model.ServiceDetail{}, err
	}
	tasks, err := p.tasks(ctx, ref.Scope, ref.Name)
	if err != nil {
		return model.ServiceDetail{}, err
	}
	d := model.ServiceDetail{Service: p.mapService(ref.Scope, svc), Instances: []model.Instance{}}
	for _, task := range tasks {
		d.Instances = append(d.Instances, p.mapTask(ref.Scope, ref.Name, task))
	}
	sortInstances(d.Instances)
	return d, nil
}

// Events merges the service's own event log with the stop reasons of its
// recently stopped tasks, newest first.
func (p *Provider) Events(ctx context.Context, ref model.ServiceRef, since time.Time) ([]model.Event, error) {
	svc, err := p.describeService(ctx, ref)
	if err != nil {
		return nil, err
	}
	var out []model.Event
	for _, e := range svc.Events {
		t := aws.ToTime(e.CreatedAt)
		if t.Before(since) {
			continue
		}
		msg := aws.ToString(e.Message)
		out = append(out, model.Event{Time: t, Service: ref, Severity: severityOf(msg), Message: msg})
	}
	stopped, err := p.listTasks(ctx, ref.Scope, ref.Name, types.DesiredStatusStopped)
	if err != nil {
		return nil, err
	}
	tasks, err := p.describeTasks(ctx, ref.Scope, stopped)
	if err != nil {
		return nil, err
	}
	for _, task := range tasks {
		in := p.mapTask(ref.Scope, ref.Name, task)
		if in.StoppedAt == nil || in.StoppedAt.Before(since) {
			continue
		}
		msg := "task " + in.Ref.ID + " stopped"
		if in.StopReason != "" {
			msg += ": " + in.StopReason
		}
		out = append(out, model.Event{Time: *in.StoppedAt, Service: ref, Instance: in.Ref.ID, Severity: model.SeverityWarning, Message: msg})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// Logs reads a container's awslogs stream. Without Follow it returns one page
// (the last Tail lines, or the first Tail lines after Since); with Follow it
// then polls forward until the context ends.
func (p *Provider) Logs(ctx context.Context, ref model.InstanceRef, opts provider.LogOptions) (provider.LogStream, error) {
	tasks, err := p.describeTasks(ctx, ref.Service.Scope, []string{ref.ID})
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, fmt.Errorf("task %s: %w", ref, provider.ErrNotFound)
	}
	td, err := p.taskDefinition(ctx, aws.ToString(tasks[0].TaskDefinitionArn))
	if err != nil {
		return nil, err
	}
	group, region, stream, err := p.logLocation(td, opts.Container, ref.ID)
	if err != nil {
		return nil, err
	}
	tail := opts.Tail
	if tail <= 0 {
		tail = defaultTail
	}
	client := p.logs(region)
	fetch := func(ctx context.Context, token *string) ([]model.LogLine, *string, error) {
		in := &cloudwatchlogs.GetLogEventsInput{LogGroupName: &group, LogStreamName: &stream, NextToken: token}
		switch {
		case token != nil:
			in.StartFromHead = aws.Bool(true) // required when continuing from a forward token
		case !opts.Since.IsZero():
			in.StartFromHead, in.StartTime, in.Limit = aws.Bool(true), aws.Int64(opts.Since.UnixMilli()), aws.Int32(int32(tail))
		default:
			in.StartFromHead, in.Limit = aws.Bool(false), aws.Int32(int32(tail))
		}
		out, err := client.GetLogEvents(ctx, in)
		if err != nil {
			return nil, nil, p.classify(err)
		}
		lines := make([]model.LogLine, 0, len(out.Events))
		for _, e := range out.Events {
			t := time.UnixMilli(aws.ToInt64(e.Timestamp)).UTC()
			lines = append(lines, model.LogLine{Time: &t, Text: aws.ToString(e.Message)})
		}
		return lines, out.NextForwardToken, nil
	}
	return newPollStream(ctx, fetch, opts.Follow, p.followInterval), nil
}

func (p *Provider) logLocation(td *types.TaskDefinition, container, taskID string) (group, region, stream string, err error) {
	for _, cd := range td.ContainerDefinitions {
		name := aws.ToString(cd.Name)
		if container != "" && name != container {
			continue
		}
		lc := cd.LogConfiguration
		if lc == nil || lc.LogDriver != types.LogDriverAwslogs {
			if container != "" {
				return "", "", "", fmt.Errorf("container %q does not log to CloudWatch: %w", name, provider.ErrUnsupported)
			}
			continue
		}
		prefix := lc.Options["awslogs-stream-prefix"]
		if prefix == "" {
			// Without a prefix the stream is named after the Docker container ID,
			// which the ECS API does not expose.
			return "", "", "", fmt.Errorf("container %q has no awslogs-stream-prefix: %w", name, provider.ErrUnsupported)
		}
		region = lc.Options["awslogs-region"]
		if region == "" {
			region = p.cfg.Region
		}
		return lc.Options["awslogs-group"], region, prefix + "/" + name + "/" + taskID, nil
	}
	if container != "" {
		return "", "", "", fmt.Errorf("container %q: %w", container, provider.ErrNotFound)
	}
	return "", "", "", fmt.Errorf("no container logs to CloudWatch: %w", provider.ErrUnsupported)
}

func (p *Provider) clusters(ctx context.Context) ([]string, error) {
	if len(p.cfg.Clusters) > 0 {
		return p.cfg.Clusters, nil
	}
	var names []string
	var token *string
	for {
		out, err := p.api.ListClusters(ctx, &ecs.ListClustersInput{NextToken: token})
		if err != nil {
			return nil, p.classify(err)
		}
		for _, arn := range out.ClusterArns {
			names = append(names, lastSegment(arn))
		}
		if token = out.NextToken; token == nil {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

func (p *Provider) describeAllServices(ctx context.Context, cluster string) ([]types.Service, error) {
	var arns []string
	var token *string
	for {
		out, err := p.api.ListServices(ctx, &ecs.ListServicesInput{Cluster: &cluster, NextToken: token})
		if err != nil {
			return nil, p.classify(err)
		}
		arns = append(arns, out.ServiceArns...)
		if token = out.NextToken; token == nil {
			break
		}
	}
	var svcs []types.Service
	for start := 0; start < len(arns); start += describeServicesBatch {
		end := min(start+describeServicesBatch, len(arns))
		out, err := p.api.DescribeServices(ctx, &ecs.DescribeServicesInput{
			Cluster: &cluster, Services: arns[start:end], Include: []types.ServiceField{types.ServiceFieldTags},
		})
		if err != nil {
			return nil, p.classify(err)
		}
		svcs = append(svcs, out.Services...)
	}
	return svcs, nil
}

func (p *Provider) describeService(ctx context.Context, ref model.ServiceRef) (types.Service, error) {
	out, err := p.api.DescribeServices(ctx, &ecs.DescribeServicesInput{
		Cluster: &ref.Scope, Services: []string{ref.Name}, Include: []types.ServiceField{types.ServiceFieldTags},
	})
	if err != nil {
		return types.Service{}, p.classify(err)
	}
	for _, s := range out.Services {
		if aws.ToString(s.ServiceName) == ref.Name && aws.ToString(s.Status) != "INACTIVE" {
			return s, nil
		}
	}
	return types.Service{}, fmt.Errorf("service %s: %w", ref, provider.ErrNotFound)
}

// tasks returns the running and recently stopped tasks of a cluster, or of one
// service when service is set.
func (p *Provider) tasks(ctx context.Context, cluster, service string) ([]types.Task, error) {
	var arns []string
	for _, st := range []types.DesiredStatus{types.DesiredStatusRunning, types.DesiredStatusStopped} {
		a, err := p.listTasks(ctx, cluster, service, st)
		if err != nil {
			return nil, err
		}
		arns = append(arns, a...)
	}
	return p.describeTasks(ctx, cluster, arns)
}

func (p *Provider) listTasks(ctx context.Context, cluster, service string, st types.DesiredStatus) ([]string, error) {
	var arns []string
	var token *string
	for {
		in := &ecs.ListTasksInput{Cluster: &cluster, DesiredStatus: st, NextToken: token}
		if service != "" {
			in.ServiceName = &service
		}
		out, err := p.api.ListTasks(ctx, in)
		if err != nil {
			return nil, p.classify(err)
		}
		arns = append(arns, out.TaskArns...)
		if token = out.NextToken; token == nil {
			return arns, nil
		}
	}
}

func (p *Provider) describeTasks(ctx context.Context, cluster string, arns []string) ([]types.Task, error) {
	var tasks []types.Task
	for start := 0; start < len(arns); start += describeTasksBatch {
		end := min(start+describeTasksBatch, len(arns))
		out, err := p.api.DescribeTasks(ctx, &ecs.DescribeTasksInput{Cluster: &cluster, Tasks: arns[start:end]})
		if err != nil {
			return nil, p.classify(err)
		}
		tasks = append(tasks, out.Tasks...)
	}
	return tasks, nil
}

func (p *Provider) taskDefinition(ctx context.Context, arn string) (*types.TaskDefinition, error) {
	if v, ok := p.taskDefs.Load(arn); ok {
		return v.(*types.TaskDefinition), nil
	}
	out, err := p.api.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{TaskDefinition: &arn})
	if err != nil {
		return nil, p.classify(err)
	}
	if out.TaskDefinition == nil {
		return nil, fmt.Errorf("task definition %s: %w", arn, provider.ErrNotFound)
	}
	p.taskDefs.Store(arn, out.TaskDefinition)
	return out.TaskDefinition, nil
}

func (p *Provider) mapService(cluster string, s types.Service) model.Service {
	ms := model.Service{
		Ref:      model.ServiceRef{Target: p.target, Scope: cluster, Name: aws.ToString(s.ServiceName)},
		Provider: Kind,
		Desired:  model.Ptr(int(s.DesiredCount)),
		Running:  model.Ptr(int(s.RunningCount)),
		Pending:  model.Ptr(int(s.PendingCount)),
		Revision: revisionOf(aws.ToString(s.TaskDefinition)),
	}
	for _, d := range s.Deployments {
		if aws.ToString(d.Status) == "PRIMARY" {
			ms.Rollout = &model.Rollout{State: string(d.RolloutState), StartedAt: d.CreatedAt}
		}
	}
	if len(s.Tags) > 0 {
		ms.Labels = map[string]string{}
		for _, t := range s.Tags {
			ms.Labels[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
	}
	return ms
}

func (p *Provider) mapTask(cluster, service string, t types.Task) model.Instance {
	in := model.Instance{
		Ref:        model.InstanceRef{Service: model.ServiceRef{Target: p.target, Scope: cluster, Name: service}, ID: lastSegment(aws.ToString(t.TaskArn))},
		Status:     statusOf(aws.ToString(t.LastStatus)),
		Health:     healthOf(t.HealthStatus),
		Revision:   revisionOf(aws.ToString(t.TaskDefinitionArn)),
		StartedAt:  t.StartedAt,
		StoppedAt:  t.StoppedAt,
		StopReason: aws.ToString(t.StoppedReason),
		Containers: make([]model.Container, 0, len(t.Containers)),
	}
	for _, c := range t.Containers {
		mc := model.Container{
			Name:   aws.ToString(c.Name),
			Image:  aws.ToString(c.Image),
			Digest: aws.ToString(c.ImageDigest),
			Status: strings.ToLower(aws.ToString(c.LastStatus)),
			Health: healthOf(c.HealthStatus),
			Reason: aws.ToString(c.Reason),
		}
		if c.ExitCode != nil {
			mc.ExitCode = model.Ptr(int(*c.ExitCode))
		}
		for _, b := range c.NetworkBindings {
			port := model.Port{Container: int(aws.ToInt32(b.ContainerPort)), Protocol: string(b.Protocol)}
			if b.HostPort != nil {
				port.Host = model.Ptr(int(*b.HostPort))
			}
			mc.Ports = append(mc.Ports, port)
		}
		in.Containers = append(in.Containers, mc)
	}
	sort.Slice(in.Containers, func(i, j int) bool { return in.Containers[i].Name < in.Containers[j].Name })
	return in
}

// classify keeps cancellation as is, maps ECS not-found errors to
// ErrNotFound and network failures to ErrUnreachable. A service that is not
// active (draining) cannot be operated and reads as not found too.
func (p *Provider) classify(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var cnf *types.ClusterNotFoundException
	var snf *types.ServiceNotFoundException
	var sna *types.ServiceNotActiveException
	if errors.As(err, &cnf) || errors.As(err, &snf) || errors.As(err, &sna) {
		return fmt.Errorf("%w: %w", provider.ErrNotFound, err)
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return fmt.Errorf("%s: %w: %w", p.target, provider.ErrUnreachable, err)
	}
	return err
}

func serviceOfTask(t types.Task) (string, bool) {
	return strings.CutPrefix(aws.ToString(t.Group), "service:")
}

func statusOf(s string) string {
	switch s {
	case "RUNNING":
		return model.StatusRunning
	case "PROVISIONING", "PENDING", "ACTIVATING":
		return model.StatusPending
	case "DEACTIVATING", "STOPPING", "DEPROVISIONING", "STOPPED", "DELETED":
		return model.StatusStopped
	default:
		return model.StatusUnknown
	}
}

// healthOf maps UNKNOWN to no health: ECS reports UNKNOWN both before the
// first check and when there is no check at all.
func healthOf(h types.HealthStatus) string {
	switch h {
	case types.HealthStatusHealthy:
		return model.HealthHealthy
	case types.HealthStatusUnhealthy:
		return model.HealthUnhealthy
	default:
		return ""
	}
}

var warningWords = []string{"unable", "failed", "unhealthy", "error", "insufficient"}

func severityOf(msg string) string {
	lower := strings.ToLower(msg)
	for _, w := range warningWords {
		if strings.Contains(lower, w) {
			return model.SeverityWarning
		}
	}
	return model.SeverityInfo
}

// revisionOf turns a task definition ARN into family:revision.
func revisionOf(arn string) string { return lastSegment(arn) }

func lastSegment(arn string) string {
	if i := strings.LastIndex(arn, "/"); i >= 0 {
		return arn[i+1:]
	}
	return arn
}

func sortInventory(inv *model.Inventory) {
	sort.Slice(inv.Services, func(i, j int) bool { return inv.Services[i].Ref.String() < inv.Services[j].Ref.String() })
	sortInstances(inv.Instances)
}

func sortInstances(ins []model.Instance) {
	sort.Slice(ins, func(i, j int) bool { return ins[i].Ref.String() < ins[j].Ref.String() })
}
