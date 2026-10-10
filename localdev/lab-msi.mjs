#!/usr/bin/env node
// Builds the lab MSI: the product's Windows release, built by device/installer/release-msi.mjs with the
// lab's trust anchors, and beside it the tenant file a deployment package carries, naming the lab
// tenant, the lab edge, the lab tenant's deployment key and the lab CA. Windows only.
//
//   node localdev/lab-msi.mjs
//
// Then double-click localdev\.msi\ShadowAICapture.msi. The agent installs as the ShadowAICapture
// service, enrols through the edge with the deployment key and starts capturing; its events appear
// on the dashboard for analyst@lab.test. The same folder is the agent release control-api serves
// (the browser extension's CRX and update manifest), so the lab picks it up without a restart.

import { spawnSync } from 'node:child_process';
import { cpSync, existsSync, mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';

import { ensureLabIdentity, readLabIdentity } from './identity/identity.mjs';
import { COMPOSE_FILE, IDENTITY_DIR, MSI_DIR, PKI_DIR, POLICY_KEY_ID, ROOT, TENANTS, TENANT_IDS, labAddresses } from './lab.mjs';

/** A version that rises with every hour, so each build upgrades the one installed (major.minor.build). */
export function labVersion(now = new Date()) {
  const hours = Math.floor((now.getTime() - Date.UTC(2026, 0, 1)) / 3_600_000);
  return `1.0.${Math.min(Math.max(hours, 0), 65535)}`;
}

/** The tenant file the installer copies beside the vendor configuration. */
export function tenantFile({ tenantId, deviceEndpoint, deploymentKey, caFile }) {
  return [
    '# The lab tenant. The deployment key is a secret.',
    `SAC_TENANT_ID=${tenantId}`,
    `SAC_DEVICE_ENDPOINT=${deviceEndpoint}`,
    `SAC_DEPLOYMENT_KEY=${deploymentKey}`,
    // The edge's certificate is signed by the lab CA, which no system trust store holds.
    `SAC_CA_FILE=${caFile}`,
    '',
  ].join('\r\n');
}

function run(cmd, args, opts = {}) {
  const r = spawnSync(cmd, args, { cwd: ROOT, encoding: 'utf8', shell: cmd === 'npm', ...opts });
  if (r.error || r.status !== 0) {
    throw new Error(`${cmd} ${args.join(' ')} failed${r.error ? `: ${r.error.message}` : ''}\n${`${r.stdout ?? ''}${r.stderr ?? ''}`.trim()}`);
  }
  return r.stdout ?? '';
}

function main() {
  if (process.platform !== 'win32') throw new Error('the MSI is built with WiX on Windows');
  const caFile = join(PKI_DIR, 'dev-ca.crt');
  if (!existsSync(caFile)) throw new Error('the lab has no CA yet: start it once with node localdev/run.mjs');
  ensureLabIdentity(IDENTITY_DIR, TENANT_IDS);
  const identity = readLabIdentity(IDENTITY_DIR, TENANT_IDS);
  const { dashboard, deviceEndpoint } = labAddresses(JSON.parse(run('docker', ['compose', '-f', COMPOSE_FILE, 'config', '--format', 'json'])));
  if (!existsSync(join(ROOT, 'device', 'extension', 'node_modules', 'crx3'))) run('npm', ['ci', '--prefix', 'device/extension', '--no-audit', '--no-fund']);

  const version = labVersion();
  const out = mkdtempSync(join(tmpdir(), 'sac-lab-msi-'));
  try {
    console.log(`lab-msi: building release ${version} …`);
    const built = spawnSync(process.execPath, [
      join(ROOT, 'device', 'installer', 'release-msi.mjs'),
      '--version', version,
      '--policy-key-file', identity.paths.policyPublicKey,
      '--policy-key-id', POLICY_KEY_ID,
      '--classifier-key', identity.paths.classifierKey,
      '--classifier-rules', join(ROOT, 'device', 'classifier-host', 'rules', 'default.json'),
      '--classifier-model', join(ROOT, 'device', 'classifier-host', 'rules', 'model.json'),
      '--extension-key', join(ROOT, 'device', 'extension', 'tools', 'extension-key.pem'),
      '--extension-update-url', `${dashboard.replace(/\/+$/, '')}/v1/extension/updates.xml`,
      '--wix-eula', 'wix7',
      '--out', out,
    ], { cwd: ROOT, stdio: 'inherit' });
    if (built.status !== 0) throw new Error('device/installer/release-msi.mjs failed (above)');
    // Copied into the folder rather than built there, so control-api's view of it stays mounted.
    mkdirSync(MSI_DIR, { recursive: true });
    cpSync(out, MSI_DIR, { recursive: true, force: true });
  } finally {
    rmSync(out, { recursive: true, force: true });
  }
  writeFileSync(join(MSI_DIR, 'ShadowAICapture.tenant.env'), tenantFile({
    tenantId: TENANTS.lab.id, deviceEndpoint, deploymentKey: identity.deploymentKeys.lab, caFile,
  }), { mode: 0o600 });

  const msi = join(MSI_DIR, 'ShadowAICapture.msi');
  console.log(`
lab-msi: ${msi}
         with ShadowAICapture.tenant.env beside it (tenant ${TENANTS.lab.name}, ${deviceEndpoint})

  install    double-click the MSI, or: msiexec /i "${msi}"
  uninstall  Settings > Apps > Shadow AI Capture, or: msiexec /x "${msi}"
  log        C:\\ProgramData\\ShadowAICapture\\state\\capture-core.log
  dashboard  sign in as analyst@${TENANTS.lab.domain} (node localdev/run.mjs prints the address)`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    main();
  } catch (err) {
    console.error(`lab-msi: ${err.message}`);
    process.exit(1);
  }
}
