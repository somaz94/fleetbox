import { Fragment, useId, useState, type FormEvent, type ReactNode } from "react";
import { Link, useParams } from "react-router";
import { errorMessage, QueryState } from "@/components/QueryState";
import { ServiceName } from "@/components/ServiceName";
import { Ago, InstanceStatusBadge, ServiceStatusBadge, SeverityBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Dialog } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { ApiError, type Schemas } from "@/lib/api";
import { shortDigest, shortID } from "@/lib/format";
import { useOperations } from "@/lib/permissions";
import { useEvents, useFindings, useRefresh, useRestart, useScale, useService, useTargets, type ServiceKey } from "@/lib/queries";
import { logsHref, serviceLabel } from "@/pages/links";

export function useServiceKey(): ServiceKey {
  const { target = "", scope = "", service = "" } = useParams();
  return { target, scope, service };
}

export function ServiceDetail() {
  const key = useServiceKey();
  const detail = useService(key);
  const refresh = useRefresh(key);
  const target = useTargets().data?.find((t) => t.name === key.target);
  const [done, setDone] = useState<string>();

  return (
    <div className="flex flex-col gap-6">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">
          <Link to="/services" className="text-muted-foreground hover:underline">
            Services
          </Link>{" "}
          / <ServiceName serviceRef={{ target: key.target, scope: key.scope, name: key.service }} />
        </h1>
        <div className="ml-auto flex gap-2">
          <Button variant="outline" size="sm" disabled={refresh.isPending} onClick={() => refresh.mutate()}>
            {refresh.isPending ? "Refreshing…" : "Refresh"}
          </Button>
          {detail.data && <Operations serviceKey={key} desired={detail.data.service.desired} onDone={setDone} />}
        </div>
      </div>
      {done && (
        <p role="status" className="text-sm text-muted-foreground">
          {done}
        </p>
      )}
      {refresh.isError && (
        <p role="alert" className="text-sm text-destructive">
          Refresh failed: {errorMessage(refresh.error)}
        </p>
      )}
      <QueryState query={detail}>
        {(d) => (
          <>
            <Overview service={d.service} />
            <ServiceFindings serviceKey={key} />
            <Instances serviceKey={key} instances={d.instances} logs={target?.capabilities.logs ?? false} />
            <Stops stops={d.stops} />
          </>
        )}
      </QueryState>
      <Events serviceKey={key} />
    </div>
  );
}

type Action = "restart" | "scale";

function Operations({ serviceKey, desired, onDone }: { serviceKey: ServiceKey; desired?: number; onDone: (msg: string) => void }) {
  const allowed = useOperations(serviceKey.target);
  const [open, setOpen] = useState<Action>();
  return (
    <>
      {allowed.restart && (
        <Button variant="outline" size="sm" onClick={() => setOpen("restart")}>
          Restart
        </Button>
      )}
      {allowed.scale && (
        <Button variant="outline" size="sm" onClick={() => setOpen("scale")}>
          Scale
        </Button>
      )}
      {open && (
        <OperationDialog
          action={open}
          serviceKey={serviceKey}
          desired={desired}
          onClose={() => setOpen(undefined)}
          onDone={(msg) => {
            setOpen(undefined);
            onDone(msg);
          }}
        />
      )}
    </>
  );
}

const MAX_DESIRED = 1000;

const copy = {
  restart: {
    verb: "Restart",
    submit: "Restart service",
    help: "Every instance of the service is replaced. The request is recorded in the audit log.",
  },
  scale: {
    verb: "Scale",
    submit: "Scale service",
    help: "Sets how many instances the service runs. The request is recorded in the audit log.",
  },
} as const;

