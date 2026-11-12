import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http as untyped, HttpResponse } from "msw";
import { navigation } from "@/lib/api";
import { me } from "@/test/fixtures";
import { renderAt } from "@/test/render";
import { http, server } from "@/test/server";

test("shows the user and signs out through the issuer", async () => {
  renderAt("/");
  expect(await screen.findByText("alice")).toHaveAttribute("title", "platform");
  await userEvent.setup().click(screen.getByRole("button", { name: "Sign out" }));
  expect(navigation.assign).toHaveBeenCalledWith("https://sso.example.com/logout");
});

test("a failed sign-out is reported", async () => {
  server.use(untyped.post(`${window.location.origin}/auth/logout`, () => new HttpResponse(null, { status: 500 })));
  renderAt("/");
  await userEvent.setup().click(await screen.findByRole("button", { name: "Sign out" }));
  expect(await screen.findByText("sign-out failed")).toBeInTheDocument();
});

test("no sign-out without a session", async () => {
  server.use(http.get("/me", ({ response }) => response(200).json({ ...me, user: "anonymous", groups: [], mode: "none" })));
  renderAt("/");
  expect(await screen.findByText("anonymous")).toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "Sign out" })).not.toBeInTheDocument();
});

test("an unknown path shows the not-found page", async () => {
  renderAt("/nowhere");
  expect(await screen.findByText("Page not found")).toBeInTheDocument();
});
