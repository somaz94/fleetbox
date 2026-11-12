import type { UseQueryResult } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { ApiError } from "@/lib/api";

export function errorMessage(error: unknown): string {
  if (error instanceof ApiError && error.code === "not_found") {
    return "Not found, or you hold no role on its target.";
  }
  return error instanceof Error ? error.message : String(error);
}

export function QueryState<T>({ query, children }: { query: UseQueryResult<T>; children: (data: T) => ReactNode }) {
  if (query.isPending) {
    return <p className="text-sm text-muted-foreground">Loading…</p>;
  }
  if (query.isError) {
    return (
      <p role="alert" className="text-sm text-destructive">
        {errorMessage(query.error)}
      </p>
    );
  }
  return <>{children(query.data)}</>;
}
