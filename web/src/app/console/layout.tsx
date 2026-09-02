import type { ReactNode } from 'react';
import { Shell } from './shell';

export default function ConsoleLayout({ children }: { children: ReactNode }) {
  return <Shell>{children}</Shell>;
}
