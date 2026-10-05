#!/usr/bin/env node
// installer/render.mjs - generate every platform artefact from installer/manifest.mjs.
//
//   node installer/render.mjs           # (re)write installer/generated/**
//   node installer/render.mjs --check   # fail if generated output differs from the manifest
//   node installer/render.mjs --list    # print the configuration catalogue and exit
//   node installer/render.mjs --generic --os windows [--policy-key HEX] [--policy-key-id ID] [--classifier-pubkey HEX]
//                                       # print the generic, vendor-wide capture-core.env a package installs
//
// The check is the same one contracts/tools/generate.mjs runs for the wire contract: the committed
// output is a function of one source, and drift is a build failure rather than a review comment.
// Nothing here writes a tenant, a token or a key - the generated files carry layout and
// placeholders only; the tenant package file (ShadowAICapture.tenant.env) carries the values.

import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { CONFIG, LAYOUT, PRODUCT, TENANT_PACKAGE, configFor, expandDefault, genericProfile } from './manifest.mjs';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const OUT = join(ROOT, 'installer', 'generated');
const ARGS = process.argv.slice(2);
const CHECK = ARGS.includes('--check');
const LIST = ARGS.includes('--list');
const ARGS_IDX = ARGS.indexOf('--args');

function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}

/**
 * The argv-rendering mode. A config file is a set of per-tenant facts; this turns it into the exact
 * flag list capture-core resolves, so the Windows MSI, the macOS pkg and the Linux wrapper all
 * consume one mapping rather than three. Bools are `--flag=value` because Go's flag package does
 * not consume a separate token for them.
 */
if (ARGS_IDX !== -1) {
  const file = ARGS[ARGS_IDX + 1];
  if (!file) {
    console.error('render.mjs --args requires a config file path');
    process.exit(2);
  }
  const os = valueOf('--os', 'linux');
  if (!LAYOUT[os]) {
    console.error(`render.mjs --args: --os must be one of ${Object.keys(LAYOUT).join(', ')}`);
    process.exit(2);
  }
  const values = {};
  for (const line of readFileSync(file, 'utf8').split('\n')) {
    const m = /^([A-Z0-9_]+)=(.*)$/.exec(line.trim());
    if (m) values[m[1]] = m[2];
  }
  const parts = [];
  for (const c of configFor(os)) {
    const v = c.env in values ? values[c.env] : expandDefault(c, LAYOUT[os]);
    if (v === undefined || v === '') continue;
    if (c.kind === 'bool') parts.push(`${c.flag}=${v}`);
    else if (os === 'windows') parts.push(`${c.flag} "${v}"`);
    else parts.push(`${c.flag} "${v}"`);
  }
  process.stdout.write(parts.join(' ') + '\n');
  process.exit(0);
}

/**
 * The complete-profile mode. A profile may be a partial override (the lab one names the endpoint,
 * the CA and the token but not the install-layout paths). capture-core resolves only what the file
 * contains, so the installer must write a COMPLETE file: this merges the profile over the platform
 * defaults from the manifest and prints every key as KEY=VALUE.
 *
 *   node installer/render.mjs --env [FILE] --os windows
 */
const ENV_IDX = ARGS.indexOf('--env');
if (ENV_IDX !== -1) {
  const candidate = ARGS[ENV_IDX + 1];
  const file = candidate && !candidate.startsWith('--') ? candidate : '';
  const os = valueOf('--os', 'linux');
  if (!LAYOUT[os]) {
    console.error(`render.mjs --env: --os must be one of ${Object.keys(LAYOUT).join(', ')}`);
    process.exit(2);
  }
  const values = {};
  if (file) {
    for (const line of readFileSync(file, 'utf8').split('\n')) {
      const m = /^([A-Z0-9_]+)=(.*)$/.exec(line.trim());
      if (m) values[m[1]] = m[2];
    }
  }
  const lines = [`# ${PRODUCT.displayName} enrolment profile (complete; profile merged over the ${os} defaults).`];
  for (const c of configFor(os)) {
    const v = c.env in values ? values[c.env] : expandDefault(c, LAYOUT[os]);
    lines.push(`${c.env}=${v}`);
  }
  process.stdout.write(lines.join('\n') + '\n');
  process.exit(0);
}

