import { groupByTarget, isLongStopped, LONG_STOPPED_MS, sortServices, splitLongStopped } from "@/lib/services";
import { dockerServices, services } from "@/test/fixtures";

const now = Date.parse("2026-11-13T00:00:00Z");
const [old, recent, running] = dockerServices as [(typeof dockerServices)[0], (typeof dockerServices)[0], (typeof dockerServices)[0]];
const at = (ms: number) => ({ ...recent, stoppedSince: new Date(now - ms).toISOString() });

test("folds a down service once it has been stopped for more than seven days", () => {
  expect(isLongStopped(old, now)).toBe(true);
  expect(isLongStopped(at(LONG_STOPPED_MS - 1000), now)).toBe(false);
  expect(isLongStopped(at(LONG_STOPPED_MS + 1000), now)).toBe(true);
  expect(isLongStopped({ ...old, status: "degraded" }, now)).toBe(false);
  expect(isLongStopped({ ...old, stoppedSince: undefined }, now)).toBe(false);
  expect(splitLongStopped([old, running], now)).toEqual([[running], [old]]);
});

test("sorts by each key in both directions, name breaking ties", () => {
  const list = [running, old, at(1000)];
  const names = (key: Parameters<typeof sortServices>[1], dir: Parameters<typeof sortServices>[2]) =>
    sortServices(list, key, dir).map((s) => s.ref.name);
  expect(names("service", "asc")).toEqual(["server", "old-build", "redis"]);
  expect(names("service", "desc")).toEqual(["redis", "old-build", "server"]);
  expect(names("status", "asc")).toEqual(["old-build", "redis", "server"]);
  expect(names("running", "desc")).toEqual(["server", "old-build", "redis"]);
  // Nothing stopped sorts last whichever way the column runs.
  expect(names("stopped", "asc")).toEqual(["old-build", "redis", "server"]);
  expect(names("stopped", "desc")).toEqual(["redis", "old-build", "server"]);
  expect(list.map((s) => s.ref.name)).toEqual(["server", "old-build", "redis"]);
});

test("groups by target in name order, keeping the list's order inside", () => {
  const groups = groupByTarget([...services, running]);
  expect(groups.map(([t, rows]) => [t, rows.length])).toEqual([
    ["build-host-1", 1],
    ["host-a", 1],
    ["prod-aws", 2],
  ]);
});
