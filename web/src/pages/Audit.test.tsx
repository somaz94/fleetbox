import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { audit } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("lists operations with their result and links to the service", async () => {
  renderAt("/audit");
  const user = await screen.findByTitle("sub-alice");
  expect(user).toHaveTextContent("alice");
  const row = user.closest("tr")!;
  expect(within(row).getByText('{"desired":3}')).toBeInTheDocument();
  expect(within(row).getByText("ok")).toBeInTheDocument();
  expect(within(row).getByRole("link", { name: "prod-aws/web/api" })).toHaveAttribute("href", "/services/prod-aws/web/api");
  expect(screen.getByText(/needs the operator role/)).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
});

test("filters by target and pages through older entries", async () => {
  const queries: string[] = [];
  server.use(
    http.get("/audit", ({ request, response }) => {
      const q = new URL(request.url).searchParams;
      queries.push(q.toString());
      return q.get("before")
        ? response(200).json({ entries: [{ ...audit[0]!, id: 1, user: "bob" }] })
        : response(200).json({ entries: audit, next: 2 });
    }),
  );
  const { router } = renderAt("/audit");
  const user = userEvent.setup();
  await screen.findByTitle("sub-alice");
  await user.selectOptions(await screen.findByRole("combobox", { name: "Target" }), "prod-aws");
  expect(router.state.location.search).toBe("?target=prod-aws");
  await user.click(await screen.findByRole("button", { name: "Load more" }));
  expect(await screen.findByText("bob")).toBeInTheDocument();
  expect(queries).toEqual(["limit=50", "target=prod-aws&limit=50", "target=prod-aws&before=2&limit=50"]);
});

test("says when nothing is recorded", async () => {
  server.use(http.get("/audit", ({ response }) => response(200).json({ entries: [] })));
  renderAt("/audit");
  expect(await screen.findByText("No operations recorded.")).toBeInTheDocument();
});

test("shows a failed read", async () => {
  server.use(http.get("/audit", ({ response }) => response(500).json({ error: { code: "internal", message: "internal error" } })));
  renderAt("/audit");
  expect(await screen.findByRole("alert")).toHaveTextContent("internal error");
});

test("the audit log is in the navigation", async () => {
  renderAt("/");
  expect(await screen.findByRole("link", { name: "Audit" })).toHaveAttribute("href", "/audit");
});
