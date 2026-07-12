import type { Metadata } from "next";
import "./globals.css";
import { AppProvider } from "@/lib/store";
import { Header } from "@/components/Header";

export const metadata: Metadata = {
  title: "chaos-ui · conductor",
  description: "Administration and chaos-testing console for the conductor engine",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <AppProvider>
          <Header />
          <main className="mx-auto max-w-[1400px] px-5 py-6">{children}</main>
        </AppProvider>
      </body>
    </html>
  );
}
