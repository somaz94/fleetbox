import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { dockerServices, services } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("lists services and passes the URL filters to the API", async () => {
  const queries: string[] = [];
  server.use(
    http.get("/services", ({ request, response }) => {
      queries.push(new URL(request.url).search);
      return response(200).json(services);
    }),
  );
  const { router } = renderAt("/services?status=degraded&q=api");
  const link = await screen.findByRole("link", { name: /prod-aws\/web\/api/ });
  expect(link).toHaveAttribute("href", "/services/prod-aws/web/api");
  expect(within(link.closest("tr")!).getByText("/ 2")).toBeInTheDocument();
  expect(within(screen.getByRole("link", { name: /build-host-1\/ci\/runner/ }).closest("tr")!).getByText("stale")).toBeInTheDocument();
  expect(queries[0]).toBe("?status=degraded&q=api");

  const user = userEvent.setup();
  await user.selectOptions(await screen.findByRole("combobox", { name: "Target" }), "prod-aws");
  await waitFor(() => expect(router.state.location.search).toBe("?status=degraded&q=api&target=prod-aws"));
  await user.selectOptions(screen.getByRole("combobox", { name: "Status" }), "");
  await waitFor(() => expect(router.state.location.search).toBe("?q=api&target=prod-aws"));
  await user.selectOptions(screen.getByRole("combobox", { name: "Provider" }), "ecs");
  await user.clear(screen.getByRole("textbox", { name: "Search" }));
  await waitFor(() => expect(router.state.location.search).toBe("?target=prod-aws&provider=ecs"));
  await waitFor(() => expect(queries.at(-1)).toBe("?target=prod-aws&provider=ecs"));
});

test("ignores an unknown status in the URL", async () => {
  const queries: string[] = [];
  server.use(
    http.get("/services", ({ request, response }) => {
      queries.push(new URL(request.url).search);
      return response(200).json([]);
    }),
  );
  renderAt("/services?status=bogus");
  expect(await screen.findByText("No services match.")).toBeInTheDocument();
  expect(queries[0]).toBe("");
});

test("folds services stopped for more than seven days and shows standalone containers by name", async () => {
  server.use(http.get("/services", ({ response }) => response(200).json(dockerServices)));
  renderAt("/services");
  const redis = await screen.findByRole("link", { name: /host-b\/redis/ });
  expect(redis).toHaveAttribute("href", "/services/host-b/_standalone/redis");
  expect(within(redis).getByText("standalone")).toBeInTheDocument();
  expect(redis.textContent).not.toContain("_standalone");

  const folded = screen.getByText("Stopped for more than 7 days (1)").closest("details")!;
  expect(folded).not.toHaveAttribute("open");
  expect(within(folded).getByRole("link", { name: /host-b\/old-build/ })).toBeInTheDocument();
  expect(within(folded).queryByRole("link", { name: /redis/ })).not.toBeInTheDocument();
});

test("sorts by a column and groups by target through the URL", async () => {
  server.use(http.get("/services", ({ response }) => response(200).json(dockerServices)));
  const { router } = renderAt("/services");
  const user = userEvent.setup();
  const current = (await screen.findByRole("link", { name: /host-b\/redis/ })).closest("table")!;
  const names = () => within(current).getAllByRole("link").map((a) => a.textContent);
  expect(names()).toEqual(["host-a/git/server", "host-b/redis standalone"]);

  await user.click(within(current).getByRole("button", { name: "Running" }));
  await waitFor(() => expect(router.state.location.search).toBe("?sort=running"));
  expect(within(current).getByRole("columnheader", { name: /Running/ })).toHaveAttribute("aria-sort", "ascending");
  expect(names()).toEqual(["host-b/redis standalone", "host-a/git/server"]);

  await user.click(within(current).getByRole("button", { name: /Running/ }));
  await waitFor(() => expect(router.state.location.search).toBe("?sort=running&dir=desc"));
  await user.click(within(current).getByRole("button", { name: "Service" }));
  await waitFor(() => expect(router.state.location.search).toBe(""));

  await user.click(screen.getByRole("checkbox", { name: "Group by target" }));
  await waitFor(() => expect(router.state.location.search).toBe("?group=target"));
  // Each table groups on its own: the folded one has a host-b group too.
  expect(await within(current).findByRole("rowheader", { name: "host-b (1)" })).toBeInTheDocument();
  expect(within(current).getByRole("rowheader", { name: "host-a (1)" })).toBeInTheDocument();
  await user.click(screen.getByRole("checkbox", { name: "Group by target" }));
  await waitFor(() => expect(router.state.location.search).toBe(""));
});
