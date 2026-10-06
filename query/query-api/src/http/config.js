// config.js — the environment this service reads, in one place.
//
// A configuration error is a refusal to start, never a defaulted value.

/** The names, spelled once. */
export const ENV = Object.freeze({
  HTTP_ADDR: 'SAC_HTTP_ADDR',
  PG_HOST: 'SAC_PG_HOST',
  PG_PORT: 'SAC_PG_PORT',
  PG_DATABASE: 'SAC_PG_DATABASE',
  PG_USER: 'SAC_PG_USER',
  PG_PASSWORD: 'SAC_PG_PASSWORD',
  PG_SSLMODE: 'SAC_PG_SSLMODE',
  CONTENT_VAULT_URL: 'SAC_CONTENT_VAULT_URL',
  CURSOR_KEY: 'SAC_CURSOR_KEY',
  AUTH_ISSUER: 'SAC_AUTH_ISSUER',
  AUTH_AUDIENCE: 'SAC_AUTH_AUDIENCE',
  AUTH_JWKS_URL: 'SAC_AUTH_JWKS_URL',
  // Injected by Container Apps for the managed identity; read only when SAC_PG_PASSWORD is unset.
  IDENTITY_ENDPOINT: 'IDENTITY_ENDPOINT',
  IDENTITY_HEADER: 'IDENTITY_HEADER',
  AZURE_CLIENT_ID: 'AZURE_CLIENT_ID',
});

/** This service's audience in the product token. */
export const DEFAULT_AUTH_AUDIENCE = 'sac-query';

/** The shortest cursor key accepted: an HMAC-SHA256 key should carry at least 256 bits. */
export const MIN_CURSOR_KEY_LENGTH = 32;

const DEFAULT_PORT = 8080;
const DEFAULT_PG_PORT = 5432;
const SSL_MODES = Object.freeze(['require', 'verify-full', 'disable']);

export class ConfigError extends Error {
  constructor(message) {
    super(message);
    this.name = 'ConfigError';
  }
}

function text(env, name) {
  const v = env[name];
  return typeof v === 'string' ? v.trim() : '';
}

function required(env, name) {
  const v = text(env, name);
  if (v === '') throw new ConfigError(`${name} is required`);
  return v;
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
  return value.replace(/\/+$/, '');
}

/** `host:port`, `:port` or `host`. An empty host binds every interface. */
export function parseAddr(addr) {
  const value = (addr ?? '').trim();
  if (value === '') return { host: '0.0.0.0', port: DEFAULT_PORT };
  const idx = value.lastIndexOf(':');
  if (idx < 0) return { host: value, port: DEFAULT_PORT };
  const host = value.slice(0, idx).replace(/^\[|\]$/g, '');
  const port = Number(value.slice(idx + 1));
  if (!Number.isInteger(port) || port < 0 || port > 65_535) {
    throw new ConfigError(`${ENV.HTTP_ADDR} has an invalid port: ${JSON.stringify(value)}`);
  }
  return { host: host === '' ? '0.0.0.0' : host, port };
}

function pgPort(env) {
  const raw = text(env, ENV.PG_PORT);
  if (raw === '') return DEFAULT_PG_PORT;
  const port = Number(raw);
  if (!Number.isInteger(port) || port < 1 || port > 65_535) throw new ConfigError(`${ENV.PG_PORT} is not a port: ${JSON.stringify(raw)}`);
  return port;
}

/**
 * Read the environment.
 *
 * @param {Record<string,string|undefined>} [env]
 * @returns {object} a frozen configuration
 */
export function loadConfig(env = process.env) {
  const host = required(env, ENV.PG_HOST);
  const database = required(env, ENV.PG_DATABASE);
  const user = required(env, ENV.PG_USER);
  const sslMode = (text(env, ENV.PG_SSLMODE) || 'require').toLowerCase();
  if (!SSL_MODES.includes(sslMode)) {
    throw new ConfigError(`${ENV.PG_SSLMODE} must be one of ${SSL_MODES.join(', ')}; got ${JSON.stringify(sslMode)}`);
  }

  // Without a password every connection authenticates with the managed identity's Entra token.
  const password = text(env, ENV.PG_PASSWORD);
  const identity = Object.freeze({
    endpoint: text(env, ENV.IDENTITY_ENDPOINT),
    header: text(env, ENV.IDENTITY_HEADER),
    clientId: text(env, ENV.AZURE_CLIENT_ID),
  });
  if (password === '' && (identity.endpoint === '' || identity.header === '')) {
    throw new ConfigError(
      `${ENV.PG_PASSWORD} is not set and there is no managed identity (${ENV.IDENTITY_ENDPOINT}, ${ENV.IDENTITY_HEADER}) to fetch a database token with`,
    );
  }

  const cursorKey = required(env, ENV.CURSOR_KEY);
  if (cursorKey.length < MIN_CURSOR_KEY_LENGTH) {
    throw new ConfigError(`${ENV.CURSOR_KEY} must be at least ${MIN_CURSOR_KEY_LENGTH} characters`);
  }

  const issuer = absoluteUrl(ENV.AUTH_ISSUER, required(env, ENV.AUTH_ISSUER));
  const jwksUrl = text(env, ENV.AUTH_JWKS_URL);

  return Object.freeze({
    http: Object.freeze(parseAddr(text(env, ENV.HTTP_ADDR))),
    pg: Object.freeze({
      host,
      port: pgPort(env),
      database,
      user,
      password,
      sslMode,
      identity,
    }),
    contentVaultUrl: absoluteUrl(ENV.CONTENT_VAULT_URL, required(env, ENV.CONTENT_VAULT_URL)),
    cursorKey: Buffer.from(cursorKey, 'utf8'),
    auth: Object.freeze({
      // The issuer exactly as tokens carry it in `iss`.
      issuer: text(env, ENV.AUTH_ISSUER),
      audience: text(env, ENV.AUTH_AUDIENCE) || DEFAULT_AUTH_AUDIENCE,
      jwksUrl: jwksUrl ? absoluteUrl(ENV.AUTH_JWKS_URL, jwksUrl) : `${issuer}/.well-known/jwks.json`,
    }),
  });
}

/** The database target for logs, with no credential. */
export function describeDatabase(cfg) {
  const { host, port, database, user, sslMode, password } = cfg.pg;
  return `postgres://${user}@${host}:${port}/${database} (auth=${password ? 'password' : 'entra'}, sslmode=${sslMode})`;
}
