package ecs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwtypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"
	"gopkg.in/yaml.v3"

	"github.com/somaz94/fleetbox/internal/config"
	"github.com/somaz94/fleetbox/internal/model"
	"github.com/somaz94/fleetbox/internal/provider"
	"github.com/somaz94/fleetbox/internal/provider/providertest"
)

const (
	acct     = "arn:aws:ecs:ap-northeast-2:123456789012"
	pageSize = 2
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fakeECS struct {
	mu       sync.Mutex
	clusters []string
	services map[string][]types.Service
	tasks    map[string][]types.Task
	taskDefs map[string]*types.TaskDefinition
	calls    map[string]int
	err      error
}

func (f *fakeECS) begin(ctx context.Context, op string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[op]++
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.err
}

func (f *fakeECS) cluster(name *string) error {
	if _, ok := f.services[aws.ToString(name)]; !ok {
		return &types.ClusterNotFoundException{Message: aws.String("cluster not found")}
	}
	return nil
}

func page[T any](all []T, token *string) ([]T, *string) {
	start := 0
	if token != nil {
		fmt.Sscan(*token, &start)
	}
	end := min(start+pageSize, len(all))
	var next *string
	if end < len(all) {
		next = aws.String(fmt.Sprint(end))
	}
	return all[start:end], next
}

func (f *fakeECS) ListClusters(ctx context.Context, in *ecs.ListClustersInput, _ ...func(*ecs.Options)) (*ecs.ListClustersOutput, error) {
	if err := f.begin(ctx, "ListClusters"); err != nil {
		return nil, err
	}
	var arns []string
	for _, c := range f.clusters {
		arns = append(arns, acct+":cluster/"+c)
	}
	p, next := page(arns, in.NextToken)
	return &ecs.ListClustersOutput{ClusterArns: p, NextToken: next}, nil
}

func (f *fakeECS) ListServices(ctx context.Context, in *ecs.ListServicesInput, _ ...func(*ecs.Options)) (*ecs.ListServicesOutput, error) {
	if err := f.begin(ctx, "ListServices"); err != nil {
		return nil, err
	}
	if err := f.cluster(in.Cluster); err != nil {
		return nil, err
	}
	var arns []string
	for _, s := range f.services[*in.Cluster] {
		arns = append(arns, aws.ToString(s.ServiceArn))
	}
	p, next := page(arns, in.NextToken)
	return &ecs.ListServicesOutput{ServiceArns: p, NextToken: next}, nil
}

func (f *fakeECS) DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, _ ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if err := f.begin(ctx, "DescribeServices"); err != nil {
		return nil, err
	}
	if err := f.cluster(in.Cluster); err != nil {
		return nil, err
	}
	if len(in.Services) > describeServicesBatch {
		return nil, errors.New("too many services in one call")
	}
	out := &ecs.DescribeServicesOutput{}
	for _, want := range in.Services {
		found := false
		for _, s := range f.services[*in.Cluster] {
			if aws.ToString(s.ServiceArn) == want || aws.ToString(s.ServiceName) == want {
				out.Services = append(out.Services, s)
				found = true
			}
		}
		if !found {
			out.Failures = append(out.Failures, types.Failure{Arn: aws.String(want), Reason: aws.String("MISSING")})
		}
	}
	return out, nil
}

func (f *fakeECS) ListTasks(ctx context.Context, in *ecs.ListTasksInput, _ ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if err := f.begin(ctx, "ListTasks"); err != nil {
		return nil, err
	}
	if err := f.cluster(in.Cluster); err != nil {
		return nil, err
	}
	var arns []string
	for _, t := range f.tasks[*in.Cluster] {
		if aws.ToString(t.DesiredStatus) != string(in.DesiredStatus) {
			continue
		}
		if in.ServiceName != nil && aws.ToString(t.Group) != "service:"+*in.ServiceName {
			continue
		}
		arns = append(arns, aws.ToString(t.TaskArn))
	}
	p, next := page(arns, in.NextToken)
	return &ecs.ListTasksOutput{TaskArns: p, NextToken: next}, nil
}

func (f *fakeECS) DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, _ ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if err := f.begin(ctx, "DescribeTasks"); err != nil {
		return nil, err
	}
	if err := f.cluster(in.Cluster); err != nil {
		return nil, err
	}
	out := &ecs.DescribeTasksOutput{}
	for _, want := range in.Tasks {
		for _, t := range f.tasks[*in.Cluster] {
			arn := aws.ToString(t.TaskArn)
			if arn == want || lastSegment(arn) == want {
				out.Tasks = append(out.Tasks, t)
			}
		}
	}
	return out, nil
}

