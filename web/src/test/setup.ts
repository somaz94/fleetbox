import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { navigation } from "@/lib/api";
import { FakeEventSource } from "@/test/eventsource";
import { server } from "@/test/server";

Object.defineProperty(globalThis, "EventSource", { value: FakeEventSource, writable: true });

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
beforeEach(() => {
  navigation.assign = vi.fn();
  FakeEventSource.instances = [];
});
afterEach(() => {
  cleanup();
  server.resetHandlers();
});
afterAll(() => server.close());
