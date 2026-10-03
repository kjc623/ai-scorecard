// types.js — what a value is, in both directions.
//
// PostgreSQL sends every value of a result set with its type's *output function*, and reads what a
// Bind carries with its type's *input function*. That symmetry is the whole design here: the client
// never guesses a type from the bytes, it decodes by the OID that RowDescription named, and it
// renders a parameter with the one text form the target type's input function is documented to
// accept. The caller's SQL says what the type is (`$1::uuid`, `$2::text[]` — compile.js casts every
// placeholder), so there is exactly one source of truth for each side.
//
// Three judgements are visible in the code below, because each of them is a place a driver can
// silently lose data:
//
//   1. Lossless or explicit. int8 becomes a number only while it fits a double exactly; past that
//      it is the exact decimal string. numeric becomes a number only when the decimal it would
//      print is the decimal that arrived. A value that cannot be represented is returned as the
//      text the server sent, never as the nearest number.
//   2. 'timestamp' (no zone) is read as UTC. A JS Date is an instant, and a zoneless wall time is
//      not one; the alternative is the host's local zone, which would make a value's meaning depend
//      on where the process runs. Every stored timestamp in this schema is UTC, so UTC is the
//      reading that agrees with the data.
//   3. An unknown OID is the text the server sent. A column type this file has never heard of
//      (money, an array, a domain over one, something an extension added) is not a reason to fail a
//      query, and it is certainly not a reason to guess.

import { CLIENT_CODE, PgClientError } from './error.js';

/** The type OIDs this client decodes by name. Values outside this table are returned as text. */
export const OID = Object.freeze({
  bool: 16,
  bytea: 17,
  char: 18,
  name: 19,
  int8: 20,
  int2: 21,
  int4: 23,
  text: 25,
  oid: 26,
  json: 114,
  xml: 142,
  float4: 700,
  float8: 701,
  bpchar: 1042,
  varchar: 1043,
  date: 1082,
  time: 1083,
  timestamp: 1114,
  timestamptz: 1184,
  interval: 1186,
  timetz: 1266,
  numeric: 1700,
  uuid: 2950,
  jsonb: 3802,
  void: 2278,
});

/**
 * Types whose text form is already the JS value: strings, and the types JS has no distinct value
 * for (time, timetz and interval stay strings; inventing a Date for 'time' or a duration object for
 * 'interval' would be a second, private type system).
 */
const AS_TEXT = new Set([
  OID.text,
  OID.varchar,
  OID.bpchar,
  OID.char,
  OID.name,
  OID.xml,
  OID.uuid,
  OID.time,
  OID.timetz,
  OID.interval,
]);

function decodeError(dataTypeID, text, why) {
  return new PgClientError(
    CLIENT_CODE.DECODE,
    `a column of type ${dataTypeID} arrived as ${JSON.stringify(text)}: ${why}`,
  );
}

function decodeBool(text, dataTypeID) {
  if (text === 't') return true;
  if (text === 'f') return false;
  throw decodeError(dataTypeID, text, 'bool is only ever "t" or "f"');
}

function decodeBytea(text) {
  if (text.startsWith('\\x')) return Buffer.from(text.slice(2), 'hex');
  // The escape format, for a server configured with bytea_output = escape. A byte that is not
  // printable arrives as \nnn in octal, and a literal backslash as \\.
  const bytes = [];
  for (let i = 0; i < text.length; i += 1) {
    if (text[i] !== '\\') {
      bytes.push(text.charCodeAt(i));
      continue;
    }
    const next = text[i + 1];
    if (next === '\\') {
      bytes.push(0x5c);
      i += 1;
      continue;
    }
    const octal = text.slice(i + 1, i + 4);
    if (/^[0-7]{3}$/.test(octal)) {
      bytes.push(Number.parseInt(octal, 8));
      i += 3;
      continue;
    }
    throw decodeError(OID.bytea, text, 'an escape that is neither \\\\ nor an octal byte');
  }
  return Buffer.from(bytes);
}

/**
 * int8: a number while the value survives the trip, the exact decimal text when it would not.
 * '9007199254740993' is a valid bigint whose neighbours collapse into it as doubles, so returning
 * Number() there would answer a different question than the one asked.
 */
function decodeInt8(text, dataTypeID) {
  const value = Number(text);
  if (!Number.isSafeInteger(value)) return text;
  if (String(value) !== text) throw decodeError(dataTypeID, text, 'not an integer');
  return value;
}

function decodeInteger(text, dataTypeID) {
  const value = Number(text);
  if (!Number.isInteger(value)) throw decodeError(dataTypeID, text, 'not an integer');
  return value;
}