/**
 * The generic mode: the vendor-wide capture-core.env a package installs for every tenant. The trust
 * anchors are build inputs (release-msi.mjs resolves them), so they are arguments, not manifest
 * constants; without them the file pins nothing.
 */
function genericEnv(os, anchors) {
  const lines = [
    `# ${PRODUCT.displayName} - vendor-wide configuration (generic package; installer/render.mjs --generic).`,
    `# Read first; ${TENANT_PACKAGE.fileName}, installed beside it as tenant.env, is read after it and wins.`,
    '# It carries no tenant, key or token. Do not edit: an upgrade replaces it.',
  ];
  for (const [env, v] of genericProfile(os, anchors)) lines.push(`${env}=${v}`);
  return lines.join('\n') + '\n';
}

if (ARGS.includes('--generic')) {
  const os = valueOf('--os', 'linux');
  if (!LAYOUT[os]) {
    console.error(`render.mjs --generic: --os must be one of ${Object.keys(LAYOUT).join(', ')}`);
    process.exit(2);
  }
  const anchors = {};
  for (const [flag, env] of [['--policy-key', 'SAC_POLICY_KEY'], ['--policy-key-id', 'SAC_POLICY_KEY_ID'], ['--classifier-pubkey', 'SAC_CLASSIFIER_PUBKEY']]) {
    const v = valueOf(flag, undefined);
    if (v !== undefined) anchors[env] = v;
  }
  process.stdout.write(genericEnv(os, anchors));
  process.exit(0);
}

if (LIST) {
  for (const c of CONFIG) {
    const req = c.required ? ' required' : '';
    const sec = c.secret ? ' secret' : '';
    const scope = c.scope ? ` ${c.scope}` : '';
    console.log(`${c.env.padEnd(26)} ${c.flag.padEnd(22)} ${c.kind}${req}${sec}${scope}`);
    console.log(`  ${c.desc}`);
  }
  process.exit(0);
}

const GENERATED_BY = 'generated by installer/render.mjs from installer/manifest.mjs - edit the manifest, not this file';

// ---------------------------------------------------------------------------------------------
// Helpers

/** A one-line comment a service manager will not confuse with content. */
function comment(text) {
  return `# ${text}`;
}

/**
 * The env file: every variable, grouped by the section comments in CONFIG order, with secrets left
 * empty. This is the file the customer's MDM replaces; the committed one is the template.
 */
function envExample() {
  const lines = [comment(GENERATED_BY), comment(`${PRODUCT.displayName} ${PRODUCT.version} - endpoint configuration template`), ''];
  lines.push(comment('This is the CATALOGUE, not an installed file. A device reads two files with repeated'));
  lines.push(comment('--config-file, the later winning: capture-core.env, the vendor-wide file the package'));
  lines.push(comment('installs (render.mjs --generic; no [tenant] or [lab] key), then tenant.env, copied from'));
  lines.push(comment(`${TENANT_PACKAGE.fileName} beside the package (the [tenant] keys; control-api writes it).`));
  lines.push(comment('[lab] keys are optional overrides for a lab profile or a local run.'));
  lines.push('');
  const LinuxL = LAYOUT.linux;
  let section = '';
  for (const c of CONFIG) {
    if (c.section !== section) {
      section = c.section ?? '';
      if (section) lines.push('', comment(section));
    }
    lines.push(comment(c.scope ? `[${c.scope}] ${c.desc}` : c.desc));
    const value = c.secret ? '' : expandDefault(c, LinuxL);
    lines.push(`${c.env}=${value}`);
  }
  lines.push('');
  return lines.join('\n');
}

/**
 * The POSIX wrapper. It no longer maps the profile to flags itself: the agent reads the files with
 * --config-file, so the flag names live in one place (capture-core) instead of three, and a path or
 * secret with spaces needs no quoting. The tenant file is passed second so it wins; a prefix install
 * for development has only the one file, so its absence is not an error here.
 */
