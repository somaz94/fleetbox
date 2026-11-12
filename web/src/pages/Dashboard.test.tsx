import { screen, within } from "@testing-library/react";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("summarises targets and services and lists what needs attention", async () => {
  renderAt("/");
  expect(await screen.findByText("1 stale or unreachable")).toBeInTheDocument();
  expect(screen.getByRole("link", { name: "2 healthy" })).toHaveAttribute("href", "/services?status=healthy");
  expect(screen.getByRole("link", { name: "1 degraded" })).toHaveAttribute("href", "/services?status=degraded");

  const attention = screen.getByText("Needs attention").closest("div")!.parentElement!;
  expect(within(attention).getByText("dial tcp 192.0.2.10:22: i/o timeout")).toBeInTheDocument();
  expect(within(attention).getByRole("link", { name: "prod-aws/web/api" })).toHaveAttribute("href", "/services/prod-aws/web/api");
  expect(within(attention).getByText("1 of 2 running")).toBeInTheDocument();

  const row = screen.getByRole("link", { name: "prod-aws" }).closest("tr")!;
  expect(within(row).getByText("read-only")).toBeInTheDocument();
  expect(within(row).getByText("ok")).toBeInTheDocument();
});

test("marks targets that never refreshed", async () => {
  server.use(
    http.get("/targets", ({ response }) =>
      response(200).json([
        {
          name: "a", provider: "ecs", readonly: false, stale: false, serviceCount: 0, instanceCount: 0,
          capabilities: { logs: false, followLogs: false, events: false, restart: false, scale: false },
        },
        {
          name: "b", provider: "ecs", readonly: false, stale: false, serviceCount: 0, instanceCount: 0, error: "no route",
          capabilities: { logs: false, followLogs: false, events: false, restart: false, scale: false },
        },
      ]),
    ),
    http.get("/services", ({ response }) => response(200).json([])),
  );
  renderAt("/");
  expect(await screen.findByText("waiting")).toBeInTheDocument();
  expect(screen.getAllByText("unreachable").length).toBeGreaterThan(0);
});

test("leaves out the attention card when nothing needs it", async () => {
  server.use(
    http.get("/targets", ({ response }) => response(200).json([])),
    http.get("/services", ({ response }) => response(200).json([])),
  );
  renderAt("/");
  expect(await screen.findByText("all current")).toBeInTheDocument();
  expect(screen.queryByText("Needs attention")).not.toBeInTheDocument();
});

test("reports an API error", async () => {
  server.use(http.get("/targets", ({ response }) => response(500).json({ error: { code: "internal", message: "internal error" } })));
  renderAt("/");
  expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
});
