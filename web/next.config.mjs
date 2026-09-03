/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // Standalone output traces the files the server actually needs, so the runtime image
  // is close to the size of the application rather than of node_modules.
  output: 'standalone',
  // The console is a browser client for the NEXUS API. It holds no data of its own and
  // proxies nothing: every request goes straight to the control plane, so there is one
  // place where authorisation is enforced rather than two.
  env: {
    NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080',
  },
};

export default nextConfig;
