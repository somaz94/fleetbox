import { Link } from "react-router";
import { FilterSelect } from "@/components/FilterSelect";
import { QueryState } from "@/components/QueryState";
import { ServiceName } from "@/components/ServiceName";
import { Ago, ServiceStatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { SortableHead, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { oneOf, useFilterParams } from "@/lib/params";
import { serviceStatuses, useServices, useTargets, type ServiceFilter } from "@/lib/queries";
import { groupByTarget, LONG_STOPPED_DAYS, sortDirs, sortKeys, sortServices, splitLongStopped, type SortDir, type SortKey } from "@/lib/services";
import { serviceHref } from "@/pages/links";

type Sort = { key: SortKey; dir: SortDir; toggle: (key: SortKey) => void };

export function Services() {
  const [params, set, setAll] = useFilterParams();
  const filter: ServiceFilter = {
    target: params.get("target") || undefined,
    provider: params.get("provider") || undefined,
    status: oneOf(serviceStatuses, params.get("status")),
    q: params.get("q") || undefined,
  };
  // Sorting and grouping are the console's own; they never reach the API.
  const sort: Sort = {
    key: oneOf(sortKeys, params.get("sort")) ?? "service",
    dir: oneOf(sortDirs, params.get("dir")) ?? "asc",
    toggle: (key) => {
      const dir = key === sort.key && sort.dir === "asc" ? "desc" : "asc";
      setAll({ sort: key === "service" ? "" : key, dir: dir === "asc" ? "" : dir });
    },
  };
  const grouped = params.get("group") === "target";
  const targets = useTargets();
  const services = useServices(filter);
  const providers = [...new Set((targets.data ?? []).map((t) => t.provider))].sort();

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Services</h1>
      <div className="flex flex-wrap items-center gap-2">
        <Input
          aria-label="Search"
          placeholder="target/name"
          value={filter.q ?? ""}
          onChange={(e) => set("q", e.target.value)}
          className="w-64"
        />
        <FilterSelect
          label="Target"
          all="All targets"
          value={filter.target}
          values={(targets.data ?? []).map((t) => t.name)}
          onChange={(v) => set("target", v)}
        />
        <FilterSelect label="Provider" all="All providers" value={filter.provider} values={providers} onChange={(v) => set("provider", v)} />
        <FilterSelect label="Status" all="Any status" value={filter.status} values={serviceStatuses} onChange={(v) => set("status", v)} />
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" checked={grouped} onChange={(e) => set("group", e.target.checked ? "target" : "")} />
          Group by target
        </label>
      </div>
      <QueryState query={services}>
        {(list) => {
          if (list.length === 0) return <p className="text-sm text-muted-foreground">No services match.</p>;
          const [current, longStopped] = splitLongStopped(sortServices(list, sort.key, sort.dir), Date.now());
          return (
            <>
              {current.length > 0 && <ServiceTable list={current} sort={sort} grouped={grouped} />}
              {longStopped.length > 0 && (
                <details className="rounded-lg border">
                  <summary className="cursor-pointer px-3 py-2 text-sm font-medium">
                    Stopped for more than {LONG_STOPPED_DAYS} days ({longStopped.length})
                  </summary>
                  <ServiceTable list={longStopped} sort={sort} grouped={grouped} />
                </details>
              )}
            </>
          );
        }}
      </QueryState>
    </div>
  );
}

function ServiceTable({ list, sort, grouped }: { list: Schemas["Service"][]; sort: Sort; grouped: boolean }) {
  const head = (label: string, key: SortKey) => (
    <SortableHead label={label} active={sort.key === key} dir={sort.dir} onSort={() => sort.toggle(key)} />
  );
  const groups: [string | undefined, Schemas["Service"][]][] = grouped ? groupByTarget(list) : [[undefined, list]];
  return (
    <Table>
      <TableHeader>
        <TableRow>
          {head("Service", "service")}
          <TableHead>Provider</TableHead>
          {head("Status", "status")}
          {head("Running", "running")}
          {head("Stopped", "stopped")}
          <TableHead>Revision</TableHead>
          <TableHead>Rollout</TableHead>
        </TableRow>
      </TableHeader>
      {groups.map(([target, rows]) => (
        <TableBody key={target ?? ""}>
          {target !== undefined && (
            <TableRow>
              <th scope="rowgroup" colSpan={7} className="bg-muted/50 px-3 py-2 text-left font-medium">
                {target} <span className="font-normal text-muted-foreground">({rows.length})</span>
              </th>
            </TableRow>
          )}
          {rows.map((s) => (
            <ServiceRow key={serviceHref(s.ref)} s={s} />
          ))}
        </TableBody>
      ))}
    </Table>
  );
}

function ServiceRow({ s }: { s: Schemas["Service"] }) {
  return (
    <TableRow>
      <TableCell>
        <Link className="font-medium hover:underline" to={serviceHref(s.ref)}>
          <ServiceName serviceRef={s.ref} />
        </Link>
        {s.stale && (
          <Badge tone="warn" className="ml-2" title="The target's last refresh failed">
            stale
          </Badge>
        )}
      </TableCell>
      <TableCell>{s.provider}</TableCell>
      <TableCell>
        <ServiceStatusBadge status={s.status} />
      </TableCell>
      <TableCell>
        {s.instances.running}
        {s.desired !== undefined && <span className="text-muted-foreground"> / {s.desired}</span>}
      </TableCell>
      <TableCell>{s.stoppedSince && <Ago at={s.stoppedSince} />}</TableCell>
      <TableCell className="font-mono text-xs">{s.revision ?? ""}</TableCell>
      <TableCell>{s.rollout?.state ?? ""}</TableCell>
    </TableRow>
  );
}
