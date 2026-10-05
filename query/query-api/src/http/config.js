// config.js — the environment this service reads, in one place.
//
// The names are the deployment's names, not this service's invention: azure/main.bicep passes
// SAC_ROLE, SAC_PG_HOST, SAC_PG_DATABASE, SAC_CONTENT_VAULT_URL and SAC_APPINSIGHTS to the
// query-api container app, and localdev/tools/check-config-agreement.mjs fails the build if a name
// the deployment passes is read by nothing, or a name a binary reads is passed by nothing. Adding a
// name here without adding it there is a gate failure, which is the point.
//
// WHY THERE IS NO PASSWORD. A deployed PostgreSQL on this architecture authenticates the managed
// identity, so the credential is a token the platform hands the process, not a secret in the
// environment. ingestion/ingest-api/cmd/ingest-api/config.go states the same rule for the Go side.
// SAC_PG_PASSWORD exists only for a local lab, and its absence is the normal case rather than an
// error: the DSN simply carries no password.

/** The database role this service runs as. database/schema.sql grants it SELECT and no writes. */
export const DEFAULT_ROLE = 'sac_query';

/** The names, spelled once. */
export const ENV = Object.freeze({
  ROLE: 'SAC_ROLE',
  HTTP_ADDR: 'SAC_HTTP_ADDR',
  DEV_TRUST_PRINCIPAL: 'SAC_DEV_TRUST_PRINCIPAL',
  PG_HOST: 'SAC_PG_HOST',
  PG_PORT: 'SAC_PG_PORT',
  PG_DATABASE: 'SAC_PG_DATABASE',
  PG_USER: 'SAC_PG_USER',
  PG_PASSWORD: 'SAC_PG_PASSWORD',
  PG_SSLMODE: 'SAC_PG_SSLMODE',
  CONTENT_VAULT_URL: 'SAC_CONTENT_VAULT_URL',
  CONTENT_SEARCH_SCOPE: 'SAC_CONTENT_SEARCH_SCOPE',
  APPINSIGHTS: 'SAC_APPINSIGHTS',
  STATEMENT_TIMEOUT_MS: 'SAC_PG_STATEMENT_TIMEOUT_MS',
  LOCK_TIMEOUT_MS: 'SAC_PG_LOCK_TIMEOUT_MS',
  MAX_CONCURRENCY: 'SAC_MAX_CONCURRENCY',
  MAX_QUEUE: 'SAC_MAX_QUEUE',
  MAX_CONNECTIONS: 'SAC_MAX_CONNECTIONS',
  // The product access token (contract §2): control-api is the one issuer. The issuer is empty in
  // the memory lab, where the development principal is the only path.
  AUTH_ISSUER: 'SAC_AUTH_ISSUER',
  AUTH_AUDIENCE: 'SAC_AUTH_AUDIENCE',
  AUTH_JWKS_URL: 'SAC_AUTH_JWKS_URL',
});

/** This service's audience in the product token (contract §2). */
export const DEFAULT_AUTH_AUDIENCE = 'sac-query';

/** A configuration error is a refusal to start, never a defaulted value. §12.3's fail-closed rule. */
export class ConfigError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ConfigError';
  }
}

const DEFAULT_PORT = 8080;
const DEFAULT_PG_PORT = 5432;
/** §12.3: statement_timeout bounds every statement server-side. 10s is this service's default. */
const DEFAULT_STATEMENT_TIMEOUT_MS = 10_000;
/** A lock wait must be shorter than the statement budget, or a blocked read consumes all of it. */
const DEFAULT_LOCK_TIMEOUT_MS = 5_000;
/** §12.3: 8 concurrent statements per tenant, 4 per user, a bounded queue of 32. */
const DEFAULT_MAX_CONCURRENCY = 8;
const DEFAULT_MAX_QUEUE = 32;
/**
 * §12.3: "the global pool is capped at ~40 connections pending verification of the instance
 * ceiling (master doc Q12)". The 40 is this service's share of that ceiling, not a measured limit;
 * Q12 is what closes it, and until then this is the number to change rather than to assume.
 */
