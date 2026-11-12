# CLAUDE.md — fleetbox

Web console for containers outside Kubernetes: Amazon ECS and Docker hosts first, other platforms as providers. Runs as one service in a Kubernetes cluster and serves a React console plus a versioned JSON API. Go backend.

> **Status**: v0.1 in progress. Providers (ECS, Docker over SSH), the poller, the cache, the store and `serve` / `check` exist; the OpenAPI spec, the API and the console do not. `docs/DESIGN.md` is the source of truth for scope and decisions.

<br/>

## Build & Test

```bash
make build   # bin/fleetbox
make test    # go test -race with coverage, two packages at a time
make lint    # gofmt check and go vet
```

`make help` lists every target.

<br/>

## Project Structure

One role per directory:

- `cmd`: entry point; `cmd/cli`: the `serve`, `check` and `version` commands
- `internal/model`: provider-neutral types (service, instance, event, stop record)
- `internal/config`: configuration loading and validation
- `internal/provider`: the provider contract and registry; `providertest` is the conformance suite; one directory per provider
- `internal/inventory`: the in-memory cache and the poller that fills it
- `internal/store`: SQLite persistence for stop history and the audit log

<br/>

## Decisions

The reasoning for each is in `docs/DESIGN.md`. Changing one means changing that document first.

- The server is the only component that calls cloud or host APIs. There is no CLI client in the first releases; a later one is a client of `/api/v1`.
- `api/openapi.yaml` (not written yet) is the API contract. Go server code (`oapi-codegen`) and console types (`openapi-typescript`) are generated from it and never edited by hand.
- A provider registers by kind in `init()`, decodes its own config block with `config.DecodeStrict`, and must pass `providertest`. Adding a provider touches no core, API or console code; `cmd/cli/root.go` only imports it.
- A field a provider cannot supply is absent (nil), never zero.
- The React build is embedded with `go:embed`; the deliverable is one image and one Helm chart.
- Authentication is built-in OIDC with a server-side session (BFF). Trusted-proxy headers are an opt-in mode.
- SQLite through `modernc.org/sqlite` (no cgo), one replica, behind the `store.Store` interface.

<br/>

## Common Pitfalls

- **Docker over SSH**: `golang.org/x/crypto/ssh` running `docker system dial-stdio`, never an `ssh` binary (the distroless image has none). Host keys must be pinned in `known_hosts`; trust on first use is refused.
- **Docker Engine API**: the client in `internal/provider/docker/engine.go` uses unversioned paths so each daemon answers in its own version. Do not pin a version prefix.
- **ECS stop history**: ECS forgets stopped tasks within hours; the poller writes them to the store on every refresh. Do not drop stopped tasks from `Inventory`.
- **Test data**: instance IDs in fixtures must be unique in their short form (Docker shows 12 characters); the conformance suite rejects duplicates.

<br/>

## Conventions

- Documentation and code comments: English only.
- Commits: single-line English Conventional Commits.
- This is a public repository: examples use RFC 5737 addresses (`192.0.2.0/24`), `example.com` and the documentation AWS account ID `123456789012`. Never real hosts, accounts, issuers or user names.
- No test reaches a real cloud or host.
- Credentials are never logged, returned by the API or written to the store.
- Unknown configuration keys are errors.
- Coverage stays at 90% or above, excluding `main`.
- Do not push to remote unless explicitly requested.
