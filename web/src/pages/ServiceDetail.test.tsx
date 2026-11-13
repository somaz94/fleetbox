import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { detail, dockerDetail, targets } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("shows the service, its instances, stops and events", async () => {
  renderAt("/services/prod-aws/web/api");
  const row = (await screen.findByTitle("0123456789abcdef0123")).closest("tr")!;
  expect(within(row).getByText("running, healthy")).toBeInTheDocument();
  expect(within(row).getByText(/example\/api:1.2 sha256:0123456789ab/)).toBeInTheDocument();
  expect(within(row).getByText("exit 137")).toBeInTheDocument();
  expect(within(row).getByRole("link", { name: "Logs" })).toHaveAttribute(
    "href",
    "/services/prod-aws/web/api/instances/0123456789abcdef0123/logs",
  );
  expect(screen.getByText("1 of 2")).toBeInTheDocument();
  expect(screen.getByText("fleetbox.io/app=api")).toBeInTheDocument();
  expect(screen.getByText("OutOfMemoryError")).toBeInTheDocument();
  expect(await screen.findByText("service api has reached a steady state")).toBeInTheDocument();
});

test("refresh reads the service fresh and shows a failure", async () => {
  let refreshed = 0;
  server.use(
    http.post("/services/{target}/{scope}/{service}/refresh", ({ response }) => {
      refreshed++;
      return refreshed === 1
        ? response(200).json({ ...detail, stops: [] })
        : response(502).json({ error: { code: "unreachable", message: "the host did not answer" } });
    }),
  );
  renderAt("/services/prod-aws/web/api");
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Refresh" }));
  expect(await screen.findByText("No stops recorded.")).toBeInTheDocument();
  await user.click(screen.getByRole("button", { name: "Refresh" }));
  expect(await screen.findByText("Refresh failed: the host did not answer")).toBeInTheDocument();
});

test("a provider without events says so, and logs are hidden without the capability", async () => {
  server.use(
    http.get("/services/{target}/{scope}/{service}/events", ({ response }) =>
      response(501).json({ error: { code: "unsupported", message: "no events" } }),
    ),
    http.get("/targets", ({ response }) =>
      response(200).json(targets.map((t) => ({ ...t, capabilities: { ...t.capabilities, logs: false } }))),
    ),
    http.get("/services/{target}/{scope}/{service}", ({ response }) =>
      response(200).json({
        ...detail,
        service: { ...detail.service, stale: true, revision: undefined, rollout: undefined, labels: undefined, desired: undefined },
        instances: [],
        stops: [],
      }),
    ),
  );
  renderAt("/services/prod-aws/web/api");
  expect(await screen.findByText("This provider has no event timeline.")).toBeInTheDocument();
  expect(await screen.findByText("No instances.")).toBeInTheDocument();
  expect(screen.getByText(/last refresh failed/)).toBeInTheDocument();
  expect(screen.queryByRole("link", { name: "Logs" })).not.toBeInTheDocument();
});

test("an empty event timeline and a reload", async () => {
  let calls = 0;
  server.use(
    http.get("/services/{target}/{scope}/{service}/events", ({ response }) => {
      calls++;
      return response(200).json([]);
    }),
  );
  renderAt("/services/prod-aws/web/api");
  expect(await screen.findByText("No events.")).toBeInTheDocument();
  await userEvent.setup().click(screen.getByRole("button", { name: "Reload" }));
  await screen.findByText("No events.");
  expect(calls).toBeGreaterThanOrEqual(2);
});

test("a hidden or unknown service reads as not found", async () => {
  server.use(
    http.get("/services/{target}/{scope}/{service}", ({ response }) =>
      response(404).json({ error: { code: "not_found", message: 'target "secret": not found' } }),
    ),
    http.get("/services/{target}/{scope}/{service}/events", ({ response }) =>
      response(404).json({ error: { code: "not_found", message: 'target "secret": not found' } }),
    ),
  );
  renderAt("/services/secret/web/api");
  const alerts = await screen.findAllByRole("alert");
  expect(alerts[0]).toHaveTextContent("Not found, or you hold no role on its target.");
});

test("folds each instance's details and lists container facts", async () => {
  renderAt("/services/prod-aws/web/api");
  const row = (await screen.findByTitle("0123456789abcdef0123")).closest("tr")!;
  expect(within(row).getByTitle("Ports (host→container)")).toHaveTextContent("80→8080/tcp, 9090");
  expect(within(row).getByText(/no health check/)).toBeInTheDocument();
  expect(within(row).getByText(/image built/)).toBeInTheDocument();

  const details = row.nextElementSibling as HTMLElement;
  expect(within(details).getByText("Details")).toBeInTheDocument();
  for (const text of ["0.25 vCPU", "512 MiB", "FARGATE", "ap-northeast-2a", "192.0.2.20"]) {
    expect(within(details).getByText(text)).toBeInTheDocument();
  }
  expect(within(details).queryByLabelText("Mounts")).not.toBeInTheDocument();
});

test("shows a stopped standalone container with its mounts and addresses", async () => {
  server.use(http.get("/services/{target}/{scope}/{service}", ({ response }) => response(200).json(dockerDetail)));
  renderAt("/services/host-b/_standalone/redis");
  const heading = await screen.findByRole("heading", { level: 1 });
  expect(heading).toHaveTextContent("host-b/redis standalone");
  expect(heading.textContent).not.toContain("_standalone");
  expect(await screen.findByText("Stopped")).toBeInTheDocument();

  const row = screen.getByTitle("aaaaaaaaaaaa").closest("tr")!;
  expect(within(row).getByText((_, el) => el?.tagName === "P" && /^stopped .*ago$/.test(el.textContent ?? ""))).toBeInTheDocument();
  const details = row.nextElementSibling as HTMLElement;
  expect(within(details).getByText("unless-stopped")).toBeInTheDocument();
  expect(within(details).getByText("IP (lan)")).toBeInTheDocument();
  const mounts = within(details).getByLabelText("Mounts");
  expect(within(mounts).getByText(/\/data\/redis → \/data/)).toBeInTheDocument();
  expect(within(mounts).getByText(/\? → \/conf \(ro\)/)).toBeInTheDocument();

  // An instance with nothing beyond its status gets no details row.
  const bare = screen.getByTitle("bbbbbbbbbbbb").closest("tr")!;
  expect(bare.nextElementSibling).toBeNull();
});
