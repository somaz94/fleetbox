import { Fragment, useState } from "react";
import { Link } from "react-router";
import { FilterSelect } from "@/components/FilterSelect";
import { QueryState } from "@/components/QueryState";
import { Ago, SeverityBadge } from "@/components/status";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/input";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@/components/ui/table";
import type { Schemas } from "@/lib/api";
import { oneOf, useFilterParams } from "@/lib/params";
import { findingKinds, severities, useFindings, useTargets, type FindingFilter } from "@/lib/queries";
import { serviceHref, serviceLabel } from "@/pages/links";

export function Findings() {
  const [params, set] = useFilterParams();
  const filter: FindingFilter = {
    kind: oneOf(findingKinds, params.get("kind")),
    target: params.get("target") || undefined,
    severity: oneOf(severities, params.get("severity")),
    state: params.get("state") === "resolved" ? "resolved" : undefined,
  };
  const targets = useTargets();
  const findings = useFindings(filter);
  const resolved = filter.state === "resolved";

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">Findings</h1>
      <div className="flex flex-wrap gap-2">
        <NativeSelect aria-label="State" value={filter.state ?? ""} onChange={(e) => set("state", e.target.value)}>
          <option value="">Open</option>
          <option value="resolved">Recently resolved</option>
        </NativeSelect>
        <FilterSelect label="Kind" all="All kinds" value={filter.kind} values={findingKinds} onChange={(v) => set("kind", v)} />
        <FilterSelect label="Severity" all="Any severity" value={filter.severity} values={severities} onChange={(v) => set("severity", v)} />
        <FilterSelect
          label="Target"
          all="All targets"
          value={filter.target}
          values={(targets.data ?? []).map((t) => t.name)}
          onChange={(v) => set("target", v)}
        />
      </div>
      <QueryState query={findings}>
        {(list) =>
          list.length === 0 ? (
            <p className="text-sm text-muted-foreground">{resolved ? "Nothing resolved recently." : "No open findings."}</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Kind</TableHead>
                  <TableHead>Severity</TableHead>
                  <TableHead>Finding</TableHead>
                  <TableHead>Services</TableHead>
                  <TableHead>{resolved ? "Resolved" : "Open since"}</TableHead>
                  <TableHead>
                    <span className="sr-only">Evidence</span>
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.map((f) => (
                  <FindingRow key={f.id} finding={f} resolved={resolved} />
                ))}
              </TableBody>
            </Table>
          )
        }
      </QueryState>
    </div>
  );
}

function FindingRow({ finding: f, resolved }: { finding: Schemas["Finding"]; resolved: boolean }) {
  const [open, setOpen] = useState(false);
  const evidenceId = `finding-${f.id}-evidence`;
  return (
    <Fragment>
      <TableRow>
        <TableCell>
          <Badge title={f.rule}>{f.kind}</Badge>
        </TableCell>
        <TableCell>
          <SeverityBadge severity={f.severity} />
        </TableCell>
        <TableCell>{f.summary}</TableCell>
        <TableCell>
          <ServiceLinks refs={f.services} />
        </TableCell>
        <TableCell>
          <Ago at={resolved ? f.resolvedAt : f.openedAt} />
        </TableCell>
        <TableCell>
          {f.evidence.length > 0 && (
            <Button variant="ghost" size="sm" aria-expanded={open} aria-controls={evidenceId} onClick={() => setOpen(!open)}>
              Evidence
            </Button>
          )}
        </TableCell>
      </TableRow>
      {open && (
        <TableRow id={evidenceId}>
          <TableCell colSpan={6}>
            <dl className="grid grid-cols-[max-content_1fr] gap-x-4 gap-y-1 text-sm">
              {f.evidence.map((e, i) => (
                <Fragment key={i}>
                  <dt className="text-muted-foreground">{e.label}</dt>
                  <dd className="break-all font-mono text-xs">
                    {e.service && (
                      <Link className="mr-2 font-sans hover:underline" to={serviceHref(e.service)}>
                        {serviceLabel(e.service)}
                      </Link>
                    )}
                    {e.value}
                  </dd>
                </Fragment>
              ))}
            </dl>
          </TableCell>
        </TableRow>
      )}
    </Fragment>
  );
}

function ServiceLinks({ refs }: { refs: Schemas["ServiceRef"][] }) {
  return (
    <ul className="flex flex-col gap-0.5">
      {refs.map((r) => (
        <li key={serviceHref(r)}>
          <Link className="hover:underline" to={serviceHref(r)}>
            {serviceLabel(r)}
          </Link>
        </li>
      ))}
    </ul>
  );
}
