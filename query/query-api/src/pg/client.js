// client.js — the frozen seam. `createClient({...})` returns a connection that runs statements.
//
// This is the only database driver in this repository that is not `psql` or a build-tagged Go
// driver, because there is deliberately no external dependency anywhere in the tree and the host is
// offline. The interface is fixed by what plan.js calls (src/plan.js:332): `query(text, params)`,
// `begin()`, `commit()`, `rollback()`, and nothing else is required of it.
//
//   const client = createClient({ host, port, database, user, password, ssl, statementTimeoutMs,
//                                 applicationName });
//   await client.connect();                       // rejects: the database never says maybe
//   const { rows, rowCount, fields } = await client.query('SELECT x FROM t WHERE y = $1', [y]);
//   await client.begin(); ... await client.commit();   // or rollback()
//   await client.close();
//   client.ready                                  // false from the first byte of a query, so a
//                                                 // caller can tell "connected" from "usable now"
//
// THE DECISIONS A CALLER CAN OBSERVE, each of them argued where it is taken:
//
//   * One connection, one statement at a time. A second concurrent `query()` rejects with
//     SAC_BUSY rather than being queued: a queue would hide a "one client per request" bug behind
//     mysterious response ordering, and `ready` already answers the question a queue would paper
//     over. A statement issued while the handshake is still running waits for it, because that is
//     the same connection and the ordering is unambiguous.
//   * The transport is the caller's decision, and it is required. No option means no connection:
//     an unencrypted session has to be asked for by name (`sslMode: 'disable'`) and a TLS session
//     is verified (`ssl: true` → verify-full). There is no `rejectUnauthorized: false` path in
//     this file at all — not even for a flag — because a mode that encrypts without authenticating
//     the server is the one failure that cannot be noticed later.
//   * `close()` is terminal and never rejects. A client that could be reopened would need a second
//     answer to "which connection is this?", and a teardown that can fail is a teardown nobody
//     calls. A pool creates clients; it does not resurrect them.
//   * A statement timeout is set through the startup packet's `options`, not by running `SET`
//     afterwards: it is then in force before the first statement, it costs no round trip, and it
//     avoids the one place a GUC cannot take a bind parameter (utility statements do not accept
//     parameters, so `SET statement_timeout = $1` is a syntax error — the value would have to be
//     interpolated, and this client does not interpolate).

import net from 'node:net';
import tls from 'node:tls';
import { createHash } from 'node:crypto';

import { CLIENT_CODE, PgClientError, PgError } from './error.js';
import {
  MessageReader,
  bindMessage,
  describeMessage,
  executeMessage,
  md5Password,
  parseAuthentication,
  parseBackendKeyData,
  parseCommandComplete,
  parseDataRow,
  parseErrorOrNotice,
  parseNotificationResponse,
  parseParameterStatus,
  parseReadyForQuery,
  parseRowDescription,
  passwordMessage,
  saslInitialResponse,
  saslResponse,
  parseMessage,
  rowCountOfTag,
  sslRequestMessage,
  startupMessage,
  syncMessage,
  terminateMessage,
} from './protocol.js';
import { createScramClient, randomNonce } from './scram.js';
import { decodeValue, encodeParam } from './types.js';

export { CLIENT_CODE, PgClientError, PgError, isPgError, sqlStateOf } from './error.js';

const DEFAULT_PORT = 5432;
const DEFAULT_CONNECT_TIMEOUT_MS = 10_000;
const CLOSE_GRACE_MS = 1_000;
const KEEPALIVE_MS = 30_000;
const NOTICE_LIMIT = 20;

/**
 * The transport modes this client offers.
 *
 * 'require' is deliberately absent. It is the mode that encrypts without checking the certificate —
 * the mode that turns TLS into obfuscation against exactly the attacker TLS exists for. A caller
 * whose server has a private CA passes it as `tls: { ca }` (or `ssl: { ca }`) instead, and gets
 * both encryption and authentication.
 */
export const SSL_MODES = Object.freeze(['disable', 'prefer', 'verify-full']);

/** How each mode answers the SSLRequest. Kept as data so the mode list cannot drift from the behaviour. */
const TLS_MODES = new Set(['prefer', 'verify-full']);

/** Node's TLS failure codes, so a certificate problem is reported as one instead of as a dead socket. */
const TLS_FAILURE = /^(ERR_TLS|ERR_SSL|DEPTH_|CERT_|UNABLE_TO_|SELF_SIGNED|HOSTNAME_MISMATCH|ERR_OSSL)/;

