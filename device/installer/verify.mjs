#!/usr/bin/env node
// device/installer/verify.mjs - the installer gate.
//
//   node device/installer/verify.mjs                  # source checks, then capture-core --print-config
//   node device/installer/verify.mjs --no-exec        # source checks only (no Go build)
//   node device/installer/verify.mjs --release DIR    # also check a built release (Windows)
//
// It checks that the configuration catalogue is exactly the one capture-core reads, that the three
// platform packages start the agent the same way and register the extension's native messaging host
// for its pinned id, that the generated files match the manifest, that the generic file can carry no
// tenant data, and that the real binary accepts the generic file followed by a tenant file. A check
// that cannot run is reported SKIP with the reason, never as a pass.

import { spawnSync } from 'node:child_process';
import { mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

import { extensionId } from '../extension/tools/extension-id.mjs';
import { CONFIG, LAYOUT, NATIVE_HOST, START_MENU_SHORTCUT, TENANT_PACKAGE, UNINSTALL_CLEANUP, genericEnv, genericProfile, nativeHostManifest } from './manifest.mjs';
import { DATA_FOLDER_ACL, drift } from './render.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
const { values: opts } = parseArgs({ options: { 'no-exec': { type: 'boolean', default: false }, release: { type: 'string' } } });
const read = (rel) => readFileSync(join(ROOT, rel), 'utf8');

let failures = 0;
let skipped = 0;
function check(name, ok, detail = '') {
  const tag = ok === 'SKIP' ? ' SKIP ' : ok ? '  ok  ' : ' FAIL ';
  console.log(`${tag} ${name}${detail ? ` - ${detail}` : ''}`);
  if (ok === 'SKIP') skipped++;
  else if (!ok) failures++;
}

// ---------------------------------------------------------------------------------------------
// The catalogue is capture-core's catalogue.

const AGENT_DIR = join(ROOT, 'device', 'capture-core', 'cmd', 'capture-core');
const agentSource = readdirSync(AGENT_DIR)
  .filter((f) => f.endsWith('.go') && !f.endsWith('_test.go'))
  .map((f) => readFileSync(join(AGENT_DIR, f), 'utf8'))
  .join('\n');
const agentFlags = new Set([...agentSource.matchAll(/\.(?:String|Bool|Int|Int64|Duration)Var\(\s*[^,]+,\s*"([a-z0-9-]+)"/g)].map((m) => `--${m[1]}`));
const agentCatalogue = new Map([...agentSource.matchAll(/"(SAC_[A-Z0-9_]+)"\s*:\s*"(?:--)?([a-z0-9-]+)"/g)].map((m) => [m[1], `--${m[2]}`]));

check('capture-core registers its flags where this gate can read them', agentFlags.size >= CONFIG.length, `${agentFlags.size} flags`);
const unknownFlags = CONFIG.filter((c) => !agentFlags.has(c.flag)).map((c) => `${c.env}->${c.flag}`);
check('every catalogue key sets a flag capture-core registers', unknownFlags.length === 0, unknownFlags.join(', ') || `${CONFIG.length} keys`);
const catalogueDrift = [
  ...CONFIG.filter((c) => agentCatalogue.get(c.env) !== c.flag).map((c) => `${c.env} (installer)`),
  ...[...agentCatalogue.keys()].filter((k) => !CONFIG.some((c) => c.env === k)).map((k) => `${k} (capture-core)`),
];
check("the catalogue equals capture-core's config-file catalogue", catalogueDrift.length === 0, catalogueDrift.join(', ') || `${agentCatalogue.size} keys`);

// ---------------------------------------------------------------------------------------------
// The catalogue is consistent, and the generic file cannot carry tenant data.

const envs = new Set(CONFIG.map((c) => c.env));
const flags = new Set(CONFIG.map((c) => c.flag));
check('no key or flag is listed twice', envs.size === CONFIG.length && flags.size === CONFIG.length);
check('every key is SAC_*', CONFIG.every((c) => /^SAC_[A-Z0-9_]+$/.test(c.env)));
const tenantScope = CONFIG.filter((c) => c.scope === 'tenant').map((c) => c.env);
check('the tenant package carries exactly the tenant-scope keys', tenantScope.join() === TENANT_PACKAGE.keys.join(), TENANT_PACKAGE.keys.join(', '));
const requiredEmpty = CONFIG.filter((c) => c.required && c.default === '' && c.scope !== 'tenant').map((c) => c.env);
check('every required key without a default comes from the tenant file', requiredEmpty.length === 0, requiredEmpty.join(', ') || 'none');
check('no secret has a default or reaches the generic file', CONFIG.filter((c) => c.secret).every((c) => c.default === '' && c.scope));
for (const os of Object.keys(LAYOUT)) {
  const pairs = genericProfile(os, { SAC_POLICY_KEY: 'a'.repeat(64), SAC_CLASSIFIER_PUBKEY: 'b'.repeat(64) });
  const scoped = pairs.filter(([k]) => CONFIG.find((c) => c.env === k).scope).map(([k]) => k);
  const unexpanded = pairs.filter(([, v]) => v.includes('<')).map(([k]) => k);
  const value = (key) => pairs.find(([k]) => k === key)?.[1];
  check(
    `the generic ${os} file has only vendor keys and names the installed state and classifier directories`,
    scoped.length === 0 && unexpanded.length === 0 && value('SAC_STATE_DIR') === LAYOUT[os].statedir && value('SAC_CLASSIFIER_RELEASE') === LAYOUT[os].classifierdir,
    [...scoped, ...unexpanded].join(', ') || `${pairs.length} keys`,
  );
}

// ---------------------------------------------------------------------------------------------
// The generated files are the manifest's, and every package does the same things.

const drifted = drift();
check('device/installer/generated matches device/installer/manifest.mjs', drifted.length === 0, drifted.join(', ') || 'run node device/installer/render.mjs to regenerate');

const wxs = read('device/installer/generated/windows/ShadowAICapture.wxs');
const unit = read('device/installer/generated/linux/shadow-ai-capture.service');
const plist = read('device/installer/generated/macos/com.shadowaicapture.capture-core.plist');
const installSh = read('device/installer/linux/install.sh');
const buildPkg = read('device/installer/macos/build-pkg.sh');

const tenantExample = read(`device/installer/generated/${TENANT_PACKAGE.fileName}.example`);
check('the tenant file example carries exactly the package keys', [...tenantExample.matchAll(/^([A-Z0-9_]+)=/gm)].map((m) => m[1]).join() === TENANT_PACKAGE.keys.join());

const W = LAYOUT.windows;
check(
  'Windows: the service runs capture-core with the vendor file, then the tenant file',
  wxs.includes(`<ServiceInstall Id="Svc" Name="${W.serviceName}"`) &&
    wxs.includes('Arguments="--config-file &quot;[PROFILEFOLDER]capture-core.env&quot; --config-file &quot;[PROFILEFOLDER]tenant.env&quot;"'),
);
check(
  'Linux: the unit runs capture-core with the vendor file, then the tenant file',
  unit.includes(`ExecStart=${LAYOUT.linux.bindir}/capture-core --config-file ${LAYOUT.linux.configFile} --config-file ${LAYOUT.linux.tenantFile}\n`),
);
const D = LAYOUT.darwin;
check(
  'macOS: the daemon runs capture-core with the vendor file, then the tenant file',
  plist.includes([`${D.bindir}/capture-core`, '--config-file', D.configFile, '--config-file', D.tenantFile].map((a) => `    <string>${a}</string>`).join('\n')),
);
check(
  'Windows: the tenant file is copied from the folder the MSI runs from, and its absence fails the install first',
  /<CopyFile Id="CopyTenantEnv" SourceProperty="TENANTENV_DIR" SourceName="ShadowAICapture\.tenant\.env"\s+DestinationDirectory="PROFILEFOLDER" DestinationName="tenant\.env" \/>/.test(wxs) &&
    wxs.includes('<ResolveSource After="CostInitialize" Condition="NOT Installed" />') &&
    wxs.includes('<Custom Action="CheckTenantConfig" After="CostFinalize" Condition="NOT Installed" />') &&
    !/<File [^>]*tenant\.env/i.test(wxs),
);
check(
  'Windows: profile and state are SYSTEM and Administrators only, and users may read cli',
  Object.entries(DATA_FOLDER_ACL).every(([d, sddl]) => wxs.includes(`<CreateFolder Directory="${d}"><PermissionEx Sddl="${sddl}" /></CreateFolder>`)) &&
    !/;;;BU\)/.test(DATA_FOLDER_ACL.PROFILEFOLDER + DATA_FOLDER_ACL.STATEFOLDER) &&
    wxs.includes('<Directory Id="CLIFOLDER" Name="cli" />'),
);
const S = START_MENU_SHORTCUT;
const helperAumid = /const AppUserModelID = "([^"]+)"/.exec(read('device/capture-core/userhelper/toast.go'))?.[1];
check(
  "Windows: the Start-menu shortcut carries the AppUserModelID capture-core's notifications are shown under",
  helperAumid === S.appUserModelId &&
    /<File Id="Fil_CaptureCore"[^>]*>\s*(?:<!--[^>]*-->\s*)?<Shortcut Id="Sc_StartMenu" Directory="ProgramMenuFolder"/.test(wxs) &&
    wxs.includes(`<ShortcutProperty Key="System.AppUserModel.ID" Value="${S.appUserModelId}" />`),
  helperAumid ? `${S.appUserModelId}, capture-core ${helperAumid}` : 'AppUserModelID not found in device/capture-core/userhelper/toast.go',
);
const U = UNINSTALL_CLEANUP;
const cleanupArg = /const uninstallCleanupArg = "([^"]+)"/.exec(agentSource)?.[1];
const cleanupLog = /const uninstallLogName = "([^"]+)"/.exec(agentSource)?.[1];
const xmlAttr = (v) => v.replace(/&/g, '&amp;').replace(/"/g, '&quot;');
check(
  "Windows: a full uninstall runs capture-core's cleanup as SYSTEM after the service stops, with the service's files, and ignores its exit code",
  cleanupArg === U.argument && cleanupLog === U.log &&
    wxs.includes('<CustomAction Id="UninstallCleanup" FileRef="Fil_CaptureCore" Execute="deferred" Impersonate="no" Return="ignore"') &&
    wxs.includes(`ExeCommand="${U.argument} --config-file &quot;[PROFILEFOLDER]capture-core.env&quot; --config-file &quot;[PROFILEFOLDER]tenant.env&quot;" />`) &&
    wxs.includes(`<Custom Action="UninstallCleanup" After="StopServices" Condition="${xmlAttr(U.condition)}" />`) &&
    U.condition === 'REMOVE="ALL" AND NOT UPGRADINGPRODUCTCODE',
  cleanupArg ? `${U.argument}, capture-core ${cleanupArg}, log ${cleanupLog}` : 'uninstallCleanupArg not found in device/capture-core/cmd/capture-core',
);
check('Windows: the MSI version is a build input with no default', wxs.includes('Version="$(var.ProductVersion)"') && !wxs.includes('define ProductVersion'));

const nativeApp = /NATIVE_APP\s*=\s*'([^']+)'/.exec(read('device/extension/src/native.js'))?.[1];
check("the native messaging host name is the one the extension connects to", nativeApp === NATIVE_HOST.name, nativeApp ?? 'NATIVE_APP not found in device/extension/src/native.js');
const id = extensionId();
for (const [os, dir] of [['windows', 'windows'], ['linux', 'linux'], ['darwin', 'macos']]) {
  const host = JSON.parse(read(`device/installer/generated/${dir}/${NATIVE_HOST.name}.json`));
  const want = nativeHostManifest(os, id);
  check(`${os}: the native messaging host allows chrome-extension://${id}/ and runs ${want.path}`, JSON.stringify(host) === JSON.stringify(want));
}
check(
  'Windows: the host manifest is registered for Chrome and Edge',
  NATIVE_HOST.registryKeys.every((k) => wxs.includes(`<RegistryKey Root="HKLM" Key="${k}" ForceDeleteOnUninstall="yes">`)) && wxs.includes('<RegistryValue Type="string" Value="[#Fil_NativeHost]" />'),
);
check('Linux: install.sh installs the host manifest for Chrome and Edge', NATIVE_HOST.dirs.linux.every((d) => installSh.includes(d)));
check('macOS: the package installs the host manifest for Chrome and Edge', NATIVE_HOST.dirs.darwin.every((d) => buildPkg.includes(d)));

