import { Link } from "react-router";
import { QueryState } from "@/components/QueryState";
import { ServiceStatusBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Input, NativeSelect } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { oneOf, useFilterParams } from "@/lib/params";
import { useServices, useTargets, type ServiceFilter } from "@/lib/queries";
import { serviceHref } from "@/pages/links";

const statuses: Schemas["ServiceStatus"][] = ["healthy", "degraded", "down", "idle"];

export function Services() {
  const [params, set] = useFilterParams();
  const filter: ServiceFilter = {
    target: params.get("target") || undefined,
    provider: params.get("provider") || undefined,
    status: oneOf(statuses, params.get("status")),
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
        <NativeSelect aria-label="Target" value={filter.target ?? ""} onChange={(e) => set("target", e.target.value)}>
          <option value="">All targets</option>
          {(targets.data ?? []).map((t) => (
            <option key={t.name} value={t.name}>
              {t.name}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect aria-label="Provider" value={filter.provider ?? ""} onChange={(e) => set("provider", e.target.value)}>
          <option value="">All providers</option>
          {providers.map((p) => (
            <option key={p} value={p}>
              {p}
            </option>
          ))}
        </NativeSelect>
        <NativeSelect aria-label="Status" value={filter.status ?? ""} onChange={(e) => set("status", e.target.value)}>
          <option value="">Any status</option>
          {statuses.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </NativeSelect>
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
