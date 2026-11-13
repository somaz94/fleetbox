import type { Schemas } from "@/lib/api";

export const t0 = "2026-01-01T00:00:00Z";

// Typed against the spec, so a contract change breaks these at compile time.
export const targets: Schemas["Target"][] = [
  {
    name: "build-host-1",
    provider: "docker",
    readonly: false,
    capabilities: { logs: true, followLogs: true, events: true, restart: true, scale: false },
    refreshedAt: t0,
    attemptedAt: t0,
    stale: true,
    error: "dial tcp 192.0.2.10:22: i/o timeout",
    serviceCount: 1,
    instanceCount: 1,
  },
  {
    name: "prod-aws",
    provider: "ecs",
    env: "prod",
    readonly: true,
    capabilities: { logs: true, followLogs: false, events: true, restart: true, scale: true },
    refreshedAt: t0,
    attemptedAt: t0,
    stale: false,
    serviceCount: 2,
    instanceCount: 3,
  },
];

export const services: Schemas["Service"][] = [
  {
    ref: { target: "prod-aws", scope: "web", name: "api" },
    provider: "ecs",
    status: "degraded",
    desired: 2,
    running: 1,
    pending: 0,
    revision: "api:7",
    rollout: { state: "IN_PROGRESS", startedAt: t0 },
    labels: { "fleetbox.io/app": "api" },
    instances: { running: 1, pending: 0, stopped: 1, unhealthy: 0 },
    stale: false,
  },
  {
    ref: { target: "prod-aws", scope: "web", name: "worker" },
    provider: "ecs",
    status: "healthy",
    desired: 1,
    running: 1,
    instances: { running: 1, pending: 0, stopped: 0, unhealthy: 0 },
    stale: false,
  },
  {
    ref: { target: "build-host-1", scope: "ci", name: "runner" },
    provider: "docker",
    status: "healthy",
    instances: { running: 1, pending: 0, stopped: 0, unhealthy: 0 },
    stale: true,
  },
];

export const detail: Schemas["ServiceDetail"] = {
  service: services[0]!,
  instances: [
    {
      id: "0123456789abcdef0123",
      status: "running",
      health: "healthy",
      revision: "api:7",
      startedAt: t0,
      restarts: 0,
      containers: [
        { name: "app", image: "example/api:1.2", digest: "sha256:0123456789abcdef0123", status: "running", exitCode: 0 },
        { name: "sidecar", image: "example/proxy:1", status: "exited", exitCode: 137 },
      ],
    },
  ],
  stops: [{ instanceId: "fedcba9876543210", stoppedAt: t0, reason: "OutOfMemoryError", exitCode: 137, image: "example/api:1.1" }],
};

export const me: Schemas["Me"] = {
  user: "alice",
  groups: ["platform"],
  mode: "oidc",
  targets: [
    { name: "build-host-1", role: "operator" },
    { name: "prod-aws", role: "operator" },
  ],
};

export const viewer: Schemas["Me"] = { ...me, user: "vic", groups: ["developers"], targets: me.targets.map((t) => ({ ...t, role: "viewer" })) };

// An operable copy of the read-only ECS target.
export const operable: Schemas["Target"][] = targets.map((t) => (t.name === "prod-aws" ? { ...t, readonly: false } : t));

export const audit: Schemas["AuditEntry"][] = [
  {
    id: 3,
    time: t0,
    user: "alice",
    subject: "sub-alice",
    service: { target: "prod-aws", scope: "web", name: "api" },
    action: "scale",
    params: '{"desired":3}',
    result: "ok",
  },
  {
    id: 2,
    time: t0,
    user: "vic",
    service: { target: "prod-aws", scope: "web", name: "api" },
    action: "restart",
    result: "denied",
    error: 'forbidden: restart needs the operator role on target "prod-aws"',
  },
];
