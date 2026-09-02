import type { Metadata } from "next";
import "./globals.css";
import { ActorPicker } from "./actor-picker";

export const metadata: Metadata = {
  title: "BEDA Enquiry Review",
  description: "Approval queue for classified inbound enquiries",
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en">
      <body>
        <header className="top">
          <div>
            <h1>BEDA Enquiry Review</h1>
            <div className="sub">
              Nothing is sent to a customer without an approval on this screen.
            </div>
          </div>
          <ActorPicker />
        </header>
        <main>{children}</main>
      </body>
    </html>
  );
}
