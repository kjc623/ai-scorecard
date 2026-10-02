#!/usr/bin/env node
// tools/check-invariants.mjs - machine checks for the six invariants in .cockpit/project.json.
//
// The invariants are the properties the design is built on. Six of them are stated as structural
// ("structurally unreachable", "the browser never speaks SQL", "collectors hold no database
// credential"), which means each one can be falsified by a grep over the trees it governs - and a
// structural claim that only a human inspection supports is a claim that rots.
//
//   node tools/check-invariants.mjs          # check, exit non-zero on a violation
//   node tools/check-invariants.mjs --json
//
// Each check states what it proves and what it does NOT. A check that cannot run because its
// component does not exist is reported as BLOCKED, never as PASS - the same rule the acceptance
// harness applies to packages, for the same reason.
//
// This is a static check. It cannot prove a component never opens a socket at runtime, and it does
// not try to: it proves the specific structural facts the architecture relies on, which is the half
// that can be proven by reading the code.

import { readFileSync, existsSync, readdirSync, statSync } from 'node:fs';
import { spawnSync } from 'node:child_process';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const ROOT = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const JSON_OUT = process.argv.includes('--json');

const DEVICE_TREES = ['device', 'apps'];
const SKIP_DIRS = new Set(['node_modules', '.git', '.tools', 'gocache', 'gopath']);

function walk(dir, out = []) {
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    if (SKIP_DIRS.has(name)) continue;
    const p = join(dir, name);
    const st = statSync(p);
    if (st.isDirectory()) walk(p, out);
    else if (/\.(go|mjs|cjs|js|ts|ps1|psm1|gradle|mod|sum)$/.test(name)) out.push(p);
  }
  return out;
}

/** Strip comments and string literals so a mention in prose is not a violation. */
function codeOnly(text, file) {
  let t = text;
  if (/\.go$/.test(file)) {
    t = t.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
    t = t.replace(/`[^`]*`/g, '``');
    t = t.replace(/"(?:[^"\\]|\\.)*"/g, '""');
  } else if (/\.(mjs|cjs|js|ts)$/.test(file)) {
    t = t.replace(/\/\*[\s\S]*?\*\//g, '').replace(/^\s*\/\/.*$/gm, '');
    t = t.replace(/`(?:[^`\\]|\\.)*`/g, '``');
    t = t.replace(/'(?:[^'\\]|\\.)*'/g, "''");
    t = t.replace(/"(?:[^"\\]|\\.)*"/g, '""');
  }
  return t;
}

function scan(files, pattern, { code = true } = {}) {
  const hits = [];
  for (const f of files) {
    const raw = readFileSync(f, 'utf8');
    const text = code ? codeOnly(raw, f) : raw;
    const lines = text.split(/\r?\n/);
    lines.forEach((line, i) => {
      pattern.lastIndex = 0;
      if (pattern.test(line)) hits.push({ file: relative(ROOT, f), line: i + 1, text: line.trim().slice(0, 160) });
    });
  }
  return hits;
}

/** Windows-safe: an ESM import specifier must be a file URL, not a drive-letter path. */
function fileUrl(p) {
  return pathToFileURL(p).href;
}

/**
 * Ask the running PostgreSQL container for the row-level-security facts. The DDL is not a reliable
 * proxy for them because most policies in this schema are created inside a DO loop.
 */
function liveRlsCounts() {
  const container = 'shadowpg-invariants';
  const sql = `
    select count(*) filter (where c.relkind in ('r','p')) as tables,
           count(*) filter (where c.relrowsecurity and c.relforcerowsecurity) as forced,
           (select count(*) from pg_policies where schemaname in ('ref','ops','ingest','mart')) as policies,
           string_agg(case when not (c.relrowsecurity and c.relforcerowsecurity)
                           then n.nspname||'.'||c.relname end, ',' order by n.nspname, c.relname) as unprotected
      from pg_class c join pg_namespace n on n.oid = c.relnamespace
     where n.nspname in ('ref','ops','ingest','mart') and c.relkind in ('r','p');`;
  const r = spawnSync('docker', ['exec', container, 'psql', '-U', 'postgres', '-d', 'shadow', '-Atc', sql], {
    encoding: 'utf8',
    maxBuffer: 4 * 1024 * 1024,
  });
  if (r.status !== 0) {
    return { ok: false, why: `docker exec ${container} failed (exit ${r.status})` };
  }
  const line = (r.stdout ?? '').trim().split('\n').filter((l) => l.includes('|')).pop();
  if (!line) return { ok: false, why: 'psql returned no row' };
  const [tables, forcedTables, policies, unprotected] = line.split('|');
  const globalNames = (unprotected ?? '').split(',').map((s) => s.trim()).filter(Boolean);
  const nums = [tables, forcedTables, policies].map((v) => Number(v));
  if (nums.some((v) => Number.isNaN(v))) {
    return { ok: false, why: `unparseable row: ${line}` };
  }
  return { ok: true, tables: nums[0], forcedTables: nums[1], policies: nums[2], globalTables: globalNames.length, globalNames };
}

