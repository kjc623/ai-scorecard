// protocol.js — the v3 wire, as bytes out and bytes in.
//
// The whole of the framing is: one byte naming the message, then an Int32 length that counts itself.
// A reader therefore either holds a complete message or holds nothing usable, which is why a
// hand-written client of this size is a reasonable thing for this repository to own rather than a
// dependency it cannot install.
//
// Two decisions in here are load-bearing and are explained where they are taken:
//
//   * `query()` always uses the extended protocol (Parse/Bind/Describe/Execute/Sync), never the
//     simple one. There is no code path in this file that puts a parameter value into SQL text,
//     so "the tenant predicate is the only thing scoping a read" cannot be undone by a quoting
//     bug here.
//   * Parameters and results are both in *text* format. Text is not a compromise: PostgreSQL
//     renders each value with its type's output function and parses what we send with its input
//     function, so a JS string, number, Date, boolean, Buffer or array each have exactly one
//     documented text form (see types.js), and the type identity that decides the decoding comes
//     from RowDescription rather than from a guess at the bytes.
//
// Nothing here knows about sockets, buffers on the wire, or the client's state: it is pure
// encode/decode, which is what makes it unit-testable with no database anywhere near it.

import { CLIENT_CODE, PgClientError } from './error.js';

/** 3.0 as the startup packet spells it: major << 16 | minor. */
export const PROTOCOL_VERSION_3 = 3 << 16;

/** The magic "please upgrade to TLS" request code, in place of a protocol version. */
export const SSL_REQUEST_CODE = 80877103;

const EMPTY = Buffer.alloc(0);

/** Every message is preceded by its own length including that length field. */
const HEADER_BYTES = 4;

/**
 * A framed message: one type byte, an Int32 length, the payload. The length is computed here rather
 * than passed in, because a wrong length is a protocol desynchronisation that no test can attribute
 * to a caller.
 *
 * @param {string} type one character, e.g. 'P'
 * @param {Buffer} [payload]
 * @returns {Buffer}
 */
export function frame(type, payload = EMPTY) {
  if (typeof type !== 'string' || type.length !== 1) {
    throw new PgClientError(CLIENT_CODE.PROTOCOL, `message type must be one character, got ${JSON.stringify(type)}`);
  }
  const out = Buffer.allocUnsafe(1 + HEADER_BYTES + payload.length);
  out[0] = type.charCodeAt(0);
  out.writeInt32BE(HEADER_BYTES + payload.length, 1);
  payload.copy(out, 1 + HEADER_BYTES);
  return out;
}

/** A NUL-terminated UTF-8 string, the only string form the frontend protocol has. */
export function cstring(value) {
  const text = String(value);
  // A NUL inside a field would end it early and shift every following byte: the server would read a
  // different database name, or a different statement, without anything failing. Refusing is the
  // only safe reading of "this value cannot be represented here".
  if (text.includes('\0')) {
    throw new PgClientError(CLIENT_CODE.CONFIG, 'a protocol string cannot contain a NUL byte');
  }
  const out = Buffer.allocUnsafe(Buffer.byteLength(text, 'utf8') + 1);
  out.write(text, 0, 'utf8');
  out[out.length - 1] = 0;
  return out;
}

/** Int32, network byte order. */
export function int32(value) {
  const out = Buffer.allocUnsafe(4);
  out.writeInt32BE(value, 0);
  return out;
}

/** Int16, network byte order. */
export function int16(value) {
  const out = Buffer.allocUnsafe(2);
  out.writeInt16BE(value, 0);
  return out;
}

/**
 * The StartupMessage: no type byte, because the server has no idea who is talking yet. `undefined`
 * values are omitted rather than sent empty, so "not configured" and "configured as empty" stay
 * distinguishable to the server's GUC handling.
 *
 * @param {Record<string, string|number|undefined|null>} parameters
 * @returns {Buffer}
 */
export function startupMessage(parameters) {
  const parts = [int32(PROTOCOL_VERSION_3)];
  for (const [key, value] of Object.entries(parameters)) {
    if (value === undefined || value === null) continue;
    parts.push(cstring(key), cstring(value));
  }
  parts.push(Buffer.from([0]));
  const body = Buffer.concat(parts);
  return Buffer.concat([int32(body.length + HEADER_BYTES), body]);
}