func (f *fakeECS) DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	if err := f.begin(ctx, "DescribeTaskDefinition"); err != nil {
		return nil, err
	}
	return &ecs.DescribeTaskDefinitionOutput{TaskDefinition: f.taskDefs[aws.ToString(in.TaskDefinition)]}, nil
}

type fakeLogs struct {
	mu     sync.Mutex
	inputs []cloudwatchlogs.GetLogEventsInput
	err    error
}

func (f *fakeLogs) GetLogEvents(ctx context.Context, in *cloudwatchlogs.GetLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.GetLogEventsOutput, error) {
	f.mu.Lock()
	f.inputs = append(f.inputs, *in)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	ev := func(ms int64, msg string) cwtypes.OutputLogEvent {
		return cwtypes.OutputLogEvent{Timestamp: aws.Int64(ms), Message: aws.String(msg)}
	}
	switch aws.ToString(in.NextToken) {
	case "":
		return &cloudwatchlogs.GetLogEventsOutput{Events: []cwtypes.OutputLogEvent{ev(1000, "one"), ev(2000, "two")}, NextForwardToken: aws.String("f1")}, nil
	case "f1":
		return &cloudwatchlogs.GetLogEventsOutput{Events: []cwtypes.OutputLogEvent{ev(3000, "three")}, NextForwardToken: aws.String("f2")}, nil
	default:
		return &cloudwatchlogs.GetLogEventsOutput{NextForwardToken: in.NextToken}, nil
	}
}

func (f *fakeECS) UpdateService(ctx context.Context, in *ecs.UpdateServiceInput, _ ...func(*ecs.Options)) (*ecs.UpdateServiceOutput, error) {
	if err := f.begin(ctx, "UpdateService"); err != nil {
		return nil, err
	}
	if err := f.cluster(in.Cluster); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, s := range f.services[*in.Cluster] {
		if aws.ToString(s.ServiceName) != aws.ToString(in.Service) {
			continue
		}
		if in.DesiredCount != nil {
			s.DesiredCount = *in.DesiredCount
		}
		if in.ForceNewDeployment {
			s.Deployments = []types.Deployment{{Status: aws.String("PRIMARY"), RolloutState: types.DeploymentRolloutStateInProgress, CreatedAt: aws.Time(t0.Add(time.Hour))}}
		}
		f.services[*in.Cluster][i] = s
		return &ecs.UpdateServiceOutput{Service: &s}, nil
	}
	return nil, &types.ServiceNotFoundException{Message: aws.String("service not found")}
}

func svc(cluster, name string, desired, running int32, tags ...types.Tag) types.Service {
	return types.Service{
		ServiceName:    aws.String(name),
		ServiceArn:     aws.String(acct + ":service/" + cluster + "/" + name),
		Status:         aws.String("ACTIVE"),
		DesiredCount:   desired,
		RunningCount:   running,
		TaskDefinition: aws.String(acct + ":task-definition/" + name + ":7"),
		Deployments: []types.Deployment{
			{Status: aws.String("ACTIVE"), RolloutState: types.DeploymentRolloutStateCompleted},
			{Status: aws.String("PRIMARY"), RolloutState: types.DeploymentRolloutStateInProgress, CreatedAt: aws.Time(t0)},
		},
		Events: []types.ServiceEvent{
			{CreatedAt: aws.Time(t0.Add(time.Minute)), Message: aws.String("(service " + name + ") has reached a steady state.")},
			{CreatedAt: aws.Time(t0.Add(2 * time.Minute)), Message: aws.String("(service " + name + ") was unable to place a task.")},
		},
		Tags: tags,
	}
}

