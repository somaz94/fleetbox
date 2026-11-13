import { Link, useSearchParams } from "react-router";
import { errorMessage } from "@/components/QueryState";
import { Ago } from "@/components/status";
import { Badge, type Tone } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { useAudit, useTargets } from "@/lib/queries";
import { serviceHref } from "@/pages/links";

const tones: Record<Schemas["AuditEntry"]["result"], Tone> = { ok: "good", pending: "neutral", denied: "warn", error: "bad" };

export function Audit() {
  const [params, setParams] = useSearchParams();
  const target = params.get("target") || undefined;
  const targets = useTargets();
  const audit = useAudit(target);
  const entries = audit.data?.pages.flatMap((p) => p.entries) ?? [];

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Audit log</h1>
      <div className="flex gap-2">
        <NativeSelect
          aria-label="Target"
          value={target ?? ""}
          onChange={(e) => setParams(e.target.value ? { target: e.target.value } : {}, { replace: true })}
        >
          <option value="">All targets</option>
          {(targets.data ?? []).map((t) => (
            <option key={t.name} value={t.name}>
              {t.name}
            </option>
          ))}
        </NativeSelect>
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
                    {e.service.target}/{e.service.scope}/{e.service.name}
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