const DEFAULT_MAX_CONNECTIONS = 40;

function text(env, name) {
  const v = env[name];
  return typeof v === 'string' && v.trim() !== '' ? v.trim() : '';
}

function intOr(env, name, fallback, { min = 1, max = Number.MAX_SAFE_INTEGER } = {}) {
  const raw = text(env, name);
  if (raw === '') return fallback;
  const n = Number(raw);
  if (!Number.isInteger(n) || n < min || n > max) {
    throw new ConfigError(`${name} must be an integer between ${min} and ${max}; got ${JSON.stringify(raw)}`);
  }
  return n;
}

/** An http(s) URL with a host, or a refusal to start naming the variable. */
function absoluteUrl(name, value) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new ConfigError(`${name} is not a URL: ${JSON.stringify(value)}`);
  }
  if ((url.protocol !== 'https:' && url.protocol !== 'http:') || url.host === '') {
    throw new ConfigError(`${name} must be an absolute http(s) URL; got ${JSON.stringify(value)}`);
  }
  return url;
}

/**
 * Split `host:port` / `:port` / `host` the way every service in this repository does.
 *
 * The deployment passes the listen address as a bare `:8080` (a container must bind 0.0.0.0, and
 * its own loopback is unreachable), while a local run usually wants `127.0.0.1:8080`.
 */
export function parseAddr(addr) {
  const value = (addr ?? '').trim();
  if (value === '') return { host: '0.0.0.0', port: DEFAULT_PORT };
  const idx = value.lastIndexOf(':');
  if (idx < 0) return { host: value, port: DEFAULT_PORT };
  const host = value.slice(0, idx).replace(/^\[|\]$/g, '');
  const rawPort = value.slice(idx + 1);
  const port = Number(rawPort);
  if (!Number.isInteger(port) || port < 0 || port > 65_535) {
    throw new ConfigError(`${ENV.HTTP_ADDR} has an invalid port: ${JSON.stringify(value)}`);
  }
  return { host: host === '' ? '0.0.0.0' : host, port };
}

/**
 * Read the environment.
 *
 * @param {Record<string,string|undefined>} [env]
 * @returns {object} a frozen configuration
 */
