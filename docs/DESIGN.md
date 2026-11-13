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
            │  ├─ /api/v1/*      JSON, generated from api/openapi.yaml; authorisation, audit      │
            │  ├─ /api/v1/*/logs SSE                                                              │
            │  └─ /*             embedded React build                                             │
            │        │                                                                            │
            │        ▼                                                                            │
            │  core (service layer)                                                               │
            │   ├─ cache ◀── poller / event watchers                                              │
            │   ├─ findings (drift, stuck, why, hygiene) ── notify ──────────────────▶ Slack      │
            │   ├─ store (SQLite: audit log, stop history, findings, sessions)                    │
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

A target covers every cluster in its region unless `clusters` names a subset. Each target assumes a role. Read and operate are separate roles: a target configured without `operateRoleArn` cannot be operated, whatever the user's role. The operate role needs `ecs:UpdateService`; both roles are assumed from the server's own identity, never one from the other.

### Docker

| Need | API |
|---|---|
| Services | container list grouped by `com.docker.compose.project` / `com.docker.compose.service` labels |
| Digest | image inspect `RepoDigests` |
| Events | the `events` stream, which also keeps the cache current without polling |
| Logs | container logs stream, demultiplexed |
| Restart | container restart for each container of the service in turn, stopping at the first failure; a container removed since the listing is skipped, and a service with none left is not found; one-off containers from `docker compose run` are left alone, as `docker compose restart` does |
| Scale | not supported: the compose file lives on the host |

- The client is a minimal Engine API client covering only the calls above, not the Docker SDK, which would pull a large dependency tree for six requests. Paths are unversioned so each daemon answers in its own current API version; daemons have raised their minimum supported version before.
- A service's revision is the digest its running containers share; it is absent when they run different digests or a locally built image with no repository digest.
- Only `fleetbox.io/*` container labels surface as service labels; compose adds a dozen labels per container that say nothing about the app.

Transport is an interface inside the provider:

- **ssh** (first release): connects with `golang.org/x/crypto/ssh` and runs `docker system dial-stdio` on the host. It does not shell out to an `ssh` binary, which the distroless image does not have. A host key must be pinned through `known_hosts`; a target without one is refused rather than trusted on first use. `dockerPath` names the CLI when it is not on the PATH of a non-interactive session, as on NAS appliances that install it under `/usr/local/bin`; it must be an absolute path of letters, digits and `._/-`, because the remote shell parses it.
- **agent** (later): a small `fleetbox-agent` on the host dials out to the console and exposes only the operations fleetbox uses. No inbound port, and the console no longer holds a credential equal to root on the host.

Access to the Docker API is root on that host, which is why targets default to read-only and why the agent transport exists.

<br/>

## Data Flow

- **Poller**: one loop per target, interval configurable per target, each refresh bounded by that interval. ECS targets poll; Docker targets also subscribe to events, refreshing once per burst of changes, and the interval remains as a safety net.
- **Cache**: an in-memory snapshot per target, replaced atomically. A target whose last refresh failed keeps its previous snapshot, marked stale with the error and the time of the last success.
- **Store** (SQLite through `modernc.org/sqlite`, no cgo): audit log, stopped-instance history, login sessions, and findings with their open and resolve transitions. Behind a `Store` interface.
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
| `GET /api/v1/findings` | open or resolved findings the caller may see; `kind`, `target`, `severity`, `state` | v0.4 |

- A service's status is derived in core, the same for every transport: `idle` when nothing is desired, `down` when nothing runs, `degraded` when fewer run than desired or a running instance is unhealthy, otherwise `healthy`. A provider without a desired count (Docker) is judged on its instances alone.
- Every error, including an unknown route or a malformed parameter, answers with the `Error` schema and a code (`bad_request`, `unauthorized`, `forbidden`, `not_found`, `unsupported`, `unreachable`, `timeout`, `internal`). An internal error's text is logged, not returned.
- Tests validate every JSON response against the spec, status code included, so the Go output and the generated TypeScript types cannot drift apart.
- The logs endpoint is written by hand rather than generated, because the generated strict server copies a body without flushing. Once the stream has started, a failure can only arrive as an `error` event.
- Every `/api/*` request needs a caller: without one it answers 401, and the console sends the browser to `/auth/login`. A target the caller holds no role on is left out of every list and answers 404 exactly like an unknown target, so names cannot be probed.

<br/>

## Console

- Vite, React, TypeScript, TanStack Query, React Router, Tailwind with shadcn/ui. The build is embedded into the Go binary with `go:embed`, so the image stays one artefact. API types come from `api/openapi.yaml` through `openapi-typescript`, and requests go through `openapi-fetch`, so a contract change fails the console's typecheck.
- The server answers every path outside `/api`, `/auth` and the probes with the build: a file when one exists, `index.html` otherwise, so client-side routes survive a reload. Hashed assets are cached for a year, `index.html` never. A Content-Security-Policy allows only the build's own scripts, styles and connections, with no inline code and no framing.
- A 401 from the API sends the browser to `/auth/login` with the current path as `next`. Every request other than a `GET` carries the CSRF header. Sign-out is offered only when `/api/v1/me` reports `oidc` mode.
- Lists and details refetch from the server cache on an interval; the event timeline, which the provider answers fresh, loads once and on request. SSE is used for logs only. A failed log stream stops rather than letting `EventSource` reconnect, which would replay the tail, and offers a reconnect.
- Screens: dashboard (targets, stale or failing targets, open findings), services (filterable list), service detail (instances, revision, rollout, events, recent stop reasons), logs, findings, audit log.
- Operations open a confirmation that requires typing the service name. The console hides what the user may not do; the server enforces it regardless.
- Log lines are untrusted text and are rendered as text, never as HTML.

<br/>

## Authentication and Authorisation

- **Built-in OIDC, backend-for-frontend**: the server runs the authorisation-code flow with PKCE (`/auth/login`, `/auth/callback`) and issues an httpOnly, `SameSite=Lax`, `Secure` session cookie with the `__Host-` prefix, so a sibling subdomain cannot plant one. No token reaches the browser, and none is kept: the ID token is verified (signature, issuer, audience, expiry, nonce), its user and groups are copied into the session, and it is discarded. Any OIDC issuer works; Keycloak is the reference. The redirect URL is `<server.publicURL>/auth/callback`, so `publicURL` is required with OIDC, and it must be https unless its host is loopback (local development, where the cookie drops `Secure` and the prefix).
- **Sessions** live in the store, so a restart or a new release does not sign anyone out. Only a SHA-256 hash of the cookie value is stored. A session lasts `auth.oidc.sessionTTL` from sign-in and is not refreshed; a change to a user's groups takes effect at their next sign-in. Expired sessions are deleted on each sign-in.
- **Issuer outages**: discovery runs on the first login, not at start-up, and a failure is retried on the next one. Signed-in users keep working while the issuer is down, since a request only reads the session.
- **Sign-out**: `POST /auth/logout` deletes the session and answers with where the browser should go next: the issuer's `end_session_endpoint` when it publishes one, so the issuer's session ends too, otherwise the console root.
- **Login state**: each login's state, PKCE verifier, nonce and post-login path travel in a ten-minute cookie sealed with AES-GCM under a key made at start-up. The cookie binds the callback to the browser that started it, so a link carrying someone else's state is refused, and starting logins costs the server no memory, so nobody can exhaust it without signing in. A restart abandons logins in progress; they are simply started again. The post-login path must be a local path without backslashes.
- **Trusted-proxy mode** (optional, `auth.proxy`): accept the user and groups from headers set by oauth2-proxy or Pomerium. It is off by default because it is only safe when nothing but the proxy can reach the server, and it keeps the loopback rule below, so the proxy runs as a sidecar in the same pod. `server.publicURL` is required so the CSRF check knows the browser's origin, which a TLS-terminating sidecar hides from the request.
- **Roles**: `viewer` and `operator`, granted by mapping groups to target name globs. The most permissive matching rule wins, and a target no rule grants is invisible to the user. Reading a service fresh (`POST …/refresh`) is a read, so viewers may do it. The groups are the ID token claim named by `auth.oidc.groupsClaim`, taken as written (Keycloak's group mapper prefixes `/` when "full group path" is on).
- **Read-only targets**: a target is read-only unless its configuration says `readonly: false`, and a read-only target refuses every operation whatever the role. A target with no operate role (ECS) cannot be operated either.
- **Operation checks**, in this order: a target the caller holds no role on answers 404 like an unknown one; a viewer gets 403; a read-only target gets 403; an operation the provider lacks gets 501. Every check happens on the server, whatever the console shows.
- **CSRF**: every request other than `GET`, `HEAD` and `OPTIONS` must carry the `X-Fleetbox-CSRF` header and an `Origin` equal to `server.publicURL` (or, without one, to the request's own host). A browser adds a custom header cross-origin only after a CORS preflight, which fleetbox never grants.
- **Audit**: every operation request on a configured target, allowed or refused, is recorded with user, OIDC subject, target, service, action, parameters and result. An allowed operation is written as `pending` before the provider is called and updated to `ok` or `error` afterwards, so one that cannot be recorded is not performed, and one interrupted by a restart stays visible as `pending`. The log keeps error codes and the messages a caller would see, never internal error text. A caller reads the entries on the targets they hold a role on (`GET /api/v1/audit`, newest first, paged by id).
- No unauthenticated mode when bound to anything but loopback. Without `auth.oidc` or `auth.proxy`, the loopback-only server treats every caller as an operator, and it refuses any request whose `Host` is not a loopback name, so a page that reaches it by DNS rebinding gets nothing.

<br/>

## Credentials

The server uses each SDK's default credential chain, so the method is a deployment choice, not a code path.

| AWS method (in-cluster) | Notes |
|---|---|
| Self-hosted IRSA: the cluster's service-account issuer registered as an IAM OIDC provider, then `AssumeRoleWithWebIdentity` | No static keys. The issuer's discovery document and JWKS must be publicly reachable |
| IAM Roles Anywhere | No static keys. A self-managed CA as trust anchor avoids Private CA cost; the image must carry `aws_signing_helper` |
| Static access key in a Secret | Simplest; rotation and leakage are the operator's problem |

From that base identity each target assumes its read role, and its operate role only for an operation.

Docker SSH keys and `known_hosts`, and Slack webhook URLs, are mounted from Secrets. Credentials are never logged, never returned by the API and never written to the store.

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
    clientSecretFile: /etc/fleetbox/oidc-secret   # omit for a public client
    scopes: [profile, email]                      # besides openid
    groupsClaim: groups
    sessionTTL: 12h
  # or, instead of oidc, behind an authenticating sidecar:
  # proxy:
  #   userHeader: X-Forwarded-User
  #   groupsHeader: X-Forwarded-Groups
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
    ecs:
      region: ap-northeast-2
      readRoleArn: arn:aws:iam::123456789012:role/fleetbox-read
  - name: dev-aws
    provider: ecs
    env: dev
    readonly: false
    ecs:
      region: ap-northeast-2
      readRoleArn: arn:aws:iam::123456789012:role/fleetbox-read
      operateRoleArn: arn:aws:iam::123456789012:role/fleetbox-operate
  - name: build-host-1
    provider: docker
    docker:
      transport: ssh
      host: 192.0.2.10
      user: fleetbox
      keyFile: /etc/fleetbox/ssh/id_ed25519
      knownHostsFile: /etc/fleetbox/ssh/known_hosts
findings:                     # every key optional; these are the defaults
  interval: 1m
  driftAfter: 1h
  stuckAfter: 15m
  failedStopWindow: 24h
  imageMaxAge: 2160h
  disabled: []                # kinds to switch off, e.g. [hygiene]
notify:
  slack:
    - name: platform          # unique; keys the route's delivery cursor
      webhookURLFile: /etc/fleetbox/slack/platform
      targets: ["*"]
      kinds: [drift, stuck, why]   # omit for every kind
```

A target is read-only unless it says `readonly: false`, so operating is an opt-in per target. Unknown keys are an error, not a warning: a misspelled `readonly` must not silently change what a target allows.

`fleetbox check` loads the file and builds every provider without contacting any target, so a configuration can be validated in CI or before a rollout.

<br/>

## Findings

A finding is a condition fleetbox noticed and keeps track of. An evaluator in `internal/findings` runs on its own interval over the cache and the store's stop history; it never calls a provider.

| Finding | Rule | Raised after |
|---|---|---|
| **drift** | Services that are the same app (the `fleetbox.io/app` tag or label, otherwise the same service name) in targets of different `env` within one provider, running different image digests. Matching never crosses providers, and a target without `env` takes no part | `driftAfter` |
| **stuck** | ECS: a rollout `IN_PROGRESS` since it started, or fewer running than desired. Docker: a restart count that keeps rising. Any provider: a running instance that is unhealthy | `stuckAfter` |
| **why** | A stop within `failedStopWindow` that exited with a code other than 0 or a stop signal's (130, 137, 143), or was killed for memory (Docker's `OOMKilled`, ECS's `OutOfMemoryError` on the container), read from the store, so it outlives ECS's own short memory of stopped tasks. A deployment or a `docker compose restart` stops containers with a signal, and is not a failure | at once |
| **hygiene** | A running image tagged `latest`, or with neither tag nor digest; a Docker image older than `imageMaxAge`; a service none of whose containers has a health check | at once |

- Each finding has a kind, a rule within it, a severity (`warning`, or `info` for hygiene), a summary and the evidence it was raised on: the digests per environment, how long a rollout has run, the failed stops with their exit codes, the image.
- A condition first seen is pending; once it has lasted its rule's delay it opens, and when it is gone it resolves. One that comes back later opens as a new finding. Only open and resolved findings are shown or notified, so a passing condition never surfaces.
- Findings and every open and resolve transition are kept in the store, so a restart neither forgets how long a condition has lasted nor repeats a notification. Resolved findings are kept as long as stop history.
- A target whose last refresh failed is not judged: a finding involving it stays as it was until the target answers again, so an unreachable host does not resolve and reopen everything on it.
- A field a provider cannot supply decides nothing. An ECS image's age would need the registry, so the image-age rule skips ECS; a container whose health check is unknown is not reported as lacking one.
- A finding is shown to a caller only if they hold a role on every target it involves, so drift between production and development does not reveal production digests to someone who sees development only.
- Thresholds are configuration. Switching a kind off closes its open findings without a notification; the console lists them as resolved.

<br/>

## Notifications

- Slack incoming webhooks, configured as routes. Each route names the targets (globs) and kinds it wants and the file holding its webhook URL. A transition goes to a route when every target the finding involved at that transition matches, so a finding whose services change while it is open can reach a route on only one of its two transitions.
- A message is sent when a finding opens and when it resolves, and for nothing else. Operations are recorded in the audit log, not announced.
- Delivery reads the store's transitions in order, and each route keeps its own cursor there, moved only after Slack accepts the message. A failed delivery is retried with backoff and never holds up the evaluator; a restart resumes where delivery stopped. A new route starts at the latest transition, so enabling one does not replay history.
- Delivery is at least once: a crash or a timeout after Slack accepted a message, before the cursor moved, sends that message again. Transitions are kept as long as stop history, so a route failing for longer loses the oldest ones.
- The client follows no redirects: one would hand the URL on as a `Referer` and turn the post into a bodiless `GET`. A webhook URL must be https unless its host is loopback.
- A webhook URL is a credential: it is read from a file and never logged, returned by the API or written to the store, error messages included.

<br/>

## Deployment

- One container image published to Docker Hub as `somaz940/fleetbox`, the same registry as the sibling projects: distroless, non-root, multi-architecture, the React build embedded.
- A Helm chart in `helm/fleetbox`, published as an OCI artefact on GHCR and as a classic repository on the `gh-pages` branch: Deployment with one replica and the `Recreate` strategy (SQLite on a PVC), Service, optional Ingress, a ServiceAccount whose token is mounted only as the audience-bound AWS web-identity token, and Secrets for OIDC, SSH, Slack webhooks and static AWS material referenced by name. The chart supports OIDC only: trusted-proxy mode needs the proxy as a sidecar, which a deployment builds by hand. It refuses to render without `config.auth.oidc` and `config.server.publicURL`, mirroring the server's refusal to serve beyond loopback unauthenticated, and it sets `store.path` itself.
- A release is a `vX.Y.Z` tag: the image, the GitHub release (notes from git-cliff) and the chart publish from it, and the major tag (`v1`) follows automatically. The chart's own `version` is bumped by hand in the release commit; its `appVersion` follows the tag.
- Health endpoints: `/healthz` for liveness, `/readyz` once configuration loaded and the store opened. Readiness does not depend on any target being reachable.
- Prometheus metrics: refresh duration and errors per target, cache age, operations by result.

<br/>

## Testing

- Go: table-driven unit tests; providers against fake SDK clients through `providertest`; HTTP handlers through `httptest` with a fake provider; coverage 90% or above, excluding `main`.
- Console: Vitest and Testing Library against MSW handlers typed by the OpenAPI spec through `openapi-msw`, so a fixture that drifts from the contract fails to compile.
- End-to-end: the server with fake providers, driven by Playwright. The Docker provider also runs against a real local daemon over SSH in CI.
- No test calls a real cloud API.

<br/>

## Roadmap

| Release | Scope |
|---|---|
| v0.1 | OpenAPI spec, core, registry and `providertest`, ECS and Docker (ssh) read paths, poller, cache, store |
| v0.2 | Read-only console, OIDC login with role mapping on reads, Helm chart, image |
| v0.3 | Restart and scale for operators, audit log |
| v0.4 | Findings (drift, stuck, why, hygiene), Slack notifier |
| Later | Docker agent transport, CLI as an API client, a third provider, interactive shell, revision history for drift, ECS image age |

<br/>

## Open Questions

- Which AWS credential method the reference deployment uses. It depends on whether the cluster's service-account issuer can be published.
- Whether Docker targets should be discoverable from an inventory source (for example an Ansible inventory) instead of listed one by one.
