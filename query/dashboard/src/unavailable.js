// unavailable.js — what this dashboard cannot render from the documented API, stated in the UI.
//
// The Lead asked for exactly this: "If you find a state you cannot render because the API does not
// expose it, that is exactly what I want in your report — do not paper over it in the client."
//
// So it is not only in the report. A dashboard that silently omits a panel teaches its users that
// the panel has nothing to say, which is the same failure mode as rendering a blind spot as a
// zero. Every gap below is rendered as a row with the reason and the source that would fill it.
//
// This file is also the registry the §14 tests read: "no content search" and "no export links" are
// properties of this catalogue, not comments in a renderer.

/**
 * @typedef {object} Gap
 * @property {string} id
 * @property {string} screen      the information-architecture screen it belongs to
 * @property {string} title
 * @property {string} needs       what the API would have to expose
 * @property {string} consequence what a user cannot do today
 * @property {'absent'|'partial'} severity
 */

/** @type {ReadonlyArray<Gap>} */
export const GAPS = Object.freeze([
  Object.freeze({
    id: 'content-search',
    screen: 'search',
    title: 'Content search (Q9, first half)',
    needs: 'POST /v1/content-search, executed by content-vault over ingest.search_text, returning bounded snippets with an index-coverage block.',
    consequence: 'An analyst cannot search prompt text or attachment filenames from these screens: the DSL has no text predicate by design, so nothing here renders as "no matches". The Explore page does carry a prompt-text search, which goes to the content vault through its own endpoint; it returns snippets with no index-coverage block, and it does not search attachment filenames.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'content-retrieval',
    screen: 'event',
    title: 'Approved content retrieval (§8)',
    needs: 'Retrieval request, second approval, audit-first reveal, and a content-vault call.',
    consequence: 'Event detail on this screen shows metadata and the content_state answer, and a screenshot of it cannot be mistaken for one that revealed content. The Explore page carries the retrieval: its event panel takes a case reference and a second approver and shows content only after the content vault approves and audits the read. The second approver is named by the requester; nothing yet makes that person approve.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'export',
    screen: 'exports',
    title: 'Exports (§9) and subject export (§10)',
    needs: 'The export planner and job state machine (requested → authorised → running → sealed → delivered → expired) and the destination configuration.',
    consequence: 'No export can be requested, listed or receipted from this dashboard. There is no link to render and none is faked.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'settings',
    screen: 'settings',
    title: 'Settings: collection modes, retention, holds',
    needs: 'An audited admin API in control-api for the tenant\'s collection mode, retention periods, holds and sanction decisions (backlog task 12), as Settings → Deployment already has for deployment.',
    consequence: 'An admin can download the agent package, choose device verification and manage deployment keys and SCIM tokens under Settings → Deployment, but cannot see or change collection mode, retention or holds here. Reading configuration through the query DSL would be the wrong shape: a change is an audited act, not a read.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'rejected-histogram',
    screen: 'degraded',
    title: 'Rejected-envelope histogram by reason',
    needs: 'ingest.rejected as a registered source (reason_code, received_at, device_id).',
    consequence: 'An agent defect reported by ingest cannot be diagnosed from the dashboard. The quarantine table holds the diagnosis without the content precisely so this panel is possible; the DSL does not expose it.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'reconciliation-drift',
    screen: 'degraded',
    title: 'Reconciliation drift (C34)',
    needs: 'ops.reconciliation_run as a registered source (drift_found, checks, started_at, finished_at).',
    consequence: 'Drift between the two deletion mechanisms is recorded and alerted, but this dashboard cannot show it. The table is granted to the query role; it is not in the DSL.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'clock-skew',
    screen: 'degraded',
    title: 'Per-device clock skew (C26)',
    needs: 'A skew figure per device — ops.collector_state.detail is jsonb and is not exposed as a dimension or measure.',
    consequence: 'Both clocks are shown side by side on every row and the device clock is labelled as possibly skewed, but the skew itself cannot be quantified here.',
    severity: 'partial',
  }),
  Object.freeze({
    id: 'index-expiry-drift',
    screen: 'degraded',
    title: 'Index expiry drift (§15.5)',
    needs: 'Read access to ingest.search_text.expires_at, which sac_query does not hold and the DSL does not register.',
    consequence: 'Index rows surviving their expiry cannot be seen from the dashboard. The component that answers analyst questions is deliberately unable to read the index at all.',
    severity: 'absent',
  }),
  Object.freeze({
    id: 'degraded-share',
    screen: 'degraded',
    title: 'Degraded-classification share over the window',
    needs: 'degraded_events as a measure on a tool- or class-level aggregate, or a confidence dimension on one.',
    consequence: 'The classes screen carries degraded_events per cell when the source is mart.agg_class_period, so classifier health is visible there. A tenant-wide degraded share has no aggregate behind it and is not approximated by paging events.',
    severity: 'partial',
  }),
  Object.freeze({
    id: 'low-merge-share',
    screen: 'activity',
    title: 'Low-merge-confidence share (R9)',
    needs: 'A merge_confidence measure on an aggregate, or a count over a window on the event list.',
    consequence: 'The activity screen counts low-confidence merges over the rows it has in hand and labels the figure "of N rows in this page". A window-wide share would need a narrower read than the page, and inventing one would be worse than saying so.',
    severity: 'partial',
  }),
  Object.freeze({
    id: 'anchor-head',
    screen: 'audit',
    title: 'Anchored chain head and anchor time (§3.10)',
    needs: 'The anchor record written to write-once storage.',
    consequence: 'The audit screen reports that this page\'s hash links verify, and says the anchor head is not exposed. Whole-chain verification remains the reconciler\'s job.',
    severity: 'partial',
  }),
  Object.freeze({
    id: 'top-n-per-bucket',
    screen: 'tools',
    title: 'Top-N tools per bucket',
    needs: 'A window function over the grouped result, which the DSL does not express.',
    consequence: 'The tools screen ranks within a bucket-major ordering, so a page limit truncates whole buckets rather than returning the top N of each. Reported as a DSL limitation rather than approximated in the client.',
    severity: 'partial',
  }),
  Object.freeze({
    id: 'person-enumeration',
    screen: 'unsanctioned',
    title: 'A list of people sorted by volume',
    needs: 'Nothing: this is forbidden, not missing. mart.agg_user_period refuses a read without a subject filter, and the client refuses to build one.',
    consequence: 'The product has no leaderboard and this dashboard cannot construct one. Recorded here so the constraint is visible rather than merely absent.',
    severity: 'absent',
  }),
]);

