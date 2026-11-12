import { act, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { FakeEventSource } from "@/test/eventsource";
import { renderAt } from "@/test/render";

const base = "/services/build-host-1/ci/runner/instances/0123456789abcdef0123/logs";

test("streams lines as text and follows when the target can", async () => {
  renderAt(base);
  await screen.findByRole("checkbox", { name: "Follow" });
  const source = FakeEventSource.latest();
  expect(source.url).toBe("/api/v1/services/build-host-1/ci/runner/instances/0123456789abcdef0123/logs?tail=500&follow=true");
  act(() => {
    source.open();
    source.emit("line", { text: "<img src=x onerror=alert(1)>", stream: "stderr" });
    source.emit("line", { text: "plain", time: "2026-01-01T00:00:00Z" });
  });
  expect(screen.getByText("<img src=x onerror=alert(1)>")).toBeInTheDocument();
  expect(screen.getByLabelText("Log output").querySelector("img")).toBeNull();
  expect(screen.getByText("open")).toBeInTheDocument();

  const user = userEvent.setup();
  await user.click(screen.getByRole("checkbox", { name: "Follow" }));
  expect(source.closed).toBe(true);
  expect(FakeEventSource.latest().url).toContain("follow=false");
});

test("a non-follow stream ends, and the tail and container choices reach the URL", async () => {
  // prod-aws cannot follow; the instance has two containers.
  renderAt("/services/prod-aws/web/api/instances/0123456789abcdef0123/logs?container=app");
  const user = userEvent.setup();
  await user.selectOptions(await screen.findByRole("combobox", { name: "Container" }), "sidecar");
  await user.selectOptions(screen.getByRole("combobox", { name: "Lines" }), "100");
  const source = FakeEventSource.latest();
  expect(source.url).toBe("/api/v1/services/prod-aws/web/api/instances/0123456789abcdef0123/logs?container=sidecar&tail=100&follow=false");
  expect(screen.queryByRole("checkbox", { name: "Follow" })).not.toBeInTheDocument();
  act(() => source.emit("end", ""));
  expect(source.closed).toBe(true);
  expect(screen.getByText("ended")).toBeInTheDocument();
  expect(screen.getByText("No output.")).toBeInTheDocument();
});

test("a server error event shows its message and allows a reconnect", async () => {
  renderAt(base);
  await screen.findByRole("checkbox", { name: "Follow" });
  const first = FakeEventSource.latest();
  act(() => first.emit("error", { error: { code: "unreachable", message: "the host closed the stream" } }));
  expect(screen.getByRole("alert")).toHaveTextContent("the host closed the stream");
  expect(first.closed).toBe(true);
  await userEvent.setup().click(screen.getByRole("button", { name: "Reconnect" }));
  expect(FakeEventSource.latest()).not.toBe(first);
  expect(screen.queryByRole("alert")).not.toBeInTheDocument();
});

test("a dropped connection and a non-JSON error both fail the stream", async () => {
  renderAt(base);
  await screen.findByRole("checkbox", { name: "Follow" });
  act(() => FakeEventSource.latest().drop());
  expect(screen.getByRole("alert")).toHaveTextContent("the log stream was interrupted");
  await userEvent.setup().click(screen.getByRole("button", { name: "Reconnect" }));
  act(() => FakeEventSource.latest().emit("error", "plain failure"));
  expect(screen.getByRole("alert")).toHaveTextContent("plain failure");
});
