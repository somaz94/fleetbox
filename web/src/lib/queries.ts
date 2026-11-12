import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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

export function useRefresh(key: ServiceKey) {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: () => unwrap(client.POST(`${path}/refresh`, { params: { path: key } })),
    onSuccess: (detail) => {
      qc.setQueryData(["service", key], detail);
      qc.invalidateQueries({ queryKey: ["services"] });
      qc.invalidateQueries({ queryKey: ["events", key] });
    },
  });
}
