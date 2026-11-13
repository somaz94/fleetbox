import { Link } from "react-router";
import { FilterSelect } from "@/components/FilterSelect";
import { errorMessage } from "@/components/QueryState";
import { Ago } from "@/components/status";
import { Badge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { useFilterParams } from "@/lib/params";
import { useAudit, useTargets } from "@/lib/queries";
import { serviceHref, serviceLabel } from "@/pages/links";

const tones: Record<Schemas["AuditEntry"]["result"], Tone> = { ok: "good", pending: "neutral", denied: "warn", error: "bad" };

export function Audit() {
  const [params, set] = useFilterParams();
  const target = params.get("target") || undefined;
  const targets = useTargets();
  const audit = useAudit(target);
  const entries = audit.data?.pages.flatMap((p) => p.entries) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Audit log</h1>
      <div className="flex gap-2">
        <FilterSelect
          label="Target"
          all="All targets"
          value={target}
          values={(targets.data ?? []).map((t) => t.name)}
          onChange={(v) => set("target", v)}
        />
      </div>
      {audit.isPending && <p className="text-sm text-muted-foreground">Loading…</p>}
      {audit.isError && (
        <p role="alert" className="text-sm text-destructive">
          {errorMessage(audit.error)}
        </p>
      )}
      {audit.isSuccess && entries.length === 0 && <p className="text-sm text-muted-foreground">No operations recorded.</p>}
      {entries.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>When</TableHead>
              <TableHead>User</TableHead>
              <TableHead>Service</TableHead>
              <TableHead>Action</TableHead>
              <TableHead>Result</TableHead>
              <TableHead>Detail</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {entries.map((e) => (
              <TableRow key={e.id}>
                <TableCell>
                  <Ago at={e.time} />
                </TableCell>
                <TableCell title={e.subject}>{e.user}</TableCell>
                <TableCell>
                  <Link to={serviceHref(e.service)} className="hover:underline">
                    {serviceLabel(e.service)}
                  </Link>
                </TableCell>
                <TableCell>
                  {e.action}
                  {e.params && <span className="ml-2 font-mono text-xs text-muted-foreground">{e.params}</span>}
                </TableCell>
                <TableCell>
                  <Badge tone={tones[e.result]}>{e.result}</Badge>
                </TableCell>
                <TableCell className="text-muted-foreground">{e.error}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {audit.hasNextPage && (
        <Button variant="outline" size="sm" className="self-start" disabled={audit.isFetchingNextPage} onClick={() => audit.fetchNextPage()}>
          {audit.isFetchingNextPage ? "Loading…" : "Load more"}
        </Button>
      )}
    </div>
  );
}
