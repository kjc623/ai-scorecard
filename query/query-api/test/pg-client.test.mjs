// pg-client.test.mjs — the wire client, in two halves that prove different things.
//
// THE UNIT HALF needs no database. It pins the parts a server only reports on indirectly: message
// framing, the SCRAM proof against RFC 7677's published vector, the type decoders, the parameter
// encoder, and the mapping from an ErrorResponse to a typed error. A scripted peer over a real TCP
// socket then exercises the socket path itself — the startup packet, MD5 and SCRAM-SHA-256
// authentication, a bound parameter crossing the wire and coming back, and an ErrorResponse becoming
// a typed error — because "it round-trips through my own encoder" is not evidence that it works on a
// socket.
//
// THE INTEGRATION HALF runs against a real PostgreSQL when one is reachable, and SKIPS with a stated
// reason when there is none, following db.test.mjs: a skip is not a pass and nothing here reports one
// as the other. It deliberately needs no schema objects — a temp table and pg_catalog are enough —
// because what it proves is the protocol, not what the database happens to contain.

import test from 'node:test';
import assert from 'node:assert/strict';
import net from 'node:net';
import { spawnSync } from 'node:child_process';
import { createHash, createHmac, pbkdf2Sync } from 'node:crypto';

import {
  CLIENT_CODE,
  PgClientError,
  PgError,
  SSL_MODES,
  createClient,
  isPgError,
  sqlStateOf,
} from '../src/pg/client.js';
import {
  MessageReader,
  SSL_REQUEST_CODE,
  bindMessage,
  cstring,
  frame,
  int16,
  int32,
  parseAuthentication,
  parseCommandComplete,
  parseDataRow,
  parseErrorOrNotice,
  parseMessage,
  parseRowDescription,
  sslRequestMessage,
  startupMessage,
} from '../src/pg/protocol.js';
import { createScramClient, parseAttributes, saslName } from '../src/pg/scram.js';
import { OID, decodeValue, encodeParam } from '../src/pg/types.js';
import { findContainer } from './helpers.mjs';

/** A value that would be a second statement if it were ever interpolated into SQL text. */
const HOSTILE = "it's; DROP TABLE ops.audit; --";

/** Await a call that must reject, and hand back the rejection. */
async function failure(promise) {
  try {
    await promise;
  } catch (error) {
    return error;
  }
  throw new Error('expected a rejection, but the call resolved');
}

/** A NUL-terminated string out of a message payload, with the offset just past it. */
function readCString(buffer, offset) {
  const end = buffer.indexOf(0, offset);
  if (end < 0) throw new Error(`no NUL terminator after offset ${offset}`);
  return { value: buffer.toString('utf8', offset, end), next: end + 1 };
}

/** x ^ y, byte for byte, for the independent SCRAM arithmetic the double does. */
function xorBytes(left, right) {
  const out = Buffer.allocUnsafe(left.length);
  for (let i = 0; i < left.length; i += 1) out[i] = left[i] ^ right[i];
  return out;
}

// ---------------------------------------------------------------------------------------------
// Framing
// ---------------------------------------------------------------------------------------------

test('a message is a type byte, a length that counts itself, and the payload', () => {
  assert.equal(frame('Q', Buffer.from('x')).toString('hex'), '510000000578');
  assert.equal(frame('X').toString('hex'), '5800000004');
  assert.equal(int32(8).toString('hex'), '00000008');
  assert.equal(int16(258).toString('hex'), '0102');
  assert.equal(cstring('ab').toString('hex'), '616200');
  // 8 bytes, and the SSLRequest code 80877103 (0x04d2162f) where the version would be.
  assert.equal(sslRequestMessage().toString('hex'), '0000000804d2162f');
  // A type that is not one byte cannot be framed, and a wrong length is a desynchronised stream.
  assert.throws(() => frame('PQ'), (error) => error instanceof PgClientError && error.code === CLIENT_CODE.PROTOCOL);
});

test('the startup packet names the identity and omits what was not configured', () => {
  const packet = startupMessage({
    user: 'query-api',
    database: 'shadow',
    application_name: undefined,
    client_encoding: 'UTF8',
    options: '-c statement_timeout=250',
  });
  assert.equal(packet.readInt32BE(0), packet.length);
  assert.equal(packet.readInt32BE(4), 3 << 16);
  const parameters = {};
  let offset = 8;
  while (packet[offset] !== 0) {
    const key = readCString(packet, offset);
    const value = readCString(packet, key.next);
    parameters[key.value] = value.value;
    offset = value.next;
  }
  assert.deepEqual(parameters, {
    user: 'query-api',
    database: 'shadow',
    client_encoding: 'UTF8',
    options: '-c statement_timeout=250',
  });
  // A NUL inside a field would end it early and shift every following byte: refused, not truncated.
  assert.throws(() => startupMessage({ user: 'a\0b' }), (error) => error.code === CLIENT_CODE.CONFIG);
});

test('the reader reassembles whole messages from any chunk boundary', () => {
  const stream = Buffer.concat([
    frame('S', Buffer.concat([cstring('server_version'), cstring('17.11')])),
    frame('D', Buffer.concat([int16(2), int32(1), Buffer.from('a'), int32(-1)])),
    frame('C', Buffer.from('SELECT 1\0')),
  ]);
  const shape = (messages) => messages.map((message) => `${message.type}:${message.payload.toString('hex')}`);

  const atOnce = new MessageReader();
  const whole = atOnce.push(stream);
  assert.equal(whole.length, 3);
  assert.deepEqual(parseDataRow(whole[1].payload), [Buffer.from('a'), null]);

  // The same bytes, one at a time: a message split across chunks must reassemble identically.
  const trickle = new MessageReader();
  const pieces = [];
  for (let i = 0; i < stream.length; i += 1) pieces.push(...trickle.push(stream.subarray(i, i + 1)));
  assert.deepEqual(shape(pieces), shape(whole));

  // A length below the header size means this is not the protocol any more; guessing is worse.
  assert.throws(
    () => new MessageReader().push(Buffer.from('5100000002', 'hex')),
    (error) => error instanceof PgClientError && error.code === CLIENT_CODE.PROTOCOL,
  );
});

