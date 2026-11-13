import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { detail, me, operable, viewer } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

const apiPath = "/services/prod-aws/web/api";

function serveOperableTargets() {
  server.use(http.get("/targets", ({ response }) => response(200).json(operable)));
}

test("restart runs only once the service name is typed", async () => {
  serveOperableTargets();
  const calls: string[] = [];
  server.use(
    http.post("/services/{target}/{scope}/{service}/restart", ({ params, response }) => {
      calls.push(`${params.target}/${params.scope}/${params.service}`);
      return response(200).json(detail);
    }),
  );
  renderAt(apiPath);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Restart" }));
  const dialog = screen.getByRole("dialog", { name: "Restart prod-aws/web/api" });
  const confirm = within(dialog).getByRole("button", { name: "Restart service" });
  expect(confirm).toBeDisabled();
  await user.type(within(dialog).getByRole("textbox"), "ap");
  expect(confirm).toBeDisabled();
  await user.type(within(dialog).getByRole("textbox"), "i");
  await user.click(confirm);
  expect(await screen.findByRole("status")).toHaveTextContent("Restart of prod-aws/web/api started.");
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(calls).toEqual(["prod-aws/web/api"]);
});

test("scale sends the desired count and keeps the dialog open on failure", async () => {
  serveOperableTargets();
  const bodies: unknown[] = [];
  server.use(
    http.post("/services/{target}/{scope}/{service}/scale", async ({ request, response }) => {
      bodies.push(await request.json());
      return bodies.length === 1
        ? response(403).json({ error: { code: "forbidden", message: 'target "prod-aws" is read-only' } })
        : response(200).json(detail);
    }),
  );
  renderAt(apiPath);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Scale" }));
  const dialog = screen.getByRole("dialog", { name: "Scale prod-aws/web/api" });
  const count = within(dialog).getByRole("spinbutton", { name: "Desired instances" });
  expect(count).toHaveValue(2);
  expect(count).toHaveAccessibleDescription("A whole number from 0 to 1000.");
  const confirm = within(dialog).getByRole("button", { name: "Scale service" });
  await user.type(within(dialog).getByRole("textbox"), "api");
  for (const bad of ["-1", "1001", "1.5"]) {
    await user.clear(count);
    await user.type(count, bad);
    expect(confirm).toBeDisabled();
  }
  await user.clear(count);
  await user.type(count, "5");
  await user.click(confirm);
  expect(await within(dialog).findByRole("alert")).toHaveTextContent('Scale failed: target "prod-aws" is read-only');
  await user.click(confirm);
  expect(await screen.findByRole("status")).toHaveTextContent("prod-aws/web/api scaled to 5.");
  expect(bodies).toEqual([{ desired: 5 }, { desired: 5 }]);
});

test("cancel closes the dialog without a request", async () => {
  serveOperableTargets();
  renderAt(apiPath);
  const user = userEvent.setup();
  await user.click(await screen.findByRole("button", { name: "Restart" }));
  await user.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Cancel" }));
  expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "Restart" })).toHaveFocus();
});

test("operations wait for the service to load", async () => {
  serveOperableTargets();
  let release = () => {};
  const loaded = new Promise<void>((resolve) => (release = resolve));
  server.use(
    http.get("/services/{target}/{scope}/{service}", async ({ response }) => {
      await loaded;
      return response(200).json(detail);
    }),
  );
  const { queryClient } = renderAt(apiPath);
  await waitFor(() => expect(queryClient.getQueryState(["targets"])?.status).toBe("success"));
  await screen.findByText(me.user);
  expect(screen.queryByRole("button", { name: "Scale" })).not.toBeInTheDocument();
  release();
  expect(await screen.findByRole("button", { name: "Scale" })).toBeInTheDocument();
});

test("operations are hidden on a read-only target", async () => {
  renderAt(apiPath);
  expect(await screen.findByRole("button", { name: "Refresh" })).toBeInTheDocument();
  await screen.findByText("1 of 2");
  expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Scale" })).not.toBeInTheDocument();
});

test("a viewer is offered no operations", async () => {
  serveOperableTargets();
  server.use(http.get("/me", ({ response }) => response(200).json(viewer)));
  renderAt(apiPath);
  await screen.findByText("1 of 2");
  await screen.findByText("vic");
  expect(screen.queryByRole("button", { name: "Restart" })).not.toBeInTheDocument();
});

test("a Docker service offers restart but not scale", async () => {
  renderAt("/services/build-host-1/ci/runner");
  expect(await screen.findByRole("button", { name: "Restart" })).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Scale" })).not.toBeInTheDocument();
});
