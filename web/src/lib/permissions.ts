import { useMe, useTargets } from "@/lib/queries";

export type Operations = { restart: boolean; scale: boolean };

// useOperations is what the console offers on a target: the operator role, a
// target that is not read-only, and a provider that can do it. The server
// checks the same again.
export function useOperations(target: string): Operations {
  const me = useMe().data;
  const t = useTargets().data?.find((x) => x.name === target);
  const operator = me?.targets.some((r) => r.name === target && r.role === "operator") ?? false;
  const open = operator && t !== undefined && !t.readonly;
  return { restart: open && t.capabilities.restart, scale: open && t.capabilities.scale };
}
