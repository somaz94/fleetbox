import type { RouteObject } from "react-router";
import { Layout } from "@/components/Layout";
import { Dashboard } from "@/pages/Dashboard";
import { Logs } from "@/pages/Logs";
import { NotFound, RouteError } from "@/pages/NotFound";
import { ServiceDetail } from "@/pages/ServiceDetail";
import { Services } from "@/pages/Services";

export const routes: RouteObject[] = [
  {
    path: "/",
    element: <Layout />,
    errorElement: <RouteError />,
    children: [
      { index: true, element: <Dashboard /> },
      { path: "services", element: <Services /> },
      { path: "services/:target/:scope/:service", element: <ServiceDetail /> },
      { path: "services/:target/:scope/:service/instances/:instance/logs", element: <Logs /> },
      { path: "*", element: <NotFound /> },
    ],
  },
];
