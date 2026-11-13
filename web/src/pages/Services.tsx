import { Link } from "react-router";
import { FilterSelect } from "@/components/FilterSelect";
import { QueryState } from "@/components/QueryState";
import { ServiceStatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import { oneOf, useFilterParams } from "@/lib/params";
import { serviceStatuses, useServices, useTargets, type ServiceFilter } from "@/lib/queries";
import { serviceHref } from "@/pages/links";

export function Services() {
  const [params, set] = useFilterParams();
  const filter: ServiceFilter = {
    target: params.get("target") || undefined,
    provider: params.get("provider") || undefined,
    status: oneOf(serviceStatuses, params.get("status")),
    q: params.get("q") || undefined,
  };
  const targets = useTargets();
  const services = useServices(filter);
  const providers = [...new Set((targets.data ?? []).map((t) => t.provider))].sort();

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Services</h1>
      <div className="flex flex-wrap gap-2">
        <Input
          aria-label="Search"
          placeholder="target/scope/name"
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
      </div>
      <QueryState query={services}>
        {(list) =>
          list.length === 0 ? (
            <p className="text-sm text-muted-foreground">No services match.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Service</TableHead>
                  <TableHead>Provider</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Running</TableHead>
                  <TableHead>Revision</TableHead>
                  <TableHead>Rollout</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((s) => (
                  <TableRow key={serviceHref(s.ref)}>
                    <TableCell>
                      <Link className="font-medium hover:underline" to={serviceHref(s.ref)}>
                        <span className="text-muted-foreground">
                          {s.ref.target}/{s.ref.scope}/
                        </span>
                        {s.ref.name}
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
                    <TableCell className="font-mono text-xs">{s.revision ?? ""}</TableCell>
                    <TableCell>{s.rollout?.state ?? ""}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )
        }
      </QueryState>
    </div>
  );
}
