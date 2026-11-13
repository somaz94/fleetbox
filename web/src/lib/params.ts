import { useSearchParams } from "react-router";

// oneOf keeps a URL value only if it is one of the allowed ones, so a stale or
// hand-edited link never reaches the API as a 400.
export function oneOf<T extends string>(values: readonly T[], v: string | null): T | undefined {
  return values.includes(v as T) ? (v as T) : undefined;
}

// useFilterParams keeps a list's filters in the URL, so a reload or a shared
// link shows the same list; a change replaces the history entry rather than adding one.
export function useFilterParams(): [URLSearchParams, (key: string, value: string) => void] {
  const [params, setParams] = useSearchParams();
  const set = (key: string, value: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        if (value) next.set(key, value);
        else next.delete(key);
        return next;
      },
      { replace: true },
    );
  return [params, set];
}
