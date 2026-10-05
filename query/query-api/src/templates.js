// templates.js — §3's ten questions as named templates over mart/ops.
//
// "GraphQL over the event tables. Rejected: unbounded query shape … and the ten questions are
// known in advance, so the flexibility buys nothing" (ADR 0003). A template is therefore sugar
// over the closed DSL: it fills in the source, the dimensions, the measures and the ordering —
// the parts the document fixes — and the caller supplies only the values it is allowed to
// choose (a window, a bucket, a filter from the template's own allowed set, a page size).
//
// A template never compiles to SQL of its own. `expand()` returns a DSL document, which goes
// through the same validate -> guard -> compile path as a hand-written document, so there is
// exactly one place that can put text into a statement.

import { BUCKETS } from './registry.js';
import { REASON, unsupported } from './errors.js';
import {
  classTotalStatement,
  deviceStatusStatement,
  erasureEvidenceStatement,
  flushCheckStatement,
  orgCoverageStatement,
  submissionDetailStatement,
} from './blocks.js';

const ISO_UTC = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d{1,6})?Z$/;
const DATE_ONLY = /^\d{4}-\d{2}-\d{2}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function str(params, name) {
  const value = params[name];
  if (typeof value !== 'string' || value.length === 0) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, `Template parameter "${name}" must be a non-empty string.`, { parameter: name });
  }
  return value;
}

function optionalStr(params, name) {
  const value = params[name];
  if (value === undefined || value === null) return undefined;
  return str(params, name);
}

function oneOf(params, name, allowed) {
  const value = params[name];
  if (value === undefined || value === null) return undefined;
  if (!allowed.includes(value)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, `Template parameter "${name}" must be one of: ${allowed.join(', ')}.`, {
      parameter: name,
      allowed,
      received: value,
    });
  }
  return value;
}

function windowOf(params, { required = true } = {}) {
  const value = params.window;
  if (value === undefined || value === null) {
    if (!required) return undefined;
    throw unsupported(REASON.MISSING_WINDOW, 'A window is required: no read in this DSL is unbounded.', {
      example: { from: '2026-09-01T00:00:00Z', to: '2026-10-01T00:00:00Z' },
    });
  }
  if (typeof value !== 'object' || Array.isArray(value)) {
    throw unsupported(REASON.BAD_WINDOW, 'window must be an object with from and to.', {});
  }
  const { from, to } = value;
  if (typeof from !== 'string' || !ISO_UTC.test(from) || typeof to !== 'string' || !ISO_UTC.test(to)) {
    throw unsupported(REASON.BAD_WINDOW, 'window.from and window.to must be ISO-8601 UTC instants; a bare date is not a window.', { from, to });
  }
  return { from, to };
}

function bucketOf(params, fallback) {
  const value = params.bucket;
  if (value === undefined || value === null) return fallback;
  if (!BUCKETS.includes(value)) {
    throw unsupported(REASON.UNKNOWN_BUCKET, `Unknown bucket "${String(value)}".`, { bucket: value, known_buckets: BUCKETS });
  }
  return value;
}

function limitOf(params, fallback, max) {
  const value = params.limit;
  if (value === undefined || value === null) return fallback;
  if (!Number.isInteger(value) || value < 1) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'limit must be a positive integer.', { limit: value });
  }
  if (value > max) {
    throw unsupported(REASON.COST_ESTIMATE_EXCEEDED, `limit ${value} exceeds this template's ${max}-row cap.`, { max_limit: max, requested: value });
  }
  return value;
}

function cursorOf(params) {
  const value = params.cursor;
  if (value === undefined || value === null) return undefined;
  if (typeof value !== 'string') throw unsupported(REASON.MALFORMED_DOCUMENT, 'cursor must be a string.', {});
  return value;
}

function eq(field, value, op = 'eq') {
  return value === undefined ? null : { field, op, value };
}

function filtersOf(entries) {
  return entries.filter(Boolean);
}

/**
 * @typedef {object} Template
 * @property {string} name
 * @property {number} question  brief §3.6 question number
 * @property {string} title
 * @property {string} source
 * @property {'aggregate'|'list'|'single'} kind
 * @property {ReadonlyArray<string>} params
 * @property {(params:object)=>{document:object, extras?:ReadonlyArray<Function>}} build
 * @property {ReadonlyArray<string>} notes
 */

