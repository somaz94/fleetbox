import { http as untyped, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createOpenApiHttp } from "openapi-msw";
import type { paths } from "@/api/schema";
import { detail, me, services, targets } from "@/test/fixtures";

export const http = createOpenApiHttp<paths>({ baseUrl: `${window.location.origin}/api/v1` });

export const handlers = [
  http.get("/me", ({ response }) => response(200).json(me)),
  http.get("/targets", ({ response }) => response(200).json(targets)),
  http.get("/services", ({ response }) => response(200).json(services)),
  http.get("/services/{target}/{scope}/{service}", ({ response }) => response(200).json(detail)),
  http.get("/services/{target}/{scope}/{service}/events", ({ response }) =>
    response(200).json([{ time: "2026-01-01T00:01:00Z", severity: "warning", message: "service api has reached a steady state" }]),
  ),
  untyped.post(`${window.location.origin}/auth/logout`, () => HttpResponse.json({ redirect: "https://sso.example.com/logout" })),
];

export const server = setupServer(...handlers);
