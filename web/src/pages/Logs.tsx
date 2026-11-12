import { useEffect, useRef, useState } from "react";
import { Link, useParams, useSearchParams } from "react-router";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { NativeSelect } from "@/components/ui/input";
import { logsURL, useLogStream, type LogOptions } from "@/hooks/useLogStream";
import { shortID } from "@/lib/format";
import { useService, useTargets } from "@/lib/queries";
import { serviceHref } from "@/pages/links";
import { useServiceKey } from "@/pages/ServiceDetail";

const tails = [100, 500, 1000, 5000];

export function Logs() {
  const key = useServiceKey();
  const { instance = "" } = useParams();
  const [params, setParams] = useSearchParams();
  const target = useTargets().data?.find((t) => t.name === key.target);
  const canFollow = target?.capabilities.followLogs ?? false;
  const containers = useService(key).data?.instances.find((i) => i.id === instance)?.containers.map((c) => c.name) ?? [];

  const opts: LogOptions = {
    container: params.get("container") || undefined,
    tail: Number(params.get("tail")) || 500,
    follow: canFollow && params.get("follow") !== "false",
  };
  const [generation, setGeneration] = useState(0);
  const { lines, state, error } = useLogStream(logsURL(key, instance, opts), generation);

  const bottom = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (opts.follow) bottom.current?.scrollIntoView?.({ block: "end" });
  }, [lines, opts.follow]);

  const set = (k: string, v: string) =>
    setParams(
      (prev) => {
        const next = new URLSearchParams(prev);
        next.set(k, v);
        return next;
      },
      { replace: true },
    );

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-xl font-semibold">
        <Link to={serviceHref({ target: key.target, scope: key.scope, name: key.service })} className="text-muted-foreground hover:underline">
          {key.target}/{key.scope}/{key.service}
        </Link>{" "}
        / <span className="font-mono" title={instance}>{shortID(instance)}</span> logs
      </h1>
      <div className="flex flex-wrap items-center gap-2 text-sm">
        {containers.length > 1 && (
          <NativeSelect aria-label="Container" value={opts.container ?? ""} onChange={(e) => set("container", e.target.value)}>
            <option value="">Default container</option>
            {containers.map((c) => (
              <option key={c} value={c}>
                {c}
              </option>
            ))}
          </NativeSelect>
        )}
        <NativeSelect aria-label="Lines" value={String(opts.tail)} onChange={(e) => set("tail", e.target.value)}>
          {tails.map((n) => (
            <option key={n} value={n}>
              last {n}
            </option>
          ))}
        </NativeSelect>
        {canFollow && (
          <label className="flex items-center gap-2">
            <input type="checkbox" checked={opts.follow} onChange={(e) => set("follow", String(e.target.checked))} />
            Follow
          </label>
        )}
        <StreamBadge state={state} />
        {state === "failed" && (
          <Button variant="outline" size="sm" onClick={() => setGeneration((g) => g + 1)}>
            Reconnect
          </Button>
        )}
      </div>
      {error && (
        <p role="alert" className="text-sm text-destructive">
          {error}
        </p>
      )}
      <div className="max-h-[70vh] overflow-auto rounded-lg border bg-muted/40 p-3 font-mono text-xs" aria-label="Log output">
        {lines.length === 0 && state !== "connecting" && <p className="text-muted-foreground">No output.</p>}
        {lines.map((l, n) => (
          // Log lines are untrusted: rendered as text, never as HTML.
          <div key={n} className="flex gap-3 whitespace-pre-wrap break-all">
            {l.time && <span className="shrink-0 text-muted-foreground">{l.time}</span>}
            <span className={l.stream === "stderr" ? "text-red-700 dark:text-red-300" : undefined}>{l.text}</span>
          </div>
        ))}
        <div ref={bottom} />
      </div>
    </div>
  );
}

function StreamBadge({ state }: { state: string }) {
  const tone = state === "open" ? "good" : state === "failed" ? "bad" : "neutral";
  return <Badge tone={tone}>{state}</Badge>;
}
