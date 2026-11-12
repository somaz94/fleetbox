import { Link, useRouteError } from "react-router";

export function NotFound() {
  return (
    <div className="flex flex-col gap-2">
      <h1 className="text-xl font-semibold">Page not found</h1>
      <Link to="/" className="text-sm hover:underline">
        Back to the dashboard
      </Link>
    </div>
  );
}

export function RouteError() {
  const error = useRouteError();
  return (
    <div role="alert" className="mx-auto max-w-3xl p-6">
      <h1 className="text-xl font-semibold">Something went wrong</h1>
      <p className="mt-2 text-sm text-muted-foreground">{error instanceof Error ? error.message : String(error)}</p>
    </div>
  );
}