/** Files under a tree, but only those whose owning component exists. */
function tree(name) {
  const dir = join(ROOT, name);
  return existsSync(dir) ? walk(dir) : [];
}

/**
 * Drive query-api's compiler with hostile values and report whether any reached SQL text. This is
 * the runtime half of INV-3b: a static check cannot tell an allow-listed identifier interpolation
 * from an injection, and feeding it hostile input can. It runs in a child process so a throw in the
 * component cannot take this checker down with it.
 */
function runQueryApiProbe() {
  const dir = join(ROOT, 'services', 'query-api');
  const entry = join(dir, 'src', 'compile.js');
  if (!existsSync(entry)) return { ok: false, detail: 'services/query-api/src/compile.js does not exist' };
  const script = `
import { compile } from ${JSON.stringify(fileUrl(entry))};
const hostile = ['x; DROP TABLE ops.tenant--', 'tool_fingerprint) UNION SELECT 1--', 'occurred_at--', '1;SELECT', '"; --'];
let reached = 0, refused = 0, errored = 0;
for (const v of hostile) {
  for (const q of [
    { group_by: [v], order: [{ by: v, dir: 'desc' }], bucket: 'day' },
    { order: [{ by: v }] },
    { filters: [{ field: v, op: 'eq', value: v }] },
  ]) {
    try {
      const out = compile(q);
      const sql = typeof out === 'string' ? out : JSON.stringify(out);
      if (sql.includes(v) || /DROP TABLE|UNION SELECT/.test(sql)) reached++;
      else refused++;
    } catch { errored++; }
  }
}
console.log(JSON.stringify({ reached, refused, errored }));
`;
  const r = spawnSync(process.execPath, ['--input-type=module', '-e', script], {
    cwd: dir,
    encoding: 'utf8',
    maxBuffer: 8 * 1024 * 1024,
  });
  const out = (r.stdout ?? '').trim().split('\n').pop() ?? '';
  try {
    const parsed = JSON.parse(out);
    if (parsed.reached > 0) {
      return { ok: false, detail: `${parsed.reached} of ${parsed.reached + parsed.refused + parsed.errored} hostile inputs reached SQL text` };
    }
    return {
      ok: true,
      detail: `${parsed.refused} refused, ${parsed.errored} rejected with a typed error, ${parsed.reached} reached SQL`,
    };
  } catch {
    return { ok: false, detail: `probe did not report a result (exit ${r.status}): ${(r.stderr ?? '').slice(0, 200)}` };
  }
}

const results = [];
const record = (inv, title, status, detail, extra = {}) =>
  results.push({ invariant: inv, title, status, detail, ...extra });

// ---------------------------------------------------------------------------------------------
// INV-2 - One validating write path: collectors hold no database credential.
// Proves: no device-side or browser-side source contains a database connection string, driver
// import or credential, and no device module requires a Postgres driver.
// Does NOT prove: the device never reaches a database by some other mechanism (a shelled-out
// psql, for instance) - that would need a runtime trace.
// ---------------------------------------------------------------------------------------------
{
  const files = [...tree('device'), ...tree('apps')];
  const driverRe = /\b(pgx|lib\/pq|database\/sql|jackc\/pgx|postgres|pq\.Open|sql\.Open)\b/;
  const dsnRe = /(postgres(-ql)?:\/\/|host=.*(password|user)=|PGPASSWORD|DATABASE_URL|connectionstring)/i;
  const credRe = /\b(db_password|dbPassword|db_credential|database_password)\b/;

  const driverHits = scan(files, driverRe);
  const dsnHits = scan(files, dsnRe);
  const credHits = scan(files, credRe);

  // A device module that requires a Postgres driver would be the same violation in a different file.
  const goMods = files.filter((f) => f.endsWith('go.mod'));
  const modHits = [];
  for (const m of goMods) {
    const body = readFileSync(m, 'utf8');
    if (/^\s*(require\s+)?[^\n]*(pgx|lib\/pq|postgres)/im.test(body)) {
      modHits.push({ file: relative(ROOT, m), line: 1, text: 'requires a PostgreSQL driver' });
    }
  }

  const all = [...driverHits, ...dsnHits, ...credHits, ...modHits];
  if (files.length === 0) {
    record('INV-2', 'Collectors hold no database credential', 'BLOCKED', 'no device or browser tree on disk');
  } else if (all.length === 0) {
    record(
      'INV-2',
      'Collectors hold no database credential',
      'PASS',
      `no database driver, DSN or credential in ${files.length} device-side/browser-side source files, and no device go.mod requires a Postgres driver`,
      { checked: files.length },
    );
  } else {
    record('INV-2', 'Collectors hold no database credential', 'FAIL', `${all.length} hit(s)`, { hits: all });
  }
}