export const GAP_IDS = Object.freeze(GAPS.map((g) => g.id));

/** The rows the "what this dashboard cannot show" screen renders. */
export function gapsFor(screenId) {
  return screenId ? GAPS.filter((g) => g.screen === screenId) : GAPS;
}

/** The whole catalogue, as a screen. */
export function unavailableView() {
  return Object.freeze({
    id: 'unavailable',
    title: 'What this dashboard cannot show',
    question: null,
    source: null,
    sourceLabel: null,
    subtitle: 'Every state the documented query API cannot express, with the source that would fill it. Nothing here is approximated in the client.',
    tiles: Object.freeze([
      { label: 'Screens with no API behind them', value: { kind: 'number', text: String(new Set(GAPS.filter((g) => g.severity === 'absent').map((g) => g.screen)).size) }, note: 'Content search, exports, settings.' },
      { label: 'Partial signals', value: { kind: 'number', text: String(GAPS.filter((g) => g.severity === 'partial').length) }, note: 'Shown with the caveat that makes them honest.' },
      { label: 'Forbidden, not missing', value: { kind: 'number', text: String(GAPS.filter((g) => g.id === 'person-enumeration').length) }, note: 'A leaderboard is a non-goal, enforced on both sides.' },
    ]),
    tables: Object.freeze([
      {
        title: 'Gaps',
        columns: Object.freeze([
          { key: 'screen', label: 'Screen' },
          { key: 'title', label: 'Missing' },
          { key: 'needs', label: 'What would be needed' },
          { key: 'consequence', label: 'Consequence' },
          { key: 'severity', label: 'Kind', kind: 'vocab' },
        ]),
        rows: Object.freeze(GAPS.map((row) => Object.freeze({ row, vocab: { severity: row.severity }, suppressed: false }))),
        emptyText: 'Nothing is missing.',
        suppressedCells: 0,
      },
    ]),
    series: Object.freeze([]),
    banners: Object.freeze([]),
    notes: Object.freeze([
      'These are reported rather than rendered as empty panels. A quiet panel and a blind panel must not look the same.',
    ]),
  });
}

/** A screen for a section that has no API behind it (exports, settings). */
export function noApiView({ id, title, subtitle }) {
  const rows = gapsFor(id);
  return Object.freeze({
    id,
    title,
    question: null,
    source: null,
    sourceLabel: null,
    subtitle,
    tiles: Object.freeze([]),
    tables: Object.freeze([
      {
        title: 'Why this screen is empty',
        columns: Object.freeze([
          { key: 'title', label: 'Missing' },
          { key: 'needs', label: 'What would be needed' },
          { key: 'consequence', label: 'Consequence' },
        ]),
        rows: Object.freeze(rows.map((row) => Object.freeze({ row, vocab: {}, suppressed: false }))),
        emptyText: 'Nothing is missing.',
        suppressedCells: 0,
      },
    ]),
    series: Object.freeze([]),
    banners: Object.freeze([
      { level: 'hatched', title: 'No API behind this screen', text: 'This is not "nothing happened": the query API has no endpoint for it.' },
    ]),
    notes: Object.freeze(['Reported by the client rather than hidden, so the gap is visible to whoever reads this dashboard.']),
  });
}
