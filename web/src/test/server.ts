import { http as untyped, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createOpenApiHttp } from "openapi-msw";
import type { paths } from "@/api/schema";
import { audit, detail, findings, me, resolvedFindings, services, targets } from "@/test/fixtures";

export const http = createOpenApiHttp<paths>({ baseUrl: `${window.location.origin}/api/v1` });

export const handlers = [
  http.get("/me", ({ response }) => response(200).json(me)),
  http.get("/targets", ({ response }) => response(200).json(targets)),
  http.get("/services", ({ response }) => response(200).json(services)),
  http.get("/services/{target}/{scope}/{service}", ({ response }) => response(200).json(detail)),
  http.get("/services/{target}/{scope}/{service}/events", ({ response }) =>
    response(200).json([{ time: "2026-01-01T00:01:00Z", severity: "warning", message: "service api has reached a steady state" }]),
  ),
  http.get("/audit", ({ response }) => response(200).json({ entries: audit })),
  http.get("/findings", ({ query, response }) => {
    const list = query.get("state") === "resolved" ? resolvedFindings : findings;
    const target = query.get("target");
    return response(200).json(target ? list.filter((f) => f.services.some((r) => r.target === target)) : list);
  }),
  untyped.post(`${window.location.origin}/auth/logout`, () => HttpResponse.json({ redirect: "https://sso.example.com/logout" })),
];

export const server = setupServer(...handlers);
