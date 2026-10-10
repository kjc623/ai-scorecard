// device/installer/windows/msi.mjs - compile, sign and inspect the Windows MSI.
//
// buildMsi runs WiX (v5 or later) over device/installer/generated/windows/ShadowAICapture.wxs and, with
// signing on, Authenticode-signs the two executables before they are packaged and the MSI after.
// The signer is configured by environment, exactly one of:
//   Trusted Signing     SAC_SIGN_DLIB (Azure.CodeSigning.Dlib.dll) and SAC_SIGN_METADATA (metadata.json)
//   certificate store   SAC_SIGN_THUMBPRINT
//   PFX file            SAC_SIGN_PFX, with SAC_SIGN_PFX_PASSWORD
// SAC_SIGNTOOL overrides the signtool.exe lookup and SAC_SIGN_TIMESTAMP_URL the timestamp server.
// releaseChecks reads a built release back out of the package through Windows Installer.

import { spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { existsSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync } from 'node:fs';
import { homedir, tmpdir } from 'node:os';
import { join } from 'node:path';

import { extensionId } from '../../extension/tools/extension-id.mjs';
import { CRX_FILE } from '../../extension/tools/build-crx.mjs';
import { AGENT_RELEASE_FILE, verifyAgentRelease } from '../agent-release.mjs';
import { INSTALLED, NATIVE_HOST, PRODUCT, TENANT_PACKAGE, TRUST_ANCHORS, UNINSTALL_CLEANUP } from '../manifest.mjs';
import { DATA_FOLDER_ACL } from '../render.mjs';
import { BuildError, ROOT } from '../build.mjs';

export const MSI_FILE = 'ShadowAICapture.msi';
const WXS = join(ROOT, 'device', 'installer', 'generated', 'windows', 'ShadowAICapture.wxs');

function run(cmd, args, what, env = process.env) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', env });
  if (res.error) throw new BuildError(`${what}: ${res.error.message}`);
  if (res.status !== 0) throw new BuildError(`${what} failed (exit ${res.status}):\n${`${res.stdout ?? ''}${res.stderr ?? ''}`.trim()}`);
  return res.stdout ?? '';
}

/** Windows PowerShell with the module path it builds itself: the one inherited from PowerShell 7
 *  makes it load PowerShell 7's modules, which fail. */
function powershell(args, what) {
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => k.toLowerCase() !== 'psmodulepath'));
  return run('powershell', args, what, env);
}

function onPath(name) {
  const res = spawnSync('where.exe', [name], { encoding: 'utf8' });
  return res.status === 0 ? res.stdout.split(/\r?\n/)[0].trim() : null;
}

/** The WiX CLI: on PATH, or where `dotnet tool install --global wix` puts it. */
function findWix() {
  const wix = onPath('wix') ?? [join(homedir(), '.dotnet', 'tools', 'wix.exe')].find(existsSync);
  if (!wix) throw new BuildError('WiX is not installed: dotnet tool install --global wix (WiX 5 or later)');
  return wix;
}

function findSigntool() {
  if (process.env.SAC_SIGNTOOL) return process.env.SAC_SIGNTOOL;
  const found = onPath('signtool');
  if (found) return found;
  const kits = join(process.env['ProgramFiles(x86)'] ?? 'C:\\Program Files (x86)', 'Windows Kits', '10', 'bin');
  const versions = existsSync(kits) ? readdirSync(kits).filter((v) => existsSync(join(kits, v, 'x64', 'signtool.exe'))).sort() : [];
  if (!versions.length) throw new BuildError('signtool.exe not found: install the Windows SDK signing tools or set SAC_SIGNTOOL');
  return join(kits, versions.at(-1), 'x64', 'signtool.exe');
}

