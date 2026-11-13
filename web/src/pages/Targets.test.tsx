import { screen, within } from "@testing-library/react";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("shows each target's state, endpoint and capabilities", async () => {
  renderAt("/targets");
  const docker = await screen.findByRole("group", { name: "build-host-1" });
  expect(within(docker).getByText("dial tcp 192.0.2.10:22: i/o timeout")).toBeInTheDocument();
  expect(within(docker).getByText("192.0.2.10:22")).toBeInTheDocument();
  expect(within(docker).getByText("stale")).toBeInTheDocument();
  expect(within(docker).getByRole("link", { name: "1" })).toHaveAttribute("href", "/services?target=build-host-1");

  const ecs = screen.getByRole("group", { name: "prod-aws" });
  expect(within(ecs).getByText("ap-northeast-2")).toBeInTheDocument();
  expect(within(ecs).getByText("read-only")).toBeInTheDocument();
  expect(within(ecs).queryByText(/timeout/)).not.toBeInTheDocument();
  const caps = within(ecs).getByRole("group", { name: "Capabilities" });
  expect(within(caps).getByText("scale")).toHaveAttribute("title", "supported");
  expect(within(caps).getByTitle("not supported")).toHaveTextContent("followLogs");
  expect(screen.getByRole("link", { name: "Targets" })).toHaveAttribute("href", "/targets");
});

test("says so when no target is visible", async () => {
  server.use(http.get("/targets", ({ response }) => response(200).json([])));
  renderAt("/targets");
  expect(await screen.findByText("No targets you can see.")).toBeInTheDocument();
});