function shWrapper(layout) {
  return [
    '#!/bin/sh',
    comment(GENERATED_BY),
    comment('Exec capture-core with the vendor file, then the tenant file (the later --config-file wins).'),
    'set -eu',
    '',
    `CONFIG="\${SAC_CONFIG_FILE:-${layout.configFile}}"`,
    `TENANT="\${SAC_TENANT_FILE:-${layout.tenantFile}}"`,
    'if [ ! -f "$CONFIG" ]; then',
    '  echo "capture-core-run: configuration file $CONFIG not found" >&2',
    '  exit 1',
    'fi',
    '',
    `BIN="\${SAC_BINDIR:-${layout.bindir}}/capture-core"`,
    'if [ ! -x "$BIN" ]; then',
    '  echo "capture-core-run: $BIN is missing or not executable" >&2',
    '  exit 1',
    'fi',
    '',
    'if [ -f "$TENANT" ]; then',
    '  exec "$BIN" --config-file "$CONFIG" --config-file "$TENANT"',
    'fi',
    'exec "$BIN" --config-file "$CONFIG"',
    '',
  ].join('\n');
}

/** The Windows console helper, for a foreground run. The service reads the same two files. */
function cmdWrapper(layout) {
  return [
    '@echo off',
    'rem ' + GENERATED_BY,
    'rem Exec capture-core with the vendor file, then the tenant file (the later config-file wins).',
    'setlocal enableextensions',
    `set "CONFIG=%SAC_CONFIG_FILE%"`,
    `if not defined CONFIG set "CONFIG=${layout.configFile}"`,
    `set "TENANT=%SAC_TENANT_FILE%"`,
    `if not defined TENANT set "TENANT=${layout.tenantFile}"`,
    'if not exist "%CONFIG%" (',
    '  echo capture-core-run: configuration file %CONFIG% not found 1>&2',
    '  exit /b 1',
    ')',
    'if exist "%TENANT%" (',
    `  "${layout.bindir}\\capture-core.exe" --config-file "%CONFIG%" --config-file "%TENANT%"`,
    ') else (',
    `  "${layout.bindir}\\capture-core.exe" --config-file "%CONFIG%"`,
    ')',
    'exit /b %ERRORLEVEL%',
    '',
  ].join('\n');
}

/**
 * The tenant package file, as control-api writes it (CRLF there; this example is LF so it diffs
 * cleanly). It is the one tenant-specific input; every value here is a placeholder.
 */
function tenantEnvExample() {
  const placeholder = {
    SAC_TENANT_ID: '00000000-0000-0000-0000-000000000000',
    SAC_DEVICE_ENDPOINT: 'https://devices.<region>.example.com',
    SAC_DEPLOYMENT_KEY: '',
    SAC_AUTH_MODE: 'dpop',
  };
  const lines = [
    comment(GENERATED_BY),
    comment(`${TENANT_PACKAGE.fileName} - the tenant package file (docs/05-platform-delivery.md §6.1).`),
    comment('control-api writes one per package download, beside the generic installer; the installer'),
    comment('copies it to the tenant.env the agent reads after its vendor-wide file. Nothing else'),
    comment('tenant-specific is in a package. The deployment key is minted per download and is secret.'),
  ];
  for (const k of TENANT_PACKAGE.keys) lines.push(`${k}=${placeholder[k] ?? ''}`);
  lines.push('');
  return lines.join('\n');
}

