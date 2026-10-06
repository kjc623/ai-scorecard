// main.mjs — the dashboard server's process entry point: read the environment, refuse to start
// without a complete configuration, listen, and drain on SIGTERM.
//
//   node server/main.mjs
//
// Logs are JSON lines on stdout.

import { ConfigError, configFromEnv, createDashboardServer } from './server.mjs';

function line(level, msg, fields = {}) {
  process.stdout.write(`${JSON.stringify({ time: new Date().toISOString(), level, msg, ...fields })}\n`);
}

const log = Object.freeze({
  info: (msg) => line('INFO', msg),
  warn: (msg) => line('WARN', msg),
  error: (msg) => line('ERROR', msg),
});

let cfg;
try {
  cfg = configFromEnv();
} catch (error) {
  if (!(error instanceof ConfigError)) throw error;
  log.error(`dashboard: refusing to start: ${error.message}`);
  process.exit(1);
}

const server = createDashboardServer({ ...cfg, log });
server.on('error', (error) => {
  log.error(`dashboard: cannot listen on ${cfg.host}:${cfg.port}: ${error.message}`);
  process.exit(1);
});
server.listen(cfg.port, cfg.host, () => {
  line('INFO', 'dashboard: listening', {
    addr: `${cfg.host}:${cfg.port}`,
    public_url: cfg.publicUrl,
    control_api: cfg.controlUrl,
    query_api: cfg.queryApiUrl,
    content_vault: cfg.vaultUrl,
  });
});

for (const signal of ['SIGINT', 'SIGTERM']) {
  process.once(signal, () => {
    log.info(`dashboard: ${signal} received; draining`);
    server.close(() => process.exit(0));
    server.closeIdleConnections?.();
    setTimeout(() => process.exit(0), 10_000).unref();
  });
}