/**
 * A connection to one PostgreSQL database.
 *
 * Everything mutable is private: the only ways to change this object's state are connect(), a
 * statement, and close(). That is what makes `ready` trustworthy.
 */
class PgClient {
  #options;

  #host = 'localhost';

  #port = DEFAULT_PORT;

  #user = null;

  #database = null;

  #password = null;

  #applicationName = null;

  #statementTimeoutMs = null;

  #connectTimeoutMs = DEFAULT_CONNECT_TIMEOUT_MS;

  #sslMode = null;

  #tlsOptions = null;

  /** new | connecting | ready | closing | closed | failed */
  #state = 'new';

  #socket = null;

  #reader = new MessageReader();

  #settleConnect = null;

  #connectPromise = null;

  #pending = null;

  #closePromise = null;

  #settleClose = null;

  #deadline = null;

  #closeTimer = null;

  #scram = null;

  /** The authentication step currently running, if any; the handshake waits for it before it is ready. */
  #authInFlight = null;

  #encrypted = false;

  /** True between writing the SSLRequest and reading its one-byte answer. */
  #awaitingSslReply = false;

  #sslBuffer = Buffer.alloc(0);

  #parameters = new Map();

  #notices = [];

  #notifications = [];

  #backend = null;

  #transactionStatus = null;

  #failure = null;

  constructor(options) {
    this.#options = options ?? {};
  }

  /** True only while the connection could accept a statement: connected, idle, not shutting down. */
  get ready() {
    return this.#state === 'ready' && this.#pending === null;
  }

  /** True when this session is encrypted. With `sslMode: 'prefer'`, false means the server declined. */
  get encrypted() {
    return this.#encrypted;
  }

  /** The resolved transport mode, which is what a caller asserting on `encrypted` needs to know. */
  get sslMode() {
    return this.#sslMode;
  }

  /** 'idle' | 'transaction' | 'failed' — the server's own view, from every ReadyForQuery. */
  get transactionStatus() {
    return this.#transactionStatus;
  }