test('RowDescription and DataRow decode positionally, and NULL is not an empty string', () => {
  const described = parseRowDescription(Buffer.concat([
    int16(2),
    cstring('a'), int32(0), int16(0), int32(OID.int4), int16(4), int32(-1), int16(0),
    cstring('b'), int32(0), int16(0), int32(OID.text), int16(-1), int32(-1), int16(0),
  ]));
  assert.deepEqual(
    described.map((field) => ({ name: field.name, dataTypeID: field.dataTypeID })),
    [{ name: 'a', dataTypeID: OID.int4 }, { name: 'b', dataTypeID: OID.text }],
  );

  const row = parseDataRow(Buffer.concat([int16(2), int32(4), Buffer.from('1234'), int32(-1)]));
  assert.equal(row[0].toString(), '1234');
  assert.equal(row[1], null);
  assert.equal(parseDataRow(Buffer.concat([int16(1), int32(0)]))[0].length, 0);
  assert.equal(parseCommandComplete(Buffer.from('INSERT 0 3\0')), 'INSERT 0 3');
});

test('Bind carries values as text, and the statement text never contains them', () => {
  const statement = 'SELECT $1::text AS echoed';
  const parsed = parseMessage(statement);
  const bound = bindMessage([
    encodeParam(HOSTILE),
    encodeParam(7),
    encodeParam(null),
    encodeParam(['a,b', 'q"x']),
    encodeParam(true),
  ]);

  // The Parse payload is the statement name, the statement text, and a parameter count of zero:
  // the SQL that goes on the wire is the SQL the caller wrote.
  assert.equal(parsed.subarray(5).toString('utf8'), `\0${statement}\0\0\0`);
  assert.equal(parsed.subarray(5).includes(HOSTILE), false);

  // And the Bind payload is a layout the protocol defines, read back here independently of the
  // code that wrote it: empty portal, empty statement, no format codes, then the values.
  const payload = bound.subarray(5);
  const portal = readCString(payload, 0);
  const name = readCString(payload, portal.next);
  let offset = name.next;
  const formatCount = payload.readInt16BE(offset);
  offset += 2;
  const parameterCount = payload.readInt16BE(offset);
  offset += 2;
  const parameters = [];
  for (let i = 0; i < parameterCount; i += 1) {
    const length = payload.readInt32BE(offset);
    offset += 4;
    if (length < 0) {
      parameters.push(null);
      continue;
    }
    parameters.push(payload.subarray(offset, offset + length));
    offset += length;
  }
  const resultFormatCount = payload.readInt16BE(offset);
  offset += 2;

  assert.equal(portal.value, '');
  assert.equal(name.value, '');
  assert.equal(formatCount, 0, 'no format codes means every parameter is text');
  assert.equal(resultFormatCount, 0, 'and every result column is text too');
  assert.equal(offset, payload.length, 'the message ends exactly where the layout says it does');
  assert.deepEqual(parameters, [
    Buffer.from(HOSTILE, 'utf8'),
    Buffer.from('7'),
    null,
    Buffer.from('{"a,b","q\\"x"}'),
    Buffer.from('true'),
  ]);
});

test('an ErrorResponse becomes a typed error carrying its SQLSTATE', () => {
  const payload = Buffer.from(
    'SFATAL\0VERROR\0C23505\0Mduplicate key value violates unique constraint "ops_audit_pkey"\0'
    + 'DKey (id)=(1) already exists.\0nops_audit_pkey\0tops.audit\0\0',
    'utf8',
  );
  const { fields, mapped } = parseErrorOrNotice(payload);
  const error = new PgError(mapped.code, mapped.message, fields, mapped);
  assert.equal(error.name, 'PgError');
  assert.equal(error.code, '23505');
  assert.equal(error.sqlState, '23505');
  assert.equal(error.severity, 'FATAL');
  assert.equal(error.detail, 'Key (id)=(1) already exists.');
  assert.equal(error.constraint, 'ops_audit_pkey');
  assert.equal(error.table, 'ops.audit');
  assert.match(error.message, /duplicate key/);
  assert.equal(sqlStateOf(error), '23505');
  assert.equal(isPgError(error), true);

  // A local failure is not a SQLSTATE, and that is what makes the HTTP layer's mapping a one-liner.
  const local = new PgClientError(CLIENT_CODE.CONNECTION, 'the server closed the connection');
  assert.equal(sqlStateOf(local), null);
  assert.equal(isPgError(local), false);
});

// ---------------------------------------------------------------------------------------------
// SCRAM (RFC 5802, RFC 7677)
// ---------------------------------------------------------------------------------------------

const RFC_CLIENT_FIRST = 'n,,n=user,r=rOprNGfwEbeRWgbNEkqO';
const RFC_SERVER_FIRST = 'r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096';
const RFC_CLIENT_FINAL = 'c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=';
const RFC_SERVER_FINAL = 'v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=';

test("the SCRAM client proof is RFC 7677's published vector, byte for byte", async () => {
  const scram = createScramClient({ user: 'user', password: 'pencil', nonce: 'rOprNGfwEbeRWgbNEkqO' });
  assert.equal(scram.mechanism, 'SCRAM-SHA-256');
  assert.equal(scram.clientFirstMessage(), RFC_CLIENT_FIRST);
  await scram.receiveServerFirst(RFC_SERVER_FIRST);
  assert.equal(await scram.clientFinalMessage(), RFC_CLIENT_FINAL);
  // The server's own proof is verified, not ignored: that verification is the difference between
  // authenticating the server and merely proving ourselves to whoever answered the socket.
  scram.verifyServerFinal(RFC_SERVER_FINAL);
});

