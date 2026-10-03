// scram.js — the client half of SCRAM-SHA-256 (RFC 5802, RFC 7677), which is what a PostgreSQL
// that asks a TCP client for a password asks for.
//
// The exchange is a challenge-response in three moves, and each move checks something rather than
// just computing something:
//
//   client-first  the client's nonce, and a GS2 header saying it offers no channel binding.
//   server-first  the salt and iteration count, and the server's nonce. The server's nonce must
//                 *extend* the client's, which is what stops a replayed challenge from another
//                 session, or a challenge from a server that never saw this connection, from
//                 producing anything useful.
//   server-final  the server's own proof. Verifying it is the move most hand-written clients skip,
//                 and skipping it turns the exchange into one-way authentication: the client proves
//                 it knows the password to whatever answered the socket, and learns nothing. It is
//                 verified here, in constant time, and a mismatch fails the connection.
//
// Two things this file deliberately does not do, because a wrong implementation of either is worse
// than none:
//
//   * No SASLprep (RFC 4013) of the password. For the ASCII secrets a deployment and a lab both
//     use, SASLprep is the identity. For a non-ASCII password containing a character SASLprep
//     would normalise, this client will fail authentication with 28P01 — a plain refusal, rather
//     than a silent normalisation that agrees with nobody.
//   * No channel binding. `SCRAM-SHA-256-PLUS` is not selected even when a TLS connection offered
//     it, because binding the exchange to the server's certificate is a different mechanism with a
//     different transcript, and this client does not implement it.

import { createHash, createHmac, pbkdf2, randomBytes, timingSafeEqual } from 'node:crypto';
import { CLIENT_CODE, PgClientError } from './error.js';

/** The only mechanism this file implements, and the name that goes in the SASLInitialResponse. */
export const SCRAM_MECHANISM = 'SCRAM-SHA-256';

/** 'n' = the client does not support channel binding; the empty authzid; the final comma. */
const GS2_HEADER = 'n,,';
const CLIENT_KEY = 'Client Key';
const SERVER_KEY = 'Server Key';
const DIGEST_LENGTH = 32;

function auth(code, message) {
  return new PgClientError(code, message);
}

/**
 * RFC 5802's saslname escaping, applied to a username that goes into the message. The client sends
 * the startup packet's `user` as its real identity; this field is the SASL name, and a name holding
 * a comma or an equals sign has to survive the attribute syntax.
 */
export function saslName(value) {
  return String(value).replace(/=/g, '=3D').replace(/,/g, '=2C');
}

/** The client nonce: printable ASCII without a comma, which base64 gives for free. */
export function randomNonce(bytes = 18) {
  return randomBytes(bytes).toString('base64');
}

/**
 * `a=1,b=2` into an object. Values may contain '=' (base64 padding), so the split is at the first
 * one. A mandatory extension ('m=') is refused: RFC 5802 requires the client to fail the exchange
 * rather than ignore it, because an ignored extension is one the server believes was honoured.
 */
export function parseAttributes(message) {
  const attributes = {};
  for (const part of String(message).split(',')) {
    const split = part.indexOf('=');
    if (split < 1) throw auth(CLIENT_CODE.AUTH, 'the server sent a SCRAM message this client cannot parse');
    attributes[part.slice(0, split)] = part.slice(split + 1);
  }
  if (attributes.m !== undefined) {
    throw auth(CLIENT_CODE.AUTH, "the server requires a SCRAM extension this client does not implement (an 'm=' attribute)");
  }
  return attributes;
}

/** PBKDF2, asynchronously: a server is free to demand an iteration count that would block an HTTP server's event loop. */
function derive(password, salt, iterations) {
  return new Promise((resolve, reject) => {
    pbkdf2(password, salt, iterations, DIGEST_LENGTH, 'sha256', (error, key) => {
      if (error) reject(auth(CLIENT_CODE.AUTH, `the SCRAM key derivation failed: ${error.message}`));
      else resolve(key);
    });
  });
}

function hmac(key, data) {
  return createHmac('sha256', key).update(data, 'utf8').digest();
}

function sha256(data) {
  return createHash('sha256').update(data).digest();
}

function xor(left, right) {
  const out = Buffer.allocUnsafe(left.length);
  for (let i = 0; i < left.length; i += 1) out[i] = left[i] ^ right[i];
  return out;
}