/** signtool arguments for the configured signer. */
export function signerArgs(env = process.env) {
  const configured = ['SAC_SIGN_DLIB', 'SAC_SIGN_THUMBPRINT', 'SAC_SIGN_PFX'].filter((k) => env[k]);
  if (configured.length !== 1) throw new BuildError('signing needs exactly one of SAC_SIGN_DLIB (with SAC_SIGN_METADATA), SAC_SIGN_THUMBPRINT or SAC_SIGN_PFX');
  const base = ['sign', '/v', '/fd', 'SHA256', '/td', 'SHA256'];
  if (env.SAC_SIGN_DLIB) {
    if (!env.SAC_SIGN_METADATA) throw new BuildError('Trusted Signing needs SAC_SIGN_METADATA (the account and profile metadata.json)');
    return [...base, '/tr', env.SAC_SIGN_TIMESTAMP_URL || 'http://timestamp.acs.microsoft.com', '/dlib', env.SAC_SIGN_DLIB, '/dmdf', env.SAC_SIGN_METADATA];
  }
  const ts = ['/tr', env.SAC_SIGN_TIMESTAMP_URL || 'http://timestamp.digicert.com'];
  if (env.SAC_SIGN_THUMBPRINT) return [...base, ...ts, '/sha1', env.SAC_SIGN_THUMBPRINT];
  return [...base, ...ts, '/f', env.SAC_SIGN_PFX, ...(env.SAC_SIGN_PFX_PASSWORD ? ['/p', env.SAC_SIGN_PFX_PASSWORD] : [])];
}

/** Build <outDir>\ShadowAICapture.msi from a Windows stage. `wixEula` accepts the WiX EULA by id. */
export function buildMsi({ stage, outDir, version, sign = false, wixEula, log = console.log }) {
  const wix = findWix();
  const signArgs = sign ? signerArgs() : null;
  const signtool = sign ? findSigntool() : null;
  const signFile = (file) => {
    run(signtool, [...signArgs, file], `signtool ${file}`);
    log(`signed ${file}`);
  };
  if (sign) for (const exe of ['capture-core.exe', 'classifier-host.exe']) signFile(join(stage, 'bin', exe));

  const msi = join(outDir, MSI_FILE);
  const args = ['build', WXS, '-arch', 'x64', '-pdbtype', 'none', '-d', `ProductVersion=${version}`, '-d', `StageDir=${stage}`, '-o', msi];
  if (wixEula) args.push('-acceptEula', wixEula);
  run(wix, args, 'wix build');
  if (sign) signFile(msi);
  return msi;
}

/** What a built MSI says about itself, through Windows Installer's COM object (Read-MsiInfo.ps1). */
export function readMsi(msi, tables = []) {
  const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'device', 'installer', 'windows', 'Read-MsiInfo.ps1'), '-Path', msi];
  if (tables.length) args.push('-Table', tables.join(','));
  return JSON.parse(powershell(args, 'Read-MsiInfo.ps1'));
}

/** Whether Windows reads a valid Authenticode signature on the file. */
export function isSigned(file) {
  const out = powershell(['-NoProfile', '-Command', `(Get-AuthenticodeSignature -LiteralPath '${file.replace(/'/g, "''")}').Status`], 'Get-AuthenticodeSignature');
  return out.trim() === 'Valid';
}

const sha256 = (file) => createHash('sha256').update(readFileSync(file)).digest('hex');

function findFile(dir, name) {
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) {
      const hit = findFile(p, name);
      if (hit) return hit;
    } else if (entry.name.toLowerCase() === name.toLowerCase()) return p;
  }
  return null;
}

/**
 * Check a release directory against the package it describes. Returns [name, ok, detail] rows:
 * release-msi.mjs refuses a build with a failing row, and verify.mjs --release reports them.
 */