func task(cluster, id, group, desired, last string) types.Task {
	return types.Task{
		TaskArn:           aws.String(acct + ":task/" + cluster + "/" + id),
		Group:             aws.String(group),
		DesiredStatus:     aws.String(desired),
		LastStatus:        aws.String(last),
		TaskDefinitionArn: aws.String(acct + ":task-definition/api:7"),
		HealthStatus:      types.HealthStatusHealthy,
		StartedAt:         aws.Time(t0),
		CreatedAt:         aws.Time(t0.Add(-time.Minute)),
		LaunchType:        types.LaunchTypeFargate,
		AvailabilityZone:  aws.String("ap-northeast-2a"),
		Cpu:               aws.String("256"),
		Memory:            aws.String("512"),
		Containers: []types.Container{
			{Name: aws.String("app"), Image: aws.String("example/api:1.0"), ImageDigest: aws.String("sha256:aaa"), LastStatus: aws.String(last),
				HealthStatus:      types.HealthStatusUnknown,
				NetworkInterfaces: []types.NetworkInterface{{PrivateIpv4Address: aws.String("192.0.2.20")}},
				NetworkBindings: []types.NetworkBinding{
					{ContainerPort: aws.Int32(8080), HostPort: aws.Int32(32768), Protocol: types.TransportProtocolTcp},
					{ContainerPort: aws.Int32(9090)},
				}},
			{Name: aws.String("envoy"), Image: aws.String("envoy:1"), LastStatus: aws.String(last), HealthStatus: types.HealthStatusUnhealthy,
				NetworkInterfaces: []types.NetworkInterface{{PrivateIpv4Address: aws.String("192.0.2.20")}, {}}},
		},
	}
}

func newFake() *fakeECS {
	stopped := task("web", "t-stop", "service:api", "STOPPED", "STOPPED")
	stopped.StoppedAt = aws.Time(t0.Add(3 * time.Minute))
	stopped.StoppedReason = aws.String("Essential container in task exited")
	stopped.Containers[0].ExitCode = aws.Int32(137)

	web := []types.Service{
		svc("web", "api", 2, 2, types.Tag{Key: aws.String("fleetbox.io/app"), Value: aws.String("api")}),
		svc("web", "worker", 1, 0),
	}
	for i := range 11 {
		web = append(web, svc("web", fmt.Sprintf("extra-%02d", i), 0, 0))
	}
	return &fakeECS{
		clusters: []string{"web", "batch", "empty"},
		services: map[string][]types.Service{
			"web":   web,
			"batch": {svc("batch", "nightly", 1, 1)},
			"empty": nil,
		},
		tasks: map[string][]types.Task{
			"web": {
				task("web", "t-2", "service:api", "RUNNING", "RUNNING"),
				task("web", "t-1", "service:api", "RUNNING", "PENDING"),
				stopped,
				task("web", "t-x", "family:adhoc", "RUNNING", "RUNNING"),
			},
			"batch": {task("batch", "t-9", "service:nightly", "RUNNING", "RUNNING")},
		},
		taskDefs: map[string]*types.TaskDefinition{
			acct + ":task-definition/api:7": {ContainerDefinitions: []types.ContainerDefinition{
				{Name: aws.String("envoy")},
				{Name: aws.String("app"), HealthCheck: &types.HealthCheck{Command: []string{"CMD-SHELL", "curl -f http://localhost/"}},
					LogConfiguration: &types.LogConfiguration{LogDriver: types.LogDriverAwslogs,
						Options: map[string]string{"awslogs-group": "/ecs/api", "awslogs-stream-prefix": "ecs"}}},
				{Name: aws.String("noprefix"), LogConfiguration: &types.LogConfiguration{LogDriver: types.LogDriverAwslogs,
					Options: map[string]string{"awslogs-group": "/ecs/x"}}},
				{Name: aws.String("other"), LogConfiguration: &types.LogConfiguration{LogDriver: types.LogDriverAwslogs,
					Options: map[string]string{"awslogs-group": "/ecs/o", "awslogs-stream-prefix": "o", "awslogs-region": "us-east-1"}}},
			}},
		},
	}
}

func newTestProvider(f *fakeECS, logs *fakeLogs, regions *[]string) *Provider {
	p := NewWithClients("prod", Config{Region: "ap-northeast-2"}, f, func(r string) LogsAPI {
		if regions != nil {
			*regions = append(*regions, r)
		}
		return logs
	}).WithOperateClient(f)
	p.followInterval = time.Millisecond
	return p
}

var apiRef = model.ServiceRef{Target: "prod", Scope: "web", Name: "api"}

func TestConformance(t *testing.T) {
	providertest.Run(t, func(*testing.T) provider.Provider { return newTestProvider(newFake(), &fakeLogs{}, nil) }, providertest.Fixture{
		Target:        "prod",
		Known:         apiRef,
		KnownInstance: model.InstanceRef{Service: apiRef, ID: "t-2"},
		Missing:       model.ServiceRef{Target: "prod", Scope: "web", Name: "nope"},
	})
}

