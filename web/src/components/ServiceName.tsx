import { Badge } from "@/components/ui/badge";
import type { Schemas } from "@/lib/api";
import { isStandalone } from "@/pages/links";

// ServiceName shows a ref as people read it: the path in muted text, the name
// in full, and a badge in place of Docker's `_standalone` scope.
export function ServiceName({ serviceRef }: { serviceRef: Schemas["ServiceRef"] }) {
  const standalone = isStandalone(serviceRef);
  return (
    <>
      <span className="text-muted-foreground">
        {serviceRef.target}/{standalone ? "" : `${serviceRef.scope}/`}
      </span>
      {serviceRef.name}
      {standalone && " "}
      {standalone && (
        <Badge className="ml-2" title="A container outside compose">
          standalone
        </Badge>
      )}
    </>
  );
}
