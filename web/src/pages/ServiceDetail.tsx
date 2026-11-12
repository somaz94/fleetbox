import { Link, useParams } from "react-router";
import { errorMessage, QueryState } from "@/components/QueryState";
import { Ago, InstanceStatusBadge, ServiceStatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, type Schemas } from "@/lib/api";
import { shortDigest, shortID } from "@/lib/format";
import { useEvents, useRefresh, useService, useTargets, type ServiceKey } from "@/lib/queries";
import { logsHref } from "@/pages/links";

export function useServiceKey(): ServiceKey {
  const { target = "", scope = "", service = "" } = useParams();
  return { target, scope, service };
}

export function ServiceDetail() {
  const key = useServiceKey();
  const detail = useService(key);
  const refresh = useRefresh(key);
  const target = useTargets().data?.find((t) => t.name === key.target);

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">
          <Link to="/services" className="text-muted-foreground hover:underline">
            Services
          </Link>{" "}
          / {key.target}/{key.scope}/{key.service}
        </h1>
        <Button variant="outline" size="sm" className="ml-auto" disabled={refresh.isPending} onClick={() => refresh.mutate()}>
          {refresh.isPending ? "Refreshing…" : "Refresh"}
        </Button>
      </div>
      {refresh.isError && (
        <p role="alert" className="text-sm text-destructive">
          Refresh failed: {errorMessage(refresh.error)}
        </p>
      )}
      <QueryState query={detail}>
        {(d) => (
          <>
            <Overview service={d.service} />
            <Instances serviceKey={key} instances={d.instances} logs={target?.capabilities.logs ?? false} />
            <Stops stops={d.stops} />
          </>
        )}
      </QueryState>
      <Events serviceKey={key} />
    </div>
  );
}

function Overview({ service: s }: { service: Schemas["Service"] }) {
  const fields: [string, React.ReactNode][] = [
    ["Status", <ServiceStatusBadge key="s" status={s.status} />],
    ["Provider", s.provider],
    ["Running", s.desired !== undefined ? `${s.instances.running} of ${s.desired}` : String(s.instances.running)],
    ["Pending", String(s.instances.pending)],
    ["Unhealthy", String(s.instances.unhealthy)],
  ];
  if (s.revision) fields.push(["Revision", <span key="r" className="font-mono text-xs">{s.revision}</span>]);
  if (s.rollout) fields.push(["Rollout", <span key="o">{s.rollout.state} {s.rollout.startedAt && <Ago at={s.rollout.startedAt} />}</span>]);
  return (
    <Card>
      <CardContent className="pt-4">
        {s.stale && (
          <p className="mb-3 text-sm text-amber-700 dark:text-amber-300">
            The target's last refresh failed; this is the last data it returned.
          </p>
        )}
        <dl className="grid grid-cols-2 gap-x-6 gap-y-2 text-sm sm:grid-cols-4">
          {fields.map(([k, v]) => (
            <div key={k}>
              <dt className="text-muted-foreground">{k}</dt>
              <dd>{v}</dd>
            </div>
          ))}
        </dl>
        {s.labels && Object.keys(s.labels).length > 0 && (
          <div className="mt-3 flex flex-wrap gap-1">
            {Object.entries(s.labels).map(([k, v]) => (
              <Badge key={k} className="font-mono">
                {k}={v}
              </Badge>
            ))}
          </div>
        )}
      </CardContent>
    </Card>
  );
}

