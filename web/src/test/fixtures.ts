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
    endpoint: [
      { name: "transport", value: "ssh" },
      { name: "host", value: "192.0.2.10:22" },
    ],
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
    endpoint: [
      { name: "region", value: "ap-northeast-2" },
      { name: "clusters", value: "all" },
    ],
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
      createdAt: t0,
      cpu: 0.25,
      memoryMiB: 512,
      launchType: "FARGATE",
      zone: "ap-northeast-2a",
      addresses: [{ ip: "192.0.2.20" }],
      containers: [
        {
          name: "app",
          image: "example/api:1.2",
          digest: "sha256:0123456789abcdef0123",
          status: "running",
          exitCode: 0,
          ports: [{ container: 8080, host: 80, protocol: "tcp" }, { container: 9090 }],
          imageCreated: t0,
          healthCheck: true,
        },
        { name: "sidecar", image: "example/proxy:1", status: "exited", exitCode: 137, healthCheck: false },
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

export const findings: Schemas["Finding"][] = [
  {
    id: 3,
    kind: "drift",
    rule: "digest",
    severity: "warning",
    summary: "api runs different images in dev, prod",
    services: [
      { target: "dev-aws", scope: "web", name: "api" },
      { target: "prod-aws", scope: "web", name: "api" },
    ],
    evidence: [
      { label: "dev", value: "sha256:new", service: { target: "dev-aws", scope: "web", name: "api" } },
      { label: "prod", value: "sha256:old", service: { target: "prod-aws", scope: "web", name: "api" } },
    ],
    firstSeen: t0,
    lastSeen: "2026-01-01T02:00:00Z",
    openedAt: "2026-01-01T01:00:00Z",
  },
  {
    id: 2,
    kind: "stuck",
    rule: "below-desired",
    severity: "warning",
    summary: "runs 1 of 2 desired",
    services: [{ target: "prod-aws", scope: "web", name: "api" }],
    evidence: [],
    firstSeen: t0,
    lastSeen: "2026-01-01T02:00:00Z",
    openedAt: "2026-01-01T00:15:00Z",
  },
  {
    id: 1,
    kind: "hygiene",
    rule: "latest-tag",
    severity: "info",
    summary: "runs images tagged latest",
    services: [{ target: "build-host-1", scope: "ci", name: "runner" }],
    evidence: [{ label: "image", value: "example/runner:latest" }],
    firstSeen: t0,
    lastSeen: t0,
    openedAt: t0,
  },
];

export const resolvedFindings: Schemas["Finding"][] = [
  { ...findings[1]!, id: 9, summary: "runs 0 of 2 desired", resolvedAt: "2026-01-01T03:00:00Z" },
];

const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();

// Docker services as a host with years of leftovers shows them: one stopped
// long ago, one stopped an hour ago, one in compose and running.
export const dockerServices: Schemas["Service"][] = [
  {
    ref: { target: "host-b", scope: "_standalone", name: "old-build" },
    provider: "docker",
    status: "down",
    instances: { running: 0, pending: 0, stopped: 1, unhealthy: 0 },
    stoppedSince: "2024-03-01T00:00:00Z",
    stale: false,
  },
  {
    ref: { target: "host-b", scope: "_standalone", name: "redis" },
    provider: "docker",
    status: "down",
    instances: { running: 0, pending: 0, stopped: 1, unhealthy: 0 },
    stoppedSince: hoursAgo(1),
    stale: false,
  },
  {
    ref: { target: "host-a", scope: "git", name: "server" },
    provider: "docker",
    status: "healthy",
    instances: { running: 2, pending: 0, stopped: 0, unhealthy: 0 },
    stale: false,
  },
];

export const dockerDetail: Schemas["ServiceDetail"] = {
  service: dockerServices[1]!,
  instances: [
    {
      id: "aaaaaaaaaaaa",
      status: "stopped",
      startedAt: t0,
      stoppedAt: hoursAgo(1),
      stopReason: "exited with code 1",
      restartPolicy: "unless-stopped",
      addresses: [{ network: "lan", ip: "192.0.2.6" }],
      mounts: [
        { type: "bind", source: "/data/redis", destination: "/data", readOnly: false },
        { type: "volume", destination: "/conf", readOnly: true },
      ],
      containers: [{ name: "redis", image: "redis:7", status: "exited", exitCode: 1 }],
    },
    { id: "bbbbbbbbbbbb", status: "stopped", containers: [{ name: "redis", image: "redis:7" }] },
  ],
  stops: [],
};
