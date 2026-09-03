/**
 * A second workload, in a different runtime.
 *
 * It exists so NEXUS is demonstrably not hard-wired to one language: the same pipeline
 * definition, the same build and deploy path, a different detected runtime. Failure
 * injection matches the Go service so both can drive the same demonstration.
 *
 *   FAIL_HEALTHCHECK=true   /healthz returns 503 immediately
 *   FAIL_AFTER=30s          /healthz serves, then starts failing
 */

import { createServer } from 'node:http';

const PORT = process.env.PORT || 3000;
const VERSION = process.env.VERSION || 'dev';
const startedAt = Date.now();

let degraded = process.env.FAIL_HEALTHCHECK === 'true';

if (process.env.FAIL_AFTER) {
  const ms = parseDuration(process.env.FAIL_AFTER);
  if (ms > 0) {
    setTimeout(() => {
      degraded = true;
      console.log(`degrading deliberately after ${process.env.FAIL_AFTER}`);
    }, ms).unref();
  }
}

const server = createServer((req, res) => {
  const send = (status, body) => {
    res.writeHead(status, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify(body));
  };

  switch (req.url) {
    case '/healthz':
      // An unhealthy service returns a status and a reason, not a dropped connection.
      // NEXUS stores the reason alongside the probe result.
      return degraded
        ? send(503, { status: 'unhealthy', reason: 'dependency check failed' })
        : send(200, {
            status: 'ok',
            version: VERSION,
            uptime: `${Math.round((Date.now() - startedAt) / 1000)}s`,
          });

    case '/readyz':
      return degraded ? send(503, { status: 'not ready' }) : send(200, { status: 'ready' });

    default:
      return send(200, {
        service: 'demo-node-service',
        version: VERSION,
        message: 'deployed by NEXUS',
      });
  }
});

server.listen(PORT, () => {
  console.log(`demo-node-service ${VERSION} listening on :${PORT} (degraded=${degraded})`);
});

// SIGTERM is what an orchestrator sends first. Closing the server rather than exiting
// immediately lets in-flight requests finish, which is the difference between a rolling
// update being invisible and dropping a handful of requests.
process.on('SIGTERM', () => {
  console.log('SIGTERM received, draining');
  server.close(() => process.exit(0));
});

function parseDuration(value) {
  const match = /^(\d+)(ms|s|m)$/.exec(value.trim());
  if (!match) return 0;
  const amount = Number(match[1]);
  return match[2] === 'ms' ? amount : match[2] === 's' ? amount * 1000 : amount * 60_000;
}
