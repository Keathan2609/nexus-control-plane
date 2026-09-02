import { redirect } from 'next/navigation';

// The console has no marketing surface. The root is the overview.
export default function Index() {
  redirect('/console');
}