/** The SSLRequest: a startup packet whose version field is a request to negotiate TLS. */
export function sslRequestMessage() {
  return Buffer.concat([int32(8), int32(SSL_REQUEST_CODE)]);
}

// ---------------------------------------------------------------------------------------------
// Frontend messages
// ---------------------------------------------------------------------------------------------

/**
 * Parse: name the statement and hand over its text. The parameter count is zero, which means "infer
 * each parameter's type from where it is used" — and it always can, because compile.js casts every
 * placeholder (`$1::uuid`, `$2::text[]`). That is why this client never asks its caller for types:
 * the caller's SQL already carries them, and asking would be a second source of truth for the same
 * fact.
 */
export function parseMessage(text, { statement = '' } = {}) {
  return frame('P', Buffer.concat([cstring(statement), cstring(text), int16(0)]));
}

/**
 * Bind: the parameter values, as text, plus NULLs as length -1. Both format-code lists are empty,
 * which the protocol defines as "all text" for parameters and results.
 *
 * @param {Array<Buffer|null>} encoded each value already rendered by types.encodeParam, or null
 * @param {{portal?: string, statement?: string}} [names]
 */
export function bindMessage(encoded, { portal = '', statement = '' } = {}) {
  const parts = [cstring(portal), cstring(statement), int16(0), int16(encoded.length)];
  for (const value of encoded) {
    if (value === null || value === undefined) {
      parts.push(int32(-1));
      continue;
    }
    parts.push(int32(value.length), value);
  }
  parts.push(int16(0));
  return frame('B', Buffer.concat(parts));
}

/** Describe a portal: the RowDescription of what Bind produced, which is what decides decoding. */
export function describeMessage(kind = 'P', name = '') {
  return frame('D', Buffer.concat([Buffer.from(kind, 'ascii'), cstring(name)]));
}

/** Execute a portal. Zero means "every row", so there is no PortalSuspended to handle. */
export function executeMessage({ portal = '', maxRows = 0 } = {}) {
  return frame('E', Buffer.concat([cstring(portal), int32(maxRows)]));
}

/** Sync: the point at which the server answers and returns to ReadyForQuery, error or not. */
export function syncMessage() {
  return frame('S');
}

/** Terminate: a graceful goodbye, so the server can reap the backend instead of waiting for a reset. */
export function terminateMessage() {
  return frame('X');
}

/**
 * A password message. Type 'p' serves cleartext, MD5 and both SASL messages; the server knows which
 * one it asked for, so the shape is decided by the authentication exchange rather than duplicated
 * here.
 */
export function passwordMessage(password) {
  return frame('p', cstring(password));
}

/** SASLInitialResponse: the mechanism name, then the client-first message. */
export function saslInitialResponse(mechanism, initial) {
  const body = Buffer.from(initial, 'utf8');
  return frame('p', Buffer.concat([cstring(mechanism), int32(body.length), body]));
}

/** SASLResponse: the client-final message. No length prefix, unlike the initial response. */
export function saslResponse(data) {
  return frame('p', Buffer.from(data, 'utf8'));
}

/**
 * The MD5 password response: `md5` + md5(md5(password + user) + salt).
 *
 * The inner hash is not a strengthening of the outer one — it exists so a server that stores
 * md5(password + user) can check the response without learning the password. It is offered for a
 * local server whose pg_hba says md5; a deployed PostgreSQL here authenticates a managed identity
 * and never asks for it.
 *
 * @param {string} password
 * @param {string} user
 * @param {Buffer} salt the four bytes the server sent
 * @param {(algorithm: string, data: Buffer|string) => import('node:crypto').Hash} [hash]
 */
export function md5Password(password, user, salt, hash) {
  const inner = hash('md5').update(`${password}${user}`, 'utf8').digest('hex');
  return `md5${hash('md5').update(Buffer.concat([Buffer.from(inner, 'ascii'), salt])).digest('hex')}`;
}

// ---------------------------------------------------------------------------------------------
// Backend messages
// ---------------------------------------------------------------------------------------------

/**
 * Reassembles the byte stream into whole messages.
 *
 * Chunks are kept as a list and consumed by offset rather than concatenated on every push: a result
 * set arrives in many chunks, and one Buffer.concat per chunk is quadratic in the response size —
 * the difference between a client that is fine on a lab and one that is not on a real read.
 */
export class MessageReader {
  #chunks = [];

  #size = 0;

