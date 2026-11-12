// Docker shows 12 characters of an ID; ECS task IDs read the same way.
export function shortID(id: string): string {
  return id.length > 12 ? id.slice(0, 12) : id;
}

export function shortDigest(digest: string | undefined): string {
  if (!digest) return "";
  const [algo, hex] = digest.split(":");
  return hex ? `${algo}:${hex.slice(0, 12)}` : digest.slice(0, 12);
}

const units: [Intl.RelativeTimeFormatUnit, number][] = [
  ["day", 86400],
  ["hour", 3600],
  ["minute", 60],
  ["second", 1],
];

const relative = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

export function ago(iso: string | undefined, now: Date = new Date()): string {
  if (!iso) return "never";
  const seconds = Math.round((new Date(iso).getTime() - now.getTime()) / 1000);
  const [unit, size] = units.find(([, size]) => Math.abs(seconds) >= size) ?? ["second", 1];
  return relative.format(Math.round(seconds / size), unit);
}

export function absolute(iso: string | undefined): string {
  return iso ? new Date(iso).toLocaleString() : "";
}
