import { QueryClient } from "@tanstack/react-query";
import { ApiError } from "@/lib/api";

// A client error will not change on retry; a server or network error might.
export function newQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: {
      queries: {
        retry: (count, error) => !(error instanceof ApiError && error.status < 500) && count < 2,
      },
    },
  });
}