test('SCRAM refuses a replayed challenge and a server that cannot prove itself', async () => {
  const replayed = createScramClient({ password: 'pencil', nonce: 'rOprNGfwEbeRWgbNEkqO' });
  replayed.clientFirstMessage();
  await assert.rejects(
    () => replayed.receiveServerFirst('r=aDIFFERENTnonce,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096'),
    (error) => error.code === CLIENT_CODE.AUTH && /does not extend/.test(error.message),
  );

  const tampered = createScramClient({ user: 'user', password: 'pencil', nonce: 'rOprNGfwEbeRWgbNEkqO' });
  tampered.clientFirstMessage();
  await tampered.receiveServerFirst(RFC_SERVER_FIRST);
  await tampered.clientFinalMessage();
  assert.throws(
    () => tampered.verifyServerFinal(`v=${Buffer.alloc(32).toString('base64')}`),
    (error) => error.code === CLIENT_CODE.AUTH && /did not prove/.test(error.message),
  );
  assert.throws(
    () => tampered.verifyServerFinal('e=invalid-proof'),
    (error) => /refused the SCRAM exchange/.test(error.message),
  );

  // A mandatory extension a client does not implement must fail the exchange, not be ignored.
  assert.throws(() => parseAttributes('m=ext,r=abc'), (error) => error.code === CLIENT_CODE.AUTH);
  assert.equal(saslName('a,b=c'), 'a=2Cb=3Dc');
});

// ---------------------------------------------------------------------------------------------
// Types and parameters
// ---------------------------------------------------------------------------------------------

test('every decoded type is a JS value, and nothing is silently narrowed', () => {
  const cases = [
    ['text', OID.text, 'hello', 'hello'],
    ['varchar', OID.varchar, 'hello', 'hello'],
    ['char', OID.char, 'x', 'x'],
    ['int2', OID.int2, '-3', -3],
    ['int4', OID.int4, '2147483647', 2147483647],
    ['int8 within a double', OID.int8, '9007199254740991', 9007199254740991],
    ['int8 beyond a double', OID.int8, '9007199254740993', '9007199254740993'],
    ['float4', OID.float4, '1.5', 1.5],
    ['float8 Infinity', OID.float8, 'Infinity', Infinity],
    ['float8 NaN', OID.float8, 'NaN', NaN],
    ['numeric exactly representable', OID.numeric, '0.1', 0.1],
    ['numeric with trailing zeros', OID.numeric, '1.10', 1.1],
    ['numeric beyond a double', OID.numeric, '123456789012345678901234567890.5', '123456789012345678901234567890.5'],
    ['bool true', OID.bool, 't', true],
    ['bool false', OID.bool, 'f', false],
    ['uuid', OID.uuid, '11111111-2222-4333-8444-555555555555', '11111111-2222-4333-8444-555555555555'],
    ['json', OID.json, '{"a":[1,2]}', { a: [1, 2] }],
    ['jsonb', OID.jsonb, '{"a":[1,2]}', { a: [1, 2] }],
    ['bytea, hex form', OID.bytea, '\\x0a0b', Buffer.from([0x0a, 0x0b])],
    ['bytea, escape form', OID.bytea, '\\012\\013', Buffer.from([0x0a, 0x0b])],
    ['NULL', OID.text, null, null],
    // A type this file has never heard of is the text the server sent, never a guess.
    ['money', 790, '$1,234.56', '$1,234.56'],
    ['an array type', 1009, '{a,b}', '{a,b}'],
  ];
  for (const [label, oid, text, expected] of cases) {
    const actual = decodeValue(oid, text === null ? null : Buffer.from(text, 'utf8'));
    assert.deepEqual(actual, expected, `${label} decoded to the wrong value`);
  }
  assert.throws(
    () => decodeValue(OID.bool, Buffer.from('maybe')),
    (error) => error.code === CLIENT_CODE.DECODE,
  );
});

test('timestamps are instants, and a zone-less timestamp is read as UTC', () => {
  const iso = (oid, text) => decodeValue(oid, Buffer.from(text)).toISOString();
  assert.equal(iso(OID.timestamptz, '2026-10-02 12:34:56.789+00'), '2026-10-02T12:34:56.789Z');
  assert.equal(iso(OID.timestamptz, '2026-10-02 12:34:56.789+02'), '2026-10-02T10:34:56.789Z');
  assert.equal(iso(OID.timestamptz, '2026-10-02 12:34:56.789-05:30'), '2026-10-02T18:04:56.789Z');
  assert.equal(iso(OID.timestamp, '2026-10-02 12:00:00'), '2026-10-02T12:00:00.000Z');
  assert.equal(iso(OID.timestamp, '2026-10-02T12:00:00.5'), '2026-10-02T12:00:00.500Z');
  assert.equal(iso(OID.date, '2026-10-02'), '2026-10-02T00:00:00.000Z');
  // Microseconds past the first three digits are dropped, not rounded into the next millisecond.
  assert.equal(iso(OID.timestamptz, '2026-10-02 12:00:00.000999+00'), '2026-10-02T12:00:00.000Z');
  // The values that are not instants keep the text they arrived as rather than becoming a Date
  // that says something the database did not.
  assert.equal(decodeValue(OID.timestamptz, Buffer.from('infinity')), 'infinity');
  assert.equal(decodeValue(OID.date, Buffer.from('-infinity')), '-infinity');
  assert.equal(decodeValue(OID.timestamptz, null), null);
});

