// main.js — the process entry point for query-api.
//
// Two things happen here and nothing else: the configuration is resolved, and the service is
// started or it refuses to start. A read path that cannot reach its database does not "come up
// degraded and find out later" — it says so at startup, because the alternative is a container that
// passes its liveness probe while every query times out.
//
//   node src/http/main.js                        # reads the environment
//   SAC_ROLE=sac_query SAC_PG_HOST=... node src/http/main.js
//
// It reads SAC_HTTP_ADDR for where to listen. The deployment passes none, so the default is
// 0.0.0.0:8080 — a container's loopback is unreachable, which is finding F2 from docs/lab/LAB-COST
// turned into a default rather than a mistake.

import { loadConfig, redactedDsn, ConfigError } from './config.js';
import { createQueryServer, PATHS } from './server.js';
import { createPool } from './pool.js';
import { createVerifier } from './auth.js';

/** Graceful shutdown: stop accepting, let in-flight requests finish, then exit. */
function installSignalHandlers(service, log) {
  let closing = false;
  for (const signal of ['SIGINT', 'SIGTERM']) {
    process.on(signal, () => {
      if (closing) return;
      closing = true;
      log.info(`query-api: ${signal} received; draining`);
      service
        .close()
        .then(() => process.exit(0))
        .catch((error) => {
          log.error('query-api: shutdown failed', error);
          process.exit(1);
        });
    });
  }
}

export async function main(env = process.env, { startClient } = {}) {
  const log = console;

  let cfg;
  try {
    cfg = loadConfig(env);
  } catch (error) {
    if (error instanceof ConfigError) {
      log.error(`query-api: refusing to start: ${error.message}`);
      return 1;
    }
    throw error;
  }

  if (!cfg.pg.configured) {
    log.error(
      'query-api: refusing to start without a database: set SAC_PG_HOST and SAC_PG_DATABASE. ' +
        'The read path has no fallback store — an in-memory mode would answer every query with a ' +
        'number that came from nowhere.',
    );
    return 1;
  }

  // The driver and the pool are loaded through functions so this module can be imported, and its
  // argument handling tested, without a live connection.
  const makeClient = startClient ?? (await import('../pg/client.js')).createClient;
  const clientOptions = {
    host: cfg.pg.host,
    port: cfg.pg.port,
    database: cfg.pg.database,
    user: cfg.pg.user,
    password: cfg.pg.password,
    ssl: cfg.pg.ssl,
    statementTimeoutMs: cfg.pg.statementTimeoutMs,
    lockTimeoutMs: cfg.pg.lockTimeoutMs,
    applicationName: 'sac-query-api',
  };

  // A POOL, not a connection. One session cannot serve two requests — the second gets SAC_BUSY —
  // and §12.3 asks for 8 concurrent statements per tenant with a global ceiling of ~40. The gate in
  // server.js bounds what is admitted; the pool bounds what is opened.
  const pool = createPool({
    createClient: makeClient,
    clientOptions,
    max: cfg.limits.maxConnections,
    maxQueue: cfg.limits.maxQueue,
    log,
  });

  // Prove the configuration before listening, rather than passing a liveness probe on a service
  // that cannot read. One connection is opened and returned; a failure here is a refusal to start.
  try {
    const probe = await pool.acquire();
    await probe.query('SELECT 1');
    await pool.release(probe);
  } catch (error) {
    // No password, no host name and no DSN in the message: redactedDsn is the only rendering of
    // this configuration that is safe to print.
    log.error(`query-api: cannot reach ${redactedDsn(cfg)}: ${error?.message ?? error}`);
    await pool.close();
    return 1;
  }

  const service = createQueryServer({
    cfg,
    pool,
    log,
    verifier: createVerifier({
      issuer: cfg.oidc.issuer,
      audience: cfg.oidc.audience,
      jwksUrl: cfg.oidc.jwksUrl,
      tenantClaim: cfg.oidc.tenantClaim,
      rolesClaim: cfg.oidc.rolesClaim,
    }),
  });
  let address;
  try {
    address = await service.listen(cfg.http);
  } catch (error) {
    log.error(`query-api: cannot listen on ${cfg.http.host}:${cfg.http.port}: ${error?.message ?? error}`);
    await service.close();
    return 1;
  }

  log.info(
    `query-api: listening on ${cfg.http.host}:${cfg.http.port} as ${cfg.role} ` +
      `(${redactedDsn(cfg)}); probes ${PATHS.LIVENESS} ${PATHS.READINESS}, read path ${PATHS.QUERY}`,
  );
  if (cfg.oidc.enabled) {
    log.info(`query-api: verifying sessions from ${cfg.oidc.issuer} for audience ${cfg.oidc.audience}`);
  } else if (!cfg.devTrustPrincipal) {
    log.info('query-api: no identity provider is configured, so every read returns 403 by design');
  }

  installSignalHandlers(service, log);
  return { service, pool, address, cfg };
}

// Only run when executed, so importing this file for a test does not start a listener.
if (process.argv[1] && import.meta.url === `file://${process.argv[1].replace(/\\/g, '/')}`) {
  const result = await main();
  if (typeof result === 'number') process.exit(result);
}