/** The systemd unit. Restart-on-failure with a start limit, per cmd/capture-core/README.md §3.5. */
function systemdUnit(layout) {
  return [
    comment(GENERATED_BY),
    '[Unit]',
    `Description=${PRODUCT.displayName} endpoint agent`,
    'Documentation=file:/opt/shadow-ai-capture/README.txt',
    'After=network-online.target',
    'Wants=network-online.target',
    '',
    '[Service]',
    'Type=simple',
    `EnvironmentFile=${layout.configFile}`,
    `Environment=SAC_BINDIR=${layout.bindir}`,
    `ExecStart=${layout.bindir}/capture-core-run`,
    '# §3.5: no tight crash loop; a crash loop must stop and report absent, not restart forever.',
    'Restart=on-failure',
    'RestartSec=5',
    'StartLimitIntervalSec=60',
    'StartLimitBurst=5',
    '',
    '# The trust store and the system proxy are machine scope, so the service is root. The sandbox',
    '# keeps it to its own state and log directories.',
    'User=root',
    'NoNewPrivileges=true',
    'ProtectSystem=strict',
    'ProtectHome=true',
    'PrivateTmp=true',
    `ReadWritePaths=${layout.statedir} ${layout.logdir}`,
    '',
    '[Install]',
    'WantedBy=multi-user.target',
    '',
  ].join('\n');
}

/** The LaunchDaemon. KeepAlive on non-zero exit only: a clean stop is not a restart signal. */
function launchdPlist(layout) {
  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">',
    '<!-- ' + GENERATED_BY + ' -->',
    '<plist version="1.0">',
    '<dict>',
    '  <key>Label</key>',
    `  <string>${layout.serviceName}</string>`,
    '  <key>ProgramArguments</key>',
    '  <array>',
    `    <string>${layout.bindir}/capture-core-run</string>`,
    '  </array>',
    '  <key>EnvironmentVariables</key>',
    '  <dict>',
    '    <key>SAC_CONFIG_FILE</key>',
    `    <string>${layout.configFile}</string>`,
    '    <key>SAC_TENANT_FILE</key>',
    `    <string>${layout.tenantFile}</string>`,
    '    <key>SAC_BINDIR</key>',
    `    <string>${layout.bindir}</string>`,
    '  </dict>',
    '  <key>RunAtLoad</key>',
    '  <true/>',
    '  <key>KeepAlive</key>',
    '  <dict>',
    '    <key>SuccessfulExit</key>',
    '    <false/>',
    '  </dict>',
    '  <key>StandardOutPath</key>',
    `  <string>${layout.logdir}/capture-core.out.log</string>`,
    '  <key>StandardErrorPath</key>',
    `  <string>${layout.logdir}/capture-core.err.log</string>`,
    '</dict>',
    '</plist>',
    '',
  ].join('\n');
}

/**
 * The WiX source (v5 or later: it harvests directories with <Files>). One source builds both
 * packages. The generic one is code only: binaries, the signed classifier release, the vendor-wide
 * capture-core.env, and a service that reads that file and then tenant.env. tenant.env is not in the
 * package: it is copied at install from ShadowAICapture.tenant.env in the folder the MSI runs from
 * (the Intune content folder, an extracted ZIP), so one signed MSI serves every tenant and the
 * install command takes no properties. The lab build adds the files its profile points at
 * (-d ProfileExtras) and clears the device state on upgrade (-d FreshEnrolment).
 *
 * Why the tenant-file check is a custom action and not a launch condition: a launch condition can
 * only test properties AppSearch set, AppSearch must run before costing (ICE27), and the folder the
 * MSI runs from is known only once ResolveSource has run, after CostInitialize. An AppSearch over
 * [SourceDir] finds nothing and one over [OriginalDatabase]\.. is refused (MSI note 1324); both were
 * tried. So ResolveSource is scheduled for a first install or a major upgrade (never for a removal,
 * where it would prompt for the source), its result is copied into TENANTENV_DIR (MoveFiles rejects
 * SourceDir itself as a source folder, error 2706), and an immediate check after CostFinalize fails
 * the install before anything is changed or removed, with error 1722 whose logged command line
 * states the reason. MoveFiles overwrites an earlier tenant.env when a new one is supplied, and a
 * copied file is not removed by an uninstall, so a major upgrade without one keeps the installed
 * file; CaRemoveData removes it on a full uninstall. All of this was exercised with a per-user probe
 * package built from the same elements.
 */
