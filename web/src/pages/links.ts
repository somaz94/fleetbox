import type { Schemas } from "@/lib/api";
import type { ServiceKey } from "@/lib/queries";

export function serviceHref(ref: Schemas["ServiceRef"]): string {
  return `/services/${[ref.target, ref.scope, ref.name].map(encodeURIComponent).join("/")}`;
}

export function logsHref(key: ServiceKey, instance: string): string {
  return `${serviceHref({ target: key.target, scope: key.scope, name: key.service })}/instances/${encodeURIComponent(instance)}/logs`;
}
