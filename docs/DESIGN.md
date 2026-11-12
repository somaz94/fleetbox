# fleetbox Design

fleetbox is a web console for containers that run outside Kubernetes: Amazon ECS services and plain Docker hosts first, with other platforms added as providers. It runs as one service inside a Kubernetes cluster, is the only component that talks to the clouds and hosts, and serves a React console plus a versioned JSON API.

<br/>

## Goals and Non-goals

Goals:

- One view of every ECS service and every Docker host the operator is responsible for: what runs, which image digest, whether it is healthy, why it last stopped.
- Day-two operations from the console (restart, scale) behind roles, read-only targets and an audit log.
- Findings no single console gives: digest drift across environments, deployments stuck short of their desired count, a uniform "why did this stop" answer.
- A provider contract stable enough that a new platform (Cloud Run, Azure Container Apps, Nomad, Kubernetes) is one package that passes a shared conformance suite.

Non-goals for the first releases:

- Building or provisioning anything: no image builds, no task-definition authoring, no compose-file editing. fleetbox operates what is already deployed.
- A CLI. The API is designed so a thin CLI client can come later; until then the server is the only thing holding credentials.
- An interactive shell (ECS Exec, `docker exec`). It needs a WebSocket bridge and widens the attack surface; it is on the roadmap, not in scope.
- High availability. One replica with a local database; the storage interface leaves room for Postgres.

<br/>

## Prior Art

No existing tool covers both managed container services and Docker hosts with runtime detail. That gap shaped the scope:

| Tool | Covers | Why it is not the answer |
|---|---|---|
| Portainer, Komodo, Dokploy | Docker hosts (and Kubernetes, Swarm) | No ECS. Portainer's only managed-service support is Azure ACI, marked experimental with removal proposed |
| e1s, ecs9s | ECS in a terminal | ECS only, single user, no shared console or audit |
| Steampipe, CloudQuery, Komiser | Multi-cloud inventory | Resource lists, not runtime: no logs, stop reasons or operations |
| Backstage ECS plugin | ECS services and tasks on a catalog page | Requires running Backstage; no Docker hosts |

Two ideas are borrowed deliberately: Komodo's outbound agent (a host connects to the console, so no inbound port is opened) for the Docker agent transport, and e1s's focus on ECS deployment and task state for what the ECS detail page shows.

<br/>

## Architecture

```
            ┌──────────────────────── fleetbox (one Go binary, one image) ────────────────────────┐
 Browser ──▶│ HTTP server                                                                         │
 (React)    │  ├─ /auth/*        OIDC login, session cookie (BFF)                                 │
            │  ├─ /api/v1/*      JSON, generated from api/openapi.yaml                            │
            │  ├─ /api/v1/*/logs SSE                                                              │
            │  └─ /*             embedded React build                                             │
            │        │                                                                            │
            │        ▼                                                                            │
            │  core (service layer) ── authz ── audit                                             │
            │   ├─ cache ◀── poller / event watchers                                              │
            │   ├─ analyze (drift, stuck, why)                                                    │
            │   ├─ store (SQLite: audit log, snapshots, stop history)                             │
            │   └─ provider registry                                                              │
            │        ├─ ecs     ──────────────────────────────────────────────▶ AWS APIs          │
            │        └─ docker ── transport ── ssh   ─────────────────────────▶ dockerd           │
            │                               └─ agent (later) ◀── outbound ──── fleetbox-agent    │
            └─────────────────────────────────────────────────────────────────────────────────────┘
```

- The server is the single caller of every cloud and host API. Nothing else holds those credentials.
- API handlers and any later CLI share one core; handlers never call the HTTP API internally.
- Reads are served from the cache. A read never blocks on a cloud call unless the user asks for an on-demand refresh of one service.

<br/>

## Domain Model

Every provider maps its own nouns onto four levels:

| fleetbox | ECS | Docker host |
|---|---|---|
| **Target** (one config entry) | account (assumed role) + region | one host |
| **Scope** | cluster | compose project, or `_standalone` |
| **Service** | ECS service | compose service, or a standalone container by name |
| **Instance** | task (with its containers) | container |

A service is addressed as `<target>/<scope>/<service>`. Scope is part of the key because ECS service names are unique only within a cluster.

Normalised fields carried by every instance: `image`, `digest`, `status`, `health`, `started_at`, `stopped_at`, `stop_reason`, `exit_code`, `cpu`, `memory`, `ports`. Service-level fields: `desired`, `running`, `pending`, `revision` (task definition revision or image digest), `rollout` (state and start time of the current deployment), `labels` (ECS tags or compose labels).