  /** The ParameterStatus values the server reported, e.g. server_version. */
  get parameters() {
    return Object.freeze(Object.fromEntries(this.#parameters));
  }

  /** The last few NoticeResponses. A notice is not an error, but dropping it silently is not right either. */
  get notices() {
    return Object.freeze([...this.#notices]);
  }

  /** The last few NOTIFY deliveries. Empty unless the session used LISTEN. */
  get notifications() {
    return Object.freeze([...this.#notifications]);
  }

  /** The backend's pid and cancel key, useful in a log line and for the cancel request this client does not send. */
  get backend() {
    return this.#backend;
  }

  /** Why the connection is unusable, or null. The message a readiness probe should report. */
  get failure() {
    return this.#failure;
  }

  /**
   * Open the connection: TCP, then TLS if the mode asks for it, then the startup packet and the
   * authentication exchange, and resolve once the server says ReadyForQuery.
   *
   * @returns {Promise<void>} rejects on any failure; a half-open client is never returned
   */
  async connect() {
    if (this.#state === 'ready') return;
    if (this.#state === 'connecting') return this.#connectPromise;
    if (this.#state !== 'new') {
      throw new PgClientError(
        CLIENT_CODE.CLOSED,
        'this client has already been used: a PgClient is one connection, and a connection that is gone is not reopened — create a new client',
      );
    }
    try {
      this.#resolveOptions();
    } catch (error) {
      // Bad configuration is a failure of this client, not of the database, and it never clears:
      // the client is unusable from here on rather than "not connected yet".
      this.#state = 'failed';
      this.#failure = error;
      throw error;
    }
    this.#state = 'connecting';
    this.#settleConnect = {};
    this.#connectPromise = new Promise((resolve, reject) => {
      this.#settleConnect.resolve = resolve;
      this.#settleConnect.reject = reject;
    });
    // A caller may reasonably ignore the promise and rely on `ready`; without a handler that would
    // surface as an unhandled rejection warning in their process rather than as our error.
    this.#connectPromise.catch(() => {});
    this.#startDeadline();

    const socket = net.connect({ host: this.#host, port: this.#port });
    this.#socket = socket;
    this.#attach(socket);
    socket.once('connect', () => {
      if (!TLS_MODES.has(this.#sslMode)) {
        this.#writeOrFail(startupMessage(this.#startupParameters()));
        return;
      }
      this.#awaitingSslReply = true;
      this.#writeOrFail(sslRequestMessage());
    });
    return this.#connectPromise;
  }

  /**
   * Run one statement, with its values bound and never interpolated.
   *
   * @param {string} text SQL with $1, $2 placeholders
   * @param {Array<unknown>} [params] one value per placeholder; the SQL casts decide the type
   * @returns {Promise<{rows: Array<Record<string, unknown>>, rowCount: number, fields: Array<{name: string, dataTypeID: number}>}>}
   */
  async query(text, params) {
    if (this.#state === 'connecting') await this.#connectPromise;
    if (this.#state !== 'ready') throw this.#notUsable();
    if (this.#pending) {
      throw new PgClientError(
        CLIENT_CODE.BUSY,
        `a statement is already in flight on ${this.#describe()}: one connection runs one statement at a time, so give each concurrent caller its own client`,
      );
    }
    if (typeof text !== 'string' || text.length === 0) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'query(text, params) needs a non-empty SQL string');
    }
    const values = params ?? [];
    if (!Array.isArray(values)) {
      throw new PgClientError(CLIENT_CODE.PARAM, `params must be an array of values, one per placeholder, got ${typeof params}`);
    }
    const encoded = values.map((value) => encodeParam(value));

    const pending = {
      fields: [],
      rows: [],
      command: null,
      error: null,
      resolve: null,
      reject: null,
    };
    const settled = new Promise((resolve, reject) => {
      pending.resolve = resolve;
      pending.reject = reject;
    });
    this.#pending = pending;
    try {
      // One write for the whole exchange. Parse/Bind/Describe/Execute/Sync travel as a single
      // batch, so the server never sees a half-sent statement and the round trips collapse to one.
      this.#write(Buffer.concat([
        parseMessage(text),
        bindMessage(encoded),
        describeMessage(),
        executeMessage(),
        syncMessage(),
      ]));
    } catch (error) {
      this.#pending = null;
      throw error;
    }
    return settled;
  }

  /** BEGIN. Returns the statement's result, which carries nothing but the command tag. */
  async begin() {
    return this.query('BEGIN');
  }

  /** COMMIT. */
  async commit() {
    return this.query('COMMIT');
  }

  /** ROLLBACK. */
  async rollback() {
    return this.query('ROLLBACK');
  }

  /**
   * Terminate the session politely and wait for the socket to close. Idempotent, and it resolves
   * even if the connection was already dead: a statement in flight is rejected, because a result
   * that arrives after close() has nowhere to go.
   */
  async close() {
    if (this.#state === 'closed') return;
    if (this.#state === 'closing') return this.#closePromise;
    this.#clearDeadline();
    const pending = this.#pending;
    this.#pending = null;
    if (pending) {
      pending.reject(new PgClientError(CLIENT_CODE.CLOSED, 'the connection was closed before the statement completed'));
    }
    if (this.#state === 'connecting') {
      this.#state = 'closing';
      this.#settleConnect.reject(new PgClientError(CLIENT_CODE.CLOSED, 'the connection was closed while it was being established'));
      this.#settleClose = null;
      this.#closePromise = Promise.resolve();
      this.#destroy();
      return this.#closePromise;
    }
    this.#state = 'closing';
    this.#closePromise = new Promise((resolve) => {
      this.#settleClose = resolve;
    });
    try {
      this.#write(terminateMessage());
      this.#socket.end();
    } catch {
      // The socket is already gone. Terminate is a courtesy, not a requirement: the server reaps a
      // backend whose socket closed just as well.
      this.#finishClose();
      return this.#closePromise;
    }
    // A server that does not close promptly must not hold a shutdown open; the grace period turns
    // a lingering FIN into a destroyed socket.
    this.#closeTimer = setTimeout(() => this.#finishClose(), CLOSE_GRACE_MS);
    return this.#closePromise;
  }

  // -------------------------------------------------------------------------------------------
  // Options
  // -------------------------------------------------------------------------------------------

  /** Resolve and validate every option. Throws rather than guessing: connect() turns this into a rejection. */
  #resolveOptions() {
    const options = this.#options;
    if (options === null || typeof options !== 'object') {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'createClient() needs an options object');
    }
    const host = options.host ?? 'localhost';
    if (typeof host !== 'string' || host.length === 0) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'host must be a non-empty hostname or address');
    }
    const port = options.port ?? DEFAULT_PORT;
    if (!Number.isInteger(port) || port < 1 || port > 65535) {
      throw new PgClientError(CLIENT_CODE.CONFIG, `port must be an integer between 1 and 65535, got ${String(options.port)}`);
    }
    if (typeof options.user !== 'string' || options.user.length === 0) {
      throw new PgClientError(
        CLIENT_CODE.CONFIG,
        'user is required: the startup packet names the identity the session runs as (SAC_ROLE in a deployment)',
      );
    }
    const database = options.database ?? options.user;
    if (typeof database !== 'string' || database.length === 0) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'database must be a non-empty name');
    }
    const password = options.password ?? null;
    if (password !== null && typeof password !== 'string') {
      throw new PgClientError(
        CLIENT_CODE.CONFIG,
        'password must be a string when present; this client authenticates the identity the caller configured — it does not mint tokens for a managed identity, so a deployed process that has no password simply leaves this out',
      );
    }
    const applicationName = options.applicationName ?? null;
    if (applicationName !== null && typeof applicationName !== 'string') {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'applicationName must be a string');
    }
    const statementTimeoutMs = options.statementTimeoutMs ?? null;
    if (statementTimeoutMs !== null && (!Number.isInteger(statementTimeoutMs) || statementTimeoutMs < 0 || statementTimeoutMs > 2147483647)) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'statementTimeoutMs must be a whole number of milliseconds between 0 and 2147483647');
    }
    const connectTimeoutMs = options.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS;
    if (!Number.isInteger(connectTimeoutMs) || connectTimeoutMs < 1) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'connectTimeoutMs must be a positive whole number of milliseconds');
    }
    this.#host = host;
    this.#port = port;
    this.#user = options.user;
    this.#database = database;
    this.#password = password;
    this.#applicationName = applicationName;
    this.#statementTimeoutMs = statementTimeoutMs;
    this.#connectTimeoutMs = connectTimeoutMs;
    this.#sslMode = this.#resolveSslMode();
    this.#tlsOptions = this.#resolveTlsOptions();
  }

  /**
   * The transport mode, from `ssl` and `sslMode`.
   *
   * The two must not contradict each other, and one of them must be present: this is the place the
   * requirement that an unencrypted connection be an explicit choice is implemented, and it is
   * implemented by refusing to choose. `ssl: false` is itself an explicit statement (it is one of
   * the frozen options), so it means 'disable' without needing the second spelling.
   */
  #resolveSslMode() {
    const { ssl, sslMode } = this.#options;
    const sslIsObject = ssl !== null && typeof ssl === 'object';
    if (ssl !== undefined && typeof ssl !== 'boolean' && !sslIsObject) {
      throw new PgClientError(CLIENT_CODE.CONFIG, 'ssl must be true, false, or an object of TLS options');
    }
    if (sslMode !== undefined) {
      if (!SSL_MODES.includes(sslMode)) {
        throw new PgClientError(
          CLIENT_CODE.CONFIG,
          `sslMode must be one of ${SSL_MODES.join(', ')}: 'require' is deliberately not offered because it encrypts without authenticating the server — pass the CA instead (tls: { ca })`,
        );
      }
      if ((ssl === true || sslIsObject) && sslMode !== 'verify-full') {
        throw new PgClientError(CLIENT_CODE.CONFIG, `ssl and sslMode contradict each other: ssl asks for a verified TLS session, sslMode ${sslMode} does not`);
      }
      if (ssl === false && sslMode !== 'disable') {
        throw new PgClientError(CLIENT_CODE.CONFIG, `ssl: false and sslMode: '${sslMode}' contradict each other`);
      }
      return sslMode;
    }
    if (ssl === true || sslIsObject) return 'verify-full';
    if (ssl === false) return 'disable';
    throw new PgClientError(
      CLIENT_CODE.CONFIG,
      'the transport was not chosen: pass ssl: true to require TLS with certificate verification, or sslMode: \'disable\' to accept an unencrypted connection — this client will not pick for you, because a silent fallback to plaintext is the one mistake that cannot be detected afterwards',
    );
  }

  /** Extra TLS material (a private CA, a client certificate), with verification kept on. */
  #resolveTlsOptions() {
    const { ssl, tls: tlsOptions } = this.#options;
    const merged = {
      ...(ssl !== null && typeof ssl === 'object' ? ssl : {}),
      ...(tlsOptions ?? {}),
    };
    if (merged.rejectUnauthorized === false) {
      throw new PgClientError(
        CLIENT_CODE.CONFIG,
        'this client verifies the server certificate and does not offer a way to switch that off; if the server uses a private CA, pass it as `tls: { ca }` so the certificate can be checked against something',
      );
    }
    return merged;
  }

  #startDeadline() {
    this.#deadline = setTimeout(() => {
      this.#failTransport(new PgClientError(
        CLIENT_CODE.CONNECT_TIMEOUT,
        `the connection to ${this.#describe()} was not ready within ${this.#connectTimeoutMs}ms (TCP, TLS, authentication and ReadyForQuery all count)`,
      ));
    }, this.#connectTimeoutMs);
    // A pending connect deadline must not keep a process alive on its own.
    this.#deadline.unref?.();
  }

  #clearDeadline() {
    if (this.#deadline === null) return;
    clearTimeout(this.#deadline);
    this.#deadline = null;
  }

  // -------------------------------------------------------------------------------------------
  // Transport
  // -------------------------------------------------------------------------------------------

  #attach(socket) {
    // A query is a few small writes that must not wait on Nagle's algorithm, and a half-open
    // connection is otherwise indistinguishable from a slow statement. Keepalive probes are a
    // hint, not a guarantee: the real bound on a statement is the server's statement_timeout.
    socket.setNoDelay(true);
    socket.setKeepAlive(true, KEEPALIVE_MS);
    // Every handler ignores events from a socket that is no longer the transport, which is what
    // makes wrapping the raw socket in TLS safe: the raw socket's listeners stay attached (so an
    // error on it is never unhandled) but stop being acted on.
    socket.on('data', (chunk) => {
      if (this.#socket === socket) this.#onData(chunk);
    });
    socket.on('error', (cause) => {
      if (this.#socket === socket) this.#onSocketError(cause);
    });
    socket.on('close', () => {
      if (this.#socket === socket) this.#onSocketClose();
    });
  }

  #onData(chunk) {
    if (this.#awaitingSslReply) {
      this.#onSslReply(chunk);
      return;
    }
    this.#dispatchChunk(chunk);
  }

  #onSslReply(chunk) {
    this.#sslBuffer = this.#sslBuffer.length > 0 ? Buffer.concat([this.#sslBuffer, chunk]) : chunk;
    if (this.#sslBuffer.length === 0) return;
    const answer = this.#sslBuffer[0];
    const rest = this.#sslBuffer.subarray(1);
    this.#sslBuffer = Buffer.alloc(0);
    this.#awaitingSslReply = false;
    if (answer === 0x53) {
      // 'S'. The server sends this byte and then waits for the ClientHello, so anything after it is
      // a server that is not speaking this protocol; dropping the bytes would desynchronise a TLS
      // handshake that has not started yet.
      if (rest.length > 0) {
        this.#failTransport(new PgClientError(CLIENT_CODE.PROTOCOL, 'the server sent data before the TLS handshake began'));
        return;
      }
      this.#startTls();
      return;
    }
    if (answer === 0x4e) {
      // 'N'. The server has no TLS. Only an explicit 'disable' or the documented 'prefer' fallback
      // reaches here, and `encrypted` records which connection this turned out to be.
      if (this.#sslMode === 'verify-full') {
        this.#failTransport(new PgClientError(
          CLIENT_CODE.TLS,
          `the server at ${this.#describe()} does not offer TLS, and this connection requires it: pass sslMode: 'disable' for a local lab, or point this client at the TLS listener`,
        ));
        return;
      }
      this.#encrypted = false;
      if (rest.length > 0) this.#dispatchChunk(rest);
      this.#writeOrFail(startupMessage(this.#startupParameters()));
      return;
    }
    this.#failTransport(new PgClientError(
      CLIENT_CODE.PROTOCOL,
      `the server answered the TLS request with byte 0x${answer.toString(16)}, which the protocol does not define`,
    ));
  }

  #startTls() {
    const raw = this.#socket;
    const secure = tls.connect({
      ...this.#tlsOptions,
      socket: raw,
      // servername is what makes verification check the name the caller asked for rather than the
      // address it resolved to; it is overridable for a certificate issued to another name. The
      // values after the spread are the ones this client will not let an option override.
      servername: this.#tlsOptions.servername ?? this.#host,
      rejectUnauthorized: true,
    });
    this.#socket = secure;
    this.#attach(secure);
    secure.once('secureConnect', () => {
      this.#encrypted = true;
      this.#writeOrFail(startupMessage(this.#startupParameters()));
    });
  }

  #startupParameters() {
    return {
      user: this.#user,
      database: this.#database,
      application_name: this.#applicationName,
      client_encoding: 'UTF8',
      // The decoders read ISO dates and timestamps. The default is ISO already, but a server
      // configured otherwise would break every timestamp in every result, and one startup
      // parameter removes that whole class of environment-dependent failure.
      DateStyle: 'ISO, MDY',
      options: this.#statementTimeoutMs === null ? undefined : `-c statement_timeout=${this.#statementTimeoutMs}`,
    };
  }

  /** A write from an event handler: there is no caller to hand a failure to, so it fails the connection. */
  #writeOrFail(buffer) {
    try {
      this.#write(buffer);
    } catch (error) {
      this.#failTransport(error);
    }
  }

  #write(buffer) {
    const socket = this.#socket;
    if (socket === null || socket.destroyed || !socket.writable) {
      throw new PgClientError(CLIENT_CODE.CONNECTION, `the connection to ${this.#describe()} is no longer writable`);
    }
    socket.write(buffer);
  }

  #describe() {
    return `${this.#host}:${this.#port}/${this.#database}`;
  }

  // -------------------------------------------------------------------------------------------
  // Messages
  // -------------------------------------------------------------------------------------------

  #dispatchChunk(chunk) {
    let messages;
    try {
      messages = this.#reader.push(chunk);
    } catch (error) {
      this.#failTransport(error);
      return;
    }
    for (const message of messages) {
      if (this.#state === 'closed' || this.#state === 'failed') return;
      this.#dispatch(message);
    }
  }

  #dispatch(message) {
    switch (message.type) {
      case 'S': {
        const status = parseParameterStatus(message.payload);
        this.#parameters.set(status.name, status.value);
        return;
      }
      case 'N':
        this.#pushNotice(message.payload);
        return;
      case 'A':
        this.#pushNotification(message.payload);
        return;
      default:
        break;
    }
    if (this.#state === 'connecting') {
      this.#onStartupMessage(message);
      return;
    }
    if (this.#pending !== null) {
      this.#onQueryMessage(message);
      return;
    }
    if (this.#state === 'ready') {
      // Nothing was asked of the server, so there is nothing this message can be the answer to.
      this.#failTransport(new PgClientError(
        CLIENT_CODE.PROTOCOL,
        `the server sent a '${message.type}' message when no statement was in flight`,
      ));
    }
    // During close() the server may still be draining a statement we abandoned; those messages are
    // deliberately ignored rather than treated as a protocol error.
  }

  #onStartupMessage(message) {
    switch (message.type) {
      case 'R':
        // Authentication can be asynchronous (SCRAM derives a key, and verifies the server's own
        // proof), so its failures are funnelled back through #failTransport rather than thrown into
        // the socket's data handler — and the handshake is not finished until the step is, because
        // the server may pipeline its ReadyForQuery behind the last authentication message.
        this.#authInFlight = this.#onAuthentication(message.payload).catch((error) => this.#failTransport(error));
        return;
      case 'K':
        this.#backend = Object.freeze(parseBackendKeyData(message.payload));
        return;
      case 'E': {
        const { fields, mapped } = parseErrorOrNotice(message.payload);
        this.#failTransport(new PgError(mapped.code ?? 'XXXXX', mapped.message ?? 'the server refused the connection', fields, mapped));
        return;
      }
      case 'Z':
        this.#finishHandshake(message.payload);
        return;
      default:
        this.#failTransport(new PgClientError(
          CLIENT_CODE.PROTOCOL,
          `the server sent a '${message.type}' message during the handshake, which the startup sequence does not define`,
        ));
    }
  }

  /**
   * The server is ready. If an authentication step is still running — a SCRAM derivation, or the
   * check of the server's own proof — the handshake waits for it, because a pipelined
   * ReadyForQuery must not be able to report success before the authentication that preceded it
   * has had its say.
   */
  async #finishHandshake(payload) {
    if (this.#authInFlight !== null) await this.#authInFlight;
    if (this.#state !== 'connecting') return;
    this.#transactionStatus = transactionStatusOf(parseReadyForQuery(payload));
    this.#clearDeadline();
    this.#state = 'ready';
    this.#settleConnect.resolve();
  }

  async #onAuthentication(payload) {
    const authentication = parseAuthentication(payload);
    switch (authentication.method) {
      case 'ok':
        return;
      case 'cleartext': {
        if (this.#password === null) {
          throw new PgClientError(
            CLIENT_CODE.AUTH,
            'the server asked for a cleartext password and none is configured: a deployed database here authenticates a managed identity, and this client cannot mint that token — pass a password only where a password is really what authenticates',
          );
        }
        this.#write(passwordMessage(this.#password));
        return;
      }
      case 'md5': {
        if (this.#password === null) {
          throw new PgClientError(CLIENT_CODE.AUTH, 'the server asked for an MD5 password and none is configured for this client');
        }
        this.#write(passwordMessage(md5Password(this.#password, this.#user, authentication.salt, createHash)));
        return;
      }
      case 'sasl': {
        const mechanisms = authentication.mechanisms ?? [];
        if (!mechanisms.includes('SCRAM-SHA-256')) {
          throw new PgClientError(
            CLIENT_CODE.AUTH,
            `the server offered ${mechanisms.join(', ') || 'no SASL mechanism'}, and this client implements SCRAM-SHA-256: a managed-identity database offering OAUTHBEARER authenticates a token the platform issues, which a process holding no platform identity cannot produce`,
          );
        }
        if (this.#password === null) {
          throw new PgClientError(
            CLIENT_CODE.AUTH,
            'the server asked for SCRAM-SHA-256 and no password is configured for this client: in a deployment the database authenticates a managed identity, so there is no password to send, and this client does not mint that token',
          );
        }
        this.#scram = createScramClient({ password: this.#password, nonce: randomNonce() });
        this.#write(saslInitialResponse(this.#scram.mechanism, this.#scram.clientFirstMessage()));
        return;
      }
      case 'sasl-continue': {
        if (this.#scram === null) {
          throw new PgClientError(CLIENT_CODE.PROTOCOL, 'the server continued a SASL exchange this client never started');
        }
        await this.#scram.receiveServerFirst(authentication.data);
        this.#write(saslResponse(await this.#scram.clientFinalMessage()));
        return;
      }
      case 'sasl-final': {
        if (this.#scram === null) {
          throw new PgClientError(CLIENT_CODE.PROTOCOL, 'the server finished a SASL exchange this client never started');
        }
        this.#scram.verifyServerFinal(authentication.data);
        return;
      }
      default:
        throw new PgClientError(
          CLIENT_CODE.AUTH,
          `the server asked for ${authentication.method} authentication, which this client does not implement`,
        );
    }
  }

  #onQueryMessage(message) {
    const pending = this.#pending;
    switch (message.type) {
      case 'T':
        // Exactly the frozen shape: the column name and the OID that decides its decoding. The rest
        // of RowDescription (table OID, type modifier, format) is not something a caller of this
        // seam needs, and handing it out would invite a dependency on it.
        pending.fields = parseRowDescription(message.payload).map(({ name, dataTypeID }) => Object.freeze({ name, dataTypeID }));
        return;
      case 'D': {
        if (pending.fields.length === 0) {
          this.#failTransport(new PgClientError(CLIENT_CODE.PROTOCOL, 'the server sent a row before describing the columns it belongs to'));
          return;
        }
        try {
          const values = parseDataRow(message.payload);
          const row = {};
          for (let i = 0; i < pending.fields.length; i += 1) {
            row[pending.fields[i].name] = decodeValue(pending.fields[i].dataTypeID, values[i] ?? null);
          }
          pending.rows.push(row);
        } catch (error) {
          // A value this client cannot represent is not a reason to drop the connection: the rest
          // of the response is still drained to ReadyForQuery, and the caller gets the error.
          if (pending.error === null) pending.error = error;
        }
        return;
      }
      case 'C':
        pending.command = parseCommandComplete(message.payload);
        return;
      case 'E': {
        const { fields, mapped } = parseErrorOrNotice(message.payload);
        // The first error is the one that aborted the statement; a second is a consequence of it.
        if (pending.error === null) {
          pending.error = new PgError(mapped.code ?? 'XXXXX', mapped.message ?? 'the server reported an error', fields, mapped);
        }
        return;
      }
      case 'K':
        this.#backend = Object.freeze(parseBackendKeyData(message.payload));
        return;
      case 'Z':
        this.#finishStatement(message.payload);
        return;
      case '1':
      case '2':
      case '3':
      case 'n':
      case 't':
      case 'I':
      case 's':
        // ParseComplete, BindComplete, CloseComplete, NoData, ParameterDescription,
        // EmptyQueryResponse and PortalSuspended are all messages this exchange asked for; none of
        // them changes the answer, which comes from RowDescription, DataRow and CommandComplete.
        return;
      default:
        this.#failTransport(new PgClientError(
          CLIENT_CODE.PROTOCOL,
          `the server sent a '${message.type}' message in the middle of a statement, which the protocol does not define`,
        ));
    }
  }

  #finishStatement(payload) {
    const status = transactionStatusOf(parseReadyForQuery(payload));
    this.#transactionStatus = status;
    const pending = this.#pending;
    this.#pending = null;
    if (pending === null) return;
    if (pending.error !== null) {
      pending.reject(pending.error);
      return;
    }
    const rowCount = pending.command === null
      ? pending.rows.length
      : rowCountOfTag(pending.command) ?? pending.rows.length;
    pending.resolve({
      rows: pending.rows,
      rowCount,
      fields: Object.freeze(pending.fields),
      // Additive: the command tag ('SELECT 5', 'INSERT 0 1') is what rowCount was read from, and a
      // caller that wants to distinguish an INSERT from an UPDATE should not have to guess.
      command: pending.command ?? '',
    });
  }

  #pushNotice(payload) {
    const { fields, mapped } = parseErrorOrNotice(payload);
    if (this.#notices.length === NOTICE_LIMIT) this.#notices.shift();
    this.#notices.push(Object.freeze({ severity: mapped.severity, code: mapped.code, message: mapped.message, detail: mapped.detail, fields: Object.freeze({ ...fields }) }));
  }

  #pushNotification(payload) {
    if (this.#notifications.length === NOTICE_LIMIT) this.#notifications.shift();
    this.#notifications.push(Object.freeze(parseNotificationResponse(payload)));
  }

  // -------------------------------------------------------------------------------------------
  // Failure and teardown
  // -------------------------------------------------------------------------------------------

  /** The error a statement gets when the client is not usable. It carries the reason, not just the state. */
  #notUsable() {
    if (this.#state === 'failed') {
      const reason = this.#failure === null ? 'the connection failed' : this.#failure.message;
      return new PgClientError(CLIENT_CODE.NOT_CONNECTED, `this client is not usable: ${reason}`);
    }
    if (this.#state === 'closed' || this.#state === 'closing') {
      return new PgClientError(CLIENT_CODE.CLOSED, 'this client has been closed');
    }
    return new PgClientError(CLIENT_CODE.NOT_CONNECTED, 'this client is not connected yet: call connect() before using it');
  }

  #onSocketError(cause) {
    const tlsFailure = typeof cause?.code === 'string' && TLS_FAILURE.test(cause.code);
    this.#failTransport(new PgClientError(
      tlsFailure ? CLIENT_CODE.TLS : CLIENT_CODE.CONNECTION,
      tlsFailure
        ? `the TLS handshake with ${this.#describe()} failed: ${cause.message}`
        : `the connection to ${this.#describe()} failed: ${cause.message}`,
      { cause },
    ));
  }

  #onSocketClose() {
    if (this.#state === 'closing') {
      this.#finishClose();
      return;
    }
    if (this.#state === 'closed' || this.#state === 'failed') return;
    this.#failTransport(new PgClientError(
      CLIENT_CODE.CONNECTION,
      `the server closed the connection to ${this.#describe()}${this.#pending !== null ? ' while a statement was in flight' : ''}`,
    ));
  }

  /**
   * Every transport-level failure lands here: a socket error, a close we did not ask for, a
   * protocol violation, the connect deadline. Whatever was outstanding is rejected with it, and
   * the client is unusable from here on rather than half-usable.
   */
  #failTransport(error) {
    if (this.#state === 'closed' || this.#state === 'failed') return;
    this.#state = 'failed';
    this.#failure = error;
    this.#clearDeadline();
    const pending = this.#pending;
    this.#pending = null;
    this.#settleConnect?.reject(error);
    if (pending !== null) pending.reject(error);
    this.#destroy();
  }

  #finishClose() {
    if (this.#state === 'closed') return;
    this.#state = 'closed';
    this.#clearDeadline();
    if (this.#closeTimer !== null) {
      clearTimeout(this.#closeTimer);
      this.#closeTimer = null;
    }
    this.#destroy();
    this.#settleClose?.();
  }

  #destroy() {
    const socket = this.#socket;
    this.#socket = null;
    if (socket !== null && !socket.destroyed) socket.destroy();
  }
}