test('every parameter has one documented text form, and the rest are refused', () => {
  const text = (value) => encodeParam(value).toString('utf8');
  assert.equal(text('x'), 'x');
  assert.equal(text(7), '7');
  assert.equal(text(-1.5), '-1.5');
  assert.equal(text(true), 'true');
  assert.equal(text(10n), '10');
  assert.equal(text(new Date('2026-10-02T00:00:00Z')), '2026-10-02T00:00:00.000Z');
  assert.equal(text(Buffer.from([0x0a, 0x0b])), '\\x0a0b');
  assert.equal(encodeParam(null), null);
  assert.equal(encodeParam(undefined), null);
  // An array literal for the `= ANY($1::text[])` shape compile.js emits: quoted elements, an
  // unquoted NULL, and a comma or a quote inside an element that must not become structure.
  assert.equal(text(['a,b', 'q"x', 'back\\slash', null, 1, true]), '{"a,b","q\\"x","back\\\\slash",NULL,"1","true"}');
  assert.equal(text([]), '{}');
  // A value with no faithful text form is refused, not stringified into '[object Object]'.
  assert.throws(() => encodeParam({ a: 1 }), (error) => error.code === CLIENT_CODE.PARAM);
  assert.throws(() => encodeParam([[1, 2]]), (error) => error.code === CLIENT_CODE.PARAM);
  assert.throws(() => encodeParam(() => {}), (error) => error.code === CLIENT_CODE.PARAM);
  assert.throws(() => encodeParam(new Date('nonsense')), (error) => error.code === CLIENT_CODE.PARAM);
});

// ---------------------------------------------------------------------------------------------
// A protocol double: a peer that speaks just enough of v3 to be the other end of the socket
// ---------------------------------------------------------------------------------------------

const EMPTY = Buffer.alloc(0);

const authentication = (code, extra = EMPTY) => Buffer.concat([int32(code), extra]);
const saslMechanisms = (names) => Buffer.concat([...names.map((name) => cstring(name)), Buffer.from([0])]);
const errorResponse = (fields) => Buffer.concat([
  ...Object.entries(fields).map(([letter, value]) => Buffer.concat([Buffer.from(letter, 'ascii'), cstring(value)])),
  Buffer.from([0]),
]);
const rowDescription = (columns) => Buffer.concat([
  int16(columns.length),
  ...columns.map(([name, oid]) => Buffer.concat([cstring(name), int32(0), int16(0), int32(oid), int16(-1), int32(-1), int16(0)])),
]);
const dataRow = (values) => Buffer.concat([
  int16(values.length),
  ...values.map((value) => (value === null ? int32(-1) : Buffer.concat([int32(value.length), value]))),
]);

/** Reads whole messages out of a socket, including the untyped StartupMessage that comes first. */
class Peer {
  #socket;

  #buffer = Buffer.alloc(0);

  #wake = null;

  #ended = false;

  constructor(socket) {
    this.#socket = socket;
    socket.on('data', (chunk) => {
      this.#buffer = Buffer.concat([this.#buffer, chunk]);
      this.#wake?.();
    });
    socket.on('close', () => {
      this.#ended = true;
      this.#wake?.();
    });
    socket.on('error', () => {
      this.#ended = true;
      this.#wake?.();
    });
  }

  async #need(bytes) {
    while (this.#buffer.length < bytes) {
      if (this.#ended) throw new Error('the client closed the connection while the protocol double was waiting');
      await new Promise((resolve) => {
        this.#wake = resolve;
      });
      this.#wake = null;
    }
  }

  /** The next untyped packet: an SSLRequest or the StartupMessage, both length-prefixed. */
  async packet() {
    await this.#need(4);
    const length = this.#buffer.readInt32BE(0);
    await this.#need(length);
    const body = this.#buffer.subarray(4, length);
    this.#buffer = this.#buffer.subarray(length);
    return { length, body };
  }

  /** The startup packet's parameters, with the version field checked. */
  async startup() {
    const { body } = await this.packet();
    assert.equal(body.readInt32BE(0), 3 << 16, 'the startup packet must state protocol 3.0');
    const parameters = {};
    let offset = 4;
    while (body[offset] !== 0) {
      const key = readCString(body, offset);
      const value = readCString(body, key.next);
      parameters[key.value] = value.value;
      offset = value.next;
    }
    return parameters;
  }

  writeRaw(bytes) {
    this.#socket.write(bytes);
  }