/** float4/float8: 'Infinity', '-Infinity' and 'NaN' are values in PostgreSQL, not errors. */
function decodeFloat(text, dataTypeID) {
  const value = Number(text);
  if (Number.isNaN(value) && text !== 'NaN') throw decodeError(dataTypeID, text, 'not a float');
  return value;
}

/**
 * numeric is an arbitrary-precision decimal, and a double is not. The value becomes a number only
 * when the number prints back as the same decimal it arrived as — so 0.1 and 1.10 become numbers
 * (their shortest round-trip form is what arrived), while 30 significant digits stay text.
 */
function decodeNumeric(text) {
  const value = Number(text);
  if (Number.isFinite(value) && canonicalDecimal(text) === canonicalDecimal(String(value))) return value;
  return text;
}

/** A decimal in a single normal form, so '1.10', '1.1', '+1.1' and '1.1e0' are one string. */
function canonicalDecimal(text) {
  let rest = text.trim();
  let sign = '';
  if (rest.startsWith('+')) rest = rest.slice(1);
  if (rest.startsWith('-')) {
    sign = '-';
    rest = rest.slice(1);
  }
  const [mantissa, exponent = '0'] = rest.toLowerCase().split('e');
  const [integer = '0', fraction = ''] = mantissa.split('.');
  const digits = `${integer.replace(/^0+(?=\d)/, '')}${fraction}`;
  const exponentValue = Number(exponent) - fraction.length;
  const stripped = digits.replace(/^0+/, '');
  if (stripped === '') return '0';
  const significant = stripped.replace(/0+$/, '');
  const shifted = exponentValue + (stripped.length - significant.length);
  return `${sign}${significant}e${shifted}`;
}

function decodeJson(text, dataTypeID) {
  try {
    return JSON.parse(text);
  } catch (cause) {
    throw new PgClientError(CLIENT_CODE.DECODE, `a column of type ${dataTypeID} is not valid JSON`, { cause });
  }
}

const DATE = /^(\d{4,})-(\d{2})-(\d{2})(?: (BC))?$/;
const TIMESTAMP = /^(\d{4,})-(\d{2})-(\d{2})[ T](\d{2}):(\d{2}):(\d{2})(?:\.(\d+))?(?:([+-])(\d{2})(?::?(\d{2}))?(?::?(\d{2}))?)?(?: (BC))?$/;

/** The two values that are not instants and cannot become a Date. They stay the text they arrived as. */
const INFINITY = new Set(['infinity', '-infinity']);

/** Date.UTC() maps years 0-99 into the 20th century; setUTCFullYear is the only correct way in. */
function utcTime(year, month, day, hour, minute, second, milli) {
  const date = new Date(0);
  date.setUTCFullYear(year, month - 1, day);
  date.setUTCHours(hour, minute, second, milli);
  return date;
}

function decodeDate(text) {
  if (INFINITY.has(text)) return text;
  const match = DATE.exec(text);
  if (!match) throw decodeError(OID.date, text, 'not an ISO date');
  const year = match[4] ? 1 - Number(match[1]) : Number(match[1]);
  // A date is a calendar day, not an instant. UTC midnight is the only reading that is the same day
  // in every zone the process might run in, and toISOString().slice(0, 10) gets the day back.
  return utcTime(year, Number(match[2]), Number(match[3]), 0, 0, 0, 0);
}

/**
 * A timestamp, with or without a zone. A zone-less value is read as UTC (see the header). Microseconds
 * beyond the first three digits are dropped: a JS Date has milliseconds, and rounding a stored value
 * up into the next millisecond would be a silent lie about it.
 */
function parseTimestamp(text, dataTypeID) {
  if (INFINITY.has(text)) return text;
  const match = TIMESTAMP.exec(text);
  if (!match) throw decodeError(dataTypeID, text, 'not an ISO timestamp');
  const [, rawYear, month, day, hour, minute, second, fraction, sign, offHour, offMinute, offSecond, bc] = match;
  const year = bc ? 1 - Number(rawYear) : Number(rawYear);
  const milli = fraction ? Number(fraction.slice(0, 3).padEnd(3, '0')) : 0;
  const date = utcTime(year, Number(month), Number(day), Number(hour), Number(minute), Number(second), milli);
  if (sign) {
    const offset = (Number(offHour) * 3600 + Number(offMinute ?? 0) * 60 + Number(offSecond ?? 0)) * 1000;
    date.setTime(date.getTime() - (sign === '-' ? -offset : offset));
  }
  // Outside the JS Date range (PostgreSQL's timestamps run to 294276 AD) there is no instant to
  // return, and an Invalid Date would be worse than the text.
  if (!Number.isFinite(date.getTime())) return text;
  return date;
}

