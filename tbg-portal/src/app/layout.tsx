import type { Metadata } from "next";
import "./globals.css";

export const metadata: Metadata = {
  title: "TBG-CORE | Finacle Treasury & Stripe Institutional Terminal",
  description: "High-density institutional transaction banking workstation",
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="en">
      <body className="bg-[#05070c] antialiased overflow-hidden">{children}</body>
    </html>
  );
}
