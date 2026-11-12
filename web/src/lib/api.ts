import createClient, { type Middleware } from "openapi-fetch";
import type { components, paths } from "@/api/schema";

export type Schemas = components["schemas"];

// The server refuses a state-changing request without it (docs/DESIGN.md, CSRF).
export const CSRF_HEADER = "X-Fleetbox-CSRF";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

export function loginURL(location: Pick<Location, "pathname" | "search"> = window.location): string {
  return `/auth/login?next=${encodeURIComponent(location.pathname + location.search)}`;
}

// Replaced in tests, where jsdom cannot navigate.
export const navigation = {
  assign: (url: string) => window.location.assign(url),
};

const middleware: Middleware = {
  onRequest({ request }) {
    if (request.method !== "GET" && request.method !== "HEAD") {
      request.headers.set(CSRF_HEADER, "1");
    }
    return request;
  },
  onResponse({ response }) {
    if (response.status === 401) {
      navigation.assign(loginURL());
    }
    return response;
  },
};

// An absolute base, because a relative Request URL is invalid outside a browser.
// fetch is looked up per call rather than captured, so a test's mock applies.
export const client = createClient<paths>({
  baseUrl: `${window.location.origin}/api/v1`,
  fetch: (request) => globalThis.fetch(request),
});
client.use(middleware);

type Result<T> = { data?: T; error?: Schemas["Error"]; response: Response };

// A proxy in front of the server may answer with a body that is not an Error.
export async function unwrap<T>(pending: Promise<Result<T>>): Promise<T> {
  const { data, error, response } = await pending;
  if (error !== undefined || data === undefined) {
    const e = typeof error === "object" && error !== null && "error" in error ? error.error : undefined;
    throw new ApiError(response.status, e?.code ?? "internal", e?.message ?? `HTTP ${response.status}`);
  }
  return data;
}

// logout ends the server session and follows the issuer's sign-out, if any.
export async function logout(): Promise<void> {
  const response = await fetch("/auth/logout", { method: "POST", headers: { [CSRF_HEADER]: "1" } });
  if (!response.ok) {
    throw new ApiError(response.status, "internal", "sign-out failed");
  }
  const { redirect } = (await response.json()) as { redirect: string };
  navigation.assign(redirect);
}
