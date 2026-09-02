/** @type {import('next').NextConfig} */
const nextConfig = {
  reactStrictMode: true,
  poweredByHeader: false,
  // The console is a browser client for the NEXUS API. It holds no data of its own and
  // proxies nothing: every request goes straight to the control plane, so there is one
  // place where authorisation is enforced rather than two.
  env: {
    NEXT_PUBLIC_API_URL: process.env.NEXT_PUBLIC_API_URL ?? 'http://localhost:8080',
  },
};

export default nextConfig;
