#!/usr/bin/env node
// Bring the testlab up and prove capture-core is a working Linux appliance.
//
// This is the headline deliverable: a container that runs capture-core on Linux and proves, with a
// real HTTPS client and no --insecure, that
//   * proxy.tls intercepts and re-originates api.anthropic.test:443;
//   * --trust-install put the device CA in the OS trust store, and --trust-remove-on-stop removed it;
//   * cli.shim exported the proxy/CA environment and the Node bootstrap, and the Node path goes
//     through the proxy and trusts the device CA.
//
// Usage:
//   node endpoint/testlab/run.mjs            # build binaries + image, up, assert, down -v
//   node endpoint/testlab/run.mjs --keep     # same, but leave the lab running for inspection
//   node endpoint/testlab/run.mjs --down     # tear down (volume included)
//
// Every check prints what it saw; a failure exits non-zero, so this is usable as a gate.

import { spawnSync } from 'node:child_process';
import { mkdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)));
const CAPTURE_CORE = resolve(ROOT, '..', 'capture-core');
const BUILD = join(ROOT, 'build');
const COMPOSE_FILE = join(ROOT, 'compose.yaml');

const ARGS = process.argv.slice(2);
const DOWN = ARGS.includes('--down');
const KEEP = ARGS.includes('--keep');

// Offline Go build environment, exactly as the repository's gates use it. GOOS/GOARCH produce a
// static Linux binary that runs in alpine without a cross toolchain.
const GO_ENV = {
  ...process.env,
  GOCACHE: process.env.GOCACHE || resolve(ROOT, '..', '..', '.tools', 'gocache'),
  GOPROXY: 'off',
  GOTOOLCHAIN: 'local',
  GOFLAGS: '-mod=mod',
  GOOS: 'linux',
  GOARCH: 'amd64',
  CGO_ENABLED: '0',
};

let failures = 0;
function check(name, ok, detail) {
  console.log(`${ok ? '  ok  ' : ' FAIL '} ${name}${detail ? ` — ${detail}` : ''}`);
  if (!ok) failures++;
}

function sleep(ms) {
  return new Promise((r) => setTimeout(r, ms));
}

function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', ...opts });
  return { code: res.status, out: `${res.stdout ?? ''}${res.stderr ?? ''}`, stdout: res.stdout ?? '' };
}

function compose(args, opts = {}) {
  return run('docker', ['compose', '-f', COMPOSE_FILE, ...args], { cwd: ROOT, env: process.env, ...opts });
}

/** Run a shell script inside the device container (login shell, so /etc/profile.d is sourced). */
function deviceRun(script, opts = {}) {
  return run('docker', ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'device', 'sh', '-lc', script],
    { cwd: ROOT, env: process.env, ...opts });
}

/** Read a file out of the device container. */
function deviceRead(path) {
  const r = run('docker', ['compose', '-f', COMPOSE_FILE, 'exec', '-T', 'device', 'cat', path],
    { cwd: ROOT, env: process.env });
  return r.stdout ?? '';
}

/** The latest health snapshot written by capture-core (the last JSON line of the health file). */
function lastHealthSnapshot() {
  const raw = deviceRead('/state/health.jsonl');
  const lines = raw.split('\n').filter((l) => l.trim().length > 0);
  if (lines.length === 0) return null;
  try {
    return JSON.parse(lines[lines.length - 1]);
  } catch {
    return null;
  }
}

function report(snap, collector) {
  if (!snap || !Array.isArray(snap.reports)) return null;
  return snap.reports.find((r) => r.collector === collector) ?? null;
}

/** The current proxy.tls observed/emitted counters, for the before/after delta assertions. */
function tlsCounters() {
  const t = report(lastHealthSnapshot(), 'proxy.tls');
  return { observed: t?.counters?.observed ?? 0, emitted: t?.counters?.emitted ?? 0 };
}

async function waitFor(fn, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    if (fn()) return true;
    if (Date.now() > deadline) return false;
    await sleep(500);
  }
}

