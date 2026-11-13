import { useEffect, useId, useRef, type ReactNode } from "react";
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
  const opener = useRef<Element | null>(null);
  const titleId = useId();
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) {
      opener.current = document.activeElement;
      d.showModal();
    }
    // Cancel and success unmount without close(), so the browser does not restore focus.
    return () => {
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);
  return (
    <dialog
      ref={ref}
      aria-labelledby={titleId}
      onClose={onClose}
      className={cn(
        "m-auto w-full max-w-md rounded-lg border bg-background p-6 text-foreground shadow-lg backdrop:bg-black/50",
        className,
      )}
    >
      <h2 id={titleId} className="mb-4 text-lg font-semibold">{title}</h2>
      {children}
    </dialog>
  );
}
