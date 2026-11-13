import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { findings } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("lists open findings and shows their evidence on request", async () => {
  renderAt("/findings");
  const drift = (await screen.findByText("api runs different images in dev, prod")).closest("tr")!;
  expect(within(drift).getByText("drift")).toHaveAttribute("title", "digest");
  expect(within(drift).getByText("warning")).toBeInTheDocument();
  expect(within(drift).getByRole("link", { name: "dev-aws/web/api" })).toHaveAttribute("href", "/services/dev-aws/web/api");
  expect(screen.getByRole("columnheader", { name: "Open since" })).toBeInTheDocument();

  const stuck = screen.getByText("runs 1 of 2 desired").closest("tr")!;
  expect(within(stuck).queryByRole("button", { name: "Evidence" })).not.toBeInTheDocument();

  const user = userEvent.setup();
  const toggle = within(drift).getByRole("button", { name: "Evidence" });
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  const evidence = document.getElementById(toggle.getAttribute("aria-controls")!)!;
  expect(within(evidence).getByText("sha256:old")).toBeInTheDocument();
  expect(within(evidence).getByRole("link", { name: "prod-aws/web/api" })).toHaveAttribute("href", "/services/prod-aws/web/api");
  await user.click(toggle);
  expect(screen.queryByText("sha256:old")).not.toBeInTheDocument();

  const hygiene = screen.getByText("runs images tagged latest").closest("tr")!;
  await user.click(within(hygiene).getByRole("button", { name: "Evidence" }));
  expect(screen.getByText("example/runner:latest")).toBeInTheDocument();
});

test("filters through the URL and the API", async () => {
  const queries: string[] = [];
  server.use(
    http.get("/findings", ({ request, response }) => {
      queries.push(new URL(request.url).search);
      return response(200).json([]);
    }),
  );
  const { router } = renderAt("/findings?kind=bogus&severity=loud");
  expect(await screen.findByText("No open findings.")).toBeInTheDocument();
  expect(queries[0]).toBe(""); // unknown values are dropped, not sent

  const user = userEvent.setup();
  await user.selectOptions(screen.getByRole("combobox", { name: "Kind" }), "stuck");
  await user.selectOptions(screen.getByRole("combobox", { name: "Severity" }), "warning");
  await user.selectOptions(await screen.findByRole("combobox", { name: "Target" }), "prod-aws");
  expect(router.state.location.search).toContain("kind=stuck");
  expect(router.state.location.search).toContain("target=prod-aws");
  expect(queries.at(-1)).toBe("?kind=stuck&target=prod-aws&severity=warning");

  await user.selectOptions(screen.getByRole("combobox", { name: "State" }), "resolved");
  expect(await screen.findByText("Nothing resolved recently.")).toBeInTheDocument();
  expect(queries.at(-1)).toContain("state=resolved");
  await user.selectOptions(screen.getByRole("combobox", { name: "Kind" }), "");
  expect(router.state.location.search).not.toContain("kind=");
});

test("shows recently resolved findings with when they resolved", async () => {
  renderAt("/findings?state=resolved");
  expect(await screen.findByText("runs 0 of 2 desired")).toBeInTheDocument();
  expect(screen.getByRole("columnheader", { name: "Resolved" })).toBeInTheDocument();
});

test("reports an API error", async () => {
  server.use(http.get("/findings", ({ response }) => response(500).json({ error: { code: "internal", message: "internal error" } })));
  renderAt("/findings");
  expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
});

test("the dashboard counts open findings by kind", async () => {
  renderAt("/");
  const card = (await screen.findByRole("link", { name: "Open findings" })).closest("div")!.parentElement!;
  expect(await within(card).findByText("3")).toBeInTheDocument();
  expect(within(card).getByRole("link", { name: "1 drift" })).toHaveAttribute("href", "/findings?kind=drift");
  expect(within(card).queryByRole("link", { name: /why/ })).not.toBeInTheDocument();
});

test("the dashboard keeps working when findings cannot be read", async () => {
  server.use(http.get("/findings", ({ response }) => response(500).json({ error: { code: "internal", message: "internal error" } })));
  renderAt("/");
  expect(await screen.findByText("1 stale or unreachable")).toBeInTheDocument();
  const card = screen.getByRole("link", { name: "Open findings" }).closest("div")!.parentElement!;
  expect(await within(card).findByRole("alert")).toHaveTextContent("internal error");
});

test("a service lists its own open findings", async () => {
  // Same target, other service: only the component's own filter keeps it out.
  server.use(
    http.get("/findings", ({ response }) =>
      response(200).json([
        ...findings,
        { ...findings[1]!, id: 7, summary: "worker runs 0 of 1 desired", services: [{ target: "prod-aws", scope: "web", name: "worker" }] },
      ]),
    ),
  );
  renderAt("/services/prod-aws/web/api");
  // The nav also says Findings, so the card is found by its own link.
  const all = await screen.findByRole("link", { name: "All on prod-aws" });
  const card = all.closest("div")!.parentElement!;
  expect(within(card).getByText("api runs different images in dev, prod")).toBeInTheDocument();
  expect(within(card).getByText("runs 1 of 2 desired")).toBeInTheDocument();
  expect(within(card).queryByText("runs images tagged latest")).not.toBeInTheDocument();
  expect(within(card).queryByText("worker runs 0 of 1 desired")).not.toBeInTheDocument();
  expect(all).toHaveAttribute("href", "/findings?target=prod-aws");
});

test("a service without findings shows no findings card", async () => {
  server.use(http.get("/findings", ({ response }) => response(200).json([])));
  const { queryClient } = renderAt("/services/prod-aws/web/api");
  await screen.findByText("1 of 2");
  await waitFor(() => expect(queryClient.getQueryState(["findings", { target: "prod-aws" }])?.status).toBe("success"));
  expect(screen.queryByRole("link", { name: "All on prod-aws" })).not.toBeInTheDocument();
});

test("a service says when its findings cannot be read", async () => {
  server.use(http.get("/findings", ({ response }) => response(500).json({ error: { code: "internal", message: "internal error" } })));
  renderAt("/services/prod-aws/web/api");
  expect(await screen.findByRole("alert")).toHaveTextContent("Findings could not be read: internal error");
});
