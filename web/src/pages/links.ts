import type { Schemas } from "@/lib/api";
import type { ServiceKey } from "@/lib/queries";

// Docker's scope for a container outside compose. It stays in URLs and is
// left out of what people read.
export const STANDALONE = "_standalone";

export function isStandalone(ref: Schemas["ServiceRef"]): boolean {
  return ref.scope === STANDALONE;
}

export function serviceLabel(ref: Schemas["ServiceRef"]): string {
  return isStandalone(ref) ? `${ref.target}/${ref.name}` : `${ref.target}/${ref.scope}/${ref.name}`;
}

export function serviceHref(ref: Schemas["ServiceRef"]): string {
  return `/services/${[ref.target, ref.scope, ref.name].map(encodeURIComponent).join("/")}`;
}

export function logsHref(key: ServiceKey, instance: string): string {
  return `${serviceHref({ target: key.target, scope: key.scope, name: key.service })}/instances/${encodeURIComponent(instance)}/logs`;
}