  async next() {
    await this.#need(5);
    const length = this.#buffer.readInt32BE(1);
    await this.#need(1 + length);
    const message = {
      type: String.fromCharCode(this.#buffer[0]),
      payload: this.#buffer.subarray(5, 1 + length),
    };
    this.#buffer = this.#buffer.subarray(1 + length);
    return message;
  }

  write(type, payload = EMPTY) {
    this.#socket.write(frame(type, payload));
  }

  /** Read one Parse/Bind/Describe/Execute/Sync batch the way a server sees it. */
  async statement() {
    const parse = await this.next();
    const bind = await this.next();
    assert.equal(parse.type, 'P');
    assert.equal(bind.type, 'B');
    assert.equal((await this.next()).type, 'D');
    assert.equal((await this.next()).type, 'E');
    assert.equal((await this.next()).type, 'S');

    const name = readCString(parse.payload, 0);
    const text = readCString(parse.payload, name.next);

    const portal = readCString(bind.payload, 0);
    const statement = readCString(bind.payload, portal.next);
    let offset = statement.next;
    const formatCount = bind.payload.readInt16BE(offset);
    offset += 2;
    const parameterCount = bind.payload.readInt16BE(offset);
    offset += 2;
    const parameters = [];
    for (let i = 0; i < parameterCount; i += 1) {
      const length = bind.payload.readInt32BE(offset);
      offset += 4;
      if (length < 0) {
        parameters.push(null);
        continue;
      }
      parameters.push(bind.payload.subarray(offset, offset + length));
      offset += length;
    }
    return {
      name: name.value,
      text: text.value,
      declaredTypes: parse.payload.readInt16BE(text.next),
      portal: portal.value,
      statement: statement.value,
      formatCount,
      parameters,
      resultFormatCount: bind.payload.readInt16BE(offset),
    };
  }
}

/** Run a scripted peer and a client against each other, and fail the test on either side's error. */
async function withWireDouble(handler, body) {
  const server = net.createServer();
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  const { port } = server.address();
  let scriptError = null;
  let settle;
  const scriptDone = new Promise((resolve) => {
    settle = resolve;
  });
  server.on('connection', (socket) => {
    // The client may destroy the socket mid-exchange (that is one of the things being tested).
    socket.on('error', () => {});
    Promise.resolve()
      .then(() => handler(new Peer(socket)))
      .catch((error) => {
        scriptError = error;
      })
      .finally(() => settle());
  });
  try {
    const result = await body(port);
    await scriptDone;
    if (scriptError !== null) throw scriptError;
    return result;
  } finally {
    server.closeAllConnections?.();
    await new Promise((resolve) => server.close(resolve));
  }
}

const MD5_VECTOR = 'md5cc27aa8b4861d25d75a155afb2bd53c6';

test('over a socket: the handshake, MD5 authentication, a bound parameter and an ErrorResponse', async () => {
  let observed = null;
  await withWireDouble(async (peer) => {
    const startup = await peer.startup();
    assert.equal(startup.user, 'postgres');
    assert.equal(startup.database, 'shadow');
    assert.equal(startup.application_name, 'pg-client-test');
    assert.equal(startup.DateStyle, 'ISO, MDY');

    peer.write('R', authentication(5, Buffer.from([1, 2, 3, 4])));
    const password = await peer.next();
    assert.equal(password.type, 'p');
    assert.equal(readCString(password.payload, 0).value, MD5_VECTOR);

    peer.write('R', authentication(0));
    peer.write('S', Buffer.concat([cstring('server_version'), cstring('17.11')]));
    peer.write('K', Buffer.concat([int32(4242), int32(99)]));
    peer.write('Z', Buffer.from('I'));

    observed = await peer.statement();
    peer.write('1');
    peer.write('2');
    peer.write('T', rowDescription([['echoed', OID.text]]));
    peer.write('D', dataRow([Buffer.from(`bound:${observed.parameters[0].toString('utf8')}`)]));
    peer.write('C', Buffer.from('SELECT 1\0'));
    peer.write('Z', Buffer.from('I'));

    const failing = await peer.statement();
    peer.write('E', errorResponse({ S: 'ERROR', C: '42P01', M: `relation "${failing.text}" does not exist` }));
    peer.write('Z', Buffer.from('I'));
  }, async (port) => {
    const client = createClient({
      host: '127.0.0.1',
      port,
      user: 'postgres',
      database: 'shadow',
      password: 'sac-lab-only',
      sslMode: 'disable',
      applicationName: 'pg-client-test',
      connectTimeoutMs: 5_000,
    });
    await client.connect();
    assert.equal(client.ready, true);
    assert.equal(client.transactionStatus, 'idle');
    assert.equal(client.parameters.server_version, '17.11');
    assert.deepEqual(client.backend, { pid: 4242, secretKey: 99 });

    const result = await client.query('SELECT $1::text AS echoed', [HOSTILE]);
    assert.equal(result.rowCount, 1);
    assert.equal(result.command, 'SELECT 1');
    assert.deepEqual(result.fields, [{ name: 'echoed', dataTypeID: OID.text }]);
    assert.equal(result.rows[0].echoed, `bound:${HOSTILE}`);

    const error = await failure(client.query('SELECT * FROM missing'));
    assert.equal(error.name, 'PgError');
    assert.equal(error.code, '42P01');
    // A statement the server refused does not kill the connection.
    assert.equal(client.ready, true);
    await client.close();
  });

  assert.equal(observed.text, 'SELECT $1::text AS echoed', 'the SQL text is exactly what the caller passed');
  assert.equal(observed.declaredTypes, 0, 'types are inferred from the caller\'s casts, not declared by this client');
  assert.deepEqual(observed.parameters, [Buffer.from(HOSTILE, 'utf8')]);
  assert.equal(observed.formatCount, 0, 'parameters travel as text');
  assert.equal(observed.resultFormatCount, 0, 'and results come back as text');
});

const SCRAM_SALT = Buffer.from('W22ZaJ0SNY7soEsUEjb6gQ==', 'base64');

/** The server half of a SCRAM exchange, with the expected proof computed here rather than reused. */
async function scramPeer(peer, { tamper }) {
  const startup = await peer.startup();
  assert.equal(startup.user, 'postgres');
  peer.write('R', authentication(10, saslMechanisms(['SCRAM-SHA-256-PLUS', 'SCRAM-SHA-256'])));

  const initial = await peer.next();
  const mechanism = readCString(initial.payload, 0);
  const declaredLength = initial.payload.readInt32BE(mechanism.next);
  const clientFirst = initial.payload.toString('utf8', mechanism.next + 4, mechanism.next + 4 + declaredLength);
  // Channel binding is not implemented, so -PLUS is not chosen even when the server offers it first.
  assert.equal(mechanism.value, 'SCRAM-SHA-256');
  // The SASL name is empty exactly as libpq sends it: PostgreSQL takes the identity from the
  // startup packet, and an empty field cannot need escaping.
  assert.match(clientFirst, /^n,,n=,r=.+$/);
  const nonce = clientFirst.slice('n,,n=,r='.length);

  const serverFirst = `r=${nonce}SERVERNONCE,s=${SCRAM_SALT.toString('base64')},i=4096`;
  peer.write('R', authentication(11, Buffer.from(serverFirst, 'utf8')));

  const finalMessage = await peer.next();
  assert.equal(finalMessage.type, 'p');
  const withoutProof = `c=biws,r=${nonce}SERVERNONCE`;
  const authMessage = `${clientFirst.slice(3)},${serverFirst},${withoutProof}`;
  const salted = pbkdf2Sync('sac-lab-only', SCRAM_SALT, 4096, 32, 'sha256');
  const clientKey = createHmac('sha256', salted).update('Client Key').digest();
  const storedKey = createHash('sha256').update(clientKey).digest();
  const clientSignature = createHmac('sha256', storedKey).update(authMessage).digest();
  const proof = xorBytes(clientKey, clientSignature).toString('base64');
  assert.equal(finalMessage.payload.toString('utf8'), `${withoutProof},p=${proof}`);

  const serverKey = createHmac('sha256', salted).update('Server Key').digest();
  const signature = createHmac('sha256', serverKey).update(authMessage).digest();
  peer.write('R', authentication(12, Buffer.from(`v=${(tamper ? Buffer.alloc(32) : signature).toString('base64')}`, 'utf8')));
  peer.write('R', authentication(0));
  peer.write('Z', Buffer.from('I'));

  if (tamper) return;
  const statement = await peer.statement();
  assert.equal(statement.text, 'SELECT 1');
  peer.write('1');
  peer.write('2');
  peer.write('T', rowDescription([['one', OID.int4]]));
  peer.write('D', dataRow([Buffer.from('1')]));
  peer.write('C', Buffer.from('SELECT 1\0'));
  peer.write('Z', Buffer.from('I'));
}

function scramTarget(port) {
  return {
    host: '127.0.0.1',
    port,
    user: 'postgres',
    database: 'shadow',
    password: 'sac-lab-only',
    sslMode: 'disable',
    connectTimeoutMs: 5_000,
  };
}

test('over a socket: SCRAM-SHA-256 authenticates and the session is usable afterwards', async () => {
  await withWireDouble((peer) => scramPeer(peer, { tamper: false }), async (port) => {
    const client = createClient(scramTarget(port));
    await client.connect();
    assert.equal(client.ready, true);
    const result = await client.query('SELECT 1');
    assert.deepEqual(result.rows, [{ one: 1 }]);
    await client.close();
  });
});

test('over a socket: a server that cannot prove it knows the password is refused', async () => {
  await withWireDouble((peer) => scramPeer(peer, { tamper: true }), async (port) => {
    const client = createClient(scramTarget(port));
    const error = await failure(client.connect());
    assert.equal(error.code, CLIENT_CODE.AUTH);
    assert.match(error.message, /did not prove/);
    assert.equal(client.ready, false);
  });
});

test('over a socket: sslMode prefer asks for TLS and continues only when the server declines', async () => {
  await withWireDouble(async (peer) => {
    const probe = await peer.packet();
    assert.equal(probe.length, 8, 'the SSLRequest is exactly eight bytes, with no type byte');
    assert.equal(probe.body.readInt32BE(0), SSL_REQUEST_CODE);
    // 'N': this server has no TLS listener, which is the one case 'prefer' falls back in.
    peer.writeRaw(Buffer.from('N', 'ascii'));
    const startup = await peer.startup();
    assert.equal(startup.user, 'postgres');

    peer.write('R', authentication(0));
    peer.write('Z', Buffer.from('I'));
    const statement = await peer.statement();
    assert.equal(statement.text, 'SELECT 1');
    peer.write('1');
    peer.write('2');
    peer.write('T', rowDescription([['one', OID.int4]]));
    peer.write('D', dataRow([Buffer.from('1')]));
    peer.write('C', Buffer.from('SELECT 1\0'));
    peer.write('Z', Buffer.from('I'));
  }, async (port) => {
    const client = createClient({ ...scramTarget(port), sslMode: 'prefer' });
    await client.connect();
    assert.equal(client.ready, true);
    // The connection recorded which of the two it turned out to be, so a caller can assert on it.
    assert.equal(client.encrypted, false);
    assert.equal(client.sslMode, 'prefer');
    const result = await client.query('SELECT 1');
    assert.deepEqual(result.rows, [{ one: 1 }]);
    await client.close();
  });
});

// ---------------------------------------------------------------------------------------------
// Client behaviour with no server to talk to
// ---------------------------------------------------------------------------------------------

test('createClient returns a client; connect() is where every failure is reported', async () => {
  const client = createClient({ host: '127.0.0.1', port: 1, user: 'postgres' });
  assert.equal(typeof client.connect, 'function');
  assert.equal(typeof client.query, 'function');
  assert.equal(typeof client.begin, 'function');
  assert.equal(typeof client.commit, 'function');
  assert.equal(typeof client.rollback, 'function');
  assert.equal(typeof client.close, 'function');
  assert.equal(client.ready, false);
  // The transport was not chosen, so connect() refuses rather than guessing — and it refuses
  // before it opens anything, which is why this needs no server to fail correctly.
  const error = await failure(client.connect());
  assert.equal(error.code, CLIENT_CODE.CONFIG);
  assert.match(error.message, /ssl/);
  assert.equal(client.ready, false);
  assert.equal(client.failure, error);
});

test('a contradictory or weakened transport is refused before a byte is written', async () => {
  const refused = [
    { ssl: true, sslMode: 'disable' },
    { ssl: false, sslMode: 'verify-full' },
    { ssl: true, sslMode: 'require' },
    { sslMode: 'require' },
    { sslMode: 'on' },
    { ssl: 'yes' },
    { ssl: false, tls: { rejectUnauthorized: false } },
    { ssl: true, tls: { rejectUnauthorized: false } },
    { user: '' },
    { port: 70_000 },
    { port: 1.5 },
    { statementTimeoutMs: -1 },
    { connectTimeoutMs: 0 },
  ];
  for (const options of refused) {
    const client = createClient({ host: '127.0.0.1', user: 'postgres', ...options });
    const error = await failure(client.connect());
    assert.equal(error.code, CLIENT_CODE.CONFIG, `expected ${JSON.stringify(options)} to be refused, got ${error.message}`);
  }
  assert.deepEqual(SSL_MODES, ['disable', 'prefer', 'verify-full']);
});

test('a client that was never connected, or is already closed, says exactly that', async () => {
  const never = createClient({ host: '127.0.0.1', user: 'postgres', sslMode: 'disable' });
  const error = await failure(never.query('SELECT 1'));
  assert.equal(error.code, CLIENT_CODE.NOT_CONNECTED);
  assert.match(error.message, /not connected yet/);
  await never.close();
  await never.close();
  assert.equal(never.ready, false);
  const closed = await failure(never.connect());
  assert.equal(closed.code, CLIENT_CODE.CLOSED);
});

test('a refused connection is a typed client error that names the address, not a hang', async () => {
  const client = createClient({
    host: '127.0.0.1',
    port: 1,
    user: 'postgres',
    sslMode: 'disable',
    connectTimeoutMs: 3_000,
  });
  const error = await failure(client.connect());
  assert.ok(error instanceof PgClientError, `expected a PgClientError, got ${error?.name}: ${error?.message}`);
  assert.ok(
    [CLIENT_CODE.CONNECTION, CLIENT_CODE.CONNECT_TIMEOUT].includes(error.code),
    `unexpected code ${error.code} (${error.message})`,
  );
  assert.match(error.message, /127\.0\.0\.1:1/);
  assert.equal(client.ready, false);
  const after = await failure(client.query('SELECT 1'));
  assert.equal(after.code, CLIENT_CODE.NOT_CONNECTED);
  await client.close();
});

// ---------------------------------------------------------------------------------------------
// A real PostgreSQL, when one is reachable
// ---------------------------------------------------------------------------------------------

const container = findContainer();

/**
 * Where a real PostgreSQL is, or why there is none.
 *
 * findContainer() is this package's own discovery (test/helpers.mjs) and is reused rather than
 * reimplemented, so the suite agrees with itself about what "a database is running" means. That
 * discovery names a container while this client needs a socket, so the published port comes from
 * docker rather than from an assumption; a container that publishes nothing is a stated skip, not a
 * connection error that looks like a client bug.
 */
function integrationTarget() {
  if (container === null) {
    return {
      skip: 'no PostgreSQL container is running (findContainer tried shadowpg, shadowpg-invariants, *shadow*): this test SKIPS and is not a pass',
    };
  }
  const published = spawnSync('docker', ['port', container, '5432/tcp'], { encoding: 'utf8' });
  if (published.error || published.status !== 0 || !(published.stdout ?? '').trim()) {
    return {
      skip: `container ${container} publishes no host port for 5432, so a TCP client cannot reach it (the psql tests use docker exec instead): this test SKIPS and is not a pass`,
    };
  }
  const mapping = published.stdout.trim().split('\n')[0].trim();
  const port = Number(mapping.slice(mapping.lastIndexOf(':') + 1));
  if (!Number.isInteger(port) || port < 1) {
    return { skip: `docker reported ${JSON.stringify(mapping)} for ${container}, which is not a readable port` };
  }
  return {
    skip: false,
    host: '127.0.0.1',
    port,
    // The lab's throwaway credential, from localdev/docker-compose.yml (POSTGRES_PASSWORD:
    // sac-lab-only, POSTGRES_DB: shadow). It is not a secret and not a deployed one: a deployed
    // PostgreSQL here authenticates a managed identity and is passed no password at all. The
    // SAC_* names are the deployment's vocabulary (ingestion/ingest-api/cmd/ingest-api/config.go),
    // read here only so a different lab can be pointed at.
    user: process.env.SAC_ROLE ?? 'postgres',
    database: process.env.SAC_PG_DATABASE ?? 'shadow',
    password: process.env.PGPASSWORD ?? 'sac-lab-only',
  };
}

const integration = integrationTarget();

/** One client, connected and always closed, for a body that asserts on it. */
async function withClient(body, extra = {}) {
  const client = createClient({
    host: integration.host,
    port: integration.port,
    user: integration.user,
    database: integration.database,
    password: integration.password,
    // The lab container has no TLS listener, and saying so is this client's requirement.
    sslMode: 'disable',
    applicationName: 'query-api-pg-client-test',
    ...extra,
  });
  await client.connect();
  try {
    return await body(client);
  } finally {
    await client.close();
  }
}

test('a real server: connect, SELECT 1, and the session the server reports', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    assert.equal(client.ready, true);
    assert.equal(client.encrypted, false);
    assert.equal(client.sslMode, 'disable');
    assert.equal(client.transactionStatus, 'idle');
    assert.match(client.parameters.server_version, /^\d+\./);

    const result = await client.query('SELECT 1 AS one, $1::text AS echo', ['hello']);
    assert.equal(result.rowCount, 1);
    assert.equal(result.command, 'SELECT 1');
    assert.deepEqual(result.fields, [{ name: 'one', dataTypeID: OID.int4 }, { name: 'echo', dataTypeID: OID.text }]);
    assert.deepEqual(result.rows, [{ one: 1, echo: 'hello' }]);

    // The startup packet's application_name is what pg_stat_activity shows, which is how an
    // operator tells one connection from another.
    const shown = await client.query('SHOW application_name');
    assert.equal(shown.rows[0].application_name, 'query-api-pg-client-test');
  });
});

