import type { Metadata } from 'next';
import type { ReactNode } from 'react';
import '@/styles/global.css';

export const metadata: Metadata = {
  title: 'NEXUS',
  description: 'Delivery control plane.',
};

export default function RootLayout({ children }: { children: ReactNode }) {
  return (
    <html lang="en">
      <body>{children}</body>
    </html>
  );
}
