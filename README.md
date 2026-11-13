# fleetbox

A web console for containers that run outside Kubernetes: Amazon ECS services and plain Docker hosts in one view, with what runs, which image digest, whether it is healthy and why it last stopped. It runs as one service in a Kubernetes cluster and is the only component holding the cloud and host credentials.

> **Status**: early releases. The provider layer (ECS, Docker over SSH), the read API, OIDC sign-in with group-to-target roles, the console, restart and scale for operators with an audit log, and findings with Slack notifications are released. Releases are listed on the [releases page](https://github.com/somaz94/fleetbox/releases), and the roadmap is in [docs/DESIGN.md](docs/DESIGN.md#roadmap).

<br/>

## Install

The Helm chart is published as a classic repository and as an OCI artefact; both serve the same versions.

```bash
helm repo add fleetbox https://somaz94.github.io/fleetbox/helm-repo
helm install fleetbox fleetbox/fleetbox -f values.yaml
# or
helm install fleetbox oci://ghcr.io/somaz94/charts/fleetbox -f values.yaml
```

The chart refuses to render without `config.auth.oidc` and `config.server.publicURL`; [helm/fleetbox/values.yaml](helm/fleetbox/values.yaml) documents every value. The image is `somaz940/fleetbox` on Docker Hub, tagged per release.

<br/>

## Why

Portainer and its peers manage Docker hosts but not ECS; e1s and the AWS console show ECS but not your Docker hosts; inventory tools such as Steampipe list resources but not what is running inside them. fleetbox joins the two runtimes and adds what neither console gives: image digest drift between environments, deployments stuck short of their desired count, and stop reasons that outlive ECS's own short memory of stopped tasks.

<br/>

## Design

[docs/DESIGN.md](docs/DESIGN.md) covers the architecture, the provider contract every platform implements, authentication, credentials, deployment and the roadmap. New platforms are added as providers that pass a shared conformance suite.

<br/>

## API

[api/openapi.yaml](api/openapi.yaml) is the contract; the server code is generated from it.

<br/>

## Configuration

[examples/config.yaml](examples/config.yaml) shows every target type. Unknown keys are rejected, and `fleetbox check --config <file>` validates a file and builds every provider without contacting any target.

A target is read-only unless its configuration says `readonly: false`. An ECS target also needs `operateRoleArn`, a role allowed `ecs:UpdateService` that trusts the server's base identity rather than the read role, before it can be restarted or scaled; Docker targets can be restarted but not scaled. Only users holding the `operator` role on a target see its operations, and every request is recorded in the audit log.

A Docker target signs in over SSH as a user who can reach the Docker socket, with the host key pinned in `known_hosts`. Where a non-interactive session does not find the `docker` CLI, as on some NAS appliances, `dockerPath` names it.

Findings (image drift across environments, stuck deployments, failed stops and image hygiene) are evaluated on an interval and need no configuration; the optional `findings` block tunes their thresholds. Slack routes under `notify.slack` announce findings as they open and resolve, each reading its incoming webhook URL from a file. A finding is shown, and sent to a route, only when every target it involves is covered.

<br/>

## Upgrading

### From v0.4

- No schema migration and no configuration change. The API only adds fields (a service's `stoppedSince`, a target's `endpoint`, instance details), so existing clients of `/api/v1` keep working.

### From v0.3

- The store gains a schema migration on first start. A v0.3 binary refuses to open the migrated database, so back up the volume before upgrading if you may need to roll back.
- Findings appear on their own after the first evaluations; Slack notifications stay off until routes are configured.

### From v0.2

- Targets now default to read-only. A v0.2 configuration keeps working but allows no operations until a target says `readonly: false`.
- The store gains a schema migration on first start. A v0.2 binary refuses to open the migrated database, so back up the volume before upgrading if you may need to roll back.

<br/>

## Development

```bash
make web     # build the console (Node 22 or later) into web/dist
make build   # bin/fleetbox, with the console embedded
make test    # Go unit tests with the race detector
make web-test # console typecheck and tests
make lint    # gofmt and go vet
make help    # every target
```

For console work, run the server on loopback and `npm run dev` in `web/`: Vite proxies `/api` and `/auth` to `http://127.0.0.1:8080` (override with `FLEETBOX_URL`).

No test reaches a real cloud, host or identity provider: providers run against fakes, the SSH transport against an in-process SSH server, sign-in against an in-process OIDC issuer, and the console against MSW handlers typed by the API spec.

<br/>

## License

[Apache License 2.0](LICENSE)