  /** Bytes received and not yet consumed. */
  get buffered() {
    return this.#size;
  }

  /**
   * @param {Buffer} chunk
   * @returns {Array<{type: string, payload: Buffer}>} every message the chunk completed, in order
   */
  push(chunk) {
    if (chunk.length > 0) {
      this.#chunks.push(chunk);
      this.#size += chunk.length;
    }
    const messages = [];
    for (;;) {
      const header = this.#peek(5);
      if (!header) break;
      const length = header.readInt32BE(1);
      // The length counts itself and cannot be smaller than itself. A value below 4 means the
      // stream is not this protocol any more (a plaintext server that answered a StartupMessage
      // with an HTTP error, say) — resynchronising is impossible and guessing is worse.
      if (length < HEADER_BYTES) {
        throw new PgClientError(
          CLIENT_CODE.PROTOCOL,
          `the server sent a message whose length field is ${length}, which cannot be a protocol message`,
        );
      }
      if (this.#size < 1 + length) break;
      const raw = this.#take(1 + length);
      messages.push({ type: String.fromCharCode(raw[0]), payload: raw.subarray(5) });
    }
    return messages;
  }

  #peek(count) {
    if (this.#size < count) return null;
    const first = this.#chunks[0];
    if (first.length >= count) return first.subarray(0, count);
    const out = Buffer.allocUnsafe(count);
    let copied = 0;
    for (const chunk of this.#chunks) {
      const take = Math.min(count - copied, chunk.length);
      chunk.copy(out, copied, 0, take);
      copied += take;
      if (copied === count) break;
    }
    return out;
  }

  #take(count) {
    const first = this.#chunks[0];
    if (first.length >= count) {
      this.#size -= count;
      if (first.length === count) this.#chunks.shift();
      else this.#chunks[0] = first.subarray(count);
      return first.subarray(0, count);
    }
    const out = Buffer.allocUnsafe(count);
    let copied = 0;
    while (copied < count) {
      const chunk = this.#chunks[0];
      const take = Math.min(count - copied, chunk.length);
      chunk.copy(out, copied, 0, take);
      copied += take;
      if (take === chunk.length) this.#chunks.shift();
      else this.#chunks[0] = chunk.subarray(take);
    }
    this.#size -= count;
    return out;
  }
}

/** The Authentication message. The code decides which extra fields are present. */
export function parseAuthentication(payload) {
  const code = payload.readInt32BE(0);
  const rest = payload.subarray(4);
  switch (code) {
    case 0:
      return { code, method: 'ok' };
    case 2:
      return { code, method: 'kerberos' };
    case 3:
      return { code, method: 'cleartext' };
    case 5:
      return { code, method: 'md5', salt: Buffer.from(rest.subarray(0, 4)) };
    case 6:
      return { code, method: 'scm' };
    case 7:
      return { code, method: 'gss' };
    case 8:
      return { code, method: 'gss-continue', data: rest };
    case 9:
      return { code, method: 'sspi' };
    case 10:
      return { code, method: 'sasl', mechanisms: splitCStrings(rest) };
    case 11:
      return { code, method: 'sasl-continue', data: rest.toString('utf8') };
    case 12:
      return { code, method: 'sasl-final', data: rest.toString('utf8') };
    default:
      return { code, method: 'unknown' };
  }
}

/** A list of NUL-terminated strings ending with an empty one, as AuthenticationSASL carries. */
export function splitCStrings(buffer) {
  const out = [];
  let start = 0;
  for (let i = 0; i < buffer.length; i += 1) {
    if (buffer[i] !== 0) continue;
    if (i === start) break;
    out.push(buffer.toString('utf8', start, i));
    start = i + 1;
  }
  return out;
}

/**
 * ErrorResponse / NoticeResponse: a sequence of (one-letter code, string) pairs ending with a zero
 * byte. Kept as the raw field map as well as the readable names, because a field this client has
 * never heard of still belongs to the caller's error report.
 */
