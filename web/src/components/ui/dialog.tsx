import { useEffect, useRef, type ReactNode } from "react";
import { cn } from "@/lib/utils";

// Dialog is a modal native <dialog>, mounted only while open: showModal runs
// once on mount, and Escape ends in onClose.
export function Dialog({
  title,
  onClose,
  className,
  children,
}: {
  title: string;
  onClose: () => void;
  className?: string;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
  }, []);
  return (
    <dialog
      ref={ref}
      aria-label={title}
      onClose={onClose}
      className={cn(
        "m-auto w-full max-w-md rounded-lg border bg-background p-6 text-foreground shadow-lg backdrop:bg-black/50",
        className,
      )}
    >
      <h2 className="mb-4 text-lg font-semibold">{title}</h2>
      {children}
    </dialog>
  );
}