/** @type {Record<string, Template>} */
export const TEMPLATES = Object.freeze({
  q1_tools_ranked: {
    name: 'q1_tools_ranked',
    question: 1,
    title: 'Which AI tools are in use, ranked, over time?',
    source: 'mart.v_tool_usage',
    kind: 'aggregate',
    params: ['window', 'bucket', 'limit', 'tool', 'sanctioned_state'],
    build(params) {
      return {
        document: {
          query_version: '1',
          source: 'mart.v_tool_usage',
          bucket: bucketOf(params, 'day'),
          dimensions: ['tool'],
          measures: ['submissions', 'users', 'bytes_total', 'blocked', 'warned', 'logged'],
          filters: filtersOf([
            eq('tool', optionalStr(params, 'tool')),
            eq('sanctioned_state', oneOf(params, 'sanctioned_state', ['sanctioned', 'unsanctioned', 'unknown'])),
          ]),
          window: windowOf(params),
          order: [{ by: 'submissions', dir: 'desc' }],
          limit: limitOf(params, 400, 2000),
        },
        // A tool with no ops.tool row still appears (the view LEFT JOINs), reporting
        // sanctioned_state NULL, which the response must render as `unknown` and never as
        // `unsanctioned` (C8, docs/04 §3.1).
        notes: [
          'Rank is by submissions with a deterministic tie-break on tool, and is never a sanction signal (§3.1).',
          'LIMIT applies to a bucket-major ordering, so it truncates whole buckets rather than returning a top-N per bucket.',
        ],
      };
    },
  },

  q2_unsanctioned_users: {
    name: 'q2_unsanctioned_users',
    question: 2,
    title: 'Which tools are unsanctioned, and who is using them?',
    source: 'mart.agg_tool_user_period',
    kind: 'aggregate',
    params: ['window', 'bucket', 'limit', 'tool', 'subject', 'sanctioned_state'],
    build(params) {
      // The question is "which are unsanctioned": the state filter is part of the question, not an
      // optional narrowing. `sanctioned_state` may name another state (e.g. `unknown` gets its own
      // list, docs/04 §3.2), but omitting it answers the template's own question.
      const sanctionState = oneOf(params, 'sanctioned_state', ['sanctioned', 'unsanctioned', 'unknown']) ?? 'unsanctioned';
      return {
        document: {
          query_version: '1',
          source: 'mart.agg_tool_user_period',
          bucket: bucketOf(params, 'day'),
          dimensions: ['tool', 'subject'],
          measures: ['submissions', 'bytes_total'],
          filters: filtersOf([
            eq('tool', optionalStr(params, 'tool')),
            eq('subject', optionalStr(params, 'subject')),
            eq('sanctioned_state', sanctionState),
          ]),
          window: windowOf(params),
          // A list of people, ordered by tool and then by person — never by volume. A
          // submissions-desc ordering would be the leaderboard the product forbids (docs/04 §11.2,
          // brief §1.2); this is the cursor docs/04 §3.2 names.
          order: [{ by: 'tool', dir: 'asc' }, { by: 'subject', dir: 'asc' }],
          limit: limitOf(params, 500, 500),
        },
        notes: [
          'Subject-bearing: this read is audited as it is served, and its cursor is server-side because the ordering key contains a subject reference (§7.2).',
          'An unscoped window beyond 7 days is refused by the cost guard, naming the narrowing that would make it servable (§3.2).',
          'Three states, not two: `unsanctioned`, `unknown` and `sanctioned` are separate answers, and `unknown` gets its own count and list (§3.2). The default here is `unsanctioned`, the template\'s own question.',
          'The unsanctioned tool set is resolved from the tool fingerprint joined to ops.tool at read time (mart.agg_tool_user_period ⋈ ops.tool); its display name comes from ref.tool_catalogue. Both precomputed, nothing scans raw events.',
          'The k-suppression cell is the (bucket, tool) group, not the person: a tool used by fewer than k people is suppressed, and a tool with enough people publishes the rows that name them (§3.2, §6.4).',
          'Ordered by tool and then by person, never by volume: this is a list, not a leaderboard (§11.2).',
        ],
      };
    },
  },

  q3_team_growth: {
    name: 'q3_team_growth',
    question: 3,
    title: 'How much is usage growing, per team?',
    source: 'mart.agg_org_period',
    kind: 'aggregate',
    params: ['window', 'bucket', 'limit', 'department', 'population'],
    build(params) {
      const query = {
        query_version: '1',
        source: 'mart.agg_org_period',
        bucket: bucketOf(params, 'day'),
        dimensions: ['department'],
        measures: ['submissions', 'users'],
        filters: filtersOf([
          eq('department', optionalStr(params, 'department')),
          eq('population', optionalStr(params, 'population')),
        ]),
        window: windowOf(params),
        order: [{ by: 'submissions', dir: 'desc' }],
        limit: limitOf(params, 2000, 2000),
      };
      return {
        document: query,
        // §3.3: the unmapped residual and mapped_user_share travel with the per-team series, so
        // a per-team view cannot silently cover 60% of usage.
        extras: [(q) => orgCoverageStatement(q)],
        notes: [
          'Empty until the directory sync: the response is not_yet_covered with reason directory_not_synced, never a flat zero line (§3.3).',
          'Department cells below k distinct users are suppressed (§6); k-suppression does not apply to subject-scoped reads.',
        ],
      };
    },
  },

  q4_class_mix: {
    name: 'q4_class_mix',
    question: 4,
    title: 'What classes of sensitive data are going into AI?',
    source: 'mart.agg_class_period',
    kind: 'aggregate',
    params: ['window', 'bucket', 'limit', 'dimensions', 'class', 'severity'],
    build(params) {
      const allowed = ['class', 'tool', 'severity', 'classifier_version'];
      // Default grouping is the class mix a dashboard shows. Adding `tool` (or
      // `classifier_version`) is available but multiplies the cell count by T, which the cost
      // guard will refuse beyond the window it fits in — that is the guard doing its job, and
      // the error names the coarser bucket that would fit.
      let dimensions = ['class', 'severity'];
      if (params.dimensions !== undefined && params.dimensions !== null) {
        if (!Array.isArray(params.dimensions) || params.dimensions.length === 0 || params.dimensions.length > 3) {
          throw unsupported(REASON.TOO_MANY_DIMENSIONS, 'dimensions must be 1 to 3 entries from: ' + allowed.join(', ') + '.', {
            allowed,
            max_dimensions: 3,
          });
        }
        for (const name of params.dimensions) {
          if (!allowed.includes(name)) {
            throw unsupported(REASON.UNKNOWN_DIMENSION, `"${String(name)}" is not a dimension of the class mix.`, { allowed });
          }
        }
        dimensions = [...params.dimensions];
      }
      return {
        document: {
          query_version: '1',
          source: 'mart.agg_class_period',
          bucket: bucketOf(params, 'day'),
          dimensions,
          measures: ['submissions', 'users', 'max_score', 'degraded_events'],
          filters: filtersOf([
            eq('class', optionalStr(params, 'class')),
            eq('severity', optionalStr(params, 'severity')),
          ]),
          window: windowOf(params),
          order: [{ by: 'submissions', dir: 'desc' }],
          limit: limitOf(params, 2000, 2000),
        },
        // §3.4: the class rows fan out (one submission with three labels is in three rows), so
        // the non-additive total is computed once, separately, from mart.agg_tool_period.
        extras: [(q) => classTotalStatement(q)],
        notes: [
          'submissions counts submissions CARRYING that class. The total from mart.agg_tool_period is a different measure and is labelled as one (§3.4).',
          'Each cell carries `users`, so §6 applies per cell: a class appearing in one person\'s submissions is that person\'s data.',
          'degraded_events travels beside the class mix so a classifier outage cannot read as a fall in sensitive data (C21).',
          'Three grouping dimensions is the DSL cap, so `classifier_version` and `severity` cannot both be shown beside class and tool; the template takes `dimensions` to choose.',
        ],
      };
    },
  },

  q5_findings: {
    name: 'q5_findings',
    question: 5,
    title: 'Which specific submissions hit a policy rule?',
    source: 'mart.v_finding',
    kind: 'list',
    params: ['window', 'limit', 'cursor', 'severity', 'rule', 'review_state', 'subject', 'tool', 'class'],
    build(params) {
      return {
        document: {
          query_version: '1',
          source: 'mart.v_finding',
          filters: filtersOf([
            eq('severity', oneOf(params, 'severity', ['low', 'medium', 'high', 'critical'])),
            eq('rule', optionalStr(params, 'rule')),
            eq('review_state', oneOf(params, 'review_state', ['open', 'disputed', 'confirmed'])),
            eq('subject', optionalStr(params, 'subject')),
            eq('tool', optionalStr(params, 'tool')),
            eq('class', optionalStr(params, 'class')),
          ]),
          window: windowOf(params),
          limit: limitOf(params, 50, 500),
          cursor: cursorOf(params),
        },
        notes: [
          'Review state is never defaulted silently: `open` means nobody has looked, not "reviewed and unremarkable" (§3.5).',
          'Severity and class are present-tense: they are read from the current ref.rule row, so a rule edit shows on every finding that names it.',
        ],
      };
    },
  },

  q6_subject_series: {
    name: 'q6_subject_series',
    question: 6,
    title: 'Has a given person\'s usage changed, or spiked?',
    source: 'mart.agg_user_period',
    kind: 'aggregate',
    params: ['subject', 'window', 'bucket', 'limit'],
    build(params) {
      const subject = str(params, 'subject');
      const query = {
        query_version: '1',
        source: 'mart.agg_user_period',
        bucket: bucketOf(params, 'day'),
        dimensions: ['subject'],
        measures: ['submissions', 'bytes_total', 'tools_used', 'block_events'],
        filters: [{ field: 'subject', op: 'eq', value: subject }],
        window: windowOf(params),
        limit: limitOf(params, 400, 400),
      };
      return {
        document: query,
        // §3.6's late-flush check: both clocks live on ingest.submission, so the annotation is
        // one bounded read over the subject's own rows.
        extras: [(q) => flushCheckStatement(subject, q.window)],
        notes: [
          'Always audited: it filters on a subject and returns one, so there is no anonymous form of it (§3.6).',
          'k-suppression does not apply: this is an explicitly subject-scoped read (§6.4).',
          'Below 14 observed buckets the spike answer is not_yet_covered, not "no change" (§3.6).',
        ],
      };
    },
  },

  q7_devices: {
    name: 'q7_devices',
    question: 7,
    title: 'What is the state of every device\'s collection?',
    source: 'mart.v_device_liveness',
    kind: 'list',
    params: ['limit', 'cursor', 'liveness', 'collector_state', 'collector', 'device_os', 'managed_state', 'region'],
    build(params) {
      return {
        document: {
          query_version: '1',
          source: 'mart.v_device_liveness',
          filters: filtersOf([
            eq('liveness', oneOf(params, 'liveness', ['reporting', 'stale', 'never_reported', 'revoked'])),
            eq('collector_state', oneOf(params, 'collector_state', ['healthy', 'degraded', 'absent', 'tampered'])),
            eq('collector', optionalStr(params, 'collector')),
            eq('device_os', oneOf(params, 'device_os', ['windows', 'macos'])),
            eq('managed_state', oneOf(params, 'managed_state', ['managed', 'unmanaged', 'unknown'])),
            eq('region', optionalStr(params, 'region')),
          ]),
          // No window: this is current state, not an event stream. The list is still bounded —
          // by the cursor page cap and by the fleet itself (≤ 5,000 devices, §3.7).
          window: windowOf(params, { required: false }),
          limit: limitOf(params, 50, 500),
          cursor: cursorOf(params),
        },
        notes: [
          'Four liveness values stay distinct: `stale`, `never_reported`, `revoked` and `reporting` are four facts, and a revoked device is not a quiet one (§3.7).',
          'The denominator is the enrolled fleet and is stated, never implied (§3.7).',
          'docs/04 §3.7 names a three-way join with ops.coverage_snapshot; that table is read as its own source because a per-day snapshot would multiply device rows by the number of days in the window.',
        ],
        // Fleet-wide counts by liveness, so the Devices cards describe the same population as the
        // list's cursor page (docs/04 §3.7). Returned as meta.extras.device_status.
        extras: [() => deviceStatusStatement()],
      };
    },
  },

  q8_activity: {
    name: 'q8_activity',
    question: 8,
    title: 'What happened in this window, for this tool or person?',
    source: 'ingest.submission',
    kind: 'list',
    params: ['window', 'limit', 'cursor', 'subject', 'tool', 'device', 'class', 'content_state', 'action', 'mode', 'department'],
    build(params) {
      return {
        document: {
          query_version: '1',
          source: 'ingest.submission',
          filters: filtersOf([
            eq('subject', optionalStr(params, 'subject')),
            eq('tool', optionalStr(params, 'tool')),
            eq('device', optionalStr(params, 'device')),
            // The class filter is served by the jsonb_path_ops GIN on `labels` and takes eq only.
            eq('class', optionalStr(params, 'class')),
            eq('content_state', oneOf(params, 'content_state', ['not_captured', 'local_only', 'uploaded', 'shredded'])),
            eq('action', oneOf(params, 'action', ['blocked', 'warned', 'logged'])),
            eq('mode', oneOf(params, 'mode', ['m0', 'm1', 'm2', 'm3'])),
            eq('department', optionalStr(params, 'department')),
          ]),
          window: windowOf(params),
          limit: limitOf(params, 50, 500),
          cursor: cursorOf(params),
        },
        notes: [
          'Cursored only: no unbounded result set over the event table (C29). A short page is not the end of the results — only next_cursor: null is.',
          'Rows are ordered and windowed by received_at, the server clock; first_occurred_at and last_occurred_at travel beside it and are never normalised into it (C26).',
          'merge_confidence = low rows are returned, not hidden: the header carries "N of M rows are low-confidence merges" (R9).',
        ],
      };
    },
  },

  q9_event_detail: {
    name: 'q9_event_detail',
    question: 9,
    title: 'What exactly was sent?',
    source: 'ingest.submission',
    kind: 'single',
    params: ['submission_id', 'received_at_hint'],
    build(params) {
      const submissionId = str(params, 'submission_id');
      if (!UUID_RE.test(submissionId)) {
        throw unsupported(REASON.TYPE_MISMATCH, 'submission_id must be a canonical uuid.', {});
      }
      const hint = params.received_at_hint;
      if (hint !== undefined && hint !== null && (typeof hint !== 'string' || !ISO_UTC.test(hint))) {
        throw unsupported(REASON.TYPE_MISMATCH, 'received_at_hint must be an ISO-8601 UTC instant.', {});
      }
      return {
        document: null,
        // Step two of §3.9's search-then-retrieve: the row, its observations (one per route, so
        // an overlapping-route count can be explained), its content_state and nothing else.
        statements: [
          () => submissionDetailStatement(submissionId),
          () => erasureEvidenceStatement(hint ?? null),
        ],
        singleRecord: true,
        notes: [
          'No query path returns full content (§8.3). content_state decides which of the four answers applies: not_captured, local_only, uploaded, shredded.',
          'A hit is a reference, not a reservation: if the record was destroyed between the search and this read, the answer is no_longer_available with the receipt (§3.9).',
        ],
      };
    },
  },

  q10_audit_trail: {
    name: 'q10_audit_trail',
    question: 10,
    title: 'What has been accessed, and by whom?',
    source: 'ops.audit',
    kind: 'list',
    params: ['window', 'limit', 'cursor', 'actor', 'action', 'object_type', 'subject', 'case'],
    build(params) {
      return {
        document: {
          query_version: '1',
          source: 'ops.audit',
          filters: filtersOf([
            eq('actor', optionalStr(params, 'actor')),
            eq('action', optionalStr(params, 'action')),
            eq('object_type', optionalStr(params, 'object_type')),
            eq('subject', optionalStr(params, 'subject')),
            eq('case', optionalStr(params, 'case')),
          ]),
          window: windowOf(params),
          limit: limitOf(params, 50, 500),
          cursor: cursorOf(params),
        },
        notes: [
          'Reading the log is itself a subject-level read: one audit row per query describing the filter and row count, and that row is not re-audited (§3.10).',
          'Hash links are verified within each returned page; a mismatch returns audit_chain_broken rather than a list that looks fine.',
        ],
      };
    },
  },
});