/**
 * One SCRAM exchange over one connection.
 *
 * @param {object} options
 * @param {string} options.password
 * @param {string} [options.user] the SASL name; PostgreSQL ignores it, so the wire sends it empty
 * @param {string} [options.nonce] injected only by a test that compares against a published vector
 */
export function createScramClient({ password, user = '', nonce = randomNonce() } = {}) {
  if (typeof password !== 'string' || password.length === 0) {
    throw auth(CLIENT_CODE.AUTH, 'the server asked for a password and none is configured for this client');
  }
  let clientFirstBare = null;
  let serverFirst = null;
  let serverNonce = null;
  let saltedPassword = null;
  let serverSignature = null;

  return {
    mechanism: SCRAM_MECHANISM,

    /** The client's nonce, exposed so a caller can log the exchange identity without logging a secret. */
    get nonce() {
      return nonce;
    },

    /** client-first-message, with the GS2 header in front of it. */
    clientFirstMessage() {
      clientFirstBare = `n=${saslName(user)},r=${nonce}`;
      return `${GS2_HEADER}${clientFirstBare}`;
    },

    /**
     * Consume server-first-message and derive the salted password from its salt and iteration count.
     *
     * @param {string} message
     * @returns {Promise<{r: string, s: string, i: string}>}
     */
    async receiveServerFirst(message) {
      if (clientFirstBare === null) throw auth(CLIENT_CODE.AUTH, 'server-first-message arrived before client-first-message was sent');
      const attributes = parseAttributes(message);
      const salt = attributes.s;
      const iterations = Number(attributes.i);
      const receivedNonce = attributes.r;
      if (!receivedNonce || !salt || !Number.isInteger(iterations) || iterations < 1) {
        throw auth(CLIENT_CODE.AUTH, 'server-first-message is missing its nonce, salt or iteration count');
      }
      if (!receivedNonce.startsWith(nonce)) {
        // A server nonce that does not extend ours is either a replay or a server that never read
        // our first message. Both are the attacker this check exists for.
        throw auth(CLIENT_CODE.AUTH, "the server's SCRAM nonce does not extend the client's nonce; the exchange is being replayed or answered by something that never saw the client-first message");
      }
      serverFirst = String(message);
      serverNonce = receivedNonce;
      saltedPassword = await derive(password, Buffer.from(salt, 'base64'), iterations);
      return attributes;
    },

    /**
     * client-final-message: the channel binding, the nonce, and the client proof.
     *
     * @returns {Promise<string>}
     */
    async clientFinalMessage() {
      if (serverFirst === null || serverNonce === null || saltedPassword === null) {
        throw auth(CLIENT_CODE.AUTH, 'client-final-message was requested before server-first-message was processed');
      }
      const channelBinding = Buffer.from(GS2_HEADER, 'utf8').toString('base64');
      const withoutProof = `c=${channelBinding},r=${serverNonce}`;
      const authMessage = `${clientFirstBare},${serverFirst},${withoutProof}`;
      const clientKey = hmac(saltedPassword, CLIENT_KEY);
      const storedKey = sha256(clientKey);
      const clientSignature = hmac(storedKey, authMessage);
      const proof = xor(clientKey, clientSignature).toString('base64');
      serverSignature = hmac(hmac(saltedPassword, SERVER_KEY), authMessage).toString('base64');
      return `${withoutProof},p=${proof}`;
    },

    /**
     * Verify server-final-message. A mismatch means the peer does not know the password, which on a
     * connection that has already carried statements is a serious finding, not a warning.
     *
     * @param {string} message
     */
    verifyServerFinal(message) {
      const attributes = parseAttributes(message);
      if (attributes.e !== undefined) {
        throw auth(CLIENT_CODE.AUTH, `the server refused the SCRAM exchange: ${attributes.e}`);
      }
      if (serverSignature === null) {
        throw auth(CLIENT_CODE.AUTH, 'server-final-message arrived before the client proved itself');
      }
      if (attributes.v === undefined) {
        throw auth(CLIENT_CODE.AUTH, 'server-final-message carries neither a verifier nor an error');
      }
      const expected = Buffer.from(serverSignature, 'base64');
      const actual = Buffer.from(attributes.v, 'base64');
      if (expected.length !== actual.length || !timingSafeEqual(expected, actual)) {
        throw auth(CLIENT_CODE.AUTH, 'the server did not prove that it knows the password: the SCRAM verifier does not match');
      }
    },
  };
}
