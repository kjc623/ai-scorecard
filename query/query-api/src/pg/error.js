// error.js — the two failure vocabularies this client can produce, kept apart on purpose.
//
// The HTTP layer's error envelope is closed: a SQLSTATE maps to a result state, and anything it
// does not recognise is a 500. That mapping is only safe while "the database said no" and "we never
// reached the database" are distinguishable, so they are two classes rather than one class with an
// optional field:
//
//   PgError        the server answered with an ErrorResponse. `code` IS the SQLSTATE (23505, 42P01,
//                  57014) and it is the field a caller switches on. `sqlState` is the same value
//                  under the name that cannot be confused with a Node errno.
//   PgClientError  this client never got as far as a statement, or the connection died. `code` is
//                  a SAC_* symbol: never five characters of SQLSTATE-shaped text, so a caller that
//                  tests a code against /^[0-9A-Z]{5}$/ cannot mistake a local failure for a
//                  database verdict. The underlying cause is preserved on `cause`.
//
// The severity, detail, hint, constraint, table and position fields are carried because the
// alternative is a caller re-parsing `message` to find out which constraint fired, which is how
// error handling rots.

/** The five-character SQLSTATE shape, so a caller can tell the two vocabularies apart mechanically. */
const SQLSTATE = /^[0-9A-Z]{5}$/;

/** Client-side failures. Every one of these means "the database did not answer this statement". */
export const CLIENT_CODE = Object.freeze({
  /** An option could not be used: a bad port, a contradictory TLS choice, a missing user. */
  CONFIG: 'SAC_CONFIG',
  /** The whole handshake (TCP, TLS, authentication, ReadyForQuery) did not finish in time. */
  CONNECT_TIMEOUT: 'SAC_CONNECT_TIMEOUT',
  /** The socket failed or closed unexpectedly while a statement was in flight. */
  CONNECTION: 'SAC_CONNECTION',
  /** A statement was issued on a client that is not connected (or no longer connected). */
  NOT_CONNECTED: 'SAC_NOT_CONNECTED',
  /** `connect()` was called on a client that has already been closed: a client is used once. */
  CLOSED: 'SAC_CLOSED',
  /** A second statement was issued while one was still in flight: one connection, one statement. */
  BUSY: 'SAC_BUSY',
  /** Authentication could not be performed or was refused by the server. */
  AUTH: 'SAC_AUTH',
  /** TLS was required by the chosen sslMode and the server refused it, or the handshake failed. */
  TLS: 'SAC_TLS',
  /** The bytes on the wire are not a sequence this protocol allows. */
  PROTOCOL: 'SAC_PROTOCOL',
  /** A parameter value has no wire representation this client is willing to guess at. */
  PARAM: 'SAC_PARAM',
  /** A value arrived in a form the type's decoder cannot represent without inventing data. */
  DECODE: 'SAC_DECODE',
});

/**
 * A failure the server reported, carrying its SQLSTATE. `message` is the server's primary message
 * verbatim: it is the text a person needs (it names the relation, the constraint, the syntax
 * position) and rewriting it here would only make the two disagree.
 */
export class PgError extends Error {
  /**
   * @param {string} code the SQLSTATE, e.g. '23505'
   * @param {string} message the server's primary message
   * @param {object} [fields] every field the ErrorResponse carried, keyed by its protocol letter
   * @param {object} [mapped] the same fields under readable names
   */
  constructor(code, message, fields = {}, mapped = {}) {
    super(message);
    this.name = 'PgError';
    this.code = code;
    this.sqlState = code;
    this.severity = mapped.severity ?? fields.S ?? null;
    this.detail = mapped.detail ?? null;
    this.hint = mapped.hint ?? null;
    this.position = mapped.position ?? null;
    this.constraint = mapped.constraint ?? null;
    this.schema = mapped.schema ?? null;
    this.table = mapped.table ?? null;
    this.column = mapped.column ?? null;
    this.dataType = mapped.dataType ?? null;
    this.routine = mapped.routine ?? null;
    this.fields = Object.freeze({ ...fields });
  }
}

/** A failure of this client rather than of the database. `code` is always one of CLIENT_CODE. */
export class PgClientError extends Error {
  /**
   * @param {string} code one of CLIENT_CODE
   * @param {string} message what happened, and the fix where one exists
   * @param {{cause?: unknown}} [options]
   */
  constructor(code, message, options = {}) {
    super(message);
    this.name = 'PgClientError';
    this.code = code;
    if (options.cause !== undefined) this.cause = options.cause;
  }
}

/**
 * The SQLSTATE of a failure, or null when there is none.
 *
 * A caller maps this to the closed error envelope and needs to answer "did the database refuse
 * this?" without importing the classes — this is that one question, asked in one place. A
 * PgClientError has no SQLSTATE by construction, which is what makes "23505 is a 409 and a dead
 * socket is a 503" a one-liner rather than a class check in every branch.
 *
 * @param {unknown} error
 * @returns {string|null}
 */
export function sqlStateOf(error) {
  const code = error?.code;
  return typeof code === 'string' && SQLSTATE.test(code) ? code : null;
}

/** True for a failure the database reported (as opposed to one this client hit getting there). */
export function isPgError(error) {
  return error instanceof PgError;
}
