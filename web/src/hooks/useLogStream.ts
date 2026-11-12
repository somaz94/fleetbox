import { useEffect, useState } from "react";
import type { Schemas } from "@/lib/api";
import type { ServiceKey } from "@/lib/queries";

// Bounds memory on a long followed stream; the oldest lines are dropped first.
export const MAX_LINES = 5000;

export type LogOptions = { container?: string; tail?: number; follow: boolean };

export type StreamState = "connecting" | "open" | "ended" | "failed";

export function logsURL(key: ServiceKey, instance: string, opts: LogOptions): string {
  const seg = [key.target, key.scope, key.service].map(encodeURIComponent).join("/");
  const q = new URLSearchParams();
  if (opts.container) q.set("container", opts.container);
  if (opts.tail) q.set("tail", String(opts.tail));
  q.set("follow", String(opts.follow));
  return `/api/v1/services/${seg}/instances/${encodeURIComponent(instance)}/logs?${q}`;
}

// Bumping generation reopens the same url (a reconnect); the old stream is closed.
export function useLogStream(url: string, generation = 0) {
  const [lines, setLines] = useState<Schemas["LogLine"][]>([]);
  const [state, setState] = useState<StreamState>("connecting");
  const [error, setError] = useState<string>();

  useEffect(() => {
    setLines([]);
    setState("connecting");
    setError(undefined);
    const source = new EventSource(url);
    source.onopen = () => setState("open");
    source.addEventListener("line", (e) => {
      const line = JSON.parse((e as MessageEvent<string>).data) as Schemas["LogLine"];
      setLines((prev) => {
        const next = prev.length >= MAX_LINES ? prev.slice(prev.length - MAX_LINES + 1) : prev.slice();
        next.push(line);
        return next;
      });
    });
    source.addEventListener("end", () => {
      source.close();
      setState("ended");
    });
    // The server's own `error` event carries an Error body; a bare error event
    // is the connection failing, which EventSource would retry and replay the
    // tail on, so the stream stops and the user reconnects instead.
    source.addEventListener("error", (e) => {
      source.close();
      const data = (e as MessageEvent<string>).data;
      let message = "the log stream was interrupted";
      if (data) {
        try {
          message = (JSON.parse(data) as Schemas["Error"]).error.message;
        } catch {
          message = data;
        }
      }
      setError(message);
      setState("failed");
    });
    return () => source.close();
  }, [url, generation]);

  return { lines, state, error };
}