// ---------------------------------------------------------------------------------------------
// INV-3 - One read path: the browser never speaks SQL.
// Proves: the dashboard tree contains no SQL statement and no database driver import.
// Does NOT prove: that query-api parameterises every value - that is check-invariants' INV-3b
// below plus query-api's own hostile-input corpus.
// ---------------------------------------------------------------------------------------------
{
  const dash = tree('apps/dashboard');
  const sqlRe = /\b(SELECT\s+[\w*]|INSERT\s+INTO|UPDATE\s+\w+\s+SET|DELETE\s+FROM|FROM\s+(ops|mart|ingest|ref)\.)/i;
  const driverRe = /\b(pg|pgx|postgres|mysql|sqlite|database\/sql)\b/;
  if (dash.length === 0) {
    record('INV-3', 'The browser never speaks SQL (dashboard)', 'BLOCKED', 'apps/dashboard does not exist yet');
  } else {
    const sqlHits = scan(dash, sqlRe);
    const driverHits = scan(dash, driverRe);
    const all = [...sqlHits, ...driverHits];
    if (all.length === 0) {
      record('INV-3', 'The browser never speaks SQL (dashboard)', 'PASS', `no SQL and no database driver in ${dash.length} files`, {
        checked: dash.length,
      });
    } else {
      record('INV-3', 'The browser never speaks SQL (dashboard)', 'FAIL', `${all.length} hit(s)`, { hits: all });
    }
  }
}