A field a provider cannot supply is absent, never zero: "no data" and "zero" are different answers in a console.

<br/>

## Provider Contract

An adapter instance is bound to one target, so no method takes a target.

```go
type Provider interface {
    Kind() string
    Capabilities() Capabilities
    Inventory(ctx context.Context) (model.Inventory, error)   // every service and instance, sorted by ref
    Service(ctx context.Context, ref model.ServiceRef) (model.ServiceDetail, error) // one service, fresh
    Events(ctx context.Context, ref model.ServiceRef, since time.Time) ([]model.Event, error) // newest first
    Logs(ctx context.Context, ref model.InstanceRef, opts LogOptions) (LogStream, error)
}

// Optional. The API answers 501 for a provider without it.
type Operator interface {
    Restart(ctx context.Context, ref model.ServiceRef) error
    Scale(ctx context.Context, ref model.ServiceRef, desired int) error
}

// Optional. Each value means "something changed, refresh"; the channel closes when the watch ends.
type Watcher interface {
    Watch(ctx context.Context) (<-chan struct{}, error)
}
```

`Inventory` returns a whole target in one call rather than services and then instances per service, because the efficient read is per target: ECS lists a cluster's tasks once and groups them, Docker lists every container once.

- Providers register by kind in `init()` and are constructed from their target config. The provider's own settings sit under a key named after its kind and are decoded strictly by the provider, so adding one does not touch configuration loading, core, the API or the console.
- `Capabilities` drives the console: a button for an operation the provider lacks is not rendered, and the API refuses it regardless.
- Every provider passes `providertest`, a shared suite run against the provider's fake SDK client. It checks the contract the rest of the system relies on: stable service keys, absent-not-zero fields, sorted output, context cancellation, error wrapping that keeps "target unreachable" distinct from "not found".
- Provider code reaches its SDK through a small interface holding only the calls it makes, so unit tests run offline.

<br/>

## Providers

### ECS

| Need | API |
|---|---|
| Services | `ListClusters`, `ListServices`, `DescribeServices` (batched by 10) |
| Tasks | `ListTasks` once per cluster for running and once for stopped tasks, `DescribeTasks` (batched by 100), grouped by the task's `service:<name>` group; `DescribeTaskDefinition` cached by ARN |
| Rollout | `deployments[].rolloutState` on the service |
| Events | service `events[]` and stopped tasks' `stoppedReason` |
| Logs | `awslogs` settings from the task definition, then CloudWatch Logs `GetLogEvents`: the last page for history, then polling the forward token to follow. `StartLiveTail` would push instead of poll and is a later optimisation |
| Restart | `UpdateService` with `forceNewDeployment` |
| Scale | `UpdateService` with `desiredCount` |

Tasks started outside a service (`RunTask`) are not listed. A container whose awslogs configuration has no `awslogs-stream-prefix` has no readable stream name through the ECS API, so its logs are reported as unsupported.

ECS keeps stopped tasks visible only for a short time. The poller records every stopped task's reason into the store, so "why did this stop" still answers after ECS has forgotten.

A target covers every cluster in its region unless `clusters` names a subset. Each target assumes a role. Read and operate are separate roles: a target configured without `operateRoleArn` cannot be operated, whatever the user's role.

### Docker

| Need | API |
|---|---|
| Services | container list grouped by `com.docker.compose.project` / `com.docker.compose.service` labels |
| Digest | image inspect `RepoDigests` |
| Events | the `events` stream, which also keeps the cache current without polling |
| Logs | container logs stream, demultiplexed |
| Restart | container restart for every container of the service |
| Scale | not supported: the compose file lives on the host |

- The client is a minimal Engine API client covering only the calls above, not the Docker SDK, which would pull a large dependency tree for six requests. Paths are unversioned so each daemon answers in its own current API version; daemons have raised their minimum supported version before.
- A service's revision is the digest its running containers share; it is absent when they run different digests or a locally built image with no repository digest.
- Only `fleetbox.io/*` container labels surface as service labels; compose adds a dozen labels per container that say nothing about the app.

Transport is an interface inside the provider:

- **ssh** (first release): connects with `golang.org/x/crypto/ssh` and runs `docker system dial-stdio` on the host. It does not shell out to an `ssh` binary, which the distroless image does not have. A host key must be pinned through `known_hosts`; a target without one is refused rather than trusted on first use.
- **agent** (later): a small `fleetbox-agent` on the host dials out to the console and exposes only the operations fleetbox uses. No inbound port, and the console no longer holds a credential equal to root on the host.

Access to the Docker API is root on that host, which is why targets default to read-only and why the agent transport exists.

<br/>

