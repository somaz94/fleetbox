import { Link } from "react-router";
import { errorMessage, QueryState } from "@/components/QueryState";
import { Ago, ServiceStatusBadge, TargetStateBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { findingKinds, serviceStatuses, useFindings, useServices, useTargets } from "@/lib/queries";
import { LONG_STOPPED_DAYS, splitLongStopped } from "@/lib/services";
import { serviceHref, serviceLabel } from "@/pages/links";

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
    <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
      <Card>
        <CardHeader>
          <CardTitle>
            <Link to="/targets" className="hover:underline">
              Targets
            </Link>
          </CardTitle>
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
            {serviceStatuses.map((s) => {
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
      <FindingsCard />
    </div>
  );
}

// FindingsCard reads on its own, so a slow or failing findings read never holds up the dashboard.
function FindingsCard() {
  const findings = useFindings();
  const list = findings.data ?? [];
  return (
    <Card>
      <CardHeader>
        <CardTitle>
          <Link to="/findings" className="hover:underline">
            Open findings
          </Link>
        </CardTitle>
      </CardHeader>
      <CardContent>
        {findings.isError ? (
          <p role="alert" className="text-sm text-destructive">
            {errorMessage(findings.error)}
          </p>
        ) : (
          <>
            <p className="text-2xl font-semibold">{findings.isPending ? "…" : list.length}</p>
            <div className="mt-1 flex flex-wrap gap-2">
              {findingKinds.map((k) => {
                const n = list.filter((f) => f.kind === k).length;
                return n > 0 ? (
                  <Link key={k} to={`/findings?kind=${k}`} aria-label={`${n} ${k}`}>
                    <Badge>{k}</Badge> <span className="text-sm">{n}</span>
                  </Link>
                ) : null;
              })}
            </div>
          </>
        )}
      </CardContent>
    </Card>
  );
}

function Attention({ targets, services }: { targets: Schemas["Target"][]; services: Schemas["Service"][] }) {
  const failing = targets.filter((t) => t.error);
  const [current, longStopped] = splitLongStopped(
    services.filter((s) => s.status === "down" || s.status === "degraded"),
    Date.now(),
  );
  if (failing.length === 0 && current.length === 0 && longStopped.length === 0) return null;
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
          {current.map((s) => (
            <li key={serviceHref(s.ref)} className="flex items-center gap-2">
              <ServiceStatusBadge status={s.status} />
              <Link className="font-medium hover:underline" to={serviceHref(s.ref)}>
                {serviceLabel(s.ref)}
              </Link>
              {s.desired !== undefined && (
                <span className="text-muted-foreground">
                  {s.instances.running} of {s.desired} running
                </span>
              )}
            </li>
          ))}
          {longStopped.length > 0 && (
            <li className="text-muted-foreground">
              <Link className="hover:underline" to="/services?status=down">
                {longStopped.length} stopped for more than {LONG_STOPPED_DAYS} days
              </Link>{" "}
              not listed
            </li>
          )}
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
