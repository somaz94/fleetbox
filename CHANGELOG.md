# Changelog

All notable changes to this project will be documented in this file.

## [v0.3.0](https://github.com/somaz94/fleetbox/compare/v0.2.0...v0.3.0) (2026-11-13)

### Features

- add operation dialogs and the audit log screen to the console ([7ad855a](https://github.com/somaz94/fleetbox/commit/7ad855a3f595dc6b593709c1dd3b54ad6bfba462))
- restart and scale ECS services and restart Docker services ([62de894](https://github.com/somaz94/fleetbox/commit/62de89496946518fd695e3d2683796f43c67f088))
- add audited restart and scale endpoints with operator and read-only checks ([dfa6587](https://github.com/somaz94/fleetbox/commit/dfa6587b262cb469574e8dba6dd6d7f0d12c9938))

### Code Refactoring

- tidy the operations path across the API, provider and console ([6bed76e](https://github.com/somaz94/fleetbox/commit/6bed76e9406ec0cb03198caed8a359bf421934b8))

### Documentation

- document operations, read-only targets and upgrading from v0.2 ([c1e742d](https://github.com/somaz94/fleetbox/commit/c1e742da52769e8419451f20604ded87331c4e02))
- drop the manual OCI tag check now that a failed push fails the run ([00b4318](https://github.com/somaz94/fleetbox/commit/00b4318406dcdd47825df04bc9859a15879c538e))
- add install section and update status after first release ([7459083](https://github.com/somaz94/fleetbox/commit/745908390e2acae91fa2f73f60548f23cb11be1a))

### Chores

- bump version to v0.3.0 ([41ff367](https://github.com/somaz94/fleetbox/commit/41ff367da5eb50b4ca2351118a24bcc76ac40061))

### Contributors

- somaz

<br/>

## [v0.2.0](https://github.com/somaz94/fleetbox/releases/tag/v0.2.0) (2026-11-12)

### Features

- add read-only React console embedded in the binary ([2f61d78](https://github.com/somaz94/fleetbox/commit/2f61d78acae8af62ad6cce1801d95a8bf6859dbf))
- add OIDC login with server-side sessions and role mapping ([d1b5aab](https://github.com/somaz94/fleetbox/commit/d1b5aab48bb8a902743aeb7610a088df8846f4a8))
- add the read API generated from an OpenAPI spec ([ec6e124](https://github.com/somaz94/fleetbox/commit/ec6e124d79212548b0775b12f9b34df7e06826d8))
- add v0.1 backend foundation for ECS and Docker hosts ([227a0f7](https://github.com/somaz94/fleetbox/commit/227a0f72e0412bcf70649dfa8152f983ba04d416))

### Builds

- **deps:** bump the go-mod-minor group across 1 directory with 2 updates (#2) ([#2](https://github.com/somaz94/fleetbox/pull/2)) ([ca53205](https://github.com/somaz94/fleetbox/commit/ca53205cb743bb20c0383877d5d6d1b4a2fc54c6))
- add release workflows, Dockerfile and Helm chart ([4dc4fd2](https://github.com/somaz94/fleetbox/commit/4dc4fd257ad20df0a209c6dbe0dc644f0e7382d7))

### Chores

- bump version to v0.2.0 ([4076c7f](https://github.com/somaz94/fleetbox/commit/4076c7f17a837b6b232993c2d974b17a3ef04b98))

### Contributors

- somaz

<br/>

