# fleetbox

A web console for containers that run outside Kubernetes: Amazon ECS services and plain Docker hosts in one view, with what runs, which image digest, whether it is healthy and why it last stopped. It runs as one service in a Kubernetes cluster and is the only component holding the cloud and host credentials.

> **Status**: early development, not released. The provider layer (ECS, Docker over SSH), the poller, the cache, the store, the read API, OIDC sign-in with group-to-target roles and a read-only console exist; operations do not yet. The roadmap is in [docs/DESIGN.md](docs/DESIGN.md#roadmap).

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