function wixSource(layout) {
  const component = (id, dir, sub, file, extra = '') =>
    [
      `      <Component Id="Cmp_${id}" Directory="${dir}" Guid="*">`,
      `        <File Id="Fil_${id}" Source="$(var.StageDir)\\${sub}\\${file}" KeyPath="yes" />`,
      ...(extra ? [extra] : []),
      '      </Component>',
    ].join('\n');
  const binComponents = [
    component('classifier_host', 'BINFOLDER', 'bin', 'classifier-host.exe'),
    component('capture_core_run', 'BINFOLDER', 'bin', 'capture-core-run.cmd'),
  ].join('\n');
  const tenantName = TENANT_PACKAGE.fileName;
  const tenantInstalled = layout.tenantFile.slice(layout.profiledir.length + 1);
  const configInstalled = layout.configFile.slice(layout.profiledir.length + 1);
  const configComponent = component(
    'capture_core_config',
    'PROFILEFOLDER',
    'etc',
    configInstalled,
    [
      `        <CopyFile Id="CopyTenantEnv" SourceProperty="TENANTENV_DIR" SourceName="${tenantName}"`,
      `                  DestinationDirectory="PROFILEFOLDER" DestinationName="${tenantInstalled}" />`,
    ].join('\n'),
  );
  const componentRefs = ['Cmp_Service', 'Cmp_classifier_host', 'Cmp_capture_core_run', 'Cmp_capture_core_config']
    .map((id) => `      <ComponentRef Id="${id}" />`)
    .join('\n');
  // Machine-context commands the install runs. None of them can fail the install: a start that
  // fails or a directory that is already gone must never abort the transaction and roll the files
  // back.
  const action = (id, command, ret = 'ignore') =>
    [
      `    <CustomAction Id="${id}" Directory="CommonAppDataFolder" Execute="deferred" Impersonate="no" Return="${ret}"`,
      `                  ExeCommand="${command}" />`,
    ].join('\n');
  const rmdir = (dir) => `&quot;[System64Folder]cmd.exe&quot; /c rmdir /s /q &quot;[${dir}]&quot;`;
  const reason =
    `Shadow AI Capture needs ${tenantName} beside ShadowAICapture.msi: none was found there and this device ` +
    'has no tenant configuration installed. Download the deployment package from Settings, Deployment and ' +
    'run the MSI from the folder that holds both files.';
  const check =
    `&quot;[System64Folder]cmd.exe&quot; /d /c if exist &quot;[TENANTENV_DIR]${tenantName}&quot; (exit 0) ` +
    `else if exist &quot;[PROFILEFOLDER]${tenantInstalled}&quot; (exit 0) else (echo ${reason} &amp; exit 1)`;

  return [
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<!-- ' + GENERATED_BY + ' -->',
    '<!--',
    '  The Windows package (docs/05-platform-delivery.md section 6.1). The generic build is code only:',
    '  the binaries, the signed classifier release, the vendor-wide capture-core.env and a service that',
    `  reads it and then tenant.env, which is copied at install from ${tenantName} in the folder`,
    '  the MSI runs from. The install command takes no properties; a missing tenant file fails the',
    '  install before anything changes. render.mjs explains each choice above wixSource. The lab build',
    '  adds the files its profile points at (ProfileExtras) and resets device state (FreshEnrolment).',
    '  (An XML comment cannot contain a double hyphen; keep it out of comments here.)',
    '-->',
    '<Wix xmlns="http://wixtoolset.org/schemas/v4/wxs">',
    '  <?ifndef ProductVersion ?>',
    `  <?define ProductVersion = "${PRODUCT.version}" ?>`,
    '  <?endif ?>',
    `  <Package Name="${PRODUCT.displayName}" Manufacturer="${PRODUCT.manufacturer}"`,
    `           Version="$(var.ProductVersion)" UpgradeCode="${PRODUCT.upgradeCode}"`,
    '           Scope="perMachine" Compressed="yes">',
    '    <!-- A rebuilt MSI of the same version must replace the installed one rather than register a',
    '         second product beside it; every build has its own ProductCode. -->',
    '    <MajorUpgrade AllowSameVersionUpgrades="yes"',
    '                  DowngradeErrorMessage="A newer version of Shadow AI Capture is already installed." />',
    '    <MediaTemplate EmbedCab="yes" />',
    '',
    '    <Property Id="ARPNOMODIFY" Value="1" />',
    '    <Property Id="ARPNOREPAIR" Value="1" />',
    '',
    '    <StandardDirectory Id="ProgramFiles64Folder">',
    '      <Directory Id="INSTALLFOLDER" Name="ShadowAICapture">',
    '        <Directory Id="BINFOLDER" Name="bin" />',
    '        <Directory Id="CLASSIFIERFOLDER" Name="classifier" />',
    '      </Directory>',
    '    </StandardDirectory>',
    '    <StandardDirectory Id="CommonAppDataFolder">',
    '      <Directory Id="CONFIGFOLDER" Name="ShadowAICapture">',
    '        <Directory Id="PROFILEFOLDER" Name="profile" />',
    '        <Directory Id="STATEFOLDER" Name="state" />',
    '        <Directory Id="LOGFOLDER" Name="log" />',
    '      </Directory>',
    '    </StandardDirectory>',
    '',
    '    <DirectoryRef Id="BINFOLDER">',
    binComponents,
    `      <Component Id="Cmp_Service" Directory="BINFOLDER" Guid="*">`,
    '        <File Id="Fil_ServiceExe" Source="$(var.StageDir)\\bin\\capture-core.exe" KeyPath="yes" />',
    '        <!-- The vendor file first, the tenant file second: the later config-file wins. -->',
    '        <ServiceInstall Id="SvcCaptureCore" Name="ShadowAICapture" DisplayName="Shadow AI Capture"',
    '                        Description="Observes AI submissions on this device and drains them to the tenant ingress."',
    '                        Type="ownProcess" Start="auto" ErrorControl="normal" Account="LocalSystem"',
    `                        Arguments="--service --service-name ShadowAICapture --config-file &quot;[PROFILEFOLDER]${configInstalled}&quot; --config-file &quot;[PROFILEFOLDER]${tenantInstalled}&quot;" />`,
    '        <!-- ServiceControl does not start the service: a start it waited on and that failed would',
    '             abort the whole install transaction and roll the files back, leaving nothing to',
    '             diagnose. CaStartService below starts it instead, and ignores the outcome. -->',
    '        <ServiceControl Id="SvcCaptureCoreControl" Name="ShadowAICapture" Stop="both" Remove="uninstall" Wait="yes" />',
    '      </Component>',
    '    </DirectoryRef>',
    '    <!-- The vendor-wide file, and the copy of the tenant file from the folder the MSI runs from. -->',
    '    <DirectoryRef Id="PROFILEFOLDER">',
    configComponent,
    '    </DirectoryRef>',
    '    <!-- The signed classifier release, tree preserved. It is vendor content, so it lives with the code. -->',
    '    <ComponentGroup Id="ClassifierFiles" Directory="CLASSIFIERFOLDER">',
    '      <Files Include="$(var.StageDir)\\classifier\\**" />',
    '    </ComponentGroup>',
    '    <?ifdef ProfileExtras ?>',
    '    <!-- Lab only: the files the lab profile points at (signed bundle, device CA, pinned edge CA). -->',
    '    <ComponentGroup Id="ProfileExtras" Directory="PROFILEFOLDER">',
    '      <Files Include="$(var.StageDir)\\profile\\**" />',
    '    </ComponentGroup>',
    '    <?endif ?>',
    '',
    '    <!-- Where the MSI runs from, for a first install or a major upgrade only. -->',
    '    <SetProperty Id="TENANTENV_DIR" Value="[SourceDir]" After="ResolveSource" Sequence="execute" Condition="NOT Installed" />',
    '    <CustomAction Id="CheckTenantConfig" Directory="CommonAppDataFolder" Execute="immediate" Return="check"',
    `                  ExeCommand="${check}" />`,
    '    <!-- Output goes to nul and the action is not waited for. sc.exe run bare from a custom',
    '         action was seen to start the service and then block for good on its own console',
    '         output, which held the installer open; the install is complete once the service is',
    '         registered, so nothing here may wait on a console. -->',
    action('CaStartService', '&quot;[System64Folder]cmd.exe&quot; /c sc.exe start ShadowAICapture &gt;nul 2&gt;&amp;1', 'asyncNoWait'),
    '    <!-- A full uninstall leaves nothing behind: the spool, the sealed credential, the logs and the',
    '         copied tenant.env are not installed files, so Windows Installer would otherwise keep them.',
    '         An upgrade keeps them, because an installer that resets a device identity is worse than',
    '         one that stops. -->',
    action('CaRemoveData', rmdir('CONFIGFOLDER')),
    '    <?ifdef FreshEnrolment ?>',
    '    <!-- This build carries a new single-use enrolment token for a server that does not know the',
    '         credential an earlier install sealed, so the device must enrol again. -->',
    action('CaResetState', rmdir('STATEFOLDER')),
    '    <?endif ?>',
    '    <InstallExecuteSequence>',
    '      <ResolveSource After="CostInitialize" Condition="NOT Installed" />',
    '      <Custom Action="CheckTenantConfig" After="CostFinalize" Condition="NOT Installed" />',
    '      <?ifdef FreshEnrolment ?>',
    '      <Custom Action="CaResetState" After="StopServices" Condition="NOT REMOVE" />',
    '      <?endif ?>',
    '      <Custom Action="CaStartService" After="InstallServices" Condition="NOT REMOVE" />',
    '      <Custom Action="CaRemoveData" After="RemoveFiles" Condition="REMOVE=&quot;ALL&quot; AND NOT UPGRADINGPRODUCTCODE" />',
    '    </InstallExecuteSequence>',
    '',
    '    <Feature Id="Main" Title="Shadow AI Capture" Level="1">',
    componentRefs,
    '      <ComponentGroupRef Id="ClassifierFiles" />',
    '      <?ifdef ProfileExtras ?>',
    '      <ComponentGroupRef Id="ProfileExtras" />',
    '      <?endif ?>',
    '    </Feature>',
    '  </Package>',
    '</Wix>',
    '',
  ].join('\n');
}

