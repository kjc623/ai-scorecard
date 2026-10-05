#!/usr/bin/env node
// installer/release-msi.mjs - build the generic, code-only Windows MSI a vendor ships.
//
//   node installer/release-msi.mjs                       # installer/dist/release/{ShadowAICapture.msi,release.json}
//   node installer/release-msi.mjs --version 0.2.0 --policy-key-file vendor-policy.pub --sign
//
// One MSI for every tenant. It carries the binaries, the signed classifier release and the
// vendor-wide capture-core.env (layout, product defaults, the policy and classifier trust anchors);
// it carries no tenant, deployment key or token. control-api's Settings -> Deployment download puts
// ShadowAICapture.tenant.env beside it, and the MSI copies that file at install, so the whole
// install command is `msiexec /i ShadowAICapture.msi /qn` (docs/05-platform-delivery.md §6.1).
//
// release.json is read back out of the built package (ProductCode, UpgradeCode, version, package
// code), never predicted. control-api reads this folder as SAC_AGENT_RELEASE_DIR.
//
//   --version X.Y.Z        MSI ProductVersion; default the manifest's. Raise it for every release
//                          an MDM should upgrade to: Intune supersedence and detection key on it.
//   --policy-key HEX | --policy-key-file FILE   the policy trust anchor (control-api's
//                          SAC_POLICY_SIGNING_KEY_FILE public half; hex, or a PEM key, private or
//                          public). Also SAC_POLICY_TRUST_KEY[_FILE]. Default: the lab's vendor key
//                          (localdev/.authlab-identity/policy-signing.pub.hex) when this checkout
//                          has a lab, else installer/.release/keys/policy-key.pub, minted as a
//                          DEVELOPMENT pair when absent.
//   --policy-key-id ID     the key id bundles must name (default policy-key-1)
//   --classifier-key FILE  classifier signing seed (hex); default installer/.release/keys/classifier.key.hex,
//                          created when absent. --classifier-rules FILE: default the development rules.
//   --src DIR              compile the binaries from DIR (an export of a release tag) instead of the working tree
//   --sign                 sign the executables and the MSI (Build-Msi.ps1 documents the signer settings)
//
// It needs WiX (v5+) and Windows. It installs nothing.

import { createHash } from 'node:crypto';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

import { PRODUCT, TENANT_PACKAGE } from './manifest.mjs';
import { BuildError, KEYS, ROOT, buildMsi, moveWixPdb, readMsi, rel, resolvePolicyAnchor, stageGeneric, writeReleaseJson } from './windows/msi.mjs';

const ARGS = process.argv.slice(2);
function valueOf(flag, fallback) {
  const i = ARGS.indexOf(flag);
  return i === -1 ? fallback : ARGS[i + 1];
}
function step(msg) {
  console.log(`release-msi: ${msg}`);
}

if (process.platform !== 'win32') {
  console.error('release-msi: the MSI is built with WiX and read back through Windows Installer; run this on Windows');
  process.exit(1);
}

const VERSION = valueOf('--version', PRODUCT.version);
const OUT = join('installer', 'dist', 'release');

try {
  if (!/^\d+\.\d+\.\d+$/.test(VERSION)) throw new BuildError(`--version ${VERSION}: an MSI ProductVersion is major.minor.build`);
  const anchor = resolvePolicyAnchor({ key: valueOf('--policy-key'), keyFile: valueOf('--policy-key-file'), keyId: valueOf('--policy-key-id') });
  step(`policy trust anchor ${anchor.policyKeyId} from ${anchor.source}`);
  if (anchor.minted) {
    step(`  the private half is in ${rel(KEYS)}; a lab control-api can sign with it (SAC_POLICY_SIGNING_KEY_FILE).`);
    step('  A vendor release passes the production public key with --policy-key-file instead.');
  }

  const staged = stageGeneric({ version: VERSION, anchor, classifierKey: valueOf('--classifier-key'), classifierRules: valueOf('--classifier-rules'), src: valueOf('--src'), log: step });
  const generic = readFileSync(join(ROOT, 'installer', '.stage', 'windows-amd64', 'etc', 'capture-core.env'), 'utf8');

  step(`building ShadowAICapture.msi ${VERSION} …`);
  const msi = buildMsi({ outDir: OUT, version: VERSION, sign: ARGS.includes('--sign') });
  moveWixPdb(OUT);

  // What went into the package, checked against the package: no copied file is shipped, the
  // service reads both files in order, and the installed vendor file is the one staged.
  const info = readMsi(msi, ['File', 'MoveFile', 'ServiceInstall']);
  const files = info.tables.File.map((f) => f.FileName.split('|').pop());
  if (files.some((f) => /tenant\.env$/i.test(f))) throw new BuildError('the generic MSI installs a tenant file; it must only copy one');
  const move = info.tables.MoveFile.find((m) => m.SourceName === TENANT_PACKAGE.fileName);
  if (!move) throw new BuildError(`the MSI has no MoveFile row for ${TENANT_PACKAGE.fileName}`);

  const release = writeReleaseJson(msi, {
    source: staged.source,
    trust: {
      policy_key_id: anchor.policyKeyId,
      policy_key: anchor.policyKey,
      classifier_pubkey: staged.classifierPubkey,
      classifier_rules: staged.classifierRules,
      generic_config_sha256: createHash('sha256').update(generic).digest('hex'),
    },
  });

  console.log(`
release-msi: built ${rel(msi)}
${JSON.stringify(release, null, 2)}

  ${rel(join(ROOT, OUT))} is what control-api serves as SAC_AGENT_RELEASE_DIR. Nothing was installed.
  Intune: Win32 app from ShadowAICapture.msi + ${TENANT_PACKAGE.fileName}; detection = MSI product code
  ${release.product_code}, version ${release.version} or later (installer/README.md, "Deploying it").`);
} catch (err) {
  if (err instanceof BuildError) {
    console.error(`release-msi: ${err.message}`);
    process.exit(1);
  }
  throw err;
}