for (const rel of ['device/installer/generated/windows/ShadowAICapture.wxs', 'device/installer/generated/macos/com.shadowaicapture.capture-core.plist']) {
  const bad = [...read(rel).matchAll(/<!--([\s\S]*?)-->/g)].filter((m) => m[1].includes('--') || m[1].endsWith('-'));
  check(`${rel}: XML comments contain no '--'`, bad.length === 0);
}
const hasSh = spawnSync('sh', ['-c', 'true']).status === 0;
for (const rel of ['device/installer/linux/install.sh', 'device/installer/linux/uninstall.sh', 'device/installer/macos/build-pkg.sh', 'device/installer/macos/scripts/preinstall', 'device/installer/macos/scripts/postinstall']) {
  check(`${rel} passes sh -n`, hasSh ? spawnSync('sh', ['-n', join(ROOT, rel)]).status === 0 : 'SKIP', hasSh ? '' : 'no sh on PATH');
}

// ---------------------------------------------------------------------------------------------
// The real binary accepts the generic file followed by a tenant file.

const PRINT = 'capture-core --print-config accepts the generic file, then a tenant file';
if (opts['no-exec']) {
  check(PRINT, 'SKIP', '--no-exec');
} else {
  const hostOs = { win32: 'windows', darwin: 'darwin' }[process.platform] ?? 'linux';
  const work = mkdtempSync(join(tmpdir(), 'sac-installer-verify-'));
  try {
    const bin = join(work, hostOs === 'windows' ? 'capture-core.exe' : 'capture-core');
    const build = spawnSync('go', ['build', '-o', bin, './cmd/capture-core'], { cwd: join(ROOT, 'device', 'capture-core'), encoding: 'utf8', env: { ...process.env, CGO_ENABLED: '0' } });
    if (build.status !== 0) {
      check(PRINT, false, `go build failed: ${`${build.stdout ?? ''}${build.stderr ?? ''}`.trim().split('\n').at(-1)}`);
    } else {
      const generic = join(work, 'capture-core.env');
      const tenant = join(work, 'tenant.env');
      writeFileSync(generic, genericEnv(hostOs, { SAC_POLICY_KEY: 'a'.repeat(64), SAC_POLICY_KEY_ID: 'policy-key-1', SAC_CLASSIFIER_PUBKEY: 'b'.repeat(64) }, '0.0.0'));
      writeFileSync(tenant, 'SAC_TENANT_ID=00000000-0000-4000-8000-00000000000a\nSAC_DEVICE_ENDPOINT=https://devices.example.com\nSAC_DEPLOYMENT_KEY=sacdk_verify\n');
      // The state directory is moved to a scratch folder on the command line (which wins over both
      // files), so the check reads nothing of an agent installed on this machine.
      const res = spawnSync(bin, ['--print-config', '--config-file', generic, '--config-file', tenant, '--state-dir', join(work, 'state')], { encoding: 'utf8' });
      const out = `${res.stdout ?? ''}${res.stderr ?? ''}`;
      check(PRINT, res.status === 0, res.status === 0 ? `${hostOs} build` : out.trim().split('\n').at(-1));
      check('  it resolves the tenant and the device endpoint from the tenant file', out.includes('00000000-0000-4000-8000-00000000000a') && out.includes('https://devices.example.com'));
      check('  it does not print the deployment key', !out.includes('sacdk_verify'));
    }
  } finally {
    rmSync(work, { recursive: true, force: true });
  }
}

// ---------------------------------------------------------------------------------------------
// A built release, read back out of the package.

if (opts.release) {
  if (process.platform !== 'win32') {
    check(`the release in ${opts.release}`, 'SKIP', 'reading an MSI needs Windows Installer');
  } else {
    const { releaseChecks } = await import('./windows/msi.mjs');
    for (const [name, ok, detail] of releaseChecks(resolve(opts.release))) check(name, ok, detail);
  }
}

console.log(`\ninstaller: ${failures === 0 ? 'all checks passed' : `${failures} check(s) FAILED`}${skipped ? ` (${skipped} skipped)` : ''}`);
process.exit(failures === 0 ? 0 : 1);