export const TEMPLATE_NAMES = Object.freeze(Object.keys(TEMPLATES).sort());

/**
 * Expand a template request into a DSL document plus its side reads.
 *
 * @param {{template:string, params?:object}} input
 * @returns {{name:string, question:number, title:string, document:object|null,
 *            statements:ReadonlyArray<Function>, notes:ReadonlyArray<string>, singleRecord:boolean}}
 */
export function expandTemplate(input) {
  if (typeof input.template !== 'string' || !TEMPLATES[input.template]) {
    throw unsupported(REASON.UNKNOWN_TEMPLATE, `Unknown template "${String(input.template)}".`, {
      template: input.template,
      known_templates: TEMPLATE_NAMES,
    });
  }
  const template = TEMPLATES[input.template];
  const params = input.params ?? {};
  if (params === null || typeof params !== 'object' || Array.isArray(params)) {
    throw unsupported(REASON.MALFORMED_DOCUMENT, 'params must be an object.', {});
  }
  for (const key of Object.keys(params)) {
    if (!template.params.includes(key)) {
      // Unknown keys are rejected, not ignored: a typo in a template parameter would otherwise
      // silently answer a broader question than the one asked (§2.3).
      throw unsupported(REASON.UNKNOWN_KEY, `Template "${template.name}" has no parameter "${key}".`, {
        template: template.name,
        key,
        known_params: template.params,
      });
    }
  }
  const built = template.build(params);
  return Object.freeze({
    name: template.name,
    question: template.question,
    title: template.title,
    source: template.source,
    kind: template.kind,
    document: built.document ?? null,
    statements: Object.freeze([...(built.statements ?? [])]),
    // `extras` are side reads that need the normalised query (its window, its bucket); plan.js
    // builds them once validation has produced it.
    extras: Object.freeze([...(built.extras ?? [])]),
    notes: Object.freeze([...(built.notes ?? [])]),
    params: Object.freeze({ ...params }),
    singleRecord: built.singleRecord ?? template.kind === 'single',
  });
}
