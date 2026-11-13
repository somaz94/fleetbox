import { useId, type ReactNode } from "react";
import { Link } from "react-router";
import { QueryState } from "@/components/QueryState";
import { Ago, TargetStateBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { Schemas } from "@/lib/api";
import { useTargets } from "@/lib/queries";

const capabilityNames: (keyof Schemas["Capabilities"])[] = ["logs", "followLogs", "events", "restart", "scale"];

export function Targets() {
  const targets = useTargets();
  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Targets</h1>
      <QueryState query={targets}>
        {(list) =>
          list.length === 0 ? (
            <p className="text-sm text-muted-foreground">No targets you can see.</p>
          ) : (
            <div className="grid gap-4 lg:grid-cols-2">
              {list.map((t) => (
                <TargetCard key={t.name} target={t} />
              ))}
            </div>
          )
        }
      </QueryState>
    </div>
  );
}

function TargetCard({ target: t }: { target: Schemas["Target"] }) {
  const titleId = useId();
  const services = `/services?target=${encodeURIComponent(t.name)}`;
  const fields: [string, ReactNode][] = [
    ["Provider", t.provider],
    ["Env", t.env ?? "—"],
    ["Refreshed", <Ago key="r" at={t.refreshedAt} />],
    ["Last attempt", <Ago key="a" at={t.attemptedAt} />],
    [
      "Services",
      <Link key="s" className="hover:underline" to={services}>
        {t.serviceCount}
      </Link>,
    ],
    ["Instances", t.instanceCount],
  ];
  return (
    <Card role="group" aria-labelledby={titleId}>
      <CardHeader>
        <CardTitle className="flex flex-wrap items-center gap-2">
          <Link id={titleId} className="hover:underline" to={services}>
            {t.name}
          </Link>
          <TargetStateBadge target={t} />
          {t.readonly && <Badge title="Operations are blocked on this target">read-only</Badge>}
        </CardTitle>
      </CardHeader>
      <CardContent className="flex flex-col gap-3 text-sm">
        {t.error && <p className="break-words text-destructive">{t.error}</p>}
        <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1">
          {fields.map(([label, value]) => (
            <div key={label} className="contents">
              <dt className="text-muted-foreground">{label}</dt>
              <dd className="break-all">{value}</dd>
            </div>
          ))}
        </dl>
        {t.endpoint && (
          <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1" aria-label="Endpoint">
            {t.endpoint.map((a) => (
              <div key={a.name} className="contents">
                <dt className="text-muted-foreground">{a.name}</dt>
                <dd className="break-all font-mono text-xs">{a.value}</dd>
              </div>
            ))}
          </dl>
        )}
        <div className="flex flex-wrap gap-1" role="group" aria-label="Capabilities">
          {capabilityNames.map((c) => (
            <Badge key={c} tone={t.capabilities[c] ? "good" : "neutral"} title={t.capabilities[c] ? "supported" : "not supported"}>
              {t.capabilities[c] ? c : <s>{c}</s>}
            </Badge>
          ))}
        </div>
      </CardContent>
    </Card>
  );
}