// OperationDialog runs an operation only once the service name is typed, so a
// stray click cannot restart or scale the wrong service.
function OperationDialog({
  action,
  serviceKey,
  desired,
  onClose,
  onDone,
}: {
  action: Action;
  serviceKey: ServiceKey;
  desired?: number;
  onClose: () => void;
  onDone: (msg: string) => void;
}) {
  const restart = useRestart(serviceKey);
  const scale = useScale(serviceKey);
  const mutation = action === "restart" ? restart : scale;
  const [typed, setTyped] = useState("");
  const [count, setCount] = useState(String(desired ?? 1));
  const n = Number(count);
  const countValid = count.trim() !== "" && Number.isInteger(n) && n >= 0 && n <= MAX_DESIRED;
  const ready = typed === serviceKey.service && (action === "restart" || countValid) && !mutation.isPending;
  const name = serviceLabel({ target: serviceKey.target, scope: serviceKey.scope, name: serviceKey.service });
  const hintId = useId();

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!ready) return;
    if (action === "restart") {
      restart.mutate(undefined, { onSuccess: () => onDone(`Restart of ${name} started.`) });
    } else {
      scale.mutate(n, { onSuccess: () => onDone(`${name} scaled to ${n}.`) });
    }
  };

  return (
    <Dialog title={`${copy[action].verb} ${name}`} onClose={onClose}>
      <form onSubmit={submit} className="flex flex-col gap-4 text-sm">
        <p className="text-muted-foreground">{copy[action].help}</p>
        {action === "scale" && (
          <div className="flex flex-col gap-1">
            <label className="flex flex-col gap-1">
              Desired instances
              <Input
                type="number"
                min={0}
                max={MAX_DESIRED}
                value={count}
                aria-invalid={!countValid}
                aria-describedby={hintId}
                onChange={(e) => setCount(e.target.value)}
              />
            </label>
            <span id={hintId} className="text-xs text-muted-foreground">
              A whole number from 0 to {MAX_DESIRED}.
            </span>
          </div>
        )}
        <label className="flex flex-col gap-1">
          <span>
            Type <code className="font-mono">{serviceKey.service}</code> to confirm
          </span>
          <Input value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false} />
        </label>
        {mutation.isError && (
          <p role="alert" className="text-destructive">
            {copy[action].verb} failed: {errorMessage(mutation.error)}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button type="submit" variant="destructive" disabled={!ready}>
            {mutation.isPending ? "Working…" : copy[action].submit}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}

function ServiceFindings({ serviceKey: k }: { serviceKey: ServiceKey }) {
  const findings = useFindings({ target: k.target });
  const mine = (findings.data ?? []).filter((f) =>
    f.services.some((r) => r.target === k.target && r.scope === k.scope && r.name === k.service),
  );
  // An empty section reads as "no findings", so a failed first read says so instead.
  if (findings.isError && !findings.data) {
    return (
      <p role="alert" className="text-sm text-destructive">
        Findings could not be read: {errorMessage(findings.error)}
      </p>
    );
  }
  if (mine.length === 0) return null;
  return (
    <Card>
      <CardHeader className="flex-row items-center">
        <CardTitle>Findings</CardTitle>
        <Link to={`/findings?target=${encodeURIComponent(k.target)}`} className="ml-auto text-sm text-muted-foreground hover:underline">
          All on {k.target}
        </Link>
      </CardHeader>
      <CardContent>
        <ul className="flex flex-col gap-2 text-sm">
          {mine.map((f) => (
            <li key={f.id} className="flex flex-wrap items-center gap-2">
              <Badge title={f.rule}>{f.kind}</Badge>
              <SeverityBadge severity={f.severity} />
              <span>{f.summary}</span>
              <span className="text-muted-foreground">
                since <Ago at={f.openedAt} />
              </span>
            </li>
          ))}
        </ul>
      </CardContent>
    </Card>
  );
}

function Overview({ service: s }: { service: Schemas["Service"] }) {
  const fields: [string, ReactNode][] = [
    ["Status", <ServiceStatusBadge key="s" status={s.status} />],
    ["Provider", s.provider],
    ["Running", s.desired !== undefined ? `${s.instances.running} of ${s.desired}` : String(s.instances.running)],
    ["Pending", String(s.instances.pending)],
    ["Unhealthy", String(s.instances.unhealthy)],
  ];
  if (s.revision) fields.push(["Revision", <span key="r" className="font-mono text-xs">{s.revision}</span>]);
  if (s.rollout) fields.push(["Rollout", <span key="o">{s.rollout.state} {s.rollout.startedAt && <Ago at={s.rollout.startedAt} />}</span>]);
  if (s.stoppedSince) fields.push(["Stopped", <Ago key="t" at={s.stoppedSince} />]);
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
                <Fragment key={i.id}>
                  <TableRow>
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
                            <ContainerFacts container={c} />
                          </li>
                        ))}
                      </ul>
                    </TableCell>
                    <TableCell className="font-mono text-xs">{i.revision ?? ""}</TableCell>
                    <TableCell>
                      <Ago at={i.startedAt} />
                      {i.stoppedAt && (
                        <p className="mt-1 text-xs text-muted-foreground">
                          stopped <Ago at={i.stoppedAt} />
                        </p>
                      )}
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
                  <InstanceDetail instance={i} columns={logs ? 7 : 6} />
                </Fragment>
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
                      {e.severity === "warning" && <SeverityBadge severity={e.severity} />}
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