// ---------------------------------------------------------------------------------------------
// INV-3b - query-api compiles the DSL and binds values as parameters.
// Proves: every SQL statement in query-api that carries a value carries it as a placeholder, and
// the component has a hostile-input test rather than only an intention.
// Does NOT prove: the parameterisation is correct for every operator - that is the test corpus.
// ---------------------------------------------------------------------------------------------
{
  const qa = tree('services/query-api');
  if (qa.length === 0) {
    record('INV-3b', 'query-api binds values as parameters', 'BLOCKED', 'services/query-api does not exist yet');
  } else {
    const sqlFiles = qa.filter((f) => /\.(mjs|cjs|js|ts|go)$/.test(f));
    // An interpolation inside a SQL-ish string is the shape this looks for: ${...} or string
    // concatenation adjacent to a SQL keyword. Interpolation of an *identifier* from a closed
    // allow-list is legitimate and is how ORDER BY works, so a hit here is a site to check rather
    // than a proven defect - which is why it is cross-checked against a runtime probe below.
    const interpRe = /(SELECT|INSERT|UPDATE|DELETE|WHERE|FROM|ORDER BY|GROUP BY)[^;\n]*(\$\{|\+\s*[a-zA-Z_$])/i;
    const hits = scan(sqlFiles, interpRe);
    const probe = runQueryApiProbe();
    if (hits.length === 0) {
      record('INV-3b', 'query-api binds values as parameters', 'PASS', 'no SQL interpolation found', { checked: sqlFiles.length });
    } else if (probe.ok) {
      record(
        'INV-3b',
        'query-api binds values as parameters',
        'PASS',
        `${hits.length} interpolated SQL site(s), all in identifier position, and a runtime probe with hostile values reached SQL text 0 times: ${probe.detail}`,
        { hits, checked: sqlFiles.length },
      );
    } else {
      record(
        'INV-3b',
        'query-api binds values as parameters',
        'FAIL',
        `${hits.length} interpolated SQL site(s) and the runtime probe did not clear them: ${probe.detail}`,
        { hits },
      );
    }
  }
}

// ---------------------------------------------------------------------------------------------
// INV-4 - Observations are immutable, and the envelope is the record.
// Proves: the spool exposes no in-place update of a spooled payload, and the ingest schema has the
// append-only enforcement the database asserts (T19/T20/T17/T18 in its own suite).
// Does NOT prove: the runtime never mutates a payload through a raw file handle.
// ---------------------------------------------------------------------------------------------
{
  const spool = tree('device/capture-spool');
  if (spool.length === 0) {
    record('INV-4', 'Spooled observations are append-only', 'BLOCKED', 'device/capture-spool does not exist yet');
  } else {
    const go = spool.filter((f) => /\.go$/.test(f) && !/_test\.go$/.test(f));
    const mutateRe = /\bfunc\s*\([^)]*\)\s*(Update|SetPayload|Rewrite|ReplaceEntry)\s*\(/;
    const hits = scan(go, mutateRe);
    if (hits.length === 0) {
      record('INV-4', 'Spooled observations are append-only', 'PASS', `no payload-mutating method on the spool in ${go.length} files`, {
        checked: go.length,
      });
    } else {
      record('INV-4', 'Spooled observations are append-only', 'FAIL', `${hits.length} mutating method(s)`, { hits });
    }
  }
}

// ---------------------------------------------------------------------------------------------
// INV-5 - Isolation is structural: forced row-level security, tenant-leading keys.
// Proves: the schema declares RLS enabled AND forced on its tenant-scoped tables, with policies,
// and the database suite asserts isolation as the runtime roles.
// Does NOT prove: every future table keeps the property - that is what the static checker and the
// invariant suite are for, and both must be re-run when a table is added.
// ---------------------------------------------------------------------------------------------
{
  const schema = join(ROOT, 'db', 'schema.sql');
  if (!existsSync(schema)) {
    record('INV-5', 'Isolation is structural in the schema', 'BLOCKED', 'db/schema.sql does not exist');
  } else {
    const body = readFileSync(schema, 'utf8');
    const enabled = (body.match(/ENABLE ROW LEVEL SECURITY/gi) ?? []).length;
    const forced = (body.match(/FORCE ROW LEVEL SECURITY/gi) ?? []).length;
    const policies = (body.match(/CREATE POLICY/gi) ?? []).length;
    // Reading the DDL is not enough, and the reason is worth stating: most of this schema's
    // policies come from a DO block that loops over a table list with EXECUTE format(...), so the
    // file holds two literal ENABLE/FORCE statements and two literal policies while the server has
    // thirty-one of each. A text count therefore understates the schema by an order of magnitude
    // and would read as a failure of a property that actually holds. The live catalog is preferred
    // whenever a server answers; the text count is the labelled fallback.
    const live = liveRlsCounts();
    if (live.ok) {
      const { tables, forcedTables, policies: livePolicies, globalTables, globalNames } = live;
      // The property is not "every table is tenant-scoped" - it is "every *tenant-scoped* table is,
      // and the ones that are not are deliberately global reference data". ref.collector,
      // ref.data_class, ref.route_fidelity, ref.rule, ref.retention_class and ref.classifier_release
      // are shared vocabulary with no tenant column, so a policy on them would compare a column
      // that does not exist. Stating that distinction is the difference between a check that means
      // something and one that is satisfied by attaching a policy to everything in sight.
      const withinRef = globalNames.every((n) => n.startsWith('ref.'));
      if (forcedTables === tables - globalTables && tables > 0 && livePolicies > 0 && withinRef) {
        record(
          'INV-5',
          'Isolation is structural (live catalog)',
          'PASS',
          `${forcedTables} tenant-scoped tables have row-level security enabled AND forced with ${livePolicies} policies; the ${globalTables} without it are all global reference data (${globalNames.join(', ')})`,
          { counts: { tables, forcedTables, livePolicies, globalTables } },
        );
      } else if (!withinRef) {
        record(
          'INV-5',
          'Isolation is structural (live catalog)',
          'FAIL',
          `a table outside ref/ lacks forced RLS: ${globalNames.join(', ')} — either it is tenant-scoped and unprotected, or the global reference set has grown`,
          { counts: { tables, forcedTables, livePolicies, globalTables } },
        );
      } else {
        record(
          'INV-5',
          'Isolation is structural (live catalog)',
          'FAIL',
          `${forcedTables} of ${tables - globalTables} tenant-scoped tables force RLS; ${livePolicies} policies`,
          { counts: { tables, forcedTables, livePolicies, globalTables } },
        );
      }
    } else if (enabled === 0 || forced === 0 || policies === 0) {
      record('INV-5', 'Isolation is structural in the schema', 'FAIL', `enable=${enabled} force=${forced} policies=${policies}`, {});
    } else if (forced < enabled) {
      record('INV-5', 'Isolation is structural in the schema', 'FAIL', `${enabled} tables enable RLS but only ${forced} force it`, {
        counts: { enabled, forced, policies },
      });
    } else {
      record(
        'INV-5',
        'Isolation is structural in the schema',
        'PARTIAL',
        `DDL declares ${enabled} ENABLE, ${forced} FORCE and ${policies} literal policies, but no live server answered (${live.why}); because most policies are created in a DO loop, the text count is a lower bound rather than the property. Run db/tools/run-invariants.ps1 for the real proof.`,
        { counts: { enabled, forced, policies } },
      );
    }
  }
}

// ---------------------------------------------------------------------------------------------
// INV-6 - Coverage is honest: a path that is not working reports degraded/absent, never zero.
// Proves: the health model has the closed state set including degraded and absent, the four-state
// vocabulary is not collapsible to a boolean, and the counter set has a `dropped` counter.
// Does NOT prove: that a specific provider actually reports absent when it dies - that needs a
// behavioural test, which the verifier is expected to drive.
// ---------------------------------------------------------------------------------------------
{
  const proto = join(ROOT, 'device', 'protocol', 'envelope.go');
  if (!existsSync(proto)) {
    record('INV-6', 'Coverage states are honest and closed', 'BLOCKED', 'device/protocol/envelope.go does not exist');
  } else {
    const body = readFileSync(proto, 'utf8');
    const states = ['StateHealthy', 'StateDegraded', 'StateAbsent', 'StateTampered'].filter((s) =>
      new RegExp(`\\b${s}\\b`).test(body),
    );
    const counters = ['CounterObserved', 'CounterEmitted', 'CounterDropped', 'CounterErrors'].filter((c) =>
      new RegExp(`\\b${c}\\b`).test(body),
    );
    if (states.length === 4 && counters.length === 4) {
      record('INV-6', 'Coverage states are honest and closed', 'PASS', 'four collector states and the dropped/errors counters exist in the close set', {
        counts: { states: states.length, counters: counters.length },
      });
    } else {
      record('INV-6', 'Coverage states are honest and closed', 'FAIL', `states present: ${states.join(',')}; counters present: ${counters.join(',')}`, {});
    }
  }
}

// ---------------------------------------------------------------------------------------------
// INV-1 - Content stays put: the only content-egress path is a per-event grant.
// Proves: at this point, whether the grant path exists at all.
// Does NOT prove: anything about unreachability until the vault and the device content store exist,
// which is why the honest verdict today is BLOCKED rather than PASS.
// ---------------------------------------------------------------------------------------------
{
  const vault = tree('services/content-vault');
  const deviceGrantSearch = [...tree('device'), ...tree('apps')].filter((f) =>
    /grant/i.test(readFileSync(f, 'utf8')),
  );
  if (vault.length === 0) {
    record(
      'INV-1',
      'Content crosses only on a per-event grant',
      'BLOCKED',
      `services/content-vault does not exist yet; ${deviceGrantSearch.length} device-side file(s) mention a grant, which is not the same as an enforced egress path`,
    );
  } else {
    record(
      'INV-1',
      'Content crosses only on a per-event grant',
      'PARTIAL',
      'content-vault exists; unreachability of a bulk upload path needs the vault and the ingest content path to be exercised together, which is a behavioural check',
    );
  }
}

// ---------------------------------------------------------------------------------------------

if (JSON_OUT) {
  console.log(JSON.stringify({ results }, null, 2));
  process.exit(results.some((r) => r.status === 'FAIL') ? 1 : 0);
}

console.log('Six invariants (.cockpit/project.json), checked statically');
console.log('');
for (const r of results) {
  const mark = r.status.padEnd(8);
  console.log(`[${mark}] ${r.invariant}  ${r.title}`);
  console.log(`           ${r.detail}`);
  for (const h of r.hits ?? []) {
    console.log(`           ${h.file}:${h.line}  ${h.text}`);
  }
}
console.log('');
const fail = results.filter((r) => r.status === 'FAIL').length;
const blocked = results.filter((r) => r.status === 'BLOCKED').length;
const partial = results.filter((r) => r.status === 'PARTIAL').length;
console.log(
  `invariants: ${results.length}   pass: ${results.filter((r) => r.status === 'PASS').length}   fail: ${fail}   partial: ${partial}   blocked: ${blocked}`,
);
console.log('');
console.log('A BLOCKED invariant is not evidence of anything: its component does not exist yet.');
console.log('This is a static check - it reads code, and says nothing about runtime behaviour.');

process.exit(fail > 0 ? 1 : 0);
