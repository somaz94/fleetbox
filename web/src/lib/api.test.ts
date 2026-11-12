import { http as untyped, HttpResponse } from "msw";
import { ApiError, client, CSRF_HEADER, loginURL, logout, navigation, unwrap } from "@/lib/api";
import { newQueryClient } from "@/lib/queryClient";
import { detail } from "@/test/fixtures";
import { http, server } from "@/test/server";

const path = { target: "prod-aws", scope: "web", service: "api" };

test("a POST carries the CSRF header and a GET does not", async () => {
  const seen: Record<string, string | null> = {};
  server.use(
    http.post("/services/{target}/{scope}/{service}/refresh", ({ request, response }) => {
      seen.post = request.headers.get(CSRF_HEADER);
      return response(200).json(detail);
    }),
    http.get("/targets", ({ request, response }) => {
      seen.get = request.headers.get(CSRF_HEADER);
      return response(200).json([]);
    }),
  );
  await unwrap(client.POST("/services/{target}/{scope}/{service}/refresh", { params: { path } }));
  await unwrap(client.GET("/targets"));
  expect(seen).toEqual({ post: "1", get: null });
});

test("a 401 sends the browser to the login with the current path", async () => {
  window.history.pushState({}, "", "/services?q=api");
  server.use(http.get("/me", ({ response }) => response(401).json({ error: { code: "unauthorized", message: "authentication required" } })));
  await expect(unwrap(client.GET("/me"))).rejects.toMatchObject({ status: 401, code: "unauthorized" });
  expect(navigation.assign).toHaveBeenCalledWith("/auth/login?next=%2Fservices%3Fq%3Dapi");
  window.history.pushState({}, "", "/");
});

test("an error body becomes an ApiError with its code", async () => {
  server.use(
    http.get("/services/{target}/{scope}/{service}", ({ response }) =>
      response(404).json({ error: { code: "not_found", message: 'target "x": not found' } }),
    ),
  );
  const err = await unwrap(client.GET("/services/{target}/{scope}/{service}", { params: { path } })).catch((e: unknown) => e);
  expect(err).toBeInstanceOf(ApiError);
  expect(err).toMatchObject({ status: 404, code: "not_found", message: 'target "x": not found' });
});

test("a response without an Error body still becomes an ApiError", async () => {
  server.use(untyped.get(`${window.location.origin}/api/v1/info`, () => new HttpResponse("bad gateway", { status: 502 })));
  await expect(unwrap(client.GET("/info"))).rejects.toMatchObject({ status: 502, code: "internal" });
});

test("loginURL keeps the path and query", () => {
  expect(loginURL({ pathname: "/services/a/b/c", search: "?x=1" })).toBe("/auth/login?next=%2Fservices%2Fa%2Fb%2Fc%3Fx%3D1");
});

test("logout follows the redirect the server returns", async () => {
  let csrf: string | null = null;
  server.use(
    untyped.post(`${window.location.origin}/auth/logout`, ({ request }) => {
      csrf = request.headers.get(CSRF_HEADER);
      return HttpResponse.json({ redirect: "https://sso.example.com/logout?x=1" });
    }),
  );
  await logout();
  expect(csrf).toBe("1");
  expect(navigation.assign).toHaveBeenCalledWith("https://sso.example.com/logout?x=1");

  server.use(untyped.post(`${window.location.origin}/auth/logout`, () => new HttpResponse("forbidden", { status: 403 })));
  await expect(logout()).rejects.toBeInstanceOf(ApiError);
});

test("queries retry server errors but not client errors", () => {
  const retry = newQueryClient().getDefaultOptions().queries?.retry as (n: number, e: unknown) => boolean;
  expect(retry(0, new ApiError(404, "not_found", ""))).toBe(false);
  expect(retry(0, new ApiError(502, "unreachable", ""))).toBe(true);
  expect(retry(0, new TypeError("network"))).toBe(true);
  expect(retry(2, new TypeError("network"))).toBe(false);
});