func TestInventory(t *testing.T) {
	f := newFake()
	inv, err := newTestProvider(f, &fakeLogs{}, nil).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(inv.Services); got != 14 {
		t.Fatalf("services = %d, want 14", got)
	}
	if f.calls["DescribeServices"] != 3 { // web: 13 services in batches of 10, batch: 1
		t.Fatalf("DescribeServices calls = %d", f.calls["DescribeServices"])
	}
	var api model.Service
	for _, s := range inv.Services {
		if s.Ref == apiRef {
			api = s
		}
	}
	if *api.Desired != 2 || *api.Running != 2 || *api.Pending != 0 || api.Revision != "api:7" {
		t.Fatalf("api = %+v", api)
	}
	if api.Rollout == nil || api.Rollout.State != "IN_PROGRESS" || !api.Rollout.StartedAt.Equal(t0) {
		t.Fatalf("rollout = %+v", api.Rollout)
	}
	if api.Labels["fleetbox.io/app"] != "api" {
		t.Fatalf("labels = %v", api.Labels)
	}

	byID := map[string]model.Instance{}
	for _, in := range inv.Instances {
		byID[in.Ref.ID] = in
	}
	if _, ok := byID["t-x"]; ok {
		t.Fatal("standalone task must not be listed")
	}
	if len(byID) != 4 {
		t.Fatalf("instances = %v", byID)
	}
	run := byID["t-2"]
	if run.Status != model.StatusRunning || run.Health != model.HealthHealthy || run.Revision != "api:7" {
		t.Fatalf("t-2 = %+v", run)
	}
	if !run.CreatedAt.Equal(t0.Add(-time.Minute)) || run.LaunchType != "FARGATE" || run.Zone != "ap-northeast-2a" ||
		*run.CPU != 0.25 || *run.MemoryMiB != 512 || !slices.Equal(run.Addresses, []model.Address{{IP: "192.0.2.20"}}) {
		t.Fatalf("t-2 detail = %+v", run)
	}
	app, envoy := run.Containers[0], run.Containers[1]
	if app.Name != "app" || app.Digest != "sha256:aaa" || app.Health != "" || app.Status != "running" {
		t.Fatalf("app = %+v", app)
	}
	if len(app.Ports) != 2 || *app.Ports[0].Host != 32768 || app.Ports[0].Protocol != "tcp" || app.Ports[1].Host != nil {
		t.Fatalf("ports = %+v", app.Ports)
	}
	if envoy.Health != model.HealthUnhealthy {
		t.Fatalf("envoy = %+v", envoy)
	}
	if byID["t-1"].Status != model.StatusPending {
		t.Fatalf("t-1 = %+v", byID["t-1"])
	}
	stop := byID["t-stop"]
	if stop.Status != model.StatusStopped || stop.StopReason == "" || *stop.Containers[0].ExitCode != 137 {
		t.Fatalf("t-stop = %+v", stop)
	}

	// Health checks come from a running task's definition, read once per ARN
	// for every task here; a stopped task reports none.
	if app.HealthCheck == nil || !*app.HealthCheck || envoy.HealthCheck == nil || *envoy.HealthCheck {
		t.Fatalf("health checks: app %v, envoy %v", app.HealthCheck, envoy.HealthCheck)
	}
	if stop.Containers[0].HealthCheck != nil {
		t.Fatal("a stopped task's health check must be absent")
	}
	if f.calls["DescribeTaskDefinition"] != 1 {
		t.Fatalf("DescribeTaskDefinition calls = %d", f.calls["DescribeTaskDefinition"])
	}
}

func TestInventoryWithoutTaskDefinitions(t *testing.T) {
	f := &failOn{fakeECS: newFake(), op: "DescribeTaskDefinition"}
	inv, err := NewWithClients("prod", Config{}, f, nil).Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["DescribeTaskDefinition"] != 1 {
		t.Fatalf("an unreadable task definition was asked for %d times", f.calls["DescribeTaskDefinition"])
	}
	for _, in := range inv.Instances {
		for _, c := range in.Containers {
			if c.HealthCheck != nil {
				t.Fatalf("%v %s: health check %v without a task definition", in.Ref, c.Name, *c.HealthCheck)
			}
		}
	}
}