function decodeTimestamp(text, dataTypeID) {
  return parseTimestamp(text, dataTypeID);
}

const DECODERS = new Map([
  [OID.bool, decodeBool],
  [OID.bytea, decodeBytea],
  [OID.int8, decodeInt8],
  [OID.int2, decodeInteger],
  [OID.int4, decodeInteger],
  [OID.oid, decodeInteger],
  [OID.float4, decodeFloat],
  [OID.float8, decodeFloat],
  [OID.numeric, decodeNumeric],
  [OID.date, decodeDate],
  [OID.timestamp, decodeTimestamp],
  [OID.timestamptz, decodeTimestamp],
  [OID.json, decodeJson],
  [OID.jsonb, decodeJson],
  [OID.void, () => null],
]);

/**
 * One value from a DataRow.
 *
 * @param {number} dataTypeID the OID RowDescription named for this column
 * @param {Buffer|null} buffer the bytes, or null for SQL NULL
 * @returns {unknown}
 */
export function decodeValue(dataTypeID, buffer) {
  if (buffer === null || buffer === undefined) return null;
  const text = buffer.toString('utf8');
  if (AS_TEXT.has(dataTypeID)) return text;
  const decoder = DECODERS.get(dataTypeID);
  return decoder ? decoder(text, dataTypeID) : text;
}

/** A one-line description of a value for an error message, without dumping a whole object into it. */
function describe(value) {
  if (value === undefined) return 'undefined';
  if (typeof value === 'object') return Array.isArray(value) ? 'an array' : `a ${value.constructor?.name ?? 'object'}`;
  return `a ${typeof value}`;
}

/**
 * The text form of one parameter, or null for SQL NULL.
 *
 * Everything here is a value the target type's input function accepts: 'true' is boolin's input,
 * an ISO 8601 string is timestamptz's, `\x0a` is bytea's. Whether it *fits* the cast is the
 * server's judgement to make — this client's job is to send the value faithfully and let the
 * SQLSTATE come back if it does not.
 *
 * @param {unknown} value
 * @returns {Buffer|null}
 */
export function encodeParam(value) {
  if (value === null || value === undefined) return null;
  if (typeof value === 'string') return Buffer.from(value, 'utf8');
  if (typeof value === 'boolean') return Buffer.from(value ? 'true' : 'false', 'ascii');
  if (typeof value === 'number') {
    // String() gives 'NaN', 'Infinity' and '-Infinity', which are exactly PostgreSQL's spellings
    // for those float8/numeric values. A cast to an integer type will refuse them, which is correct.
    return Buffer.from(String(value), 'ascii');
  }
  if (typeof value === 'bigint') return Buffer.from(value.toString(), 'ascii');
  if (value instanceof Date) {
    if (Number.isNaN(value.getTime())) {
      throw new PgClientError(CLIENT_CODE.PARAM, 'an Invalid Date has no representation: check what produced it');
    }
    return Buffer.from(value.toISOString(), 'ascii');
  }
  if (Buffer.isBuffer(value)) return Buffer.from(`\\x${value.toString('hex')}`, 'ascii');
  if (ArrayBuffer.isView(value)) {
    const view = Buffer.from(value.buffer, value.byteOffset, value.byteLength);
    return Buffer.from(`\\x${view.toString('hex')}`, 'ascii');
  }
  if (Array.isArray(value)) return Buffer.from(arrayLiteral(value), 'utf8');
  throw new PgClientError(
    CLIENT_CODE.PARAM,
    `no bind representation for ${describe(value)}: pass a string, number, bigint, boolean, Date, Buffer or a flat array and let the SQL cast it`,
  );
}

/**
 * A PostgreSQL array literal, for the `= ANY($1::text[])` shape compile.js emits for `in` filters.
 *
 * Every element is quoted, including numbers and booleans. The quotes delimit the element; they do
 * not make it a string, because the element is then read by the element type's input function
 * ('1' quoted is still the integer 1 for int4). Quoting uniformly is what keeps one code path
 * correct for an element containing a comma, a brace, a quote, a backslash or leading whitespace —
 * all of which are structural without quotes. NULL is the one element that must not be quoted,
 * because "NULL" quoted is the four-character string.
 */
function arrayLiteral(values) {
  const elements = values.map((element) => {
    if (element === null || element === undefined) return 'NULL';
    if (Array.isArray(element)) {
      throw new PgClientError(
        CLIENT_CODE.PARAM,
        'nested arrays are not supported: PostgreSQL multidimensional arrays are not a list of lists, and guessing at one would be a silent reshaping of the caller\'s data',
      );
    }
    const text = encodeParam(element).toString('utf8');
    return `"${text.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
  });
  return `{${elements.join(',')}}`;
}