function transactionStatusOf(letter) {
  if (letter === 'I') return 'idle';
  if (letter === 'T') return 'transaction';
  if (letter === 'E') return 'failed';
  return null;
}

/**
 * Open a client. Nothing here touches the network: the first byte is written by connect(), so a
 * caller can construct a client at boot and find out at connect() whether the database is there.
 *
 * @param {object} options
 * @param {string} [options.host] default 'localhost'
 * @param {number} [options.port] default 5432
 * @param {string} options.user the identity the session runs as (SAC_ROLE)
 * @param {string} [options.database] default: the user
 * @param {string|null} [options.password] absent in a deployment, where a managed identity authenticates
 * @param {boolean|object} [options.ssl] true requires a verified TLS session; an object is TLS options
 * @param {'disable'|'prefer'|'verify-full'} [options.sslMode] the transport, required when `ssl` is absent
 * @param {number} [options.statementTimeoutMs] set as a startup option, so it is in force from the first statement
 * @param {string} [options.applicationName] what pg_stat_activity shows for this session
 * @param {number} [options.connectTimeoutMs] default 10000, covering TCP, TLS, authentication and ReadyForQuery
 * @param {object} [options.tls] extra TLS options (ca, cert, key, servername); verification stays on
 * @returns {PgClient}
 */
export function createClient(options) {
  return new PgClient(options);
}