export function loadConfig(env = process.env) {
  const pgHost = text(env, ENV.PG_HOST);
  const pgDatabase = text(env, ENV.PG_DATABASE);
  const role = text(env, ENV.ROLE) || DEFAULT_ROLE;

  // The token issuer. The audience and JWKS location default from it, so the issuer alone turns the
  // token path on. Either of the other two without it is a configuration that would verify nothing
  // while looking as if it did, so it is a refusal to start rather than a silent no-op.
  const authIssuer = text(env, ENV.AUTH_ISSUER);
  const authAudience = text(env, ENV.AUTH_AUDIENCE);
  const authJwksUrl = text(env, ENV.AUTH_JWKS_URL);
  if (!authIssuer && (authAudience || authJwksUrl)) {
    throw new ConfigError(`${ENV.AUTH_ISSUER} is required when ${authAudience ? ENV.AUTH_AUDIENCE : ENV.AUTH_JWKS_URL} is set: a token is verified against the issuer that minted it.`);
  }
  if (authIssuer) {
    absoluteUrl(ENV.AUTH_ISSUER, authIssuer);
    if (authJwksUrl) absoluteUrl(ENV.AUTH_JWKS_URL, authJwksUrl);
  }

  const { host, port } = parseAddr(text(env, ENV.HTTP_ADDR));

  // sslmode mirrors libpq's spelling, because that is what an operator will have in front of them.
  // There is no "prefer" and no silent downgrade: a service that falls back to plaintext because a
  // TLS handshake failed is a service that can be stripped of TLS by an attacker who can fail one.
  const sslMode = (text(env, ENV.PG_SSLMODE) || 'require').toLowerCase();
  if (!['require', 'verify-full', 'disable'].includes(sslMode)) {
    throw new ConfigError(
      `${ENV.PG_SSLMODE} must be one of require, verify-full or disable; got ${JSON.stringify(sslMode)}. ` +
        'There is deliberately no "prefer": a silent fallback to plaintext is not a mode, it is a downgrade.',
    );
  }

  return Object.freeze({
    role,
    http: Object.freeze({ host, port }),
    pg: Object.freeze({
      host: pgHost,
      port: intOr(env, ENV.PG_PORT, DEFAULT_PG_PORT, { max: 65_535 }),
      database: pgDatabase,
      user: text(env, ENV.PG_USER) || role,
      password: text(env, ENV.PG_PASSWORD),
      sslMode,
      ssl: sslMode !== 'disable',
      statementTimeoutMs: intOr(env, ENV.STATEMENT_TIMEOUT_MS, DEFAULT_STATEMENT_TIMEOUT_MS, { min: 100 }),
      /**
       * `lock_timeout`, and why it is separate from `statement_timeout`.
       *
       * A read that blocks on a lock burns its whole statement budget waiting and then returns the
       * same error as a read that was genuinely too expensive — so the two cannot be told apart by
       * the client, and §12.3's "cancellation is safe because nothing is streamed" becomes a rule
       * nobody can act on. A shorter lock_timeout makes "someone else holds a lock" a distinct,
       * retryable answer.
       */
      lockTimeoutMs: intOr(env, ENV.LOCK_TIMEOUT_MS, DEFAULT_LOCK_TIMEOUT_MS, { min: 10 }),
      /** True when there is a database to talk to at all. Readiness is false without one. */
      configured: pgHost !== '' && pgDatabase !== '',
    }),
    contentVaultUrl: text(env, ENV.CONTENT_VAULT_URL),
    // The search scope asked of the vault. The signed policy bundle names scopes and their search
    // tiers; a scope the vault is not told about carries `disabled` (docs/06 §6.3).
    contentSearchScope: text(env, ENV.CONTENT_SEARCH_SCOPE),
    appInsights: text(env, ENV.APPINSIGHTS),
    /**
     * The product access token's issuer (control-api, contract §2). With no issuer the service has
     * no token path and, unless the development flag is on, refuses every read.
     */
    auth: Object.freeze({
      issuer: authIssuer,
      audience: authIssuer ? authAudience || DEFAULT_AUTH_AUDIENCE : '',
      jwksUrl: authIssuer ? authJwksUrl || `${authIssuer.replace(/\/+$/, '')}/.well-known/jwks.json` : '',
      enabled: authIssuer !== '',
    }),
    /**
     * Development-only principal trust.
     *
     * The tenant a query runs as is never taken from the request body (REASON.TENANT_IN_REQUEST).
     * In a deployment it comes from the verified product token. This flag is the same escape hatch
     * the Go services use for a local run (`-dev-trust-principal`); it is explicit, it is named,
     * and it is off unless set.
     */
    devTrustPrincipal: text(env, ENV.DEV_TRUST_PRINCIPAL) === '1',
    limits: Object.freeze({
      maxConcurrency: intOr(env, ENV.MAX_CONCURRENCY, DEFAULT_MAX_CONCURRENCY, { min: 1, max: 1000 }),
      maxQueue: intOr(env, ENV.MAX_QUEUE, DEFAULT_MAX_QUEUE, { min: 0, max: 10_000 }),
      maxConnections: intOr(env, ENV.MAX_CONNECTIONS, DEFAULT_MAX_CONNECTIONS, { min: 1, max: 1000 }),
    }),
  });
}

/**
 * A DSN for logs and diagnostics, with any credential removed.
 *
 * A DSN is a durable place for a mistake to live: it reaches log aggregators, crash dumps and
 * screenshots. This returns a value that is safe to print, and it is the only way this package
 * renders one.
 */
export function redactedDsn(cfg) {
  const { host, port, database, user, sslMode } = cfg.pg;
  const at = host ? `${user}@${host}:${port}/${database}` : '(unconfigured)';
  return `postgres://${at}?sslmode=${sslMode}`;
}