func TestInventoryConfiguredClusters(t *testing.T) {
	f := newFake()
	p := newTestProvider(f, &fakeLogs{}, nil)
	p.cfg.Clusters = []string{"batch"}
	inv, err := p.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if f.calls["ListClusters"] != 0 || len(inv.Services) != 1 {
		t.Fatalf("ListClusters = %d, services = %d", f.calls["ListClusters"], len(inv.Services))
	}

	p.cfg.Clusters = []string{"gone"}
	if _, err := p.Inventory(context.Background()); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestInventoryErrors(t *testing.T) {
	for _, op := range []string{"ListClusters", "ListServices", "DescribeServices", "ListTasks", "DescribeTasks"} {
		t.Run(op, func(t *testing.T) {
			f := &failOn{fakeECS: newFake(), op: op}
			p := NewWithClients("prod", Config{Region: "ap-northeast-2"}, f, nil)
			if _, err := p.Inventory(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

// failOn fails one operation only.
type failOn struct {
	*fakeECS
	op string
}

func (f *failOn) check(op string) error {
	if op == f.op {
		f.begin(context.Background(), op) // count the failed call
		return errors.New("boom")
	}
	return nil
}

func (f *failOn) ListClusters(ctx context.Context, in *ecs.ListClustersInput, o ...func(*ecs.Options)) (*ecs.ListClustersOutput, error) {
	if err := f.check("ListClusters"); err != nil {
		return nil, err
	}
	return f.fakeECS.ListClusters(ctx, in, o...)
}

func (f *failOn) ListServices(ctx context.Context, in *ecs.ListServicesInput, o ...func(*ecs.Options)) (*ecs.ListServicesOutput, error) {
	if err := f.check("ListServices"); err != nil {
		return nil, err
	}
	return f.fakeECS.ListServices(ctx, in, o...)
}

func (f *failOn) DescribeServices(ctx context.Context, in *ecs.DescribeServicesInput, o ...func(*ecs.Options)) (*ecs.DescribeServicesOutput, error) {
	if err := f.check("DescribeServices"); err != nil {
		return nil, err
	}
	return f.fakeECS.DescribeServices(ctx, in, o...)
}

func (f *failOn) ListTasks(ctx context.Context, in *ecs.ListTasksInput, o ...func(*ecs.Options)) (*ecs.ListTasksOutput, error) {
	if err := f.check("ListTasks"); err != nil {
		return nil, err
	}
	return f.fakeECS.ListTasks(ctx, in, o...)
}

func (f *failOn) DescribeTasks(ctx context.Context, in *ecs.DescribeTasksInput, o ...func(*ecs.Options)) (*ecs.DescribeTasksOutput, error) {
	if err := f.check("DescribeTasks"); err != nil {
		return nil, err
	}
	return f.fakeECS.DescribeTasks(ctx, in, o...)
}

func (f *failOn) DescribeTaskDefinition(ctx context.Context, in *ecs.DescribeTaskDefinitionInput, o ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	if err := f.check("DescribeTaskDefinition"); err != nil {
		return nil, err
	}
	return f.fakeECS.DescribeTaskDefinition(ctx, in, o...)
}

func TestService(t *testing.T) {
	f := newFake()
	p := newTestProvider(f, &fakeLogs{}, nil)
	d, err := p.Service(context.Background(), apiRef)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Instances) != 3 || d.Instances[0].Ref.ID != "t-1" {
		t.Fatalf("instances = %+v", d.Instances)
	}
	for _, in := range d.Instances {
		for _, c := range in.Containers {
			if in.Status == model.StatusRunning && c.Name == "app" && (c.HealthCheck == nil || !*c.HealthCheck) {
				t.Fatalf("refresh dropped %v app's health check: %v", in.Ref, c.HealthCheck)
			}
		}
	}

	f.services["web"][1].Status = aws.String("INACTIVE")
	if _, err := p.Service(context.Background(), model.ServiceRef{Target: "prod", Scope: "web", Name: "worker"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("inactive: err = %v", err)
	}
	if _, err := p.Service(context.Background(), model.ServiceRef{Target: "prod", Scope: "gone", Name: "api"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("missing cluster: err = %v", err)
	}
	for _, op := range []string{"ListTasks", "DescribeTasks"} {
		fp := NewWithClients("prod", Config{}, &failOn{fakeECS: newFake(), op: op}, nil)
		if _, err := fp.Service(context.Background(), apiRef); err == nil {
			t.Fatalf("%s failure not returned", op)
		}
	}
}

func TestEvents(t *testing.T) {
	p := newTestProvider(newFake(), &fakeLogs{}, nil)
	evs, err := p.Events(context.Background(), apiRef, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("events = %+v", evs)
	}
	if evs[0].Instance != "t-stop" || evs[0].Severity != model.SeverityWarning || !strings.Contains(evs[0].Message, "Essential container") {
		t.Fatalf("stop event = %+v", evs[0])
	}
	if evs[1].Severity != model.SeverityWarning || evs[2].Severity != model.SeverityInfo {
		t.Fatalf("severities = %s, %s", evs[1].Severity, evs[2].Severity)
	}

	late, err := p.Events(context.Background(), apiRef, t0.Add(150*time.Second))
	if err != nil || len(late) != 1 {
		t.Fatalf("since filter: %+v, %v", late, err)
	}
	if _, err := p.Events(context.Background(), model.ServiceRef{Target: "prod", Scope: "web", Name: "nope"}, time.Time{}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	for _, op := range []string{"ListTasks", "DescribeTasks"} {
		fp := NewWithClients("prod", Config{}, &failOn{fakeECS: newFake(), op: op}, nil)
		if _, err := fp.Events(context.Background(), apiRef, time.Time{}); err == nil {
			t.Fatalf("%s failure not returned", op)
		}
	}
}

func collect(t *testing.T, s provider.LogStream, n int) []string {
	t.Helper()
	var out []string
	for len(out) < n {
		l, err := s.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, l.Text)
	}
	return out
}

func TestLogs(t *testing.T) {
	inst := model.InstanceRef{Service: apiRef, ID: "t-2"}

	t.Run("tail without follow ends after one page", func(t *testing.T) {
		logs := &fakeLogs{}
		f := newFake()
		p := newTestProvider(f, logs, nil)
		s, err := p.Logs(context.Background(), inst, provider.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got := collect(t, s, 10); strings.Join(got, ",") != "one,two" {
			t.Fatalf("lines = %v", got)
		}
		in := logs.inputs[0]
		if aws.ToString(in.LogStreamName) != "ecs/app/t-2" || aws.ToString(in.LogGroupName) != "/ecs/api" ||
			aws.ToBool(in.StartFromHead) || aws.ToInt32(in.Limit) != defaultTail {
			t.Fatalf("input = %+v", in)
		}
		// The task definition is cached by ARN.
		if _, err := p.Logs(context.Background(), inst, provider.LogOptions{}); err != nil {
			t.Fatal(err)
		}
		if f.calls["DescribeTaskDefinition"] != 1 {
			t.Fatalf("DescribeTaskDefinition calls = %d", f.calls["DescribeTaskDefinition"])
		}
	})

	t.Run("follow continues from the forward token", func(t *testing.T) {
		logs := &fakeLogs{}
		ctx, cancel := context.WithCancel(context.Background())
		s, err := newTestProvider(newFake(), logs, nil).Logs(ctx, inst, provider.LogOptions{Follow: true, Since: t0, Tail: 5})
		if err != nil {
			t.Fatal(err)
		}
		if got := collect(t, s, 3); strings.Join(got, ",") != "one,two,three" {
			t.Fatalf("lines = %v", got)
		}
		first := logs.inputs[0]
		if !aws.ToBool(first.StartFromHead) || aws.ToInt64(first.StartTime) != t0.UnixMilli() || aws.ToInt32(first.Limit) != 5 {
			t.Fatalf("first input = %+v", first)
		}
		if second := logs.inputs[1]; aws.ToString(second.NextToken) != "f1" || !aws.ToBool(second.StartFromHead) {
			t.Fatalf("second input = %+v", second)
		}
		cancel()
		if _, err := s.Recv(); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want Canceled", err)
		}
		s.Close()
	})

	t.Run("container in another region", func(t *testing.T) {
		var regions []string
		if _, err := newTestProvider(newFake(), &fakeLogs{}, &regions).Logs(context.Background(), inst, provider.LogOptions{Container: "other"}); err != nil {
			t.Fatal(err)
		}
		if len(regions) != 1 || regions[0] != "us-east-1" {
			t.Fatalf("regions = %v", regions)
		}
	})

	t.Run("fetch error surfaces on Recv", func(t *testing.T) {
		s, err := newTestProvider(newFake(), &fakeLogs{err: errors.New("throttled")}, nil).Logs(context.Background(), inst, provider.LogOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Recv(); err == nil || !strings.Contains(err.Error(), "throttled") {
			t.Fatalf("err = %v", err)
		}
	})

	errCases := []struct {
		name string
		ref  model.InstanceRef
		opts provider.LogOptions
		edit func(*fakeECS)
		want error
	}{
		{name: "unknown task", ref: model.InstanceRef{Service: apiRef, ID: "nope"}, want: provider.ErrNotFound},
		{name: "unknown container", ref: inst, opts: provider.LogOptions{Container: "nope"}, want: provider.ErrNotFound},
		{name: "container without awslogs", ref: inst, opts: provider.LogOptions{Container: "envoy"}, want: provider.ErrUnsupported},
		{name: "awslogs without prefix", ref: inst, opts: provider.LogOptions{Container: "noprefix"}, want: provider.ErrUnsupported},
		{name: "no container logs", ref: inst, edit: func(f *fakeECS) {
			f.taskDefs[acct+":task-definition/api:7"].ContainerDefinitions = []types.ContainerDefinition{{Name: aws.String("app")}}
		}, want: provider.ErrUnsupported},
		{name: "missing task definition", ref: inst, edit: func(f *fakeECS) { f.taskDefs = map[string]*types.TaskDefinition{} }, want: provider.ErrNotFound},
	}
	for _, tt := range errCases {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			if tt.edit != nil {
				tt.edit(f)
			}
			if _, err := newTestProvider(f, &fakeLogs{}, nil).Logs(context.Background(), tt.ref, tt.opts); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}

	for _, op := range []string{"DescribeTasks", "DescribeTaskDefinition"} {
		fp := NewWithClients("prod", Config{}, &failOn{fakeECS: newFake(), op: op}, nil)
		if _, err := fp.Logs(context.Background(), inst, provider.LogOptions{}); err == nil {
			t.Fatalf("%s failure not returned", op)
		}
	}
}

func TestClassify(t *testing.T) {
	p := &Provider{target: "prod"}
	other := errors.New("access denied")
	tests := []struct {
		in   error
		want error
	}{
		{in: context.Canceled, want: context.Canceled},
		{in: fmt.Errorf("op: %w", context.DeadlineExceeded), want: context.DeadlineExceeded},
		{in: &types.ServiceNotFoundException{}, want: provider.ErrNotFound},
		{in: &net.OpError{Op: "dial", Err: errors.New("refused")}, want: provider.ErrUnreachable},
		{in: other, want: other},
	}
	for _, tt := range tests {
		if got := p.classify(tt.in); !errors.Is(got, tt.want) {
			t.Fatalf("classify(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestMappings(t *testing.T) {
	for in, want := range map[string]string{
		"RUNNING": model.StatusRunning, "PROVISIONING": model.StatusPending, "ACTIVATING": model.StatusPending,
		"STOPPING": model.StatusStopped, "STOPPED": model.StatusStopped, "": model.StatusUnknown,
	} {
		if got := statusOf(in); got != want {
			t.Fatalf("statusOf(%q) = %q", in, got)
		}
	}
	if healthOf(types.HealthStatusUnknown) != "" {
		t.Fatal("UNKNOWN health must be absent")
	}
	if severityOf("service x registered 1 targets") != model.SeverityInfo || severityOf("Task failed ELB health checks") != model.SeverityWarning {
		t.Fatal("severity heuristic broken")
	}
	if revisionOf(acct+":task-definition/api:7") != "api:7" || lastSegment("plain") != "plain" {
		t.Fatal("arn helpers broken")
	}
}

func TestNew(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "credentials"))
	t.Setenv("AWS_PROFILE", "")

	target := func(doc string) config.Target {
		var n yaml.Node
		if err := yaml.Unmarshal([]byte(doc), &n); err != nil {
			t.Fatal(err)
		}
		return config.Target{Name: "prod", Provider: Kind, Spec: *n.Content[0]}
	}

	p, err := New(target("region: ap-northeast-2\nreadRoleArn: arn:aws:iam::123456789012:role/fleetbox-read"))
	if err != nil {
		t.Fatal(err)
	}
	ep := p.(*Provider)
	if ep.Kind() != Kind || !ep.Capabilities().Logs || ep.Capabilities().Restart {
		t.Fatalf("provider = %+v", ep.Capabilities())
	}
	if err := ep.Restart(context.Background(), apiRef); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("restart without an operate role: %v", err)
	}
	p, err = New(target("region: ap-northeast-2\noperateRoleArn: arn:aws:iam::123456789012:role/fleetbox-operate"))
	if err != nil {
		t.Fatal(err)
	}
	if c := p.Capabilities(); !c.Restart || !c.Scale {
		t.Fatalf("capabilities with an operate role = %+v", c)
	}
	if a, b := ep.logs("us-east-1"), ep.logs("us-east-1"); a != b {
		t.Fatal("logs clients are not cached per region")
	}
	if _, err := New(target("readRoleArn: x")); err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("err = %v", err)
	}
	if _, err := New(target("region: x\nregoin: y")); err == nil || !strings.Contains(err.Error(), "regoin") {
		t.Fatalf("err = %v", err)
	}
	if _, err := provider.New(target("region: ap-northeast-2")); err != nil {
		t.Fatalf("registry: %v", err)
	}
	if got := ep.Endpoint(); !slices.Equal(got, []model.Attribute{{Name: "region", Value: "ap-northeast-2"}, {Name: "clusters", Value: "all"}}) {
		t.Fatalf("endpoint = %+v", got)
	}
	p, _ = New(target("region: ap-northeast-2\nclusters: [web, batch]"))
	if got := p.(provider.Describer).Endpoint(); got[1].Value != "web, batch" {
		t.Fatalf("endpoint with clusters = %+v", got)
	}
}

// A capacity provider stands in for the launch type, and sizes ECS does not
// report stay absent.
func TestTaskWithoutSizes(t *testing.T) {
	tk := task("web", "t-9", "service:api", "RUNNING", "RUNNING")
	tk.LaunchType, tk.CapacityProviderName = "", aws.String("FARGATE_SPOT")
	tk.Cpu, tk.Memory, tk.CreatedAt = nil, aws.String("lots"), nil
	in := (&Provider{target: "prod"}).mapTask("web", "api", tk)
	if in.LaunchType != "FARGATE_SPOT" || in.CPU != nil || in.MemoryMiB != nil || in.CreatedAt != nil {
		t.Fatalf("instance = %+v", in)
	}
}

func TestOperations(t *testing.T) {
	f := newFake()
	p := newTestProvider(f, &fakeLogs{}, nil)
	ctx := context.Background()
	if err := p.Scale(ctx, apiRef, 5); err != nil {
		t.Fatal(err)
	}
	if err := p.Restart(ctx, apiRef); err != nil {
		t.Fatal(err)
	}
	d, err := p.Service(ctx, apiRef)
	if err != nil {
		t.Fatal(err)
	}
	if *d.Service.Desired != 5 || d.Service.Rollout == nil || !d.Service.Rollout.StartedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("service after scale and restart = %+v", d.Service)
	}
	if f.calls["UpdateService"] != 2 {
		t.Fatalf("calls = %v", f.calls)
	}
	if err := p.Restart(ctx, model.ServiceRef{Target: "prod", Scope: "nope", Name: "api"}); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("unknown cluster: %v", err)
	}
	if err := p.Scale(ctx, apiRef, -1); err == nil {
		t.Fatal("negative desired count accepted")
	}
	inactive := newFake()
	inactive.err = &types.ServiceNotActiveException{Message: aws.String("inactive")}
	if err := newTestProvider(newFake(), &fakeLogs{}, nil).WithOperateClient(inactive).Restart(ctx, apiRef); !errors.Is(err, provider.ErrNotFound) {
		t.Fatalf("inactive service: %v", err)
	}
}

// cancelOn cancels the read while task definitions are being fetched.
type cancelOn struct {
	*fakeECS
	cancel context.CancelFunc
}

func (f *cancelOn) DescribeTaskDefinition(ctx context.Context, _ *ecs.DescribeTaskDefinitionInput, _ ...func(*ecs.Options)) (*ecs.DescribeTaskDefinitionOutput, error) {
	f.cancel()
	return nil, ctx.Err()
}

func TestReadCancelledDuringTaskDefinitions(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := NewWithClients("prod", Config{}, &cancelOn{fakeECS: newFake(), cancel: cancel}, nil)
	if _, err := p.Inventory(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("inventory: err = %v, want context.Canceled", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	p = NewWithClients("prod", Config{}, &cancelOn{fakeECS: newFake(), cancel: cancel}, nil)
	if _, err := p.Service(ctx, apiRef); !errors.Is(err, context.Canceled) {
		t.Fatalf("service: err = %v, want context.Canceled", err)
	}
}
