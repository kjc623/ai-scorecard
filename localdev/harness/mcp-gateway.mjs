#!/usr/bin/env node
// Start the Docker Desktop MCP gateway for the harness containers.
//
// The MCP Toolkit speaks stdio to clients on the host. A container cannot use that, so this runs the
// gateway over streamable HTTP on the host, where it keeps the profile, secrets and OAuth sessions
// configured in Docker Desktop. Harnesses reach it at http://host.docker.internal:<port>/mcp.
//
// The bearer token is generated once and kept in localdev/.env (ignored by git), which is also where
// localdev/harness.compose.yaml reads it from — so the gateway and the harnesses agree without anyone
// copying a token around.
//
// Usage:
//   node localdev/harness/mcp-gateway.mjs                 # profile ai_scorecard, port 8811
//   MCP_GATEWAY_PROFILE=other MCP_GATEWAY_PORT=8812 node localdev/harness/mcp-gateway.mjs

import { spawn } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { appendFileSync, existsSync, readFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_FILE = join(resolve(dirname(fileURLToPath(import.meta.url)), '..'), '.env');
const PROFILE = process.env.MCP_GATEWAY_PROFILE ?? 'ai_scorecard';
const PORT = process.env.MCP_GATEWAY_PORT ?? '8811';

function token() {
  const text = existsSync(ENV_FILE) ? readFileSync(ENV_FILE, 'utf8') : '';
  const found = /^MCP_GATEWAY_AUTH_TOKEN=(.+)$/m.exec(text);
  if (found) return found[1].trim();
  const fresh = randomBytes(32).toString('hex');
  const separator = text && !text.endsWith('\n') ? '\n' : '';
  appendFileSync(ENV_FILE, `${separator}MCP_GATEWAY_AUTH_TOKEN=${fresh}\n`);
  console.log(`mcp-gateway: wrote a new token to ${ENV_FILE}`);
  return fresh;
}

const args = ['mcp', 'gateway', 'run', '--profile', PROFILE, '--transport', 'streaming', '--port', PORT];
console.log(`mcp-gateway: docker ${args.join(' ')}`);
const child = spawn('docker', args, {
  stdio: 'inherit',
  env: { ...process.env, MCP_GATEWAY_AUTH_TOKEN: token() },
});
child.on('exit', (code) => process.exit(code ?? 1));