test('a real server: a quote and a semicolon travel as a value, not as SQL', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    const result = await client.query(
      "SELECT $1::text AS echoed, length($1::text) AS length, 'intact'::text AS marker",
      [HOSTILE],
    );
    assert.equal(result.rows[0].echoed, HOSTILE);
    assert.equal(result.rows[0].length, HOSTILE.length);
    assert.equal(result.rows[0].marker, 'intact');
    // A driver that had interpolated this value would have produced a syntax error at the
    // apostrophe; the exact wire text of the Parse message is asserted separately, against the
    // protocol double, because that is the only place it can be observed.
  });
});

test('a real server: uuid, int8, numeric, bytea, json, an array and NULL keep their values', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    const result = await client.query(
      `SELECT $1::uuid AS id,
              pg_typeof($1::uuid)::text AS id_type,
              42::int8 AS small,
              9007199254740993::int8 AS big,
              0.1::numeric AS tenth,
              123456789012345678901234567890.5::numeric AS wide,
              '{"a":[1,2]}'::jsonb AS doc,
              '\\x0a0b'::bytea AS blob,
              $2::text[] = ARRAY[$3::text, $4::text] AS array_same,
              array_length($2::text[], 1) AS array_length,
              NULL::text AS nothing`,
      ['11111111-2222-4333-8444-555555555555', ['a,b', 'q"x'], 'a,b', 'q"x'],
    );
    const row = result.rows[0];
    assert.equal(row.id, '11111111-2222-4333-8444-555555555555');
    assert.equal(row.id_type, 'uuid');
    assert.equal(row.small, 42);
    // Beyond a double the value is the text the server sent: a number here would be a different
    // number than the one in the database.
    assert.equal(row.big, '9007199254740993');
    assert.equal(row.tenth, 0.1);
    assert.equal(row.wide, '123456789012345678901234567890.5');
    assert.deepEqual(row.doc, { a: [1, 2] });
    assert.ok(Buffer.isBuffer(row.blob), 'bytea decodes to a Buffer');
    assert.equal(row.blob.toString('hex'), '0a0b');
    // The array literal this client sent was parsed into exactly the elements intended, comma and
    // quote included: this is the `= ANY($1::text[])` shape compile.js emits for `in` filters.
    assert.equal(row.array_same, true);
    assert.equal(row.array_length, 2);
    assert.equal(row.nothing, null);
  });
});

