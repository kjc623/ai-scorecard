import { test } from 'node:test';
import assert from 'node:assert/strict';
import { verify } from 'node:crypto';

import { csrPEM, deviceKey } from './device.mjs';

/** Reads one DER element at `at`: its tag, its body and where the next element starts. */
function element(buf, at = 0) {
  const tag = buf[at];
  let len = buf[at + 1];
  let start = at + 2;
  if (len & 0x80) {
    const n = len & 0x7f;
    len = 0;
    for (let i = 0; i < n; i += 1) len = (len << 8) | buf[start + i];
    start += n;
  }
  return { tag, body: buf.subarray(start, start + len), raw: buf.subarray(at, start + len), next: start + len };
}

function children(buf) {
  const out = [];
  for (let at = 0; at < buf.length;) {
    const e = element(buf, at);
    out.push(e);
    at = e.next;
  }
  return out;
}

test('the CSR is a PKCS#10 request whose signature verifies under the device key', () => {
  const pair = deviceKey();
  const pem = csrPEM(pair, 'SIM-WIN-01');
  assert.match(pem, /^-----BEGIN CERTIFICATE REQUEST-----\n([A-Za-z0-9+/=]{1,64}\n)+-----END CERTIFICATE REQUEST-----\n$/);
  const der = Buffer.from(pem.replace(/-----[A-Z ]+-----|\n/g, ''), 'base64');

  const outer = element(der);
  assert.equal(outer.tag, 0x30);
  assert.equal(outer.next, der.length, 'nothing follows the request');
  const [info, algorithm, signature] = children(outer.body);
  assert.equal(info.tag, 0x30);
  assert.equal(algorithm.raw.toString('hex'), '300a06082a8648ce3d040302', 'ecdsa-with-SHA256, no parameters');
  assert.equal(signature.tag, 0x03);
  assert.equal(signature.body[0], 0, 'no unused bits');

  const [version, subject, spki, attributes] = children(info.body);
  assert.equal(version.raw.toString('hex'), '020100');
  assert.ok(subject.raw.includes(Buffer.from('SIM-WIN-01')));
  assert.deepEqual(spki.raw, pair.publicKey.export({ type: 'spki', format: 'der' }));
  assert.equal(attributes.raw.toString('hex'), 'a000');

  assert.ok(verify('sha256', info.raw, pair.publicKey, signature.body.subarray(1)));
});

test('a long subject still encodes its lengths correctly', () => {
  const pem = csrPEM(deviceKey(), 'x'.repeat(300));
  const der = Buffer.from(pem.replace(/-----[A-Z ]+-----|\n/g, ''), 'base64');
  assert.equal(element(der).next, der.length);
});
