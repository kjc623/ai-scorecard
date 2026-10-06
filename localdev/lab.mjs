// The lab's fixed facts and the pure functions build.mjs, run.mjs, seed.mjs, lab-msi.mjs and the
// tools share. Nothing here starts a process.

import { existsSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

export const LOCALDEV = dirname(fileURLToPath(import.meta.url));
export const ROOT = resolve(LOCALDEV, '..');
export const COMPOSE_FILE = join(LOCALDEV, 'compose.yaml');
/** The lab's Docker network (compose.yaml names it), which the smoke and the harness join. */
export const NETWORK = 'sac-lab';
/** The device CA and the edge's server certificate. */
export const PKI_DIR = join(LOCALDEV, '.authlab');
/** The lab's secrets (identity/identity.mjs). */
export const IDENTITY_DIR = join(LOCALDEV, '.authlab-identity');
/** The lab MSI with its tenant file, and the agent release control-api serves from it. */
export const MSI_DIR = join(LOCALDEV, '.msi');

/** SAC_REGION of control-api and ingest-api in compose.yaml; every lab tenant lives there. */
export const REGION = 'lab';
export const POLICY_KEY_ID = 'policy-key-1';
/** The lab identity provider: realm r's issuer is OIDC_ISSUER/r, and control-api is its client. */
export const OIDC_ISSUER = 'http://oidc:8080';
export const OIDC_CLIENT_ID = 'sac-control';

/**
 * The lab tenants. `lab` is the one the lab MSI enrols a real device into; `sample` is the one the
 * smoke and the device simulator use, so invented devices never mix with a real one. Each signs in
 * through its own realm of the lab identity provider, by its email domain.
 */
export const TENANTS = Object.freeze({
  lab: Object.freeze({
    id: '10ca1ab0-0000-4000-8000-000000000001', name: 'Lab', realm: 'lab', domain: 'lab.test',
    ceiling: 'm3', contentSearch: 'full_text', contentBudgetBytesPerDay: 1024 ** 3,
  }),
  sample: Object.freeze({
    id: '5a3c0de0-7e57-4a11-9000-0000000d3a01', name: 'Northwind Freight (sample)', realm: 'sample', domain: 'sample.test',
    ceiling: 'm2', contentSearch: 'attachment_names', contentBudgetBytesPerDay: 0,
  }),
});

/** Tenant name -> id, the shape identity/identity.mjs takes. */
export const TENANT_IDS = Object.freeze(Object.fromEntries(Object.entries(TENANTS).map(([name, t]) => [name, t.id])));

/** The analyst the smoke signs in as (compose.yaml lists the lab identity provider's accounts). */
export const SMOKE_ANALYST = `analyst@${TENANTS.sample.domain}`;

/** Every image the lab runs, in build order: lab-jobs is built from the jobs image. */
export const IMAGES = Object.freeze([
  { name: 'migrate', dockerfile: 'database/Dockerfile' },
  { name: 'ingest-api', dockerfile: 'ingestion/ingest-api/Dockerfile' },
  { name: 'control-api', dockerfile: 'control/control-api/Dockerfile' },
  { name: 'content-vault', dockerfile: 'vault/content-vault/Dockerfile' },
  { name: 'query-api', dockerfile: 'query/query-api/Dockerfile' },
  { name: 'dashboard', dockerfile: 'query/dashboard/Dockerfile' },
  { name: 'jobs', dockerfile: 'jobs/Dockerfile' },
  { name: 'edge', dockerfile: 'localdev/edge/Dockerfile' },
  { name: 'oidc', dockerfile: 'localdev/oidc/Dockerfile' },
  { name: 'authlab', dockerfile: 'localdev/authlab/Dockerfile' },
  { name: 'lab-jobs', dockerfile: 'localdev/jobs/Dockerfile' },
]);

export const imageTag = (name) => `sac/${name}:lab`;

/** The `docker build` arguments for one image; the build context is the repository root. */
export function buildArgs(image) {
  return ['build', '-f', image.dockerfile, '-t', imageTag(image.name), '.'];
}

// --- the PKI ---------------------------------------------------------------------------------

const PKI_FILES = Object.freeze({ caCert: 'dev-ca.crt', caKey: 'dev-ca.key', edgeCert: 'edge-server.crt', edgeKey: 'edge-server.key' });

/** 'complete', 'missing' (a fresh checkout) or 'partial' (which the lab refuses to guess about). */
export function pkiState(dir) {
  const present = Object.values(PKI_FILES).filter((f) => existsSync(join(dir, f)));
  if (present.length === 0) return 'missing';
  return present.length === Object.keys(PKI_FILES).length ? 'complete' : 'partial';
}

export function readPKI(dir) {
  return Object.fromEntries(Object.entries(PKI_FILES).map(([k, f]) => [k, readFileSync(join(dir, f), 'utf8')]));
}

/**
 * Stores `authlab pki` output. Only for a directory with none of the files: a write fails if a file
 * exists, so material a device depends on is never replaced.
 */
export function writePKI(dir, json) {
  const p = JSON.parse(json);
  const fields = { caCert: p.ca_cert, caKey: p.ca_key, edgeCert: p.edge_cert, edgeKey: p.edge_key };
  for (const [k, v] of Object.entries(fields)) {
    if (typeof v !== 'string' || !v.includes('-----BEGIN ')) throw new Error(`authlab pki printed no ${k}`);
  }
  for (const [k, f] of Object.entries(PKI_FILES)) {
    writeFileSync(join(dir, f), fields[k], { mode: k.endsWith('Key') ? 0o600 : 0o644, flag: 'wx' });
  }
}

// --- compose ---------------------------------------------------------------------------------

/** The variables compose.yaml takes the lab's key material from. */
export function composeEnvironment(pki, identity) {
  return {
    LAB_CA_CERT_PEM: pki.caCert,
    LAB_CA_KEY_PEM: pki.caKey,
    LAB_EDGE_CERT_PEM: pki.edgeCert,
    LAB_EDGE_KEY_PEM: pki.edgeKey,
    LAB_SESSION_SIGNING_KEY: identity.sessionSigningKey,
    LAB_POLICY_SIGNING_KEY: identity.policySigningKey,
  };
}

/**
 * The addresses people and devices use, read from `docker compose config --format json`, so a port
 * or host moved in the environment or in localdev/.env is followed.
 */
export function labAddresses(config) {
  const services = config?.services ?? {};
  const env = (service, name) => services[service]?.environment?.[name] ?? '';
  const published = (service, target) => {
    const p = (services[service]?.ports ?? []).find((port) => Number(port.target) === target);
    return p ? `${p.host_ip && p.host_ip !== '0.0.0.0' ? p.host_ip : '127.0.0.1'}:${p.published}` : '';
  };
  return {
    dashboard: env('dashboard', 'SAC_PUBLIC_URL'),
    oidc: env('oidc', 'OIDC_PUBLIC_URL'),
    deviceEndpoint: env('control-api', 'SAC_PUBLIC_DEVICE_ENDPOINT'),
    postgres: published('postgres', 5432),
  };
}

/** host:port of a URL, with the scheme's default port written out. */
export function hostPort(address) {
  const u = new URL(address);
  return `${u.hostname}:${u.port || (u.protocol === 'https:' ? '443' : '80')}`;
}

/** The `docker run` arguments of the smoke; the CA and deployment key pass through the environment. */
export function smokeArgs(addresses, identity) {
  return [
    'run', '--rm', '--network', NETWORK, '-e', 'LAB_CA_CERT_PEM', '-e', 'LAB_DEPLOYMENT_KEY', imageTag('authlab'), 'smoke',
    '-tenant', TENANTS.sample.id,
    '-analyst', SMOKE_ANALYST,
    '-policy-key', identity.policyPublicKey,
    '-policy-key-id', POLICY_KEY_ID,
    '-dashboard', addresses.dashboard,
    '-resolve', `${hostPort(addresses.dashboard)}=dashboard:8080,${hostPort(addresses.oidc)}=oidc:8080`,
  ];
}