test('a real server: timestamps and dates decode to instants', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    const result = await client.query(
      `SELECT $1::timestamptz AS at,
              '2026-10-02 12:34:56.789+02'::timestamptz AS offset_at,
              '2026-10-02 12:00:00'::timestamp AS naive,
              '2026-10-02'::date AS day`,
      ['2026-10-02T12:00:00.000Z'],
    );
    const row = result.rows[0];
    assert.ok(row.at instanceof Date);
    assert.equal(row.at.toISOString(), '2026-10-02T12:00:00.000Z');
    assert.equal(row.offset_at.toISOString(), '2026-10-02T10:34:56.789Z');
    assert.equal(row.naive.toISOString(), '2026-10-02T12:00:00.000Z');
    assert.equal(row.day.toISOString(), '2026-10-02T00:00:00.000Z');
  });
});

test('a real server: BEGIN/INSERT/ROLLBACK, and a duplicate key that is a 23505', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    await client.begin();
    assert.equal(client.transactionStatus, 'transaction');
    await client.query('CREATE TEMP TABLE pg_client_probe (id int PRIMARY KEY, note text)');
    const inserted = await client.query(
      'INSERT INTO pg_client_probe (id, note) VALUES ($1, $2), ($3, $4)',
      [1, 'a', 2, "b']"],
    );
    assert.equal(inserted.rowCount, 2, 'the command tag carries the row count');
    const inside = await client.query('SELECT count(*)::int AS n FROM pg_client_probe');
    assert.equal(inside.rows[0].n, 2, 'the rows are visible inside the transaction');
    await client.rollback();
    assert.equal(client.transactionStatus, 'idle');

    // Rolled back means gone: the temp table the transaction created does not exist.
    const gone = await client.query('SELECT (to_regclass(\'pg_temp.pg_client_probe\') IS NULL) AS gone');
    assert.equal(gone.rows[0].gone, true);

    await client.begin();
    await client.query('CREATE TEMP TABLE pg_client_dup (id int PRIMARY KEY)');
    await client.query('INSERT INTO pg_client_dup (id) VALUES ($1)', [1]);
    const duplicate = await failure(client.query('INSERT INTO pg_client_dup (id) VALUES ($1)', [1]));
    assert.equal(duplicate.name, 'PgError');
    assert.equal(duplicate.code, '23505');
    assert.equal(sqlStateOf(duplicate), '23505');
    assert.equal(duplicate.constraint, 'pg_client_dup_pkey');
    // The server is the authority on the transaction's state, and it says this one has failed.
    assert.equal(client.transactionStatus, 'failed');
    const refused = await failure(client.query('SELECT 1'));
    assert.equal(refused.code, '25P02');
    await client.rollback();
    assert.equal(client.transactionStatus, 'idle');
    assert.equal(client.ready, true);
  });
});

