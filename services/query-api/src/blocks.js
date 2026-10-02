// blocks.js — the side reads that make an answer honest.
//
// Every statement here is parameterised the same way the compiled query is: identifiers come
// from this file's frozen text (or from registry.js), values are bound. None of them is callable
// with an identifier from a request.
//
// These are the reads the document requires *around* a number:
//
//   * ops.aggregate_watermark -> §4.5 freshness;
//   * ops.device + ops.coverage_snapshot -> §11.3 coverage, including the enrolled denominator;
//   * a bounded EXISTS -> §7.3 `newer_events_exist`;
//   * mart.agg_tool_period -> §3.4's non-additive total for the class screen;
//   * mart.agg_* -> §3.3's `unmapped` residual when the directory sync is partial;
//   * ingest.submission + ingest.observation -> §3.9 step two, and §3.6's late-flush check;
//   * ops.erasure_receipt -> §13's `not_found` vs `no_longer_available` decision.

import { NATIVE_BUCKETS } from './registry.js';
import { REASON, unsupported } from './errors.js';

/**
 * §4.5: each completed aggregation run upserts its watermark; this is how the read side knows how
 * current a number is. Absence of a row is `not_yet_covered`, not a zero.
 * @param {string} aggregateName
 * @param {string} bucketSize
 */
export function freshnessStatement(aggregateName, bucketSize = 'day') {
  return Object.freeze({
    id: 'freshness',
    text: [
      'SELECT w.aggregate_name, w.bucket_size, w.last_complete_bucket, w.last_run_at, w.last_run_rows,',
      '       EXTRACT(EPOCH FROM (now() - w.last_run_at))::bigint AS lag_seconds',
      '  FROM ops.aggregate_watermark w',
      ' WHERE w.tenant_id = ops.current_tenant()',
      '   AND w.aggregate_name = $1::text',
      '   AND w.bucket_size = $2::text',
    ].join('\n'),
    params: Object.freeze([aggregateName, bucketSize]),
  });
}

/**
 * §11.3 / §3.7: coverage over the ENROLLED fleet, with the gap reasons named. `unknown` is a
 * reason value in ops.coverage_snapshot's closed vocabulary, so it appears here as one rather
 * than as a blank.
 *
 * @param {{from:string, to:string}} window
 */
export function coverageStatement(window) {
  const fromDate = String(window.from).slice(0, 10);
  const toDate = String(window.to).slice(0, 10);
  return Object.freeze({
    id: 'coverage',
    text: [
      'SELECT',
      '  (SELECT count(*) FROM ops.device d',
      '    WHERE d.tenant_id = ops.current_tenant() AND d.revoked_at IS NULL) AS devices_enrolled,',
      '  (SELECT count(*) FROM ops.device d',
      '    WHERE d.tenant_id = ops.current_tenant() AND d.revoked_at IS NULL',
      "      AND d.last_seen_at > now() - interval '24 hours') AS devices_reporting,",
      '  (SELECT count(*) FROM ops.coverage_snapshot v',
      '    WHERE v.tenant_id = ops.current_tenant()',
      '      AND v.snapshot_day >= $1::date AND v.snapshot_day < $2::date',
      '      AND v.expected) AS expected_collector_days,',
      '  (SELECT count(*) FROM ops.coverage_snapshot v',
      '    WHERE v.tenant_id = ops.current_tenant()',
      '      AND v.snapshot_day >= $1::date AND v.snapshot_day < $2::date',
      '      AND v.observed) AS observed_collector_days,',
      '  (SELECT coalesce(jsonb_object_agg(g.gap_reason, g.n), \'{}\'::jsonb)',
      '     FROM (SELECT coalesce(v.gap_reason, \'unknown\') AS gap_reason, count(*) AS n',
      '             FROM ops.coverage_snapshot v',
      '            WHERE v.tenant_id = ops.current_tenant()',
      '              AND v.snapshot_day >= $1::date AND v.snapshot_day < $2::date',
      '              AND NOT v.observed',
      '            GROUP BY 1) g) AS gap_reasons',
    ].join('\n'),
    params: Object.freeze([fromDate, toDate]),
  });
}

/**
 * §7.3: "`newer_events_exist` (a bounded `EXISTS` on the same index) says rows have arrived since
 * the snapshot, so a consistent-but-stale page is not mistaken for the whole truth."
 *
 * @param {object} source a registry source with `time`
 * @param {string} upperIso the snapshot bound taken at the first page
 */
