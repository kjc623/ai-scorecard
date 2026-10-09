#!/usr/bin/env node
// tools/testbed/deploy.mjs - takes the agent release a push to main built, publishes it to the
// reference VM through Intune, and waits until the VM runs it and reports to pre-prod.
//
//   node tools/testbed/deploy.mjs [--commit <sha>] [--no-wait]
//   node tools/testbed/deploy.mjs --uninstall [--no-wait]
//
// Runs on the owner's Windows PC with the gh CLI signed in. The configuration is
// refactor-endpoint/TESTBED.md (testbed.mjs). Graph calls go through Publish-IntuneBuild.ps1 and VM
// checks through invm.ps1. Every line printed passes through the log's redaction, which knows the
// tenant file's deployment key.

import { spawn, spawnSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { parseArgs } from 'node:util';

import { checkAppId, loadConfig, saveAppId } from './testbed.mjs';

const HERE = dirname(fileURLToPath(import.meta.url));
const ROOT = join(HERE, '..', '..');

// Microsoft's Win32 Content Prep Tool, pinned to the v1.8.7 commit and checked by hash before use.
export const PREP_TOOL = {
  version: '1.8.7',
  url: 'https://raw.githubusercontent.com/microsoft/Microsoft-Win32-Content-Prep-Tool/1d6cfcbdf8c28edc596337031f74df951f38f718/IntuneWinAppUtil.exe',
  sha256: 'c1ba45b5cb939e84af064bb7ff4b38fb3dfe33c8dc1078fd9b157672eae671f6',
};

const RUN_FIELDS = 'databaseId,headSha,status,conclusion,url,number,event';
const POLL_MS = 30_000;
const WAIT_LIMIT_MS = 60 * 60_000;
const TENANT_KEYS = ['SAC_TENANT_ID', 'SAC_DEVICE_ENDPOINT', 'SAC_DEPLOYMENT_KEY'];

/**
 * The output channel. Every line is redacted against the registered secrets before it is
 * written; child output is relayed line by line, so a secret split across chunks is still caught.
 */
export function createLog(write = (s) => process.stdout.write(s)) {
  const secrets = new Set();
  const redact = (text) => {
    let out = String(text);
    for (const s of secrets) out = out.split(s).join('[redacted]');
    return out;
  };
  return {
    secret(value) {
      if (value) secrets.add(value);
    },
    redact,
    line(msg) {
      write(`${redact(`deploy: ${msg}`)}\n`);
    },
    relay(prefix, onLine = () => {}) {
      let pending = '';
      const emit = (raw) => {
        const l = raw.replace(/\r$/, '');
        onLine(l);
        write(`${redact(`${prefix}${l}`)}\n`);
      };
      return {
        push(chunk) {
          pending += chunk;
          const parts = pending.split('\n');
          pending = parts.pop();
          parts.forEach(emit);
        },
        end() {
          if (pending) emit(pending);
          pending = '';
        },
      };
    },
  };
}

/** KEY=VALUE pairs of an env file; comments and blank lines ignored. */
export function parseEnvFile(text) {
  const out = {};
  for (const raw of text.split('\n')) {
    const line = raw.replace(/\r$/, '').trim();
    if (!line || line.startsWith('#')) continue;
    const eq = line.indexOf('=');
    if (eq > 0) out[line.slice(0, eq).trim()] = line.slice(eq + 1).trim();
  }
  return out;
}

/** Checks the saved tenant file is the test tenant's on pre-prod's device edge; returns its deployment key. */
export function checkTenantFile(env, config) {
  const missing = TENANT_KEYS.filter((k) => !env[k]);
  if (missing.length) throw new Error(`the tenant file lacks ${missing.join(', ')}`);
  if (env.SAC_TENANT_ID.toLowerCase() !== config.testTenantId) {
    throw new Error(`the tenant file is for tenant ${env.SAC_TENANT_ID}, not the test tenant ${config.testTenantId}`);
  }
  let host = '';
  try {
    host = new URL(env.SAC_DEVICE_ENDPOINT).hostname.toLowerCase();
  } catch {
    // reported below
  }
  if (host !== config.deviceHostname.toLowerCase()) {
    throw new Error(`the tenant file's device endpoint is not https://${config.deviceHostname}`);
  }
  return env.SAC_DEPLOYMENT_KEY;
}

/**
 * Picks the deploy.yml run that built a commit from `gh run list --json` output: the newest push
 * run whose head is the commit. state is success, running, failed or missing.
 */
export function selectRun(runs, commit) {
  const sha = String(commit).trim().toLowerCase();
  if (!/^[0-9a-f]{7,40}$/.test(sha)) throw new Error(`"${commit}" is not a commit sha`);
  const matches = runs
    .filter((r) => r.event === 'push' && String(r.headSha).toLowerCase().startsWith(sha))
    .sort((a, b) => b.number - a.number);
  const run = matches[0];
  if (!run) return { state: 'missing' };
  if (run.status !== 'completed') return { state: 'running', run };
  return { state: run.conclusion === 'success' ? 'success' : 'failed', run };
}

/** The fields of `dsregcmd /status` the publish needs. */
export function parseDsreg(text) {
  const field = (name) => (String(text).match(new RegExp(`^\\s*${name}\\s*:\\s*(\\S+)\\s*$`, 'mi'))?.[1] ?? '');
  return {
    joined: field('AzureAdJoined').toUpperCase() === 'YES',
    deviceId: field('DeviceId').toLowerCase(),
    tenantId: field('TenantId').toLowerCase(),
  };
}

const guidKey = (g) => String(g ?? '').replace(/[{}]/g, '').toLowerCase();

/** Whether the VM runs the release: installed, running, enrolled, and reporting since it started. */
export function evaluateInstall(probe, release) {
  const waiting = [];
  const products = probe.products ?? [];
  const installed = products.find((p) => guidKey(p.product_code) === guidKey(release.product_code));
  if (!installed || installed.version !== release.version) {
    const now = products.map((p) => p.version).join(', ') || 'nothing';
    waiting.push(`the install of ${release.version} (installed: ${now})`);
  }
  if (probe.service !== 'Running') waiting.push(`the ShadowAICapture service (${probe.service ?? 'absent'})`);
  const h = probe.health;
  if (!h || h.agent_version !== release.version) {
    waiting.push(`health.json from ${release.version}`);
  } else if (!h.enrolled || !h.device_id) {
    waiting.push('a device credential');
  } else if (!h.last_heartbeat_at || !probe.service_started_at || Date.parse(h.last_heartbeat_at) < Date.parse(probe.service_started_at)) {
    waiting.push('a health report acknowledged by pre-prod');
  }
  return { done: waiting.length === 0, waiting };
}

/** Whether the agent is gone from the VM. */
export function evaluateRemoval(probe) {
  const waiting = [];
  if ((probe.products ?? []).length) waiting.push(`the uninstall (installed: ${probe.products.map((p) => p.version).join(', ')})`);
  if (probe.service) waiting.push(`removal of the ShadowAICapture service (${probe.service})`);
  return { done: waiting.length === 0, waiting };
}

// Runs in the VM as its administrator. Single quotes only: it travels as one command-line argument.
const PROBE = [
  "$ErrorActionPreference = 'Stop'",
  "$products = @(Get-ChildItem -Path 'HKLM:\\SOFTWARE\\Microsoft\\Windows\\CurrentVersion\\Uninstall' | ForEach-Object { Get-ItemProperty -Path $_.PSPath } | Where-Object { $_.DisplayName -eq 'Shadow AI Capture' } | ForEach-Object { @{ product_code = $_.PSChildName; version = $_.DisplayVersion } })",
  "$svc = Get-CimInstance -ClassName Win32_Service | Where-Object { $_.Name -eq 'ShadowAICapture' }",
  '$state = $null; $started = $null',
  'if ($svc) { $state = [string]$svc.State; if ($svc.ProcessId) { $started = (Get-Process -Id $svc.ProcessId).StartTime.ToUniversalTime().ToString(\'o\') } }',
  "$health = $null; $file = 'C:\\ProgramData\\ShadowAICapture\\state\\health.json'",
  "if (Test-Path -LiteralPath $file) { $h = Get-Content -LiteralPath $file -Raw | ConvertFrom-Json; $hb = $h.last_heartbeat_at; if ($hb -is [datetime]) { $hb = $hb.ToUniversalTime().ToString('o') }; $health = @{ agent_version = $h.agent_version; enrolled = [bool]$h.enrolled; device_id = [bool]$h.device_id; last_heartbeat_at = $hb } }",
  '@{ products = $products; service = $state; service_started_at = $started; health = $health } | ConvertTo-Json -Compress -Depth 4',
].join('; ');

const imeLog = (appId) => [
  "Get-ChildItem -Path 'C:\\ProgramData\\Microsoft\\IntuneManagementExtension\\Logs' -Filter 'AppWorkload*.log' | Sort-Object LastWriteTime",
  `ForEach-Object { Select-String -LiteralPath $_.FullName -SimpleMatch -Pattern '${appId}' }`,
  'Select-Object -Last 200',
  'ForEach-Object { $_.Line }',
].join(' | ');

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function elapsed(ms) {
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
}

function lastJson(text) {
  const line = String(text).split(/\r?\n/).reverse().find((l) => l.trim().startsWith('{'));
  if (!line) throw new Error('the VM probe returned no JSON');
  return JSON.parse(line);
}

class Tools {
  constructor(config, log) {
    this.config = config;
    this.log = log;
  }

  run(cmd, args, what, cwd = ROOT) {
    const res = spawnSync(cmd, args, { cwd, encoding: 'utf8', maxBuffer: 64 << 20, windowsHide: true });
    if (res.error) throw new Error(`${what}: ${res.error.message}`);
    if (res.status !== 0) throw new Error(`${what} failed (exit ${res.status}):\n${this.log.redact(`${res.stdout ?? ''}${res.stderr ?? ''}`.trim())}`);
    return res.stdout ?? '';
  }

  gh(args, what) {
    return this.run('gh', [...args, '--repo', this.config.githubRepository], what);
  }

  invm(args, what) {
    return this.run('powershell.exe', ['-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', join(HERE, 'invm.ps1'), ...args], what);
  }

  /** Streams a child's output through the log; onLine sees each raw line first. */
  stream(cmd, args, what, onLine) {
    return new Promise((resolve, reject) => {
      const child = spawn(cmd, args, { cwd: ROOT, windowsHide: true });
      let failure = null;
      const handle = (l) => {
        if (failure) return;
        try {
          onLine(l);
        } catch (err) {
          failure = err;
          child.kill();
        }
      };
      const out = this.log.relay('  ', handle);
      const err = this.log.relay('  ');
      child.stdout.setEncoding('utf8').on('data', (c) => out.push(c));
      child.stderr.setEncoding('utf8').on('data', (c) => err.push(c));
      child.on('error', (e) => reject(new Error(`${what}: ${e.message}`)));
      child.on('close', (code) => {
        out.end();
        err.end();
        if (failure) reject(failure);
        else if (code !== 0) reject(new Error(`${what} failed (exit ${code})`));
        else resolve();
      });
    });
  }
}

async function findRelease(tools, commit) {
  const { log } = tools;
  let pick = selectRun(JSON.parse(tools.gh(['run', 'list', '--workflow', 'deploy.yml', '--branch', 'main', '--limit', '100', '--json', RUN_FIELDS], 'gh run list')), commit);
  if (pick.state === 'missing') throw new Error(`no deploy.yml run on main was triggered by a push of ${commit}`);
  while (pick.state === 'running') {
    log.line(`deploy.yml run #${pick.run.number} for ${commit.slice(0, 12)} is ${pick.run.status}; waiting (${pick.run.url})`);
    await sleep(POLL_MS);
    const run = JSON.parse(tools.gh(['run', 'view', String(pick.run.databaseId), '--json', RUN_FIELDS], 'gh run view'));
    pick = selectRun([run], commit);
  }
  if (pick.state === 'failed') throw new Error(`deploy.yml run #${pick.run.number} for ${commit.slice(0, 12)} concluded ${pick.run.conclusion}: ${pick.run.url}`);
  return pick.run;
}

function sha256File(path) {
  return createHash('sha256').update(readFileSync(path)).digest('hex');
}

async function prepTool(dir, log) {
  const exe = join(dir, 'IntuneWinAppUtil.exe');
  if (existsSync(exe) && sha256File(exe) === PREP_TOOL.sha256) return exe;
  log.line(`download IntuneWinAppUtil ${PREP_TOOL.version}`);
  const res = await fetch(PREP_TOOL.url);
  if (!res.ok) throw new Error(`downloading IntuneWinAppUtil: HTTP ${res.status}`);
  const bytes = Buffer.from(await res.arrayBuffer());
  const got = createHash('sha256').update(bytes).digest('hex');
  if (got !== PREP_TOOL.sha256) throw new Error(`IntuneWinAppUtil.exe has sha256 ${got}, not the pinned ${PREP_TOOL.sha256}; refusing to run it`);
  mkdirSync(dir, { recursive: true });
  writeFileSync(exe, bytes);
  return exe;
}

/** Downloads the run's agent-release artifact (once) and reads release.json, checking the MSI against it. */
function readRelease(tools, run, home) {
  const dir = join(home, 'release', String(run.number));
  if (!existsSync(join(dir, 'release.json'))) {
    rmSync(dir, { recursive: true, force: true });
    mkdirSync(dir, { recursive: true });
    tools.log.line(`download the agent-release artifact of run #${run.number}`);
    tools.gh(['run', 'download', String(run.databaseId), '--name', 'agent-release', '--dir', dir], 'gh run download');
  }
  const release = JSON.parse(readFileSync(join(dir, 'release.json'), 'utf8'));
  if (!release.version || !release.product_code || !release.file) throw new Error(`${dir}\\release.json names no version, product code or file`);
  release.msi = join(dir, release.file);
  if (sha256File(release.msi) !== String(release.sha256).toLowerCase()) throw new Error(`${release.msi} does not match release.json's sha256`);
  return release;
}

/** The setup folder {MSI, tenant file}, wrapped as a .intunewin under work. */
async function wrap(tools, release, home, work) {
  const exe = await prepTool(join(home, 'tools'), tools.log);
  const setup = join(work, 'setup');
  mkdirSync(setup, { recursive: true });
  copyFileSync(release.msi, join(setup, release.file));
  copyFileSync(tools.config.tenantFile, join(setup, release.tenant_file ?? 'ShadowAICapture.tenant.env'));
  tools.log.line('wrap the MSI and the tenant file with IntuneWinAppUtil');
  tools.run(exe, ['-c', setup, '-s', join(setup, release.file), '-o', join(work, 'out'), '-q'], 'IntuneWinAppUtil');
  const intunewin = join(work, 'out', release.file.replace(/\.msi$/i, '.intunewin'));
  if (!existsSync(intunewin)) throw new Error(`IntuneWinAppUtil wrote no ${intunewin}`);
  return intunewin;
}

/** The VM's Entra device id, from dsregcmd inside it. */
function entraDeviceId(tools) {
  const dsreg = parseDsreg(tools.invm(['-Command', 'dsregcmd /status'], 'dsregcmd in the VM'));
  if (!dsreg.joined || !/^[0-9a-f-]{36}$/.test(dsreg.deviceId)) throw new Error('the VM is not Entra-joined (dsregcmd /status)');
  if (dsreg.tenantId !== tools.config.entraTenantId) throw new Error(`the VM is joined to Entra tenant ${dsreg.tenantId}, not ${tools.config.entraTenantId}`);
  return dsreg.deviceId;
}

/** Runs Publish-IntuneBuild.ps1, recording the app id the first run creates. */
function publish(tools, { intent, deviceId, release, intunewin }) {
  const { config, log } = tools;
  const args = [
    '-NoProfile', '-NonInteractive', '-ExecutionPolicy', 'Bypass', '-File', join(HERE, 'Publish-IntuneBuild.ps1'),
    '-TenantId', config.entraTenantId, '-ClientId', config.intuneClientId, '-CertificateThumbprint', config.certificateThumbprint,
    '-GroupId', config.testDeviceGroup, '-Intent', intent, '-EntraDeviceId', deviceId,
  ];
  if (config.intuneAppId) args.push('-AppId', config.intuneAppId);
  if (release) args.push('-IntuneWinFile', intunewin, '-Version', release.version, '-ProductCode', release.product_code);
  return tools.stream('powershell.exe', args, 'Publish-IntuneBuild.ps1', (l) => {
    const m = /^app-id (\S+)$/.exec(l.trim());
    if (!m) return;
    checkAppId(config.intuneAppId, m[1]);
    if (!config.intuneAppId) {
      saveAppId(m[1]);
      config.intuneAppId = m[1].toLowerCase();
      log.line(`recorded Intune app id ${config.intuneAppId} in refactor-endpoint/TESTBED.md`);
    }
  });
}

/** Polls the VM until verdict says done, or prints the IME log and fails after the limit. */
async function waitForVM(tools, verdictOf) {
  const deadline = Date.now() + WAIT_LIMIT_MS;
  let shown = '';
  for (;;) {
    let verdict;
    try {
      verdict = verdictOf(lastJson(tools.invm(['-Command', PROBE], 'probing the VM')));
    } catch (err) {
      verdict = { done: false, waiting: [`the VM to answer (${err.message.split('\n')[0]})`] };
    }
    if (verdict.done) return;
    const status = verdict.waiting.join('; ');
    if (status !== shown) tools.log.line(`waiting for ${status}`);
    shown = status;
    if (Date.now() > deadline) {
      tools.log.line(`the Intune Management Extension's AppWorkload log lines for app ${tools.config.intuneAppId}:`);
      const relay = tools.log.relay('  ');
      relay.push(tools.invm(['-Command', imeLog(tools.config.intuneAppId)], 'reading the IME log'));
      relay.end();
      throw new Error(`the VM did not get there within ${WAIT_LIMIT_MS / 60_000} minutes of the publish`);
    }
    await sleep(POLL_MS);
  }
}

async function main(argv, log) {
  const { values } = parseArgs({
    args: argv,
    options: {
      commit: { type: 'string' },
      wait: { type: 'boolean' },
      'no-wait': { type: 'boolean' },
      uninstall: { type: 'boolean', default: false },
    },
  });
  if (process.platform !== 'win32') throw new Error("the testbed tools run on the owner's Windows PC (Hyper-V, Windows PowerShell)");
  const started = Date.now();
  const config = loadConfig();
  const tools = new Tools(config, log);
  log.secret(checkTenantFile(parseEnvFile(readFileSync(config.tenantFile, 'utf8')), config));
  const home = join(process.env.LOCALAPPDATA, 'sac-testbed');

  let release = null;
  if (!values.uninstall) {
    const commit = (values.commit ?? tools.run('git', ['ls-remote', 'origin', 'refs/heads/main'], 'git ls-remote').split(/\s+/)[0]).toLowerCase();
    const run = await findRelease(tools, commit);
    release = readRelease(tools, run, home);
    log.line(`release ${release.version} (product code ${release.product_code}) from run #${run.number}, ${run.url}`);
  }

  const publishedAt = Date.now();
  const work = join(home, 'package', release ? release.version : 'uninstall');
  rmSync(work, { recursive: true, force: true });
  try {
    const intunewin = release ? await wrap(tools, release, home, work) : null;
    const deviceId = entraDeviceId(tools);
    log.line(release ? 'publish to Intune' : 'assign the Intune app as uninstall');
    await publish(tools, { intent: release ? 'required' : 'uninstall', deviceId, release, intunewin });
  } finally {
    // The package carries the deployment key; nothing keeps it once Intune has the content.
    rmSync(work, { recursive: true, force: true });
  }
  log.line('restart the Intune Management Extension in the VM');
  tools.invm(['-Command', "Restart-Service -Name 'IntuneManagementExtension' -Force"], 'restarting the Intune Management Extension');

  if (values['no-wait']) {
    log.line(`published; elapsed ${elapsed(Date.now() - started)}; not waiting`);
    return;
  }
  await waitForVM(tools, (probe) => (release ? evaluateInstall(probe, release) : evaluateRemoval(probe)));
  const what = release ? `the VM runs ${release.version}, enrolled and reporting to pre-prod` : 'the agent is uninstalled from the VM';
  log.line(`${what}; ${elapsed(Date.now() - publishedAt)} after the publish started (${elapsed(Date.now() - started)} in all)`);
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  const log = createLog();
  main(process.argv.slice(2), log).catch((err) => {
    process.stderr.write(`${log.redact(`deploy: ${err.message}`)}\n`);
    process.exit(1);
  });
}
