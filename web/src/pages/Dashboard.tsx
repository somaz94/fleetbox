import { Link } from "react-router";
import { QueryState } from "@/components/QueryState";
import { Ago, ServiceStatusBadge, TargetStateBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { useServices, useTargets } from "@/lib/queries";
import { serviceHref } from "@/pages/links";

const statuses: Schemas["ServiceStatus"][] = ["healthy", "degraded", "down", "idle"];

export function Dashboard() {
  const targets = useTargets();
  const services = useServices();

  return (
    <div className="flex flex-col gap-6">
      <h1 className="text-xl font-semibold">Dashboard</h1>
      <QueryState query={targets}>
        {(ts) => (
          <QueryState query={services}>
            {(ss) => (
              <>
                <Summary targets={ts} services={ss} />
                <Attention targets={ts} services={ss} />
                <TargetsTable targets={ts} />
              </>
            )}
          </QueryState>
        )}
      </QueryState>
    </div>
  );
}

function Summary({ targets, services }: { targets: Schemas["Target"][]; services: Schemas["Service"][] }) {
  const stale = targets.filter((t) => t.stale || (!t.refreshedAt && t.error)).length;
  const instances = targets.reduce((n, t) => n + t.instanceCount, 0);
  return (
    <div className="grid gap-4 sm:grid-cols-3">
      <Card>
        <CardHeader>
          <CardTitle>Targets</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-semibold">{targets.length}</p>
          <p className="text-sm text-muted-foreground">{stale === 0 ? "all current" : `${stale} stale or unreachable`}</p>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Services</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-semibold">{services.length}</p>
          <div className="mt-1 flex flex-wrap gap-2">
            {statuses.map((s) => {
              const n = services.filter((v) => v.status === s).length;
              return n > 0 ? (
                <Link key={s} to={`/services?status=${s}`} aria-label={`${n} ${s}`}>
                  <ServiceStatusBadge status={s} /> <span className="text-sm">{n}</span>
                </Link>
              ) : null;
            })}
          </div>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Instances</CardTitle>
        </CardHeader>
        <CardContent>
          <p className="text-2xl font-semibold">{instances}</p>
        </CardContent>
      </Card>
    </div>
  );
}

function Attention({ targets, services }: { targets: Schemas["Target"][]; services: Schemas["Service"][] }) {
  const failing = targets.filter((t) => t.error);
  const unhealthy = services.filter((s) => s.status === "down" || s.status === "degraded");
  if (failing.length === 0 && unhealthy.length === 0) return null;
  return (
    <Card>
      <CardHeader>
        <CardTitle>Needs attention</CardTitle>
      </CardHeader>
      <CardContent>
        <ul className="flex flex-col gap-2 text-sm">
          {failing.map((t) => (
            <li key={t.name} className="flex flex-wrap items-center gap-2">
              <TargetStateBadge target={t} />
              <span className="font-medium">{t.name}</span>
              <span className="text-muted-foreground">{t.error}</span>
            </li>
          ))}
          {unhealthy.map((s) => (
            <li key={serviceHref(s.ref)} className="flex items-center gap-2">
              <ServiceStatusBadge status={s.status} />
              <Link className="font-medium hover:underline" to={serviceHref(s.ref)}>
                {s.ref.target}/{s.ref.scope}/{s.ref.name}
              </Link>
              {s.desired !== undefined && (
                <span className="text-muted-foreground">
                  {s.instances.running} of {s.desired} running
                </span>
              )}
            </li>
          ))}
        </ul>
      </CardContent>
    </Card>
  );
}

function TargetsTable({ targets }: { targets: Schemas["Target"][] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Targets</CardTitle>
      </CardHeader>
      <CardContent>
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>Name</TableHead>
              <TableHead>Provider</TableHead>
              <TableHead>Env</TableHead>
              <TableHead>State</TableHead>
              <TableHead>Services</TableHead>
              <TableHead>Instances</TableHead>
              <TableHead>Refreshed</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {targets.map((t) => (
              <TableRow key={t.name}>
                <TableCell>
                  <Link className="font-medium hover:underline" to={`/services?target=${encodeURIComponent(t.name)}`}>
                    {t.name}
                  </Link>
                  {t.readonly && (
                    <Badge className="ml-2" title="Operations are blocked on this target">
                      read-only
                    </Badge>
                  )}
                </TableCell>
                <TableCell>{t.provider}</TableCell>
                <TableCell>{t.env ?? ""}</TableCell>
                <TableCell>
                  <TargetStateBadge target={t} />
                </TableCell>
                <TableCell>{t.serviceCount}</TableCell>
                <TableCell>{t.instanceCount}</TableCell>
                <TableCell>
                  <Ago at={t.refreshedAt} />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </CardContent>
    </Card>
  );
}