## Data Flow

- **Poller**: one loop per target, interval configurable per target, each refresh bounded by that interval. ECS targets poll; Docker targets also subscribe to events, refreshing once per burst of changes, and the interval remains as a safety net.
- **Cache**: an in-memory snapshot per target, replaced atomically. A target whose last refresh failed keeps its previous snapshot, marked stale with the error and the time of the last success.
- **Store** (SQLite through `modernc.org/sqlite`, no cgo): audit log, stopped-instance history, and periodic snapshots of service revisions for drift history. Behind a `Store` interface.
- **Partial failure**: one unreachable target never fails a list. It appears as a stale or errored target and every other target still renders.

<br/>

## API

`api/openapi.yaml` is the source of truth. The Go server interface is generated with `oapi-codegen`, and the console's TypeScript types with `openapi-typescript`. Generated code is never edited by hand, and CI fails when it no longer matches the spec.

`…` stands for `/api/v1/services/{target}/{scope}/{service}`.

| Method and path | Purpose | Release |
|---|---|---|
| `GET /api/v1/info` | build version | v0.1 |
| `GET /api/v1/targets` | targets with capabilities, staleness, last error and counts | v0.1 |
| `GET /api/v1/services` | filtered list across targets (`target`, `provider`, `status`, `q`) with a derived status and instance counts | v0.1 |
| `GET …` | detail from the cache: instances, rollout, revision, recorded stop history | v0.1 |
| `POST …/refresh` | read one service fresh and update the cache | v0.1 |
| `GET …/events` | the provider's event timeline, newest first | v0.1 |
| `GET …/instances/{instance}/logs` | SSE: `line`, `error` and `end` events; `container`, `tail`, `since`, `follow` | v0.1 |
| `GET /api/v1/me` | user, groups, effective role per target | v0.2 |
| `POST …/restart`, `POST …/scale` | operations, audited | v0.3 |
| `GET /api/v1/audit` | audit log | v0.3 |
| `GET /api/v1/findings` | drift, stuck, stop-reason and hygiene findings | v0.4 |

- A service's status is derived in core, the same for every transport: `idle` when nothing is desired, `down` when nothing runs, `degraded` when fewer run than desired or a running instance is unhealthy, otherwise `healthy`. A provider without a desired count (Docker) is judged on its instances alone.
- Every error, including an unknown route or a malformed parameter, answers with the `Error` schema and a code (`bad_request`, `not_found`, `unsupported`, `unreachable`, `timeout`, `internal`). An internal error's text is logged, not returned.
- Tests validate every JSON response against the spec, status code included, so the Go output and the generated TypeScript types cannot drift apart.
- The logs endpoint is written by hand rather than generated, because the generated strict server copies a body without flushing. Once the stream has started, a failure can only arrive as an `error` event.
- Until authentication lands (v0.2), the server refuses to bind beyond loopback, and refuses `auth.oidc` rather than accept it and serve unauthenticated.

<br/>

## Console

- Vite, React, TypeScript, TanStack Query, React Router, Tailwind with shadcn/ui. The build is embedded into the Go binary with `go:embed`, so the image stays one artefact.
- Lists and details refetch from the server cache on an interval. SSE is used for logs only.
- Screens: dashboard (targets, stale or failing targets, open findings), services (filterable list), service detail (instances, revision, rollout, events, recent stop reasons), logs, findings, audit log.
- Operations open a confirmation that requires typing the service name. The console hides what the user may not do; the server enforces it regardless.
- Log lines are untrusted text and are rendered as text, never as HTML.

<br/>

## Authentication and Authorisation

- **Built-in OIDC, backend-for-frontend**: the server runs the authorisation-code flow with PKCE and issues an httpOnly, `SameSite=Lax` session cookie. No token reaches the browser. Any OIDC issuer works; Keycloak is the reference.
- **Trusted-proxy mode** (optional): accept user and groups from headers set by oauth2-proxy or Pomerium. It is off by default because it is only safe when nothing can reach the pod except the proxy.
- **Roles**: `viewer` and `operator`, granted by mapping OIDC groups to target name globs. The most permissive matching rule wins.
- **Read-only targets**: `readonly: true` blocks every operation on the target whatever the role. A target with no operate role (ECS) or no operate permission (Docker) is read-only in effect.
- **CSRF**: state-changing requests must be `POST` with a custom header and a matching `Origin`.
- **Audit**: every operation, allowed or refused, is recorded with user, target, service, action, parameters and result.
- No unauthenticated mode when bound to anything but loopback.

<br/>

## Credentials

The server uses each SDK's default credential chain, so the method is a deployment choice, not a code path.