export function releaseChecks(dir) {
  const rows = [];
  const check = (name, ok, detail = '') => rows.push([name, Boolean(ok), detail]);
  const msi = join(dir, MSI_FILE);
  const releaseJson = join(dir, 'release.json');
  if (!existsSync(msi) || !existsSync(releaseJson)) {
    check(`${dir} holds ${MSI_FILE} and release.json`, false, 'build it with node device/installer/release-msi.mjs');
    return rows;
  }
  const release = JSON.parse(readFileSync(releaseJson, 'utf8'));
  const info = readMsi(msi, ['File', 'MoveFile', 'ServiceInstall', 'Registry', 'MsiLockPermissionsEx', 'CustomAction', 'InstallExecuteSequence']);

  check(
    'release.json describes the MSI',
    release.product_code === info.product_code && release.upgrade_code === info.upgrade_code && release.version === info.version && release.package_code === info.package_code && release.sha256 === sha256(msi) && release.size === statSync(msi).size,
    `${release.product_code} ${release.version}`,
  );
  check("the UpgradeCode is the manifest's", (info.upgrade_code ?? '').toUpperCase() === `{${PRODUCT.upgradeCode}}`, info.upgrade_code);

  const files = info.tables.File.map((f) => f.FileName.split('|').pop().toLowerCase());
  check('the MSI installs no tenant file', !files.some((f) => f === 'tenant.env' || f === TENANT_PACKAGE.fileName.toLowerCase()), `${files.length} files`);
  const move = info.tables.MoveFile.find((m) => m.SourceName === TENANT_PACKAGE.fileName);
  check(
    'it copies the tenant file from the folder it runs from into the profile folder',
    move && move.SourceFolder === 'TENANTENV_DIR' && move.DestFolder === 'PROFILEFOLDER' && move.DestName === 'tenant.env',
    move ? `${move.SourceFolder}\\${move.SourceName} -> ${move.DestFolder}\\${move.DestName}` : 'no MoveFile row',
  );
  const svc = info.tables.ServiceInstall[0]?.Arguments ?? '';
  check('its service reads capture-core.env, then tenant.env', /--config-file "\[PROFILEFOLDER\]capture-core\.env" --config-file "\[PROFILEFOLDER\]tenant\.env"/.test(svc), svc);
  const hostFile = info.tables.File.find((f) => f.FileName.split('|').pop() === `${NATIVE_HOST.name}.json`);
  const registered = NATIVE_HOST.registryKeys.filter((key) =>
    info.tables.Registry.some((r) => r.Root === '2' && r.Key === key && !r.Name && hostFile && r.Value === `[#${hostFile.File}]`),
  );
  check('it registers the native messaging host for Chrome and Edge', registered.length === NATIVE_HOST.registryKeys.length, registered.join(', ') || 'no HKLM registration');
  const locks = Object.fromEntries(info.tables.MsiLockPermissionsEx.map((l) => [l.LockObject, l.SDDLText]));
  check(
    'it locks profile and state to SYSTEM and Administrators and lets users read cli',
    Object.entries(DATA_FOLDER_ACL).every(([d, sddl]) => locks[d] === sddl),
    Object.keys(locks).join(', ') || 'no MsiLockPermissionsEx rows',
  );
  // The cleanup is an executable from an installed file (base type 18), run from the script (0x400)
  // as SYSTEM (0x800) with its exit code ignored (0x40), between StopServices and RemoveFiles.
  const coreFile = info.tables.File.find((f) => f.FileName.split('|').pop().toLowerCase() === 'capture-core.exe');
  const cleanup = info.tables.CustomAction.find((a) => a.Action === 'UninstallCleanup');
  const type = Number(cleanup?.Type ?? 0);
  const seq = Object.fromEntries(info.tables.InstallExecuteSequence.map((r) => [r.Action, r]));
  const at = (action) => Number(seq[action]?.Sequence ?? NaN);
  check(
    'a full uninstall runs capture-core --uninstall-cleanup as SYSTEM after StopServices and before RemoveFiles, ignoring its exit code',
    cleanup && coreFile && cleanup.Source === coreFile.File && (type & 0x3f) === 18 && (type & 0xc40) === 0xc40 &&
      cleanup.Target === `${UNINSTALL_CLEANUP.argument} --config-file "[PROFILEFOLDER]capture-core.env" --config-file "[PROFILEFOLDER]tenant.env"` &&
      seq.UninstallCleanup?.Condition === UNINSTALL_CLEANUP.condition &&
      at('StopServices') < at('UninstallCleanup') && at('UninstallCleanup') < at('RemoveFiles'),
    cleanup ? `type ${type}, sequence ${at('StopServices')} < ${at('UninstallCleanup')} < ${at('RemoveFiles')}` : 'no UninstallCleanup custom action',
  );
  check(
    'it records its version where MDM detection rules read it',
    info.tables.Registry.some((r) => r.Root === '2' && r.Key === INSTALLED.registryKey && r.Name === INSTALLED.versionValue && r.Value === '[ProductVersion]'),
    `HKLM\\${INSTALLED.registryKey} ${INSTALLED.versionValue}`,
  );
  check('release.json records whether the MSI is signed', release.signed === isSigned(msi), `signed: ${release.signed}`);

  const crx = join(dir, CRX_FILE);
  const ext = release.extension ?? {};
  check(
    'release.json describes the extension CRX beside the MSI',
    existsSync(crx) && ext.file === CRX_FILE && ext.id === extensionId() && ext.version === release.version && ext.sha256 === sha256(crx) && ext.size === statSync(crx).size,
    existsSync(crx) ? `${ext.id} ${ext.version}` : `${CRX_FILE} missing`,
  );

  const statementFile = join(dir, AGENT_RELEASE_FILE);
  const statement = existsSync(statementFile) && release.trust?.classifier_pubkey
    ? verifyAgentRelease(readFileSync(statementFile, 'utf8'), release.trust.classifier_pubkey) : null;
  check(
    `${AGENT_RELEASE_FILE} names the MSI, signed with the pinned classifier key`,
    statement && statement.type === 'agent_release' && statement.platform === 'windows-amd64' && statement.version === release.version &&
      statement.file === MSI_FILE && statement.sha256 === release.sha256 && statement.size === release.size,
    statement ? `${statement.version} ${statement.sha256}` : `${AGENT_RELEASE_FILE} missing or its signature does not verify`,
  );

  // An administrative install unpacks the files without installing or registering anything.
  const image = mkdtempSync(join(tmpdir(), 'sac-msi-admin-'));
  try {
    const admin = spawnSync('msiexec', ['/a', msi, '/qn', `TARGETDIR=${image}`]);
    const vendorFile = admin.status === 0 ? findFile(image, 'capture-core.env') : null;
    const hostManifest = admin.status === 0 ? findFile(image, `${NATIVE_HOST.name}.json`) : null;
    if (!vendorFile || !hostManifest) {
      check('an administrative extraction yields the vendor file and the host manifest', false, `msiexec /a exit ${admin.status}`);
    } else {
      const pairs = Object.fromEntries([...readFileSync(vendorFile, 'utf8').matchAll(/^([A-Z0-9_]+)=(.*)$/gm)].map((m) => [m[1], m[2].trim()]));
      const tenantKeys = TENANT_PACKAGE.keys.filter((k) => k in pairs);
      check('the installed vendor file carries no tenant key', tenantKeys.length === 0, tenantKeys.join(', ') || `${Object.keys(pairs).length} keys`);
      const pinned = TRUST_ANCHORS.every((k) => pairs[k] && pairs[k] === release.trust?.[k.slice(4).toLowerCase()]);
      check('it pins the trust anchors release.json records', pinned, `policy ${pairs.SAC_POLICY_KEY_ID}`);
      const host = JSON.parse(readFileSync(hostManifest, 'utf8'));
      check(
        "the host manifest allows the extension's pinned id and runs capture-core.exe",
        host.name === NATIVE_HOST.name && host.path === 'capture-core.exe' && host.allowed_origins?.[0] === `chrome-extension://${extensionId()}/`,
        host.allowed_origins?.[0],
      );
    }
  } finally {
    rmSync(image, { recursive: true, force: true });
  }
  return rows;
}
