import { act, renderHook } from "@testing-library/react";
import { logsURL, MAX_LINES, useLogStream } from "@/hooks/useLogStream";
import { FakeEventSource } from "@/test/eventsource";

test("logsURL escapes every segment", () => {
  expect(logsURL({ target: "a b", scope: "c/d", service: "e" }, "i?d", { follow: false })).toBe(
    "/api/v1/services/a%20b/c%2Fd/e/instances/i%3Fd/logs?follow=false",
  );
});

test("keeps at most MAX_LINES, dropping the oldest", () => {
  const { result, unmount } = renderHook(() => useLogStream("/logs"));
  const source = FakeEventSource.latest();
  act(() => {
    for (let i = 0; i < MAX_LINES + 3; i++) source.emit("line", { text: String(i) });
  });
  expect(result.current.lines).toHaveLength(MAX_LINES);
  expect(result.current.lines[0]?.text).toBe("3");
  unmount();
  expect(source.closed).toBe(true);
});
