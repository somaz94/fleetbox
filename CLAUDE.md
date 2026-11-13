# CLAUDE.md — fleetbox

Web console for containers outside Kubernetes: Amazon ECS and Docker hosts first, other platforms as providers. Runs as one service in a Kubernetes cluster and serves a React console plus a versioned JSON API. Go backend.

> **Status**: the roadmap is released up to operations: providers (ECS, Docker over SSH), the poller, the cache, the store, the API (`api/openapi.yaml`), OIDC sign-in with role mapping, the console, the Helm chart, `serve` / `check`, audited restart and scale for operators, and findings with Slack notifications. The latest version is the newest `vX.Y.Z` tag. `docs/DESIGN.md` is the source of truth for scope and decisions.

<br/>

## Build & Test

```bash
make build   # bin/fleetbox
make test    # go test -race with coverage, two packages at a time
make lint    # gofmt check and go vet
make cover   # the coverage total the 90% rule applies to
make web     # build the console into web/dist (make build embeds whatever is there)
make web-test # typecheck and test the console
make generate # regenerate api.gen.go and web/src/api/schema.d.ts after editing api/openapi.yaml
make helm-lint # lint and render the chart
make docker-build # local image as fleetbox:dev
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
- `internal/findings`: the evaluator; `rules.go` holds the rules as pure functions over the cache and stop history
- `internal/notify`: Slack delivery of finding transitions, one cursor per route
- `internal/core`: the service layer every transport calls (status derivation, filters, cache plus store, operations)
- `internal/api`: `/api/v1`; `api.gen.go` is generated, `server.go` implements it, `logs.go` is the hand-written SSE endpoint
- `internal/store`: SQLite persistence for stop history, the audit log, login sessions, findings and their transitions
- `internal/auth`: the three auth modes (none, oidc, proxy), group-to-target roles and the CSRF check; `oidctest` is an in-process OIDC issuer for tests
- `internal/console`: serves the embedded build, with the CSP and cache headers
- `web`: the React console (Vite); `embed.go` embeds `web/dist`, `src/api/schema.d.ts` is generated, `src/test` holds the MSW server and fixtures
- `helm/fleetbox`: the chart; `ci/default-values.yaml` is what CI renders and checks

<br/>

## Decisions

The reasoning for each is in `docs/DESIGN.md`. Changing one means changing that document first.

- The server is the only component that calls cloud or host APIs. There is no CLI client in the first releases; a later one is a client of `/api/v1`.
- `api/openapi.yaml` is the API contract. Go server code (`oapi-codegen`, config in `api/oapi-codegen.yaml`) and console types (`openapi-typescript`) are generated from it and never edited by hand. API types are mapped from `internal/model` in `server.go`, so the internal model can change without changing the contract.
- A provider registers by kind in `init()`, decodes its own config block with `config.DecodeStrict`, and must pass `providertest`. Adding a provider touches no core, API or console code; `cmd/cli/root.go` only imports it.
- A field a provider cannot supply is absent (nil), never zero.
- The React build is embedded with `go:embed`; the deliverable is one image and one Helm chart.
- Authentication is built-in OIDC with a server-side session (BFF). Trusted-proxy headers are an opt-in mode.
- SQLite through `modernc.org/sqlite` (no cgo), one replica, behind the `store.Store` interface.

<br/>

## Common Pitfalls

- **Docker over SSH**: `golang.org/x/crypto/ssh` running `docker system dial-stdio`, never an `ssh` binary (the distroless image has none). Host keys must be pinned in `known_hosts`; trust on first use is refused. The remote shell parses the command, so `dockerPath` accepts only an absolute path of `[A-Za-z0-9._/-]`; do not loosen it to allow `sudo` or arguments.
- **Docker Engine API**: the client in `internal/provider/docker/engine.go` uses unversioned paths so each daemon answers in its own version. Do not pin a version prefix.
- **ECS stop history**: ECS forgets stopped tasks within hours; the poller writes them to the store on every refresh. Do not drop stopped tasks from `Inventory`.
- **OpenAPI error responses are inlined per operation**, not shared through `components/responses`: with a shared response the generator gives the first status of each operation a different Go shape, which changes whenever a status is added.
- **A new status code needs the spec first**: the API tests validate every response against the spec, status included, and fail on an undocumented one.
- **Not binding beyond loopback** without `auth.oidc` is enforced in `config.Validate`, and it applies to proxy mode too, which is what keeps the trusted headers sidecar-only. Do not relax it to make a deployment work.
- **Authorisation lives in `internal/api`**: every handler, the hand-written logs endpoint included, checks the caller's role on the target and answers 404 for a hidden target. A new endpoint taking a target must do the same.
- **Operation handlers check in a fixed order**: hidden target (404), role (403), read-only target (403), capability (501), then the `pending` audit row. Refusals on a configured target are audited too; if the `pending` row cannot be written the operation does not run. The provider call and `FinishAudit` are detached from the request's cancellation, and a performed operation never answers as a failure.
- **Finding state lives in the store, not in the rules**: `rules.go` only observes; `reconcile` turns observations into pending, open and resolved findings, and leaves alone any finding on a target that cannot be judged (stale or never refreshed). The restarting rule is the one built on memory, so it is held for `stuckAfter` after start; a new rule that remembers anything between evaluations needs the same, or a restart resolves and reopens what it found.
- **Migrations are append-only once released**: a change to the schema is a new entry in `migrations`, never an edit to a released one.
- **A Slack webhook URL is a credential**: it must not reach an error or a log. `url.Error` quotes the URL, so `internal/notify` unwraps it, and its client follows no redirects (a redirect would send the URL on as `Referer`).
- **The chart and the config schema must agree**: CI renders the chart and runs `fleetbox check` on the result. A new config key needs the chart's `config` block (or `_helpers.tpl`) to produce it.
- **Release automation** (`release.yml`, `helm-release.yml`, `cliff.toml`) follows the sibling repositories and is not changed without the user's approval. `RELEASE.md` is empty in the repository on purpose; git-cliff writes it during a release. Bump `helm/fleetbox/Chart.yaml` `version` and `appVersion` in a `chore: bump version to vX.Y.Z` commit, and push it to `main` before the tag. A Helm Chart Release run that fails on the OCI push has already published gh-pages; rerun it to publish the OCI artefact.
- **Console placeholder**: `web/dist/.gitkeep` keeps `go:embed` compiling before `make web`; Vite empties `dist` on every build and copies `web/public/.gitkeep` back. Without a build the server answers 503 on `/`.
- **`web/node_modules` is excluded by an `ignore` directive in `go.mod`**, and `make lint`/`make fmt` list Go files without it. Keep both when touching either.
- **TypeScript stays on 5.x** while `openapi-typescript` needs its compiler API, which TypeScript 7 does not offer. Regenerate `web/package-lock.json` with npm 11 (`npx npm@11 install`), the version the Node 24 image runs `npm ci` with; npm 10 crashes resolving this tree.
- **`openapi-fetch` must look `fetch` up per call** (`web/src/lib/api.ts`), or MSW in tests never sees the requests.
- **The console CSP forbids inline scripts and styles.** A library that injects a `<style>` or a `style` attribute will be blocked; check the browser console after adding one.
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