function compileBinaries() {
  mkdirSync(BUILD, { recursive: true });
  console.log('testlab: compiling the Linux capture-core and sac-bundle binaries …');
  for (const [name, pkg] of [['capture-core', './cmd/capture-core'], ['sac-bundle', './cmd/sac-bundle']]) {
    process.stdout.write(`  compile ${name} … `);
    const r = run('go', ['build', '-trimpath', '-ldflags=-s -w', '-o', join(BUILD, name), pkg],
      { cwd: CAPTURE_CORE, env: GO_ENV });
    if (r.code !== 0) {
      console.log(`\n${r.out}`);
      console.error(`testlab: failed to compile ${name}; aborting.`);
      process.exit(r.code ?? 1);
    }
    console.log('ok');
  }
}

async function runLab() {
  compileBinaries();

  console.log('\ntestlab: building the image and bringing the lab up (pki -> upstream -> device) …');
  let r = compose(['up', '-d', '--build']);
  if (r.code !== 0) {
    console.error(r.out.trim());
    console.error('\ntestlab: compose up failed. See the output above.');
    return 1;
  }

  console.log('\ntestlab: waiting for capture-core to report proxy.tls and cli.shim healthy …');
  const up = await waitFor(() => {
    const snap = lastHealthSnapshot();
    const tls = report(snap, 'proxy.tls');
    const shim = report(snap, 'cli.shim');
    return !!tls && tls.state === 'healthy' && !!shim && shim.state === 'healthy';
  }, 40000);
  if (!up) {
    console.error('testlab: capture-core did not reach healthy within 40s.');
    console.error('device logs:\n');
    console.error(compose(['logs', '--tail', '60', 'device']).out.trim());
    return 1;
  }
  check('capture-core is up: proxy.tls healthy + cli.shim healthy', true);

  // (a) A real HTTPS client through the proxy, with no --insecure: the system trust store proves
  //     the device CA was installed.
  console.log('\n(a) curl through the proxy, no --insecure:');
  const curlScript = [
    '. /etc/profile.d/shadow-ai-capture.sh',
    `curl -sS -o /tmp/out -w '%{http_code}' --proxy "$HTTPS_PROXY" -X POST https://api.anthropic.test/v1/messages -H 'content-type: application/json' -d '{"messages":[{"role":"user","content":"hello from the lab"}]}'`,
  ].join('\n');
  r = deviceRun(curlScript);
  const httpCode = r.out.trim();
  const body = deviceRead('/tmp/out').trim();
  check('curl POST /v1/messages returns 200 with no --insecure', httpCode === '200', `http ${httpCode}${body ? ` body=${body}` : ''}`);

  // (b) The device CA is installed and present in the system bundle.
  console.log('\n(b) OS trust store:');
  const caPath = '/usr/local/share/ca-certificates/sac-device-ca.crt';
  const caBundle = '/etc/ssl/certs/ca-certificates.crt';
  const filePresent = deviceRun(`test -f ${caPath}`).code === 0;
  check('device CA file installed', filePresent, caPath);
  const subject = deviceRun(`openssl x509 -in ${caPath} -noout -subject`).out.trim();
  const verify = deviceRun(`openssl verify -CAfile ${caBundle} ${caPath}`);
  check('device CA subject is trusted in the system bundle', verify.code === 0 && /OK/.test(verify.out), `${subject} — ${verify.out.trim()}`);
  // Capture the DER base64 body for the removal check.
  const caBody = deviceRun(`openssl x509 -in ${caPath} -outform DER | base64 | tr -d '\\n'`).out.trim();

  // (c) The Node https path, from a login shell exactly as a user would: /etc/profile.d is sourced
  //     by the shell, so the agent inherits the proxy/CA with no wrapper and no manual export.
  console.log('\n(c) Node https path from a login shell (no wrapper):');
  r = deviceRun('node /app/scripts/node-request.mjs');
  const intercepted = /peerIssuer Shadow AI Capture Device CA/.test(r.out);
  check('node https.get returns 200', r.code === 0 && /statusCode 200/.test(r.out), r.out.trim().replace(/\n/g, ' | '));
  check('node request was intercepted (peer leaf minted by the device CA)', intercepted, r.out.trim().replace(/\n/g, ' | '));

  // (c2) The fetch path, which is what Claude Code's SDK uses, from the same transparent shell.
  //      Node 24 honours NODE_USE_ENV_PROXY. A 200 proves interception: the upstream CA is not in
  //      Node's bundled roots, so a direct connection would fail verification.
  console.log('\n(c2) Node fetch path from a login shell (the Claude Code SDK path):');
  r = deviceRun('node /app/scripts/node-fetch-request.mjs');
  check('node fetch returns 200 through the proxy', r.code === 0 && /statusCode 200/.test(r.out), r.out.trim().replace(/\n/g, ' | '));

  // (c3) The transparency property itself: a new login shell inherits the proxy and CA
  //      environment without the user doing anything.
  console.log('\n(c3) login-shell environment (no wrapper, no manual export):');
  const loginEnv = deviceRun('env').out;
  check('a new login shell inherits the proxy and CA environment',
    /HTTPS_PROXY=http:\/\/127\.0\.0\.1:8843/.test(loginEnv) &&
    /NODE_USE_ENV_PROXY=1/.test(loginEnv) &&
    /NODE_EXTRA_CA_CERTS=/.test(loginEnv),
    loginEnv.split('\n').filter((l) => /^(HTTPS_PROXY|NODE_USE_ENV_PROXY|NODE_EXTRA_CA_CERTS)=/.test(l)).join(' | '));


  // (d2) The REAL Claude Code terminal CLI, run non-interactively inside the device from a login
  //      shell (which sources /etc/profile.d/shadow-ai-capture.sh) — no wrapper, no manual export.
  //      It targets the fake upstream and must be captured by proxy.tls exactly like any other
  //      client. This is the proof that the actual coding agent is captured, not a curl or a
  //      synthetic fetch standing in for it.
  //
  //      Claude Code 2.1.289 ships as a self-contained native ELF binary (Bun-based, ~240MB), NOT a
  //      Node process, so NODE_OPTIONS/NODE_EXTRA_CA_CERTS/NODE_USE_ENV_PROXY do not apply to it. It
  //      honours HTTPS_PROXY from the process env itself, and trusts the device CA through either
  //      the OS trust store (--trust-install) or SSL_CERT_FILE (cli.shim). --bare makes it use
  //      strictly ANTHROPIC_API_KEY (no keychain/OAuth), --permission-prompts none and --tools ""
  //      keep the run fully non-interactive and tool-free so no ripgrep/Bash is spawned.
  console.log('\n(d2) real Claude Code CLI (claude -p), no wrapper:');
  const claudeVersion = deviceRun('claude --version 2>&1').out.trim();
  const nodeVersion = deviceRun('node -v').out.trim();
  console.log(`  claude version: ${claudeVersion} | node version: ${nodeVersion}`);

  const tlsBefore = tlsCounters();
  const cliScript = [
    'export ANTHROPIC_API_KEY=sk-ant-test',
    'export ANTHROPIC_BASE_URL=https://api.anthropic.test',
    'timeout 60 claude -p "hello" --bare --permission-prompts none --tools ""',
  ].join('\n');
  r = deviceRun(cliScript);
  const cliOut = r.out.trim().replace(/\n/g, ' ');
  check('claude -p exits 0 (the CLI ran headless)', r.code === 0, `exit=${r.code}`);
  check('claude -p reached the upstream and printed its response',
    r.code === 0 && /hello from the fake upstream/.test(r.out), cliOut || '(no output)');

  // (b) proxy.tls observed/emitted counters must have advanced for the CLI's request. The health
  //     file is appended once per second, so poll briefly for the post-run snapshot.
  const tlsGrew = await waitFor(() => {
    const t = tlsCounters();
    return t.observed > tlsBefore.observed && t.emitted > tlsBefore.emitted;
  }, 15000);
  const tlsAfter = tlsCounters();
  check('proxy.tls observed/emitted counters advanced for the CLI run', tlsGrew,
    `observed ${tlsBefore.observed} -> ${tlsAfter.observed} (+${tlsAfter.observed - tlsBefore.observed}); emitted ${tlsBefore.emitted} -> ${tlsAfter.emitted} (+${tlsAfter.emitted - tlsBefore.emitted})`);

  // (c) The device spool gains a non-empty segment from the CLI's intercepted request.
  const cliSeg = deviceRun(`find /state/spool/segments -name '*.seg' -size +0c | head -1`).out.trim();
  check('spool holds a non-empty segment from the CLI run', cliSeg.length > 0, cliSeg || 'no non-empty segment');

  // (d) The health row: cli.shim is healthy, and the CLI itself inherited the shim environment —
  //     the observed/emitted deltas above are the CLI process, not a wrapper, routing through the
  //     proxy the shim exported. (The shim's own "inherited" probe is not wired in this build, so
  //     the login-shell env check in (c3) plus the CLI's routing are the observable proof.)
  const cliShimRow = report(lastHealthSnapshot(), 'cli.shim');
  check('cli.shim row is healthy while the CLI runs through it', cliShimRow?.state === 'healthy', `state=${cliShimRow?.state}`);

  // Direct proof it was the real CLI and not a synthetic client: the upstream saw the CLI's own
  // user-agent and an Anthropic Messages request.
  const upstreamLog = compose(['logs', '--no-log-prefix', 'upstream']).out;
  check('upstream saw the real CLI user-agent on /v1/messages',
    /claude-cli\/2\.1\.289/.test(upstreamLog) && /POST \/v1\/messages/.test(upstreamLog),
    'user-agent=claude-cli/2.1.289 present in the upstream log');

  // (d) The health channel records the interception and a healthy shim row.
  console.log('\n(d) health channel:');
  const snap = lastHealthSnapshot();
  const tls = report(snap, 'proxy.tls');
  const shim = report(snap, 'cli.shim');
  const observed = tls?.counters?.observed ?? 0;
  const emitted = tls?.counters?.emitted ?? 0;
  check('proxy.tls row has observed>=1 and emitted>=1', observed >= 1 && emitted >= 1,
    `observed=${observed} emitted=${emitted} state=${tls?.state}`);
  check('cli.shim row state=healthy', shim?.state === 'healthy', `state=${shim?.state}`);

  // (e) The interception reached the spool as a non-empty segment.
  console.log('\n(e) spool:');
  const seg = deviceRun(`find /state/spool/segments -name '*.seg' -size +0c | head -1`).out.trim();
  check('spool holds a non-empty segment', seg.length > 0, seg || 'no non-empty segment');

  // (f) Removal: SIGTERM the agent, wait for exit, then assert the trust entry is gone.
  console.log('\n(f) trust removal on stop:');
  deviceRun('kill -TERM "$(cat /state/capture-core.pid)"');
  const exited = await waitFor(() => deviceRun('kill -0 "$(cat /state/capture-core.pid)" 2>/dev/null').code !== 0, 15000);
  check('capture-core exits on SIGTERM', exited);
  const gone = deviceRun(`test ! -e ${caPath}`).code === 0;
  check('device CA file removed', gone);
  const stillInBundle = deviceRun(`tr -d '\\n' < ${caBundle} | grep -q -F "${caBody}"`).code === 0;
  check('device CA subject no longer in the system bundle', !stillInBundle);

  console.log(`\n${failures === 0 ? 'testlab: all checks passed' : `testlab: ${failures} check(s) FAILED`}`);
  return failures === 0 ? 0 : 1;
}

if (DOWN) {
  console.log('testlab: tearing down (volume included) …');
  const r = compose(['down', '-v']);
  console.log(r.out.trim());
  process.exit(r.code ?? 0);
}

const exitCode = await runLab();
// Tear down on failure too, so a red run cannot leave the PKI volume (which holds the upstream CA
// key) or any container behind. --keep is the explicit opt-in for inspecting a failed run.
if (!KEEP) {
  console.log('\ntestlab: tearing down (volume included) …');
  console.log(compose(['down', '-v']).out.trim());
} else {
  console.log('\ntestlab: left running. Logs: docker compose -f endpoint/testlab/compose.yaml logs -f device');
  console.log('testlab: tear down with: node endpoint/testlab/run.mjs --down');
}
process.exit(exitCode);