test('a real server: a malformed statement surfaces its SQLSTATE and leaves the client usable', { skip: integration.skip }, async () => {
  await withClient(async (client) => {
    const missing = await failure(client.query('SELECT * FROM a_relation_that_does_not_exist'));
    assert.equal(missing.name, 'PgError');
    assert.equal(missing.code, '42P01');
    assert.match(missing.message, /a_relation_that_does_not_exist/);
    assert.equal(client.ready, true);

    const syntax = await failure(client.query('SELECT FROM WHERE'));
    assert.equal(syntax.code, '42601');

    const after = await client.query('SELECT 42 AS answer');
    assert.equal(after.rows[0].answer, 42);
  });
});

test('a real server: statement_timeout is in force from the first statement', { skip: integration.skip }, async () => {  await withClient(async (client) => {
    // Set through the startup packet's options: in force before the first statement, no round trip,
    // and no interpolated GUC (utility statements cannot take a bind parameter).
    const shown = await client.query('SHOW statement_timeout');
    assert.equal(shown.rows[0].statement_timeout, '250ms');
    const cancelled = await failure(client.query('SELECT pg_sleep(5)'));
    assert.equal(cancelled.name, 'PgError');
    assert.equal(cancelled.code, '57014');
    assert.equal(client.ready, true);
  }, { statementTimeoutMs: 250 });
});

test('a real server: ssl: true refuses a server that offers no TLS', { skip: integration.skip }, async () => {
  // The lab container has no TLS listener. A client asked for a verified TLS session must refuse
  // it rather than quietly continuing in the clear, which is the whole point of the mode.
  const client = createClient({
    host: integration.host,
    port: integration.port,
    user: integration.user,
    database: integration.database,
    password: integration.password,
    ssl: true,
    applicationName: 'query-api-pg-client-test',
  });
  const error = await failure(client.connect());
  assert.equal(error.code, CLIENT_CODE.TLS);
  assert.match(error.message, /does not offer TLS/);
  assert.equal(client.encrypted, false);
  assert.equal(client.ready, false);
  await client.close();
});
