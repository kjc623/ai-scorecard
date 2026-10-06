#!/usr/bin/env node
// installer/build.mjs - build the complete payload ("stage") one platform package is made from.
//
//   node installer/build.mjs --os linux --arch amd64 --version 1.4.0 \
//     --policy-key-file policy-signing.pub --policy-key-id policy-key-1 \
//     --classifier-key classifier-signing.key --classifier-rules endpoint/classifier-host/rules/default.json \
//     --classifier-model endpoint/classifier-host/rules/model.json
//
// Every input is required. The stage (installer/.stage/<os>-<arch>, or --out) holds:
//   bin/            capture-core and classifier-host, cross-compiled with CGO disabled
//   classifier/     the classifier release (rules and model), signed with --classifier-key
//   etc/            capture-core.env (the generic vendor file, pinning the policy and classifier
//                   public keys), the native messaging host manifest, and the service definition
// For Linux the stage also carries install.sh and uninstall.sh and is packed as
// installer/dist/shadow-ai-capture-<version>-linux-<arch>.tar.gz. release-msi.mjs builds the Windows
// stage this way; macos/build-pkg.sh packages a darwin stage on a Mac.

import { spawnSync } from 'node:child_process';
import { createPrivateKey, createPublicKey } from 'node:crypto';
import { copyFileSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { parseArgs } from 'node:util';

import { GO_BINARIES, LAYOUT, NATIVE_HOST, genericEnv } from './manifest.mjs';
import { drift } from './render.mjs';

export const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const GENERATED = join(ROOT, 'installer', 'generated');
const OS_DIR = { linux: 'linux', darwin: 'macos', windows: 'windows' };

export class BuildError extends Error {}

/** The options every platform build takes; release-msi.mjs adds its own. */
export const STAGE_OPTIONS = {
  version: { type: 'string' },
  'policy-key-file': { type: 'string' },
  'policy-key-id': { type: 'string' },
  'classifier-key': { type: 'string' },
  'classifier-rules': { type: 'string' },
  'classifier-model': { type: 'string' },
};

/** A hex Ed25519 public key from a key file: 64 hex digits, sac-bundle's 128-digit seed||public, or PEM. */
export function policyPublicKeyHex(text, where) {
  const t = text.trim();
  if (/^[0-9a-f]{64}$/i.test(t)) return t.toLowerCase();
  if (/^[0-9a-f]{128}$/i.test(t)) return t.slice(64).toLowerCase();
  if (t.includes('-----BEGIN')) {
    const key = t.includes('PRIVATE KEY') ? createPublicKey(createPrivateKey(t)) : createPublicKey(t);
    if (key.asymmetricKeyType !== 'ed25519') throw new BuildError(`${where}: not an Ed25519 key (${key.asymmetricKeyType})`);
    return Buffer.from(key.export({ format: 'jwk' }).x, 'base64url').toString('hex');
  }
  throw new BuildError(`${where}: expected a hex or PEM Ed25519 key`);
}

/** Check the release inputs and read the policy trust anchor. Throws BuildError naming what is missing. */
export function resolveInputs(opts) {
  const missing = Object.keys(STAGE_OPTIONS).filter((k) => !opts[k]);
  if (missing.length) throw new BuildError(`missing required input(s): ${missing.map((k) => `--${k}`).join(', ')}`);
  const [major, minor, build] = /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/.exec(opts.version)?.slice(1).map(Number) ?? [];
  if (!(major <= 255 && minor <= 255 && build <= 65535)) {
    throw new BuildError(`--version ${opts.version}: expected major.minor.build within 255.255.65535 (an MSI ProductVersion)`);
  }
  for (const k of ['policy-key-file', 'classifier-key', 'classifier-rules', 'classifier-model']) {
    if (!existsSync(opts[k])) throw new BuildError(`--${k} ${opts[k]}: no such file`);
  }
  return {
    version: opts.version,
    policyKey: policyPublicKeyHex(readFileSync(opts['policy-key-file'], 'utf8'), opts['policy-key-file']),
    policyKeyId: opts['policy-key-id'],
    classifierKey: resolve(opts['classifier-key']),
    classifierRules: resolve(opts['classifier-rules']),
    classifierModel: resolve(opts['classifier-model']),
  };
}

function run(cmd, args, opts) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', ...opts });
  const out = `${res.stdout ?? ''}${res.stderr ?? ''}`;
  if (res.error) throw new BuildError(`${cmd}: ${res.error.message}`);
  if (res.status !== 0) throw new BuildError(`${cmd} ${args.join(' ')} failed:\n${out.trim()}`);
  return out;
}

