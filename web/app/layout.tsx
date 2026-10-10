import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "LocalMesh",
  description: "Your Hardware. One AI Network.",
};

export default function RootLayout({
  children,
}: Readonly<{ children: React.ReactNode }>) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