| AWS method (in-cluster) | Notes |
|---|---|
| Self-hosted IRSA: the cluster's service-account issuer registered as an IAM OIDC provider, then `AssumeRoleWithWebIdentity` | No static keys. The issuer's discovery document and JWKS must be publicly reachable |
| IAM Roles Anywhere | No static keys. A self-managed CA as trust anchor avoids Private CA cost; the image must carry `aws_signing_helper` |
| Static access key in a Secret | Simplest; rotation and leakage are the operator's problem |

From that base identity each target assumes its read role, and its operate role only for an operation.

Docker SSH keys and `known_hosts` are mounted from a Secret. Credentials are never logged, never returned by the API and never written to the store.

<br/>

## Configuration

```yaml
server:
  listen: ":8080"
  publicURL: https://fleetbox.example.com
auth:
  oidc:
    issuer: https://sso.example.com/realms/example
    clientID: fleetbox
    clientSecretFile: /etc/fleetbox/oidc-secret
  roles:
    - group: platform
      targets: ["*"]
      role: operator
    - group: developers
      targets: ["dev-*"]
      role: operator
    - group: developers
      targets: ["*"]
      role: viewer
targets:
  - name: prod-aws
    provider: ecs
    env: prod
    readonly: true
    ecs:
      region: ap-northeast-2
      readRoleArn: arn:aws:iam::123456789012:role/fleetbox-read
  - name: dev-aws
    provider: ecs
    env: dev
    ecs:
      region: ap-northeast-2
      readRoleArn: arn:aws:iam::210987654321:role/fleetbox-read
      operateRoleArn: arn:aws:iam::210987654321:role/fleetbox-operate
  - name: build-host-1
    provider: docker
    docker:
      transport: ssh
      host: 192.0.2.10
      user: fleetbox
      keyFile: /etc/fleetbox/ssh/id_ed25519
      knownHostsFile: /etc/fleetbox/ssh/known_hosts
```

Unknown keys are an error, not a warning: a misspelled `readonly` must not silently leave a production target writable.

`fleetbox check` loads the file and builds every provider without contacting any target, so a configuration can be validated in CI or before a rollout.

<br/>

## Findings

| Finding | Rule |
|---|---|
| **drift** | Services that are the same app (same name, or the same `fleetbox.io/app` tag or label) in targets of different `env` within one provider, running different digests. Matching never crosses providers |
| **stuck** | ECS: a rollout `IN_PROGRESS`, or running below desired, for longer than a threshold. Docker: restart count rising, or unhealthy, for longer than a threshold |
| **why** | Last stop reasons per service from the store: ECS `stoppedReason` and container exit codes, Docker exit code, `OOMKilled` and the last health-check output |
| **hygiene** | `:latest` or untagged images, images older than a threshold, services with no health check |

Thresholds are configuration, and each finding carries the evidence it was raised on.

<br/>

## Deployment

- One container image published to GHCR: distroless, non-root, the React build embedded.
- A Helm chart in this repository, published as an OCI artefact: Deployment with one replica and the `Recreate` strategy (SQLite on a PVC), Service, optional Ingress, ServiceAccount (for self-hosted IRSA), Secrets for OIDC, SSH and AWS material, and an optional NetworkPolicy.
- Health endpoints: `/healthz` for liveness, `/readyz` once configuration loaded and the store opened. Readiness does not depend on any target being reachable.
- Prometheus metrics: refresh duration and errors per target, cache age, operations by result.

<br/>

## Testing

- Go: table-driven unit tests; providers against fake SDK clients through `providertest`; HTTP handlers through `httptest` with a fake provider; coverage 90% or above, excluding `main`.
- Console: Vitest and Testing Library, with MSW mocks generated from the OpenAPI spec.
- End-to-end: the server with fake providers, driven by Playwright. The Docker provider also runs against a real local daemon over SSH in CI.
- No test calls a real cloud API.

<br/>

## Roadmap

| Release | Scope |
|---|---|
| v0.1 | OpenAPI spec, core, registry and `providertest`, ECS and Docker (ssh) read paths, poller, cache, store |
| v0.2 | Read-only console, OIDC login, Helm chart, image |
| v0.3 | Role mapping, restart and scale, audit log |
| v0.4 | Findings (drift, stuck, why, hygiene), Slack notifier |
| Later | Docker agent transport, CLI as an API client, a third provider, interactive shell |

<br/>

## Open Questions

- Which AWS credential method the reference deployment uses. It depends on whether the cluster's service-account issuer can be published.
- Whether Docker targets should be discoverable from an inventory source (for example an Ansible inventory) instead of listed one by one.
