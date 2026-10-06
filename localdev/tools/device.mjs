// What a device does on the wire, for the lab's tools: a P-256 key, a PKCS#10 CSR, a first enrolment
// with the deployment key, and requests through the edge with the issued certificate.

import { generateKeyPairSync, sign } from 'node:crypto';
import { request } from 'node:https';

// --- a minimal DER encoder, enough for one CertificationRequest -------------------------------

function length(n) {
  if (n < 0x80) return Buffer.from([n]);
  const bytes = [];
  for (let v = n; v > 0; v >>= 8) bytes.unshift(v & 0xff);
  return Buffer.from([0x80 | bytes.length, ...bytes]);
}

const tlv = (tag, ...parts) => {
  const body = Buffer.concat(parts);
  return Buffer.concat([Buffer.from([tag]), length(body.length), body]);
};
const sequence = (...parts) => tlv(0x30, ...parts);
const set = (...parts) => tlv(0x31, ...parts);

const OID_COMMON_NAME = Buffer.from([0x06, 0x03, 0x55, 0x04, 0x03]); // 2.5.4.3
const OID_ECDSA_WITH_SHA256 = Buffer.from([0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02]); // 1.2.840.10045.4.3.2

/** A PKCS#10 request for the key pair, subject CN=commonName, signed ECDSA with SHA-256, as PEM. */
export function csrPEM({ privateKey, publicKey }, commonName) {
  const info = sequence(
    tlv(0x02, Buffer.from([0])), // version 1
    sequence(set(sequence(OID_COMMON_NAME, tlv(0x0c, Buffer.from(commonName, 'utf8'))))),
    publicKey.export({ type: 'spki', format: 'der' }),
    Buffer.from([0xa0, 0x00]), // no attributes
  );
  const signature = sign('sha256', info, privateKey); // DER ECDSA-Sig-Value
  const der = sequence(info, sequence(OID_ECDSA_WITH_SHA256), tlv(0x03, Buffer.from([0]), signature));
  return `-----BEGIN CERTIFICATE REQUEST-----\n${der.toString('base64').match(/.{1,64}/g).join('\n')}\n-----END CERTIFICATE REQUEST-----\n`;
}

/** A fresh device key pair. */
export function deviceKey() {
  return generateKeyPairSync('ec', { namedCurve: 'P-256' });
}

/**
 * One request to the edge. `ca` is the lab CA (it signs the edge's certificate); `cert` and `key`
 * are the device's credential, when it has one. Resolves {status, json, text}.
 */
export function edgeRequest(edge, ca, { method = 'POST', path, body, cert, key }) {
  const payload = body === undefined ? null : Buffer.from(JSON.stringify(body));
  return new Promise((done, fail) => {
    const req = request(`${edge}${path}`, {
      method,
      ca,
      cert,
      key,
      headers: payload ? { 'content-type': 'application/json', 'content-length': payload.length } : {},
    }, (res) => {
      const chunks = [];
      res.on('data', (c) => chunks.push(c));
      res.on('end', () => {
        const text = Buffer.concat(chunks).toString('utf8');
        let json = null;
        try {
          json = JSON.parse(text);
        } catch {
          // not JSON; the text is reported
        }
        done({ status: res.statusCode, json, text });
      });
    });
    req.on('error', fail);
    req.end(payload ?? undefined);
  });
}

/**
 * A first enrolment with the tenant's deployment key. Resolves {deviceId, cert, key} with the
 * certificate and key as PEM, ready for edgeRequest.
 */
export async function enrol(edge, ca, deploymentKey, device) {
  const pair = deviceKey();
  const res = await edgeRequest(edge, ca, {
    path: '/v1/enrol',
    body: { schema_version: '1.0', deployment_key: deploymentKey, csr: csrPEM(pair, device.hostname ?? 'device'), device },
  });
  if (res.status !== 200 || !res.json?.credential?.cert_pem) {
    throw new Error(`enrol ${device.hostname ?? ''}: ${res.status} ${res.text.slice(0, 300)}`);
  }
  return {
    deviceId: res.json.device_id,
    cert: res.json.credential.cert_pem,
    key: pair.privateKey.export({ type: 'pkcs8', format: 'pem' }),
  };
}
