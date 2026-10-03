// Generate the extension's pinned identity: an RSA keypair whose public half goes into
// manifest.json's `key` field, and the extension id Chromium derives from it.
//
// Why pin it at all: an unpacked extension's id is derived from its *path*, so it changes with the
// checkout directory. A native-messaging host manifest must name the exact extension id it will
// talk to (`allowed_origins`), and the register-by-policy path names the id too — so without a
// pinned key, no host registration can be prepared without first launching a browser.
//
// The id rule is Chromium's (extension_id.cc): sha256(SubjectPublicKeyInfo DER), first 16 bytes,
// each nibble mapped 0-f -> a-p.
import { createHash, generateKeyPairSync } from 'node:crypto';
import { writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const HERE = dirname(fileURLToPath(import.meta.url));

const { publicKey, privateKey } = generateKeyPairSync('rsa', {
  modulusLength: 2048,
  publicKeyEncoding: { type: 'spki', format: 'der' },
  privateKeyEncoding: { type: 'pkcs8', format: 'pem' },
});

const keyB64 = publicKey.toString('base64');
const digest = createHash('sha256').update(publicKey).digest().subarray(0, 16);
const id = [...digest].map((b) => b.toString(16).padStart(2, '0')).join('').replace(/[0-9a-f]/g, (c) => 'abcdefghijklmnop'[parseInt(c, 16)]);

// The private half is what packs a .crx later (production signs a real package). It is written to
// the ignored tools/ dir with a name that says it is a fixture, never a deployment key.
writeFileSync(join(HERE, 'extension-key.pem'), privateKey, { mode: 0o600 });
writeFileSync(join(HERE, 'extension-key.pub.der'), publicKey);
writeFileSync(join(HERE, 'extension-key.b64.txt'), keyB64 + '\n');

console.log('extension id :', id);
console.log('key (base64):', keyB64.slice(0, 40) + '...' + keyB64.slice(-12));
console.log('written      : extension-key.pem, extension-key.pub.der, extension-key.b64.txt');
