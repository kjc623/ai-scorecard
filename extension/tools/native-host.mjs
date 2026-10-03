#!/usr/bin/env node
/**
 * native-host.mjs — the file half of the native-messaging host registration.
 *
 * This tool writes the host manifest and computes the pinned extension id; `native-host.ps1` writes
 * the registry values that point at it. The split is deliberate rather than tidy-minded: Node cannot
 * spawn a child in a confined sandbox (EPERM), and PowerShell needs no child process to touch the
 * registry, so each side does what it can do everywhere.
 *
 * WHY THE PAIR EXISTS. Every report on this project carried "a connected native channel is NOT
 * VERIFIED - no host is registered on this host". That was never a limit of the extension or of
 * capture-core: the binary has shipped a `--native-host` mode since round 6, and Chromium looks for
 * the host manifest in a registry key an ordinary user can write. Two things were missing:
 * (a) something that writes those keys, and (b) a stable extension id for `allowed_origins` to name,
 * because an unpacked extension's id is derived from its path. (b) is the `key` field in
 * manifest.json — see tools/make-extension-key.mjs — and (a) is this pair.
 *
 *   node tools/native-host.mjs --write-manifest     # write the host manifest Chromium will read
 *   node tools/native-host.mjs --status             # extension id, binary, manifest, registry
 *   node tools/native-host.mjs --print-args         # the flags the host would be launched with
 *   pwsh -File tools/native-host.ps1 -Action install
 */

import { createHash } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));
const PKG = resolve(HERE, '..');
const REPO = resolve(PKG, '..');

/** The host name `src/native.js` calls `connectNative` with. One string, asserted on both sides. */
export const HOST_NAME = 'com.shadowaicapture.capture_core';

/** Where the host manifest and the host's own working state live: ignored, disposable, inspectable. */
export const HOST_DIR = join(REPO, '.tools', 'tmp', 'capture-native-host');
export const MANIFEST_FILE = join(HOST_DIR, `${HOST_NAME}.json`);

/**
 * The extension id Chromium derives from a manifest `key`: sha256 over the SubjectPublicKeyInfo DER,
 * first 16 bytes, each hex digit mapped 0-f -> a-p (extension_id.cc). It is computed from the
 * committed manifest rather than written down, so it cannot drift from what the browser will load.
 */
export function extensionIdFromManifest(manifestPath = join(PKG, 'manifest.json')) {
  const manifest = JSON.parse(readFileSync(manifestPath, 'utf8'));
  if (!manifest.key) {
    throw new Error(
      `${manifestPath} has no "key" field, so its id is derived from the checkout path and cannot be pinned`,
    );
  }
  const digest = createHash('sha256').update(Buffer.from(manifest.key, 'base64')).digest().subarray(0, 16);
  return [...digest]
    .map((b) => b.toString(16).padStart(2, '0'))
    .join('')
    .replace(/[0-9a-f]/g, (c) => 'abcdefghijklmnop'[parseInt(c, 16)]);
}

/** The built endpoint binary, or null. This only looks; building it is `go build`, not this tool. */
export function findHostExe(explicit) {
  if (explicit) return existsSync(explicit) ? resolve(explicit) : null;
  const name = process.platform === 'win32' ? 'capture-core.exe' : 'capture-core';
  const candidate = join(REPO, 'endpoint', 'capture-core', 'bin', name);
  return existsSync(candidate) ? candidate : null;
}

/**
 * The flags the native host is launched with.
 *
 * The minimum for an honest host: a spool of its own under .tools/tmp, identity stamped on every
 * envelope, and a health file so the host's own view outlives the run. No `--bundle`, deliberately:
 * §13.3 rule 5 then applies and every destination resolves to M0, which is exactly what the browser
 * gate asserts when it checks that a real body produced no content field.
 */
export function hostArgs(hostExe) {
  const state = join(HOST_DIR, 'state');
  return [
    '--native-host',
    '--spool-dir', join(state, 'spool'),
    '--spool-key', join(state, 'spool.key'),
    '--tenant-id', '00000000-0000-7000-8000-000000000001',
    '--device-id', 'dev-in-browser-check',
    '--user-ref', 'user-in-browser-check',
    '--health-file', join(state, 'health.jsonl'),
    '--log-format', 'text',
    '--log-level', 'warn',
  ];
}

/**
 * The host manifest Chromium reads. `_args` is not part of Chromium's schema — it ignores unknown
 * keys — and exists so the registration is self-describing to a human who finds it in the registry
 * and wonders which flags the endpoint was launched with.
 */
