// installer/windows/msi.mjs - the Windows package build both entry points share.
//
// release-msi.mjs builds the generic, code-only MSI a vendor ships; lab-msi.mjs builds the same MSI
// with the lab's profile files added, for this host. Both stage the payload the same way here, so
// the lab installs exactly what a customer would, plus what the lab needs and nothing in its place.
//
// The stage the MSI is built from (installer/.stage/windows-amd64):
//   bin\                 capture-core.exe, classifier-host.exe, capture-core-run.cmd (build.mjs)
//   classifier\          the signed classifier release (payload, not control-api content: docs/05 §6.1)
//   etc\capture-core.env the vendor-wide file: layout, product defaults, the trust anchors
//
// Trust anchors are build inputs, never committed: the policy public key every bundle must verify
// under, and the classifier signing key's public half. A vendor release passes its own; a lab or
// development build with none gets a development pair kept in installer/.release/keys/.

import { spawnSync } from 'node:child_process';
import { createHash, createPrivateKey, createPublicKey, generateKeyPairSync } from 'node:crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, statSync, writeFileSync } from 'node:fs';
import { basename, dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

import { PRODUCT, TENANT_PACKAGE, genericProfile } from '../manifest.mjs';

export const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..', '..');
export const STAGE = join(ROOT, 'installer', '.stage', 'windows-amd64');
export const KEYS = join(ROOT, 'installer', '.release', 'keys');
// The lab's identity material (localdev/identity/identity.mjs); read here, never written.
const LAB_IDENTITY = join(ROOT, 'localdev', '.authlab-identity');

export function rel(p) {
  return p.startsWith(ROOT) ? p.slice(ROOT.length + 1) : p;
}

export function run(cmd, args, opts = {}) {
  const res = spawnSync(cmd, args, { encoding: 'utf8', cwd: ROOT, ...opts });
  return { code: res.status ?? 1, out: `${res.stdout ?? ''}${res.stderr ?? ''}` };
}

export class BuildError extends Error {}

function must(res, what) {
  if (res.code !== 0) throw new BuildError(`${what} failed:\n${res.out.trim()}`);
  return res;
}

// ---------------------------------------------------------------------------------------------
// Trust anchors

/** A hex Ed25519 public key from any of the spellings a key file is likely to use. */
export function publicKeyHex(text, where) {
  const t = text.trim();
  if (/^[0-9a-f]{64}$/i.test(t)) return t.toLowerCase();
  // sac-bundle's --policy-priv spelling: the 64-byte private key, seed then public half.
  if (/^[0-9a-f]{128}$/i.test(t)) return t.slice(64).toLowerCase();
  if (t.includes('-----BEGIN')) {
    const key = t.includes('PRIVATE KEY') ? createPublicKey(createPrivateKey(t)) : createPublicKey(t);
    if (key.asymmetricKeyType !== 'ed25519') throw new BuildError(`${where}: not an Ed25519 key (${key.asymmetricKeyType})`);
    return Buffer.from(key.export({ format: 'jwk' }).x, 'base64url').toString('hex');
  }
  throw new BuildError(`${where}: expected a 64-hex-character Ed25519 public key or a PEM Ed25519 key`);
}

/**
 * The policy trust anchor, first found wins:
 *   --policy-key / SAC_POLICY_TRUST_KEY             a key given outright
 *   --policy-key-file / SAC_POLICY_TRUST_KEY_FILE   a key file (a vendor release names its own)
 *   localdev/.authlab-identity/policy-signing.pub.hex  the lab's vendor key, when this checkout has
 *                                                   a lab: control-api signs bundles with its private
 *                                                   half, so a package downloaded from the lab verifies
 *   installer/.release/keys/policy-key.pub          a development key an earlier build minted
 * With none, a development pair is minted into installer/.release/keys, in the two private spellings
 * a signer is likely to read (PKCS#8 PEM, and the 64-byte hex sac-bundle takes). `privateHexFile` is
 * set when this checkout holds the signing half, so a lab bundle can be signed under the same anchor.
 */
export function resolvePolicyAnchor({ key, keyFile, keyId } = {}) {
  const id = keyId || process.env.SAC_POLICY_TRUST_KEY_ID || 'policy-key-1';
  const hex = key || process.env.SAC_POLICY_TRUST_KEY;
  if (hex) return { policyKey: publicKeyHex(hex, '--policy-key'), policyKeyId: id, source: 'argument' };
  const explicit = keyFile || process.env.SAC_POLICY_TRUST_KEY_FILE;
  if (explicit) {
    if (!existsSync(explicit)) throw new BuildError(`policy key file not found: ${explicit}`);
    return { policyKey: publicKeyHex(readFileSync(explicit, 'utf8'), explicit), policyKeyId: id, source: rel(resolve(explicit)) };
  }
  for (const [pub, priv] of [
    [join(LAB_IDENTITY, 'policy-signing.pub.hex'), join(LAB_IDENTITY, 'policy-signing.key.hex')],
    [join(KEYS, 'policy-key.pub'), join(KEYS, 'policy-signing.key.hex')],
  ]) {
    if (!existsSync(pub)) continue;
    const policyKey = publicKeyHex(readFileSync(pub, 'utf8'), pub);
    const privateHexFile = existsSync(priv) && publicKeyHex(readFileSync(priv, 'utf8'), priv) === policyKey ? priv : undefined;
    return { policyKey, policyKeyId: id, source: rel(pub), privateHexFile };
  }

  mkdirSync(KEYS, { recursive: true });
  const { privateKey } = generateKeyPairSync('ed25519');
  const jwk = privateKey.export({ format: 'jwk' });
  const seed = Buffer.from(jwk.d, 'base64url');
  const pub = Buffer.from(jwk.x, 'base64url');
  writeFileSync(join(KEYS, 'policy-signing.pem'), privateKey.export({ type: 'pkcs8', format: 'pem' }), { mode: 0o600 });
  writeFileSync(join(KEYS, 'policy-signing.key.hex'), Buffer.concat([seed, pub]).toString('hex') + '\n', { mode: 0o600 });
  writeFileSync(join(KEYS, 'policy-key.pub'), pub.toString('hex') + '\n');
  return {
    policyKey: pub.toString('hex'),
    policyKeyId: id,
    source: `${rel(join(KEYS, 'policy-key.pub'))} (minted now: a DEVELOPMENT key)`,
    privateHexFile: join(KEYS, 'policy-signing.key.hex'),
    minted: true,
  };
}

// ---------------------------------------------------------------------------------------------
// The stage

/**
 * Compile the payload, sign the classifier release into it and write the vendor-wide
 * capture-core.env. Returns what the build pinned, for release.json.
 */
export function stageGeneric({ version = PRODUCT.version, anchor, classifierKey, classifierRules, src, log = console.log }) {
  log('rendering and compiling the windows/amd64 payload …');
  must(run('node', [join(ROOT, 'installer', 'render.mjs')]), 'render');
  must(run('node', [join(ROOT, 'installer', 'build.mjs'), '--os', 'windows', '--arch', 'amd64', ...(src ? ['--src', src] : [])]), 'build');

  const key = resolve(classifierKey || process.env.SAC_CLASSIFIER_SIGNING_KEY || join(KEYS, 'classifier.key.hex'));
  const rules = resolve(classifierRules || join(ROOT, 'endpoint', 'classifier-host', 'testdata', 'dev-rules.json'));
  mkdirSync(dirname(key), { recursive: true });
  const host = join(STAGE, 'bin', 'classifier-host.exe');
  const released = must(
    run(host, ['release', '--dir', join(STAGE, 'classifier'), '--state', 'enforcing', '--version', version, '--key', key, '--rules', rules]),
    'classifier-host release',
  );
  const classifierPubkey = (/pubkey=([0-9a-f]{64})/.exec(released.out) ?? [])[1];
  if (!classifierPubkey) throw new BuildError(`could not read the classifier public key from:\n${released.out.trim()}`);
  log(`classifier release ${version} signed (rules: ${rel(rules)})`);

  const anchors = { SAC_POLICY_KEY: anchor.policyKey, SAC_POLICY_KEY_ID: anchor.policyKeyId, SAC_CLASSIFIER_PUBKEY: classifierPubkey };
  const lines = [
    `# ${PRODUCT.displayName} ${version} - vendor-wide configuration (generic package).`,
    `# Read first; ${TENANT_PACKAGE.fileName}, installed beside it as tenant.env, is read after it and wins.`,
    '# It carries no tenant, key or token. Do not edit: an upgrade replaces it.',
    ...genericProfile('windows', anchors).map(([k, v]) => `${k}=${v}`),
    '',
  ];
  writeFileSync(join(STAGE, 'etc', 'capture-core.env'), lines.join('\r\n'));
  return { classifierPubkey, classifierRules: rel(rules), classifierKey: rel(key), source: src ? rel(resolve(src)) : 'working tree' };
}

// ---------------------------------------------------------------------------------------------
// The MSI

/** Run Build-Msi.ps1. `outDir` is relative to the repository root. */
export function buildMsi({ outDir, version, profileDir, tenantEnv, freshEnrolment, sign }) {
  const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'installer', 'windows', 'Build-Msi.ps1'), '-OutDir', outDir];
  if (version) args.push('-Version', version);
  if (profileDir) args.push('-ProfileDir', profileDir);
  if (tenantEnv) args.push('-TenantEnv', tenantEnv);
  if (freshEnrolment) args.push('-FreshEnrolment');
  if (sign) args.push('-Sign');
  const res = spawnSync('powershell', args, { stdio: 'inherit', cwd: ROOT });
  if (res.status !== 0) throw new BuildError('Build-Msi.ps1 failed');
  return join(ROOT, outDir, 'ShadowAICapture.msi');
}

