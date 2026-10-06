// main.js — the process entry point for query-api.
//
// It resolves the configuration (or refuses to start), opens the database pool and listens. The
// pool connects lazily: whether the database is reachable is a readiness question, answered by
// GET /readyz, so a slow database delays traffic instead of crash-looping the container.

import { pathToFileURL } from 'node:url';
import { loadConfig, describeDatabase, ConfigError } from './config.js';
import { createQueryServer, PATHS, POOL_MAX } from './server.js';
import { createVerifier } from './auth.js';
import { createPool } from '../db.js';

/** Structured JSON logs on stdout, one object per line. */
export function jsonLogger(stream = process.stdout) {
  const write = (level) => (msg, fields = {}) => {
    stream.write(`${JSON.stringify({ time: new Date().toISOString(), level, msg, ...fields })}\n`);
  };
  return { info: write('info'), warn: write('warn'), error: write('error') };
}

function installSignalHandlers(service, log) {
  let closing = false;
  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      if (closing) return;
      closing = true;
      log.info('draining', { signal });
      service
        .close()
        .then(() => process.exit(0))
        .catch((error) => {
          log.error('shutdown failed', { error: String(error?.message ?? error) });
          process.exit(1);
        });
    });
  }
}

/** Start the service. Returns the exit code on a refusal to start, or the running service. */
export async function main(env = process.env, { log = jsonLogger() } = {}) {
  let cfg;
  try {
    cfg = loadConfig(env);
  } catch (error) {
    if (error instanceof ConfigError) {
      log.error('refusing to start', { error: error.message });
      return 1;
    }
    throw error;
  }

  const pool = createPool(cfg.pg, { max: POOL_MAX, log });
  const verifier = createVerifier({ issuer: cfg.auth.issuer, audience: cfg.auth.audience, jwksUrl: cfg.auth.jwksUrl });
  const service = createQueryServer({ cfg, pool, verifier, log });
  let address;
  try {
    address = await service.listen(cfg.http);
  } catch (error) {
    log.error('cannot listen', { address: `${cfg.http.host}:${cfg.http.port}`, error: String(error?.message ?? error) });
    await service.close();
    return 1;
  }

  log.info('listening', {
    address: `${cfg.http.host}:${cfg.http.port}`,
    database: describeDatabase(cfg),
    issuer: cfg.auth.issuer,
    audience: cfg.auth.audience,
    probes: [PATHS.LIVENESS, PATHS.READINESS],
  });
  installSignalHandlers(service, log);
  return { service, pool, address, cfg };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const result = await main();
  if (typeof result === 'number') process.exit(result);
}
