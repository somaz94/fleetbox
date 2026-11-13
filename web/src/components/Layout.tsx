import { useState } from "react";
import { Link, NavLink, Outlet } from "react-router";
import { Button } from "@/components/ui/button";
import { logout } from "@/lib/api";
import { useMe } from "@/lib/queries";
import { cn } from "@/lib/utils";

const navClass = ({ isActive }: { isActive: boolean }) =>
  cn("text-muted-foreground hover:text-foreground", isActive && "text-foreground font-medium");

export function Layout() {
  const me = useMe();
  const [signOutError, setSignOutError] = useState<string>();
  const signOut = () => logout().catch((e: Error) => setSignOutError(e.message));

  return (
    <div className="min-h-screen">
      <header className="border-b">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-6 px-4">
          <Link to="/" className="font-semibold">
            fleetbox
          </Link>
          <nav className="flex gap-4 text-sm">
            <NavLink to="/" end className={navClass}>
              Dashboard
            </NavLink>
            <NavLink to="/services" className={navClass}>
              Services
            </NavLink>
            <NavLink to="/findings" className={navClass}>
              Findings
            </NavLink>
            <NavLink to="/audit" className={navClass}>
              Audit
            </NavLink>
          </nav>
          <div className="ml-auto flex items-center gap-3 text-sm">
            {signOutError && <span className="text-destructive">{signOutError}</span>}
            {me.data && (
              <span title={me.data.groups.join(", ")} className="text-muted-foreground">
                {me.data.user}
              </span>
            )}
            {me.data?.mode === "oidc" && (
              <Button variant="outline" size="sm" onClick={signOut}>
                Sign out
              </Button>
            )}
          </div>
        </div>
      </header>
      <main className="mx-auto max-w-7xl px-4 py-6">
        <Outlet />
      </main>
    </div>
  );
}