export function parseErrorOrNotice(payload) {
  const fields = {};
  let offset = 0;
  while (offset < payload.length) {
    const code = payload[offset];
    if (code === 0) break;
    offset += 1;
    const end = payload.indexOf(0, offset);
    if (end < 0) break;
    fields[String.fromCharCode(code)] = payload.toString('utf8', offset, end);
    offset = end + 1;
  }
  return {
    fields,
    mapped: {
      severity: fields.S ?? null,
      severityLocalized: fields.V ?? null,
      code: fields.C ?? null,
      message: fields.M ?? null,
      detail: fields.D ?? null,
      hint: fields.H ?? null,
      position: fields.P ?? null,
      internalPosition: fields.p ?? null,
      internalQuery: fields.q ?? null,
      where: fields.W ?? null,
      schema: fields.s ?? null,
      table: fields.t ?? null,
      column: fields.c ?? null,
      dataType: fields.d ?? null,
      constraint: fields.n ?? null,
      file: fields.F ?? null,
      line: fields.L ?? null,
      routine: fields.R ?? null,
    },
  };
}

/**
 * RowDescription: the name and type of each output column, in the order DataRow will send them.
 *
 * The type OID is the load-bearing field — it is the only honest source for how a value should be
 * decoded — and it is the one the frozen `fields` shape keeps.
 */
export function parseRowDescription(payload) {
  const count = payload.readInt16BE(0);
  const fields = [];
  let offset = 2;
  for (let i = 0; i < count; i += 1) {
    const end = payload.indexOf(0, offset);
    if (end < 0) {
      throw new PgClientError(CLIENT_CODE.PROTOCOL, 'RowDescription ended inside a column name');
    }
    const name = payload.toString('utf8', offset, end);
    offset = end + 1;
    fields.push({
      name,
      tableOID: payload.readInt32BE(offset),
      columnID: payload.readInt16BE(offset + 4),
      dataTypeID: payload.readInt32BE(offset + 6),
      dataTypeSize: payload.readInt16BE(offset + 10),
      typeModifier: payload.readInt32BE(offset + 12),
      format: payload.readInt16BE(offset + 16),
    });
    offset += 18;
  }
  return fields;
}

/** DataRow: one entry per described column; a length of -1 is SQL NULL, which is not an empty string. */
export function parseDataRow(payload) {
  const count = payload.readInt16BE(0);
  const values = [];
  let offset = 2;
  for (let i = 0; i < count; i += 1) {
    const length = payload.readInt32BE(offset);
    offset += 4;
    if (length < 0) {
      values.push(null);
      continue;
    }
    values.push(payload.subarray(offset, offset + length));
    offset += length;
  }
  return values;
}

/** CommandComplete: the command tag, e.g. 'SELECT 5' or 'INSERT 0 1'. */
export function parseCommandComplete(payload) {
  return payload.toString('utf8', 0, payload.indexOf(0) < 0 ? payload.length : payload.indexOf(0));
}

/** ReadyForQuery: 'I' idle, 'T' in a transaction, 'E' in a failed transaction. */
export function parseReadyForQuery(payload) {
  return String.fromCharCode(payload[0]);
}

/** ParameterStatus: a server setting the client is expected to track, e.g. server_version. */
export function parseParameterStatus(payload) {
  const split = payload.indexOf(0);
  const end = payload.indexOf(0, split + 1);
  return {
    name: payload.toString('utf8', 0, split),
    value: payload.toString('utf8', split + 1, end < 0 ? payload.length : end),
  };
}

/** BackendKeyData: the pid and cancel key of this session. */
export function parseBackendKeyData(payload) {
  return { pid: payload.readInt32BE(0), secretKey: payload.readInt32BE(4) };
}

/** NotificationResponse: a LISTEN/NOTIFY delivery. */
export function parseNotificationResponse(payload) {
  const pid = payload.readInt32BE(0);
  const channelEnd = payload.indexOf(0, 4);
  const channel = payload.toString('utf8', 4, channelEnd);
  const bodyEnd = payload.indexOf(0, channelEnd + 1);
  return {
    pid,
    channel,
    payload: payload.toString('utf8', channelEnd + 1, bodyEnd < 0 ? payload.length : bodyEnd),
  };
}

/** ParameterDescription: the types the server inferred for a parsed statement's parameters. */
export function parseParameterDescription(payload) {
  const count = payload.readInt16BE(0);
  const out = [];
  for (let i = 0; i < count; i += 1) out.push(payload.readInt32BE(2 + i * 4));
  return out;
}

/** The trailing integer of a command tag, or null when the command has no row count ('BEGIN'). */
export function rowCountOfTag(tag) {
  const match = /(\d+)\s*$/.exec(tag);
  return match ? Number(match[1]) : null;
}
