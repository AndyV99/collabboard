import type { Metadata } from "next";
import type { ReactNode } from "react";

import "./globals.css";

export const metadata: Metadata = {
  title: "CollabBoard",
  description:
    "Multi-tenant, real-time Kanban tool. App shell with an API health check.",
};

// Typed explicitly rather than with Next's generated `LayoutProps<"/">` global,
// so `tsc --noEmit` works on a clean checkout without a build having run first.
export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
