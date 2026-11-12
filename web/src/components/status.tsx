import { Badge, type Tone } from "@/components/ui/badge";
import type { Schemas } from "@/lib/api";
import { absolute, ago } from "@/lib/format";

const serviceTone: Record<Schemas["ServiceStatus"], Tone> = {
  healthy: "good",
  degraded: "warn",
  down: "bad",
  idle: "neutral",
};

export function ServiceStatusBadge({ status }: { status: Schemas["ServiceStatus"] }) {
  return <Badge tone={serviceTone[status]}>{status}</Badge>;
}

const instanceTone: Record<Schemas["InstanceStatus"], Tone> = {
  running: "good",
  pending: "warn",
  stopped: "neutral",
  unknown: "neutral",
};

export function InstanceStatusBadge({ status, health }: { status: Schemas["InstanceStatus"]; health?: Schemas["Health"] }) {
  const tone = health === "unhealthy" ? "bad" : instanceTone[status];
  return <Badge tone={tone}>{health && status === "running" ? `${status}, ${health}` : status}</Badge>;
}

export function TargetStateBadge({ target }: { target: Schemas["Target"] }) {
  if (!target.refreshedAt && target.error) return <Badge tone="bad">unreachable</Badge>;
  if (target.stale) return <Badge tone="warn">stale</Badge>;
  if (!target.refreshedAt) return <Badge>waiting</Badge>;
  return <Badge tone="good">ok</Badge>;
}

export function Ago({ at }: { at?: string }) {
  return <time title={absolute(at)}>{ago(at)}</time>;
}