function ContainerFacts({ container: c }: { container: Schemas["Container"] }) {
  const ports = (c.ports ?? []).map((p) => `${p.host !== undefined ? `${p.host}→` : ""}${p.container}${p.protocol ? `/${p.protocol}` : ""}`);
  return (
    <span className="ml-1 text-muted-foreground">
      {ports.length > 0 && <span title="Ports (host→container)"> · {ports.join(", ")}</span>}
      {c.healthCheck === false && <span> · no health check</span>}
      {c.imageCreated && (
        <span>
          {" "}
          · image built <Ago at={c.imageCreated} />
        </span>
      )}
    </span>
  );
}

// InstanceDetail folds the facts a provider reports beyond status: where the
// instance runs, its limits, and what it mounts. Absent facts are not shown.
function InstanceDetail({ instance: i, columns }: { instance: Schemas["Instance"]; columns: number }) {
  const fields: [string, ReactNode][] = [];
  if (i.createdAt) fields.push(["Created", <Ago key="c" at={i.createdAt} />]);
  if (i.restartPolicy) fields.push(["Restart policy", i.restartPolicy]);
  if (i.launchType) fields.push(["Launch type", i.launchType]);
  if (i.zone) fields.push(["Zone", i.zone]);
  if (i.cpu !== undefined) fields.push(["CPU", `${i.cpu} vCPU`]);
  if (i.memoryMiB !== undefined) fields.push(["Memory", `${i.memoryMiB} MiB`]);
  for (const a of i.addresses ?? []) fields.push([a.network ? `IP (${a.network})` : "IP", <code>{a.ip}</code>]);
  const mounts = i.mounts ?? [];
  if (fields.length === 0 && mounts.length === 0) return null;
  return (
    <TableRow className="hover:bg-transparent">
      <TableCell colSpan={columns} className="pt-0">
        <details>
          <summary className="cursor-pointer text-xs text-muted-foreground" aria-label={`Details of ${shortID(i.id)}`}>
            Details
          </summary>
          <dl className="mt-2 grid grid-cols-2 gap-x-6 gap-y-1 text-xs sm:grid-cols-4">
            {fields.map(([k, v], n) => (
              <div key={`${k}-${n}`}>
                <dt className="text-muted-foreground">{k}</dt>
                <dd>{v}</dd>
              </div>
            ))}
          </dl>
          {mounts.length > 0 && (
            <ul className="mt-2 flex flex-col gap-1 text-xs" aria-label="Mounts">
              {mounts.map((m, n) => (
                <li key={`${m.destination}-${n}`} className="font-mono">
                  {m.source ?? "?"} → {m.destination}
                  {m.readOnly && " (ro)"}
                  {m.type && <span className="text-muted-foreground"> {m.type}</span>}
                </li>
              ))}
            </ul>
          )}
        </details>
      </TableCell>
    </TableRow>
  );
}