function Instances({ serviceKey, instances, logs }: { serviceKey: ServiceKey; instances: Schemas["Instance"][]; logs: boolean }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Instances</CardTitle>
      </CardHeader>
      <CardContent>
        {instances.length === 0 ? (
          <p className="text-sm text-muted-foreground">No instances.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>ID</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Containers</TableHead>
                <TableHead>Revision</TableHead>
                <TableHead>Started</TableHead>
                <TableHead>Restarts</TableHead>
                {logs && <TableHead />}
              </TableRow>
            </TableHeader>
            <TableBody>
              {instances.map((i) => (
                <TableRow key={i.id}>
                  <TableCell className="font-mono text-xs" title={i.id}>
                    {shortID(i.id)}
                  </TableCell>
                  <TableCell>
                    <InstanceStatusBadge status={i.status} health={i.health} />
                    {i.stopReason && <p className="mt-1 text-xs text-muted-foreground">{i.stopReason}</p>}
                  </TableCell>
                  <TableCell>
                    <ul className="flex flex-col gap-1">
                      {i.containers.map((c) => (
                        <li key={c.name} className="text-xs">
                          <span className="font-medium">{c.name}</span>{" "}
                          <span className="font-mono text-muted-foreground" title={c.digest}>
                            {c.image} {shortDigest(c.digest)}
                          </span>
                          {c.exitCode !== undefined && c.status !== "running" && <span> exit {c.exitCode}</span>}
                        </li>
                      ))}
                    </ul>
                  </TableCell>
                  <TableCell className="font-mono text-xs">{i.revision ?? ""}</TableCell>
                  <TableCell>
                    <Ago at={i.startedAt} />
                  </TableCell>
                  <TableCell>{i.restarts ?? ""}</TableCell>
                  {logs && (
                    <TableCell>
                      <Link className="text-sm hover:underline" to={logsHref(serviceKey, i.id)}>
                        Logs
                      </Link>
                    </TableCell>
                  )}
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function Stops({ stops }: { stops: Schemas["StopRecord"][] }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Recent stops</CardTitle>
      </CardHeader>
      <CardContent>
        {stops.length === 0 ? (
          <p className="text-sm text-muted-foreground">No stops recorded.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Stopped</TableHead>
                <TableHead>Instance</TableHead>
                <TableHead>Reason</TableHead>
                <TableHead>Exit</TableHead>
                <TableHead>Image</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {stops.map((r) => (
                <TableRow key={`${r.instanceId}-${r.stoppedAt}`}>
                  <TableCell>
                    <Ago at={r.stoppedAt} />
                  </TableCell>
                  <TableCell className="font-mono text-xs" title={r.instanceId}>
                    {shortID(r.instanceId)}
                  </TableCell>
                  <TableCell>{r.reason ?? ""}</TableCell>
                  <TableCell>{r.exitCode ?? ""}</TableCell>
                  <TableCell className="font-mono text-xs" title={r.digest}>
                    {r.image ?? ""} {shortDigest(r.digest)}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </CardContent>
    </Card>
  );
}

function Events({ serviceKey }: { serviceKey: ServiceKey }) {
  const events = useEvents(serviceKey);
  const unsupported = events.error instanceof ApiError && events.error.code === "unsupported";
  return (
    <Card>
      <CardHeader className="flex-row items-center">
        <CardTitle>Events</CardTitle>
        <Button variant="ghost" size="sm" className="ml-auto" disabled={events.isFetching} onClick={() => events.refetch()}>
          Reload
        </Button>
      </CardHeader>
      <CardContent>
        {unsupported ? (
          <p className="text-sm text-muted-foreground">This provider has no event timeline.</p>
        ) : (
          <QueryState query={events}>
            {(list) =>
              list.length === 0 ? (
                <p className="text-sm text-muted-foreground">No events.</p>
              ) : (
                <ul className="flex flex-col gap-1 text-sm">
                  {list.map((e, n) => (
                    <li key={n} className="flex gap-3">
                      <span className="w-28 shrink-0 text-muted-foreground">
                        <Ago at={e.time} />
                      </span>
                      {e.severity === "warning" && <Badge tone="warn">warning</Badge>}
                      <span className="break-words">{e.message}</span>
                    </li>
                  ))}
                </ul>
              )
            }
          </QueryState>
        )}
      </CardContent>
    </Card>
  );
}
