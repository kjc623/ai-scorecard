// =====================================================================================
// db/tools/tally-log.mjs -- the one implementation of "did this run pass?"
// =====================================================================================
// Zero dependencies. Used by db/tools/run-invariants.ps1 and covered by
// db/tools/tally-log.test.mjs.
//
// WHY THIS IS A MODULE AND NOT AN INLINE grep
// -------------------------------------------
// The tally used to live inline in run-invariants.ps1 as `grep -ci ERROR`, which counted the
// provenance header's echoed command -- "ON_ERROR_STOP" contains the substring "ERROR" -- and
// reported a healthy run as a failure. Inline shell is not reachable from a test suite, so the
// bug survived until a second person ran the script. Extracting it means the rule is unit-tested
// against captured fixtures, including the exact log that was mis-counted.
//
// WHY `fail` COUNTS EVERY REAL ERROR, NOT JUST FAIL MARKERS
// ---------------------------------------------------------
// tools/accept.mjs judges the database gate on this line's `fail=` field alone. When `fail` meant
// "lines matching ERROR:  FAIL", a run that aborted on a raw SQL error -- a CHECK violation raised
// by psql rather than by an assertion's own RAISE -- produced fail=0 and was reported as PASS.
// That is the same class of defect as the false positive, in the opposite direction: a gate that
// passes a run which did not complete. `fail` therefore counts every real diagnostic, and the
// separated fields below keep the breakdown visible.
// =====================================================================================

/**
 * A real psql diagnostic. Anchored, because the provenance header quotes the documented command
 * and `ON_ERROR_STOP` contains the substring ERROR.
 *
 * psql prints errors as `psql:<file>:<line>: ERROR:  message`. Bare `ERROR:` also occurs when the
 * server's message is emitted without the prefix.
 */
export const ANCHORED_ERROR = /^(?:psql:.*)?(?:ERROR|FATAL|PANIC):/;

/** An assertion's own failure marker: the `RAISE EXCEPTION 'FAIL T<n> ...'` form. */
export const FAIL_MARKER = /^(?:psql:.*)?ERROR:\s+FAIL\b/;

/** A passing assertion notice. */
export const PASS_NOTICE = /NOTICE:\s+PASS\s+(T\d+)/g;

/**
 * Strip the provenance header that run-invariants.ps1 prepends to each log. Header lines start
 * with '#', which no psql output line does.
 */
export function stripHeader(text) {
  return text
    .split(/\r?\n/)
    .filter((line) => !line.startsWith('#'))
    .join('\n');
}

/**
 * Tally one or two captured logs.
 *
 * @param {{tests?: string, schema?: string}} logs raw log text (header optional)
 * @returns {{
 *   pass: number, fail: number, failMarkers: number, errorLines: number,
 *   schemaErrors: number, distinct: number, ids: string[], aborted: boolean
 * }}
 */
export function tallyLogs(logs = {}) {
  const testsBody = stripHeader(logs.tests ?? '');
  const schemaBody = stripHeader(logs.schema ?? '');

  const ids = [...testsBody.matchAll(PASS_NOTICE)].map((m) => m[1]);
  const errorLines = countMatches(testsBody, ANCHORED_ERROR);
  const failMarkers = countMatches(testsBody, FAIL_MARKER);
  const schemaErrors = countMatches(schemaBody, ANCHORED_ERROR);

  return {
    pass: ids.length,
    // Every real diagnostic counts as a failure. A run that aborted on a raw constraint violation
    // did not pass, whatever the absence of a FAIL marker suggests.
    fail: errorLines,
    failMarkers,
    errorLines,
    schemaErrors,
    distinct: new Set(ids).size,
    ids: [...new Set(ids)].sort((a, b) => Number(a.slice(1)) - Number(b.slice(1))),
    aborted: errorLines > 0,
  };
}

function countMatches(text, re) {
  let n = 0;
  for (const line of text.split(/\r?\n/)) if (re.test(line)) n++;
  return n;
}

/**
 * The line tools/accept.mjs parses. Its shape is a contract:
 *   /TALLY pass=(\d+) fail=(\d+) .*distinct_ids=(\d+)/
 * Keep the field order and the literal names.
 */
export function tallyLine(t) {
  return `TALLY pass=${t.pass} fail=${t.fail} error_lines=${t.errorLines} ` +
    `fail_markers=${t.failMarkers} schema_error_lines=${t.schemaErrors} distinct_ids=${t.distinct}`;
}

// ---------------------------------------------------------------------------------------
// CLI: node db/tools/tally-log.mjs <testsLog> [schemaLog]   -> JSON on stdout
// ---------------------------------------------------------------------------------------

import { readFileSync, realpathSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const isMain = (() => {
  if (!process.argv[1]) return false;
  try {
    return realpathSync(process.argv[1]) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
})();

if (isMain) {
  const [testsPath, schemaPath] = process.argv.slice(2);
  if (!testsPath) {
    console.error('usage: node tally-log.mjs <testsLog> [schemaLog]');
    process.exit(2);
  }
  const read = (p) => { try { return readFileSync(p, 'utf8'); } catch { return ''; } };
  const t = tallyLogs({ tests: read(testsPath), schema: schemaPath ? read(schemaPath) : '' });
  process.stdout.write(JSON.stringify(t) + '\n');
  process.stdout.write(tallyLine(t) + '\n');
}