export function hostManifest({ hostExe, extensionId }) {
  return {
    name: HOST_NAME,
    description:
      'Shadow AI Capture endpoint agent — development registration written by extension/tools/native-host.mjs',
    // The launcher, not the binary: Chromium passes no arguments, so the flags must live in a file
    // the launcher supplies. See LAUNCHER_FILE.
    path: LAUNCHER_FILE,
    type: 'stdio',
    allowed_origins: [`chrome-extension://${extensionId}/`],
    // Not part of Chromium's schema — it ignores unknown keys — but it is what the PowerShell half
    // turns into the launcher, and what a human reading the registry needs to see.
    _executable: hostExe,
    _args: hostArgs(hostExe),
  };
}

export function writeManifest({ hostExe, manifestPath = join(PKG, 'manifest.json') } = {}) {
  const exe = findHostExe(hostExe);
  if (!exe) {
    return {
      ok: false,
      reason: 'no_endpoint_binary',
      detail: `build it with: cd endpoint/capture-core && go build -o bin/capture-core${process.platform === 'win32' ? '.exe' : ''} ./cmd/capture-core`,
    };
  }
  const extensionId = extensionIdFromManifest(manifestPath);
  mkdirSync(HOST_DIR, { recursive: true });
  const manifest = hostManifest({ hostExe: exe, extensionId });
  writeFileSync(MANIFEST_FILE, JSON.stringify(manifest, null, 2) + '\n', 'utf8');
  return { ok: true, extensionId, hostExe: exe, manifestFile: MANIFEST_FILE, manifest };
}

/**
 * What is on disk right now. The registry half is reported by the PowerShell script; this does not
 * guess at it, because a tool that reports registry state it never read is worse than one that
 * reports nothing.
 */
/**
 * The launcher Chromium actually runs.
 *
 * Why a launcher and not the binary: **Chromium launches a native messaging host with no
 * arguments**, and a host manifest has no field for them. `capture-core` refuses to start without
 * `--spool-dir` — "a provider with nowhere to write must not start" (§3.5 step 2) — so pointing the
 * manifest at the bare binary produces a host that exits immediately, which the browser reports as
 * `Can't find manifest for native messaging host ...`, a message that names the wrong problem
 * entirely. `native-host.ps1 -Action install` writes this file from the same `_args` this module
 * prints, so the two cannot drift.
 */
export const LAUNCHER_FILE = join(HOST_DIR, `${HOST_NAME}.cmd`);

export function status({ hostExe } = {}) {
  let extensionId;
  try {
    extensionId = extensionIdFromManifest();
  } catch (e) {
    extensionId = `unavailable: ${e.message}`;
  }
  const exe = findHostExe(hostExe);
  return {
    extensionId,
    hostExe: exe ?? '(not built)',
    manifestFile: MANIFEST_FILE,
    manifestWritten: existsSync(MANIFEST_FILE),
    registeredBy: 'pwsh -File tools/native-host.ps1 -Action status',
    registryValues: [
      `HKCU\\Software\\Microsoft\\Edge\\NativeMessagingHosts\\${HOST_NAME}`,
      `HKCU\\Software\\Google\\Chrome\\NativeMessagingHosts\\${HOST_NAME}`,
    ],
  };
}

function main() {
  const args = process.argv.slice(2);
  const asJson = args.includes('--json');
  const hostExeArg = args.indexOf('--host-exe') >= 0 ? args[args.indexOf('--host-exe') + 1] : undefined;
  const emit = (o) => console.log(asJson ? JSON.stringify(o, null, 2) : print(o));

  if (args.includes('--write-manifest')) {
    const r = writeManifest({ hostExe: hostExeArg });
    emit(r);
    if (!r.ok) process.exit(1);
    return;
  }
  if (args.includes('--print-args')) {
    emit({ hostExe: findHostExe(hostExeArg) ?? '(not built)', args: hostArgs() });
    return;
  }
  if (args.includes('--status')) {
    emit(status({ hostExe: hostExeArg }));
    return;
  }

  console.log(
    'usage: node tools/native-host.mjs (--write-manifest [--host-exe P] | --status | --print-args) [--json]',
  );
  process.exit(2);
}

function print(o) {
  for (const [k, v] of Object.entries(o)) {
    if (Array.isArray(v)) {
      console.log(`${k}:`);
      for (const item of v) console.log(`  - ${typeof item === 'string' ? item : JSON.stringify(item)}`);
    } else if (v && typeof v === 'object') {
      console.log(`${k}:`);
      for (const [k2, v2] of Object.entries(v)) console.log(`  ${k2}: ${v2}`);
    } else {
      console.log(`${k}: ${v}`);
    }
  }
}

if (process.argv[1] && resolve(process.argv[1]) === resolve(fileURLToPath(import.meta.url))) main();
