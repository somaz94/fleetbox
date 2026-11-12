import { QueryClientProvider } from "@tanstack/react-query";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { createBrowserRouter, RouterProvider } from "react-router";
import "@/index.css";
import { newQueryClient } from "@/lib/queryClient";
import { routes } from "@/router";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={newQueryClient()}>
      <RouterProvider router={createBrowserRouter(routes)} />
    </QueryClientProvider>
  </StrictMode>,
);