/** What the built package says about itself (Read-MsiInfo.ps1), with any tables asked for. */
export function readMsi(msiPath, tables = []) {
  const args = ['-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', join(ROOT, 'installer', 'windows', 'Read-MsiInfo.ps1'), '-Path', msiPath];
  if (tables.length) args.push('-Table', tables.join(','));
  const res = must(run('powershell', args), 'Read-MsiInfo.ps1');
  return JSON.parse(res.out);
}

/** Whether the file carries an Authenticode signature, as Windows reads it. */
export function signatureOf(path) {
  const res = run('powershell', ['-NoProfile', '-Command', `$s = Get-AuthenticodeSignature -LiteralPath '${path.replace(/'/g, "''")}'; "$($s.Status)|$($s.SignerCertificate.Subject)"`]);
  const [status, subject] = res.out.trim().split('|');
  return { signed: status !== 'NotSigned' && Boolean(subject), status, subject: subject || null };
}

/**
 * release.json, from the package itself: the codes Intune's detection rule and control-api's
 * .intunewin metadata need are read out of the built MSI, never predicted from the build inputs.
 */
export function writeReleaseJson(msiPath, extra = {}) {
  const info = readMsi(msiPath);
  const bytes = readFileSync(msiPath);
  const sig = signatureOf(msiPath);
  const release = {
    version: info.version,
    product_code: info.product_code,
    upgrade_code: info.upgrade_code,
    package_code: info.package_code,
    sha256: createHash('sha256').update(bytes).digest('hex'),
    size: statSync(msiPath).size,
    publisher: info.manufacturer,
    signed: sig.signed,
    file: basename(msiPath),
    name: info.product_name,
    install_command: `msiexec /i ${basename(msiPath)} /qn`,
    uninstall_command: `msiexec /x ${info.product_code} /qn`,
    tenant_file: TENANT_PACKAGE.fileName,
    built_at: new Date().toISOString(),
    ...extra,
  };
  writeFileSync(join(dirname(msiPath), 'release.json'), JSON.stringify(release, null, 2) + '\n');
  return release;
}

/** WiX writes its debug database beside the MSI; keep the release folder to what is shipped. */
export function moveWixPdb(outDir) {
  const pdb = join(ROOT, outDir, 'ShadowAICapture.wixpdb');
  if (!existsSync(pdb)) return;
  const dest = join(ROOT, 'installer', '.release', 'ShadowAICapture.wixpdb');
  mkdirSync(dirname(dest), { recursive: true });
  renameSync(pdb, dest);
}

