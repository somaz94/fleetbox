# Changelog

All notable changes to this project will be documented in this file.

## [v0.6.0](https://github.com/somaz94/fleetbox/compare/v0.5.0...v0.6.0) (2026-11-13)

### Features

- add an optional Gateway API HTTPRoute to the chart ([416f633](https://github.com/somaz94/fleetbox/commit/416f633307c80ef54823374c95ff9d8504983b52))

### Code Refactoring

- split chart templates by resource ([d8c3cac](https://github.com/somaz94/fleetbox/commit/d8c3cac74edc3a698e8ce35ba4301953ff2f0290))

### Chores

- bump version to v0.6.0 ([c76398c](https://github.com/somaz94/fleetbox/commit/c76398cbf2deabedce04a057a5cf77e132a8e65b))

### Contributors

- somaz

<br/>

## [v0.5.0](https://github.com/somaz94/fleetbox/compare/v0.4.0...v0.5.0) (2026-11-13)

### Features

- add targets page, instance detail and folded stopped services ([bc3aa31](https://github.com/somaz94/fleetbox/commit/bc3aa31645822f13d63b3a34b02ec0606d5d06e3))
- add instance detail, target endpoint and stoppedSince to the API ([fb74874](https://github.com/somaz94/fleetbox/commit/fb74874fbec7d6d91ffbc0f9071e2a4d712f774a))
- report instance detail and target endpoint from providers ([3db3478](https://github.com/somaz94/fleetbox/commit/3db347876d79c09f4afbaeae798c636e37ae2990))
- add dockerPath option for hosts without docker on the SSH PATH ([ce043c3](https://github.com/somaz94/fleetbox/commit/ce043c36d8de8f4919f192b5125a1be8e6e3bad6))

### Documentation

- document Docker access and upgrading from v0.4 ([fb7fc26](https://github.com/somaz94/fleetbox/commit/fb7fc2653c99332c26229f435e1b38a630dbcb68))
- design instance detail, target endpoints and the v0.5 console ([8c628f7](https://github.com/somaz94/fleetbox/commit/8c628f7517e8a7bb11b541d24d717537fb71257d))

### Chores

- bump version to v0.5.0 ([f87d9cd](https://github.com/somaz94/fleetbox/commit/f87d9cd6e5ea6cc0a71c930931203315244b3266))

### Contributors

- somaz

<br/>

## [v0.4.0](https://github.com/somaz94/fleetbox/compare/v0.3.0...v0.4.0) (2026-11-13)

### Features

- send Slack notifications for finding transitions ([bbee708](https://github.com/somaz94/fleetbox/commit/bbee7082053049514c7a20a2c43b04c0e7361f9c))
- show findings in the console with filters and severity badges ([3384417](https://github.com/somaz94/fleetbox/commit/3384417c7d3eb877afc926562f066e1fb5b65188))
- list findings in the API, filtered by the caller's target roles ([1f5b915](https://github.com/somaz94/fleetbox/commit/1f5b91544a11c5874b7bb42662222bc421b99b58))
- evaluate drift, stuck, why and hygiene findings on an interval ([20bcea3](https://github.com/somaz94/fleetbox/commit/20bcea348a913bb7523fc40faeace2aa2b86eebc))
- report image build time and health check presence per container ([190031f](https://github.com/somaz94/fleetbox/commit/190031fc4f5892d87bb1d5f75ae973d3848f80ab))

### Code Refactoring

- tidy the findings path across store, notify and console ([f1d8c4f](https://github.com/somaz94/fleetbox/commit/f1d8c4f0871953653c8ff102d7f0d7c51e5a7f50))

### Documentation

- document findings, Slack notifications and upgrading from v0.3 ([9154bc6](https://github.com/somaz94/fleetbox/commit/9154bc684b78c284842f4c87b9e68c38eb83c274))
- design findings and Slack notifications for v0.4 ([de41702](https://github.com/somaz94/fleetbox/commit/de41702997ad804607732502a2cd9a35cfdebc13))

### Chores

- bump version to v0.4.0 ([0f48653](https://github.com/somaz94/fleetbox/commit/0f4865326d7310d2a8bf5737e17432f69a3a02f0))

### Contributors

- somaz

<br/>

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

