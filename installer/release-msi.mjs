#!/usr/bin/env node
// installer/release-msi.mjs - build the agent release control-api serves: the generic Windows MSI,
// the browser extension CRX, and release.json describing both.
//
//   node installer/release-msi.mjs --version 1.4.0 \
//     --policy-key-file policy-signing.pub --policy-key-id policy-key-1 \
//     --classifier-key classifier-signing.key \
//     --classifier-rules endpoint/classifier-host/rules/default.json \
//     --classifier-model endpoint/classifier-host/rules/model.json \
//     --extension-key extension-signing.pem \
//     [--wix-eula wix7] [--sign] [--out installer/dist/release]
//
// Every input is required; --extension-key may instead come from SAC_EXTENSION_SIGNING_KEY (the PEM
// itself). --wix-eula accepts the WiX Open Source Maintenance Fee EULA for an unattended build;
// --sign signs the executables and the MSI with the signer msi.mjs reads from the environment.
// release.json is read back out of the built package, and the build fails if any release check does.

import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { parseArgs } from 'node:util';

import { buildCrx } from '../extension/tools/build-crx.mjs';
import { BuildError, ROOT, STAGE_OPTIONS, buildStage, resolveInputs } from './build.mjs';
import { TENANT_PACKAGE } from './manifest.mjs';
import { MSI_FILE, buildMsi, isSigned, readMsi, releaseChecks } from './windows/msi.mjs';

const log = (msg) => console.log(`release-msi: ${msg}`);

try {
  if (process.platform !== 'win32') throw new BuildError('the MSI is built with WiX and read back through Windows Installer: run this on Windows');
  const { values } = parseArgs({
    options: {
      ...STAGE_OPTIONS,
      'extension-key': { type: 'string' },
      'wix-eula': { type: 'string' },
      sign: { type: 'boolean', default: false },
      out: { type: 'string', default: join(ROOT, 'installer', 'dist', 'release') },
    },
  });
  const inputs = resolveInputs(values);
  const extensionKey = values['extension-key'] ? readFileSync(values['extension-key'], 'utf8') : process.env.SAC_EXTENSION_SIGNING_KEY;
  if (!extensionKey) throw new BuildError('missing required input: --extension-key (or SAC_EXTENSION_SIGNING_KEY)');
  const out = resolve(values.out);

  const { stage, anchors, generic } = buildStage({ os: 'windows', arch: 'amd64', inputs, log });
  rmSync(out, { recursive: true, force: true });
  mkdirSync(out, { recursive: true });

  log(`build ${MSI_FILE} ${inputs.version}`);
  const msi = buildMsi({ stage, outDir: out, version: inputs.version, sign: values.sign, wixEula: values['wix-eula'], log });
  log('package the extension');
  const extension = await buildCrx({ keyPem: extensionKey, version: inputs.version, outDir: out }).catch((err) => {
    throw new BuildError(`extension: ${err.message}`);
  });

  const info = readMsi(msi);
  const bytes = readFileSync(msi);
  const release = {
    version: info.version,
    product_code: info.product_code,
    upgrade_code: info.upgrade_code,
    package_code: info.package_code,
    name: info.product_name,
    publisher: info.manufacturer,
    file: MSI_FILE,
    sha256: createHash('sha256').update(bytes).digest('hex'),
    size: bytes.length,
    signed: isSigned(msi),
    install_command: `msiexec /i ${MSI_FILE} /qn`,
    uninstall_command: `msiexec /x ${info.product_code} /qn`,
    tenant_file: TENANT_PACKAGE.fileName,
    trust: {
      policy_key: anchors.SAC_POLICY_KEY,
      policy_key_id: anchors.SAC_POLICY_KEY_ID,
      classifier_pubkey: anchors.SAC_CLASSIFIER_PUBKEY,
      generic_config_sha256: createHash('sha256').update(generic).digest('hex'),
    },
    extension,
    built_at: new Date().toISOString(),
  };
  writeFileSync(join(out, 'release.json'), JSON.stringify(release, null, 2) + '\n');

  const failed = releaseChecks(out).filter(([, ok]) => !ok);
  if (failed.length) throw new BuildError(`the built release fails its checks:\n${failed.map(([name, , detail]) => `  ${name}: ${detail}`).join('\n')}`);
  log(`built ${out}`);
  console.log(JSON.stringify(release, null, 2));
} catch (err) {
  if (!(err instanceof BuildError) && !String(err.code).startsWith('ERR_PARSE_ARGS')) throw err;
  console.error(`release-msi: ${err.message}`);
  process.exit(1);
}