export function newerEventsStatement(source, upperIso) {
  if (!source.time) {
    throw unsupported(
      REASON.MISSING_WINDOW,
      `Source "${source.id}" has no event clock, so it has no "newer events" question to answer.`,
      { source: source.id },
    );
  }
  const column = source.time.sql;
  return Object.freeze({
    id: 'newer_events',
    text: `SELECT EXISTS (SELECT 1 FROM ${source.from} WHERE ${source.tenantColumn} = ops.current_tenant() AND ${column} > $1::timestamptz LIMIT 1) AS newer_events_exist`,
    params: Object.freeze([upperIso]),
  });
}

/**
 * §3.4: "The response carries the note and computes the non-additive total once for the same
 * window from `mart.agg_tool_period.submissions` — two measures, two numbers, both labelled."
 *
 * @param {{from:string,to:string,bucket:string|null}} query
 */
export function classTotalStatement(query) {
  const bucketSize = query.bucket && NATIVE_BUCKETS.includes(query.bucket) ? query.bucket : 'day';
  return Object.freeze({
    id: 'class_total',
    text: [
      'SELECT coalesce(sum(t.submissions), 0)::bigint AS submissions_total',
      '  FROM mart.agg_tool_period t',
      ' WHERE t.tenant_id = ops.current_tenant()',
      '   AND t.bucket_start >= $1::timestamptz AND t.bucket_start < $2::timestamptz',
      '   AND t.bucket_size = $3::text',
    ].join('\n'),
    params: Object.freeze([query.window.from, query.window.to, bucketSize]),
  });
}

/**
 * §3.3: "Users with `department IS NULL` are an explicit `unmapped` series, always present,
 * beside `mapped_user_share` for the window."
 *
 * `mart.agg_org_period.department` is NOT NULL, so an unmapped user is *absent* from it rather
 * than present with a NULL. The residual is therefore a difference of two sums over the same
 * window, and both numbers are returned so the reader can see the arithmetic rather than a
 * derived percentage with no provenance.
 *
 * @param {{from:string,to:string,bucket:string|null}} query
 */
export function orgCoverageStatement(query) {
  const bucketSize = query.bucket && NATIVE_BUCKETS.includes(query.bucket) ? query.bucket : 'day';
  return Object.freeze({
    id: 'org_coverage',
    text: [
      'SELECT',
      '  (SELECT coalesce(sum(t.users), 0)::bigint FROM mart.agg_tool_period t',
      '    WHERE t.tenant_id = ops.current_tenant()',
      '      AND t.bucket_start >= $1::timestamptz AND t.bucket_start < $2::timestamptz',
      '      AND t.bucket_size = $3::text) AS users_all,',
      '  (SELECT coalesce(sum(o.users), 0)::bigint FROM mart.agg_org_period o',
      '    WHERE o.tenant_id = ops.current_tenant()',
      '      AND o.bucket_start >= $1::timestamptz AND o.bucket_start < $2::timestamptz',
      '      AND o.bucket_size = $3::text) AS users_mapped,',
      '  (SELECT count(*)::bigint FROM mart.agg_org_period o',
      '    WHERE o.tenant_id = ops.current_tenant()',
      '      AND o.bucket_start >= $1::timestamptz AND o.bucket_start < $2::timestamptz',
      '      AND o.bucket_size = $3::text) AS org_rows',
    ].join('\n'),
    params: Object.freeze([query.window.from, query.window.to, bucketSize]),
  });
}

/**
 * §3.6: "Late flush: an offline device flushing spikes received time, not behaviour — both clocks
 * are on `ingest.submission`, so rows where `received_at - first_occurred_at` exceeds an hour
 * annotate the flag as a flush."
 *
 * @param {string} subject
 * @param {{from:string,to:string}} window
 */
export function flushCheckStatement(subject, window) {
  return Object.freeze({
    id: 'flush_check',
    text: [
      'SELECT count(*)::bigint AS rows_in_window,',
      "       count(*) FILTER (WHERE s.received_at - s.first_occurred_at > interval '1 hour')::bigint AS late_flush_rows",
      '  FROM ingest.submission s',
      ' WHERE s.tenant_id = ops.current_tenant()',
      '   AND s.user_ref = $1::text',
      '   AND s.received_at >= $2::timestamptz AND s.received_at < $3::timestamptz',
    ].join('\n'),
    params: Object.freeze([subject, window.from, window.to]),
  });
}

/**
 * §3.9 step two: one submission and its observations, one row per route, so an overlapping-route
 * count can be explained rather than merely defended. No content column is selected: this
 * component cannot see content, and `content_state` says which of the four answers applies.
 *
 * @param {string} submissionId
 */
