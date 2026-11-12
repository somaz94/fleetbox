import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { services } from "@/test/fixtures";
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
