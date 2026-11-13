import type { Schemas } from "@/lib/api";
import { serviceLabel } from "@/pages/links";

type Service = Schemas["Service"];

// Hosts keep containers that stopped years ago; past this age a down service
// is folded away so it does not bury today's failures.
export const LONG_STOPPED_DAYS = 7;
export const LONG_STOPPED_MS = LONG_STOPPED_DAYS * 24 * 60 * 60 * 1000;

export function isLongStopped(s: Service, now: number): boolean {
  return s.status === "down" && s.stoppedSince !== undefined && now - Date.parse(s.stoppedSince) > LONG_STOPPED_MS;
}

export function splitLongStopped(list: Service[], now: number): [Service[], Service[]] {
  const current: Service[] = [];
  const old: Service[] = [];
  for (const s of list) (isLongStopped(s, now) ? old : current).push(s);
  return [current, old];
}

export const sortKeys = ["service", "status", "running", "stopped"] as const;
export type SortKey = (typeof sortKeys)[number];
export const sortDirs = ["asc", "desc"] as const;
export type SortDir = (typeof sortDirs)[number];

// Ascending status puts the worst first.
const statusRank: Record<Schemas["ServiceStatus"], number> = { down: 0, degraded: 1, idle: 2, healthy: 3 };

function value(s: Service, key: SortKey): number | string | undefined {
  switch (key) {
    case "service":
      return serviceLabel(s.ref);
    case "status":
      return statusRank[s.status];
    case "running":
      return s.instances.running;
    case "stopped":
      return s.stoppedSince === undefined ? undefined : Date.parse(s.stoppedSince);
  }
}

// sortServices orders by key, then by name. A service without the value
// (nothing stopped) sorts last in either direction.
export function sortServices(list: Service[], key: SortKey, dir: SortDir): Service[] {
  const sign = dir === "asc" ? 1 : -1;
  return [...list].sort((a, b) => {
    const va = value(a, key);
    const vb = value(b, key);
    if (va === undefined || vb === undefined) {
      if (va !== vb) return va === undefined ? 1 : -1;
    } else if (va !== vb) {
      return sign * (typeof va === "string" && typeof vb === "string" ? va.localeCompare(vb) : Number(va) - Number(vb));
    }
    return serviceLabel(a.ref).localeCompare(serviceLabel(b.ref));
  });
}

// groupByTarget keeps the list's order within each target; targets are sorted by name.
export function groupByTarget(list: Service[]): [string, Service[]][] {
  const groups = new Map<string, Service[]>();
  for (const s of list) {
    const rows = groups.get(s.ref.target);
    if (rows) rows.push(s);
    else groups.set(s.ref.target, [s]);
  }
  return [...groups.entries()].sort(([a], [b]) => a.localeCompare(b));
}