/**
 * Build the stage for one platform from resolved inputs. Returns what the stage pins, for
 * release.json.
 */
export function buildStage({ os, arch, inputs, out, log = console.log }) {
  if (!LAYOUT[os]) throw new BuildError(`--os must be one of ${Object.keys(LAYOUT).join(', ')}`);
  if (!['amd64', 'arm64'].includes(arch)) throw new BuildError('--arch must be amd64 or arm64');
  const drifted = drift();
  if (drifted.length) throw new BuildError(`installer/generated differs from installer/manifest.mjs (${drifted.join(', ')}); run node installer/render.mjs and commit the result`);

  const stage = resolve(out ?? join(ROOT, 'installer', '.stage', `${os}-${arch}`));
  rmSync(stage, { recursive: true, force: true });
  for (const d of ['bin', 'etc', 'classifier']) mkdirSync(join(stage, d), { recursive: true });

  const exe = os === 'windows' ? '.exe' : '';
  const goEnv = { ...process.env, GOOS: os, GOARCH: arch, CGO_ENABLED: '0' };
  for (const b of GO_BINARIES) {
    log(`compile ${b.name} ${inputs.version} for ${os}/${arch}`);
    run('go', ['build', '-trimpath', `-ldflags=-s -w -X main.version=${inputs.version}`, '-o', join(stage, 'bin', b.name + exe), b.pkg], {
      cwd: join(ROOT, b.dir),
      env: goEnv,
    });
  }

  log(`sign the classifier release with ${inputs.classifierRules}`);
  const released = run(
    'go',
    ['run', './cmd/classifier-release', '--rules', inputs.classifierRules, '--model', inputs.classifierModel, '--key', inputs.classifierKey, '--version', inputs.version, '--out', join(stage, 'classifier')],
    { cwd: join(ROOT, 'endpoint', 'classifier-host') },
  );
  const classifierPubkey = /pubkey=([0-9a-f]{64})/.exec(released)?.[1];
  if (!classifierPubkey) throw new BuildError(`classifier-release printed no pubkey:\n${released.trim()}`);

  const anchors = { SAC_POLICY_KEY: inputs.policyKey, SAC_POLICY_KEY_ID: inputs.policyKeyId, SAC_CLASSIFIER_PUBKEY: classifierPubkey };
  const generic = genericEnv(os, anchors, inputs.version);
  writeFileSync(join(stage, 'etc', 'capture-core.env'), generic);

  const generated = (file) => join(GENERATED, OS_DIR[os], file);
  copyFileSync(generated(`${NATIVE_HOST.name}.json`), join(stage, 'etc', `${NATIVE_HOST.name}.json`));
  if (os === 'linux') {
    copyFileSync(generated('shadow-ai-capture.service'), join(stage, 'etc', 'shadow-ai-capture.service'));
    for (const f of ['install.sh', 'uninstall.sh']) copyFileSync(join(ROOT, 'installer', 'linux', f), join(stage, f));
  }
  if (os === 'darwin') copyFileSync(generated('com.shadowaicapture.capture-core.plist'), join(stage, 'etc', 'com.shadowaicapture.capture-core.plist'));

  return { stage, anchors, classifierPubkey, generic };
}

/** Pack a Linux stage as the tarball a Linux deployment unpacks and runs install.sh from. */
export function packLinux(stage, version, arch) {
  const dist = join(ROOT, 'installer', 'dist');
  mkdirSync(dist, { recursive: true });
  const name = `shadow-ai-capture-${version}-linux-${arch}.tar.gz`;
  // The archive is named relative to its folder: GNU tar reads "C:\..." as a remote host.
  run('tar', ['-czf', name, '-C', stage, '.'], { cwd: dist });
  return join(dist, name);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    const { values } = parseArgs({ options: { ...STAGE_OPTIONS, os: { type: 'string' }, arch: { type: 'string', default: 'amd64' }, out: { type: 'string' } } });
    const inputs = resolveInputs(values);
    const built = buildStage({ os: values.os, arch: values.arch, inputs, out: values.out });
    console.log(`stage: ${built.stage}`);
    if (values.os === 'linux') console.log(`package: ${packLinux(built.stage, inputs.version, values.arch)}`);
    if (values.os === 'darwin') console.log(`next, on a Mac: installer/macos/build-pkg.sh --stage ${built.stage} --version ${inputs.version} --out installer/dist`);
  } catch (err) {
    if (!(err instanceof BuildError) && err.code !== 'ERR_PARSE_ARGS_UNKNOWN_OPTION') throw err;
    console.error(`build: ${err.message}`);
    process.exit(1);
  }
}