// ---------------------------------------------------------------------------------------------
// Output table

const FILES = [
  { path: 'capture-core.env.example', mode: 0o644, render: envExample },
  { path: `${TENANT_PACKAGE.fileName}.example`, mode: 0o644, render: tenantEnvExample },
  { path: 'linux/capture-core-run', mode: 0o755, render: () => shWrapper(LAYOUT.linux) },
  { path: 'linux/shadow-ai-capture.service', mode: 0o644, render: () => systemdUnit(LAYOUT.linux) },
  { path: 'macos/capture-core-run', mode: 0o755, render: () => shWrapper(LAYOUT.darwin) },
  { path: 'macos/com.shadowaicapture.capture-core.plist', mode: 0o644, render: () => launchdPlist(LAYOUT.darwin) },
  { path: 'windows/ShadowAICapture.wxs', mode: 0o644, render: () => wixSource(LAYOUT.windows) },
  { path: 'windows/capture-core-run.cmd', mode: 0o644, render: () => cmdWrapper(LAYOUT.windows) },
];

let drifted = 0;
for (const f of FILES) {
  const abs = join(OUT, f.path);
  const content = f.render();
  const before = existsSync(abs) ? readFileSync(abs, 'utf8') : null;
  if (CHECK) {
    if (before !== content) {
      console.error(`DRIFT ${f.path}`);
      drifted++;
    } else {
      console.log(`ok    ${f.path}`);
    }
    continue;
  }
  mkdirSync(dirname(abs), { recursive: true });
  writeFileSync(abs, content, { mode: f.mode });
  console.log(`write ${f.path}`);
}

if (CHECK && drifted > 0) {
  console.error(`\ninstaller/render.mjs --check: ${drifted} generated file(s) differ from installer/manifest.mjs.`);
  console.error('Run: node installer/render.mjs');
  process.exit(1);
}
if (CHECK) console.log('\ninstaller: generated output matches the manifest.');
