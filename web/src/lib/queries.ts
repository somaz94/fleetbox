import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { client, unwrap, type Schemas } from "@/lib/api";

// Lists and details read the server's cache, which the poller keeps current.
export const REFETCH_MS = 15_000;

export type ServiceKey = { target: string; scope: string; service: string };

export type ServiceFilter = {
  target?: string;
  provider?: string;
  status?: Schemas["ServiceStatus"];
  q?: string;
};

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: () => unwrap(client.GET("/me")), staleTime: 60_000 });
}

export function useTargets() {
  return useQuery({ queryKey: ["targets"], queryFn: () => unwrap(client.GET("/targets")), refetchInterval: REFETCH_MS });
}

export function useServices(filter: ServiceFilter = {}) {
  return useQuery({
    queryKey: ["services", filter],
    queryFn: () => unwrap(client.GET("/services", { params: { query: filter } })),
    refetchInterval: REFETCH_MS,
  });
}

const path = "/services/{target}/{scope}/{service}" as const;

export function useService(key: ServiceKey) {
  return useQuery({
    queryKey: ["service", key],
    queryFn: () => unwrap(client.GET(path, { params: { path: key } })),
    refetchInterval: REFETCH_MS,
  });
}

// Events are read fresh from the provider, so they load once and on demand.
export function useEvents(key: ServiceKey) {
  return useQuery({
    queryKey: ["events", key],
    queryFn: () => unwrap(client.GET(`${path}/events`, { params: { path: key } })),
    staleTime: 30_000,
  });
}

// useApplyDetail stores a service as an operation answered it and marks what
// that answer makes stale.
function useApplyDetail(key: ServiceKey) {
  const qc = useQueryClient();
  return (detail: Schemas["ServiceDetail"]) => {
    qc.setQueryData(["service", key], detail);
    qc.invalidateQueries({ queryKey: ["services"] });
    qc.invalidateQueries({ queryKey: ["events", key] });
    qc.invalidateQueries({ queryKey: ["audit"] });
  };
}

export function useRefresh(key: ServiceKey) {
  return useMutation({
    mutationFn: () => unwrap(client.POST(`${path}/refresh`, { params: { path: key } })),
    onSuccess: useApplyDetail(key),
  });
}

export function useRestart(key: ServiceKey) {
  return useMutation({
    mutationFn: () => unwrap(client.POST(`${path}/restart`, { params: { path: key } })),
    onSuccess: useApplyDetail(key),
  });
}

export function useScale(key: ServiceKey) {
  return useMutation({
    mutationFn: (desired: number) => unwrap(client.POST(`${path}/scale`, { params: { path: key }, body: { desired } })),
    onSuccess: useApplyDetail(key),
  });
}

export const AUDIT_PAGE = 50;

export function useAudit(target?: string) {
  return useInfiniteQuery({
    queryKey: ["audit", target ?? ""],
    queryFn: ({ pageParam }) =>
      unwrap(client.GET("/audit", { params: { query: { target, before: pageParam, limit: AUDIT_PAGE } } })),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next,
    refetchInterval: REFETCH_MS,
  });
}
