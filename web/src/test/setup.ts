import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { navigation } from "@/lib/api";
import { FakeEventSource } from "@/test/eventsource";
import { server } from "@/test/server";

Object.defineProperty(globalThis, "EventSource", { value: FakeEventSource, writable: true });

// jsdom has <dialog> but neither showModal nor close.
HTMLDialogElement.prototype.showModal ??= function (this: HTMLDialogElement) {
  this.open = true;
};
HTMLDialogElement.prototype.close ??= function (this: HTMLDialogElement) {
  this.open = false;
  this.dispatchEvent(new Event("close"));
};

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