export function submissionDetailStatement(submissionId) {
  return Object.freeze({
    id: 'submission_detail',
    text: [
      'SELECT s.submission_id, s.received_at, s.first_occurred_at, s.last_occurred_at,',
      '       s.user_ref, s.tool_fingerprint, s.kind, s.collection_mode, s.policy_action,',
      '       s.policy_rule_id, s.decided_locally, s.confidence, s.merge_confidence,',
      '       s.content_state, s.shredded_reason, s.size_bytes, s.content_digest, s.labels,',
      '       s.classifier_version, s.winning_source, s.winning_fidelity, s.observed_routes,',
      '       s.observation_count, s.expires_at,',
      '       o.event_id AS observation_event_id, o.source AS observation_source,',
      '       o.kind AS observation_kind, o.direction, o.occurred_at AS observation_occurred_at,',
      '       o.received_at AS observation_received_at, o.size_bytes AS observation_size_bytes,',
      '       o.labels AS observation_labels, o.policy_decision, o.detection_basis,',
      '       o.window_start, o.window_end, o.submission_count, o.bytes_total',
      '  FROM ingest.submission s',
      '  LEFT JOIN ingest.observation o',
      '    ON o.tenant_id = s.tenant_id AND o.dedup_key = s.dedup_key',
      ' WHERE s.tenant_id = ops.current_tenant()',
      '   AND s.submission_id = $1::uuid',
      ' ORDER BY o.received_at ASC, o.event_id ASC',
    ].join('\n'),
    params: Object.freeze([submissionId]),
  });
}

/**
 * §13's `not_found` rule: "if no retention run and no erasure receipt covers the window in which
 * the record would have been received, the answer is `not_found`; if one does, the answer is
 * `no_longer_available` with the reason and the receipt."
 *
 * The erasure half is answerable from `ops.erasure_receipt`. The retention half is NOT answerable
 * from `db/schema.sql` as it stands: there is no retention-run ledger, and without the row there
 * is no way to recover the expiry it would have carried. `resolveMissingRecord` therefore returns
 * `not_found` with `retention_evidence: 'no_ledger_in_schema'` rather than guessing, and DSL.md
 * records the gap.
 *
 * @param {string|null} receivedAtHint ISO instant the caller last saw the record, or null
 */
export function erasureEvidenceStatement(receivedAtHint) {
  return Object.freeze({
    id: 'erasure_evidence',
    text: [
      'SELECT r.receipt_id, r.scope_kind, r.subject_ref, r.completed_at, r.mechanisms, r.removed_counts',
      '  FROM ops.erasure_receipt r',
      ' WHERE r.tenant_id = ops.current_tenant()',
      '   AND r.completed_at IS NOT NULL',
      '   AND r.completed_at >= $1::timestamptz',
      ' ORDER BY r.completed_at DESC',
      ' LIMIT 5',
    ].join('\n'),
    params: Object.freeze([receivedAtHint ?? '1970-01-01T00:00:00Z']),
  });
}

/**
 * The `not_found` / `no_longer_available` decision of §13, as a pure function of the evidence.
 *
 * @param {object} input
 * @param {boolean} input.found
 * @param {string|null} input.receivedAtHint
 * @param {ReadonlyArray<object>} [input.receipts]
 * @returns {{result_state:string, http:number, reason?:string, receipt?:object, detail?:object}}
 */
export function resolveMissingRecord(input) {
  if (input.found) return Object.freeze({ result_state: 'ok' });
  if (!input.receivedAtHint) {
    return Object.freeze({
      result_state: 'not_found',
      http: 404,
      detail: {
        // Stated rather than hidden: with no window hint, "never existed" and "existed and was
        // purged" are indistinguishable from the id alone (it is a uuid, not a timestamp).
        purge_window_unknown: true,
      },
    });
  }
  const receipts = input.receipts ?? [];
  if (receipts.length > 0) {
    const receipt = receipts[0];
    return Object.freeze({
      result_state: 'no_longer_available',
      http: 410,
      reason: 'erasure',
      receipt: Object.freeze({
        receipt_id: receipt.receipt_id,
        scope_kind: receipt.scope_kind,
        completed_at: receipt.completed_at,
        mechanisms: receipt.mechanisms,
        removed_counts: receipt.removed_counts,
        remaining_counts: receipt.remaining_counts,
      }),
    });
  }
  return Object.freeze({
    result_state: 'not_found',
    http: 404,
    detail: { retention_evidence: 'no_ledger_in_schema' },
  });
}
