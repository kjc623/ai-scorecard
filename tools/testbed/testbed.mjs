#!/usr/bin/env node
// tools/testbed/testbed.mjs - the reference VM's configuration: the value table in
// refactor-endpoint/TESTBED.md, read into one object.
//
//   node tools/testbed/testbed.mjs     prints the configuration as JSON (invm.ps1 reads it)
//
// It refuses to run while a value the tools need is empty, and names it. Paths may use %VARIABLE%,
// expanded from the environment. The table holds paths to secrets, never a secret.

import { readFileSync, writeFileSync } from 'node:fs';
import { fileURLToPath, pathToFileURL } from 'node:url';

export const TESTBED_FILE = fileURLToPath(new URL('../../refactor-endpoint/TESTBED.md', import.meta.url));

// The owner's own product tenant, which no testbed tool may act on.
export const OWNER_TENANT = '11111111-1111-1111-1111-111111111111';

export const APP_ID_ROW = 'Intune app id';

const GUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const checks = {
  guid: [(v) => GUID.test(v), 'a GUID'],
  host: [(v) => /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/i.test(v), 'a host name'],
  repo: [(v) => /^[\w.-]+\/[\w.-]+$/.test(v), '<owner>/<repo>'],
  upn: [(v) => /^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(v), 'a user principal name'],
  thumbprint: [(v) => /^[0-9a-f]{40}$/i.test(v), 'a 40-character certificate thumbprint'],
  text: [(v) => v.length > 0, 'a value'],
};

// The rows the tools read: [row name, key, check, required].
export const FIELDS = [
  ['Device hostname', 'deviceHostname', 'host', true],
  ['Test tenant id', 'testTenantId', 'guid', true],
  ['Tenant file', 'tenantFile', 'text', true],
  ['GitHub repository', 'githubRepository', 'repo', true],
  ['VM name', 'vmName', 'text', true],
  ['Clean checkpoint', 'cleanCheckpoint', 'text', true],
  ['VM admin credential', 'vmAdminCredential', 'text', true],
  ['Console user', 'consoleUser', 'upn', true],
  ['Second user', 'secondUser', 'upn', false],
  ['Entra tenant id', 'entraTenantId', 'guid', true],
  ['Intune app registration', 'intuneClientId', 'guid', true],
  ['Publishing certificate', 'certificateThumbprint', 'thumbprint', true],
  ['Test device group', 'testDeviceGroup', 'guid', true],
  [APP_ID_ROW, 'intuneAppId', 'guid', false],
];

/** Splits a Markdown table row into its cells; `\|` is a literal pipe. */
function cells(line) {
  const parts = line.trim().replace(/^\|/, '').replace(/\|$/, '').split(/(?<!\\)\|/);
  return parts.map((c) => c.trim().replace(/\\\|/g, '|'));
}

/** The value table's rows, in file order: { name, value, line } with line the 0-based line index. */
export function tableRows(text) {
  const lines = text.split('\n');
  const start = lines.findIndex((l) => /^##\s+Values\s*$/.test(l.trimEnd()));
  if (start === -1) throw new Error('TESTBED.md has no "## Values" section');
  const rows = [];
  for (let i = start + 1; i < lines.length && !/^##\s/.test(lines[i]); i++) {
    const line = lines[i].replace(/\r$/, '');
    if (!line.trim().startsWith('|')) continue;
    const [name, value = ''] = cells(line);
    if (name === 'Name' || /^:?-+:?$/.test(name)) continue;
    rows.push({ name, value, line: i });
  }
  return rows;
}

/** A cell's value: backticks removed; a placeholder in parentheses counts as empty. */
function cellValue(raw) {
  const v = raw.trim().replace(/^`(.*)`$/, '$1').trim();
  return /^\(.*\)$/.test(v) ? '' : v;
}

function expand(value, env, name) {
  return value.replace(/%([A-Za-z_][A-Za-z0-9_]*)%/g, (_, v) => {
    if (env[v] === undefined) throw new Error(`TESTBED.md: "${name}" uses %${v}%, which is not set in this environment`);
    return env[v];
  });
}

/** Reads TESTBED.md's value table into the configuration, refusing an empty or malformed value. */
export function parseTestbed(text, env = process.env) {
  const rows = new Map(tableRows(text).map((r) => [r.name, cellValue(r.value)]));
  const missing = FIELDS.filter(([name, , , required]) => required && !rows.get(name)).map(([name]) => name);
  if (missing.length) {
    throw new Error(`TESTBED.md: fill in ${missing.map((m) => `"${m}"`).join(', ')} in the Values table before running the testbed tools`);
  }
  const config = {};
  for (const [name, key, check] of FIELDS) {
    const value = expand(rows.get(name) ?? '', env, name);
    if (value === '') {
      config[key] = '';
      continue;
    }
    const [ok, what] = checks[check];
    if (!ok(value)) throw new Error(`TESTBED.md: "${name}" must be ${what}`);
    config[key] = check === 'guid' ? value.toLowerCase() : value;
  }
  if (config.testTenantId === OWNER_TENANT) throw new Error("TESTBED.md: the test tenant id is the owner's tenant; the testbed never uses it");
  return config;
}

/** Refuses an Intune app id other than the one TESTBED.md records. */
export function checkAppId(recorded, appId) {
  if (!GUID.test(appId ?? '')) throw new Error(`refusing Intune app id "${appId}": not a GUID`);
  if (recorded && recorded.toLowerCase() !== appId.toLowerCase()) {
    throw new Error(`refusing Intune app ${appId}: TESTBED.md records ${recorded}, the only app the testbed changes`);
  }
}

/** TESTBED.md with the Intune app id recorded; refuses to replace a different recorded id. */
export function recordAppId(text, appId) {
  const row = tableRows(text).find((r) => r.name === APP_ID_ROW);
  if (!row) throw new Error(`TESTBED.md has no "${APP_ID_ROW}" row`);
  checkAppId(cellValue(row.value), appId);
  const lines = text.split('\n');
  const cr = lines[row.line].endsWith('\r') ? '\r' : '';
  const [name, , ...rest] = cells(lines[row.line].replace(/\r$/, ''));
  lines[row.line] = `| ${[name, `\`${appId.toLowerCase()}\``, ...rest.map((c) => c.replace(/\|/g, '\\|'))].join(' | ')} |${cr}`;
  return lines.join('\n');
}

export function loadConfig(env = process.env) {
  return parseTestbed(readFileSync(TESTBED_FILE, 'utf8'), env);
}

export function saveAppId(appId) {
  const text = readFileSync(TESTBED_FILE, 'utf8');
  const next = recordAppId(text, appId);
  if (next !== text) writeFileSync(TESTBED_FILE, next);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    console.log(JSON.stringify(loadConfig(), null, 2));
  } catch (err) {
    console.error(`testbed: ${err.message}`);
    process.exit(1);
  }
}
