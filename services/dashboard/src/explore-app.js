// explore-app.js — the Explore page: one search, its filters, its results, and one result's detail.
//
// Two halves, split the way app.js splits them. `createExplorer` is DOM-free: it holds the page's
// state, makes every read through the one api, and tells a listener when the state changed, so a
// test can drive a whole search without a browser. `bootExplore` is the only function here that
// touches a document.
//
// The rules the controller keeps:
//   * a query with a problem is not sent. Nothing is dropped to make it sendable;
//   * the window is resolved once per search and reused for every later page, because a cursor is
//     bound to the query it was issued for;
//   * only `next_cursor: null` ends a result set. A short page does not: rows deleted underneath an
//     iteration make a page shorter;
//   * a refusal is a state to show, never an empty list;
//   * an answer that arrives after a newer search began is discarded.

import { createQueryApi, httpTransport, createContentApi, httpContentTransport, createListExportApi, httpExportTransport, loadSession } from './transport.js';
import { renderNav } from './render.js';
import { shellNavItems, readCollapsed, wireShell } from './shell.js';
import { allowedPageIds, filterNavItems } from './session.js';
import { windowFor } from './dsl.js';
import { readState } from './states.js';
import {
  EXPLORE_DATASETS, EXPLORE_DEFAULT_DATASET, exploreDataset, exploreWindowPreset,
  parseExploreQuery, formatExploreQuery, checkExploreFilter,
  buildExploreRequest, buildExploreRecordRequest, buildExploreCollectorsRequest, encodeExploreHash, decodeExploreHash, exploreUserInput,
} from './explore-model.js';
import {
  renderExploreDatasets, renderExploreWindow,
  renderExploreProblems, renderExploreRail, renderExploreSummary, renderExploreResults, renderExploreDetail,
  renderExploreText,
} from './explore-render.js';
import { eventView } from './views.js';

const DETAIL_CLOSED = Object.freeze({ status: 'closed', key: null, row: null, record: null, submissionId: null });

/** The prompt-text search: nothing asked yet. */
const TEXT_IDLE = Object.freeze({
  status: 'idle', query: '', hits: Object.freeze([]), nextCursor: null, truncated: false,
  problem: null, moreProblem: null, loadingMore: false, pageSize: 20, ignored: Object.freeze([]),
});

/** The filters a prompt-text search composes by. The rest still filter the list only. */
const TEXT_FILTER_FIELDS = Object.freeze(['subject', 'tool', 'device', 'mode']);

/** The filters the vault can apply to a text search: person, tool, device and collection mode. */
function textFiltersOf(filters) {
  const out = {};
  for (const name of TEXT_FILTER_FIELDS) {
    const value = filters[name];
    if (typeof value === 'string' && value.trim() !== '') out[name] = value.trim();
  }
  return out;
}

/** Active filters a prompt search does not apply, so the page can say so rather than imply it did. */
function textIgnoredOf(filters) {
  return Object.keys(filters).filter((name) => !TEXT_FILTER_FIELDS.includes(name));
}
/** The content of the open record: nothing retrieved. Content is never held past the record it belongs to. */
const CONTENT_IDLE = Object.freeze({
  status: 'idle', submissionId: null, problem: null, gone: null, typed: '', kind: null, full: '', bytes: 0, grantId: null,
});

/** The event ids of a record's observations: what a retrieval names. */
function exploreEventIds(record) {
  const ids = (record?.data ?? []).map((row) => row.observation_event_id).filter((id) => typeof id === 'string' && id !== '');
  return [...new Set(ids)];
}

/** Any thrown error as a refusal state: a transport failure, a local refusal, or a bad envelope. */
function exploreRefusalFrom(error) {
  const envelope = error?.envelope
    ?? (typeof error?.toEnvelope === 'function' ? error.toEnvelope() : null)
    ?? {
      api_version: '1',
      query_version: '1',
      result_state: error?.resultState ?? 'unsupported_query_shape',
      error: { code: error?.reason ?? 'client_error', message: String(error?.message ?? error) },
    };
  return readState({ api_version: '1', query_version: '1', result_state: envelope.result_state, error: envelope.error });
}

/**
 * The page's behaviour over one api.
 *
 * @param {object} input
 * @param {{run: (body: object) => Promise<object>}} input.api
 * @param {object} [input.content]
 * @param {{list: (body: object) => Promise<object>}} [input.export]
 * @param {(url: string) => void} [input.save]  hands a download link to the browser
 * @param {() => Date} [input.now]
 * @param {(state: object) => void} [input.onChange]
 */
export function createExplorer({ api, content = null, export: exportApi = null, save = () => {}, now = () => new Date(), onChange = () => {} }) {
  const first = EXPLORE_DATASETS[EXPLORE_DEFAULT_DATASET];
  let state = Object.freeze({
    dataset: first.id,
    windowPreset: first.defaultWindow,
    filters: Object.freeze({}),
    includeClientGenerated: false,
    queryText: '',
    problems: Object.freeze([]),
    status: 'idle',
    rows: Object.freeze([]),
    result: null,
    loadingMore: false,
    moreProblem: null,
    detail: DETAIL_CLOSED,
    text: TEXT_IDLE,
    content: CONTENT_IDLE,
    /** Whether this page has a content path at all. Without one the two features say so. */
    contentAvailable: Boolean(content),
    /** The list export: idle, exporting, an error, or a saved download. */
    export: Object.freeze({ status: 'idle', problem: null, rowCount: null }),
    shell: Object.freeze({ coverage: null, freshness: null }),
  });
  /** The query the rows on screen belong to: what later pages must repeat exactly. */
  let ran = null;
  /** The prompt search on screen: what a later page must repeat, and the window it was resolved with. */
  let textRan = null;
  let searchSeq = 0;
  let detailSeq = 0;
  let textSeq = 0;
  let contentSeq = 0;

  function set(patch) {
    // Retrieved content belongs to the record it was retrieved for. Any change of the open record
    // drops it, so content cannot stay on screen beside a different event.
    if ('detail' in patch && patch.detail.submissionId !== state.content.submissionId && state.content !== CONTENT_IDLE) {
      contentSeq += 1;
      patch = { ...patch, content: CONTENT_IDLE };
    }
    state = Object.freeze({ ...state, ...patch });
    onChange(state);
  }

  async function ask(build) {
    try {
      return readState(await api.run(build()));
    } catch (error) {
      return exploreRefusalFrom(error);
    }
  }

  function shellAfter(result) {
    return Object.freeze({
      coverage: result.coverage ?? state.shell.coverage,
      freshness: result.freshness ?? state.shell.freshness,
    });
  }

  /** Run the search the state describes, from page one. */
  async function run() {
    const dataset = EXPLORE_DATASETS[state.dataset];
    const seq = ++searchSeq;
    detailSeq += 1;
    ran = null;
    if (state.problems.length > 0) {
      set({ status: 'blocked', rows: Object.freeze([]), result: null, loadingMore: false, moreProblem: null, detail: DETAIL_CLOSED });
      return state;
    }
    const query = Object.freeze({
      dataset,
      filters: state.filters,
      window: state.windowPreset ? windowFor(state.windowPreset, now()) : null,
      includeClientGenerated: state.includeClientGenerated,
    });
    set({ status: 'loading', rows: Object.freeze([]), result: null, loadingMore: false, moreProblem: null, detail: DETAIL_CLOSED });
    const result = await ask(() => buildExploreRequest(query));
    if (seq !== searchSeq) return state;
    if (result.isRefusal) {
      set({ status: 'refused', result, shell: shellAfter(result) });
      return state;
    }
    ran = query;
    set({ status: 'ready', rows: Object.freeze([...result.data]), result, shell: shellAfter(result) });
    return state;
  }

  /** Fetch the next page of the search on screen and append it. */
  async function loadMore() {
    const cursor = state.result?.page?.next_cursor;
    if (state.status !== 'ready' || !cursor || !ran || state.loadingMore) return state;
    const seq = searchSeq;
    set({ loadingMore: true, moreProblem: null });
    const result = await ask(() => buildExploreRequest({ ...ran, cursor }));
    if (seq !== searchSeq) return state;
    if (result.isRefusal) {
      // The rows already shown stay. The refusal says why the list stops here.
      set({ loadingMore: false, moreProblem: result });
      return state;
    }
    set({ loadingMore: false, rows: Object.freeze([...state.rows, ...result.data]), result, shell: shellAfter(result) });
    return state;
  }

  /**
   * Re-run what is on screen after the filter or window state changes. The list always re-runs;
   * the prompt search re-runs too when one is showing, because the rail and the window apply to
   * it. Both read the state as it is after the change.
   */
  function refresh() {
    const listRun = run();
    if (state.text.status === 'idle') return listRun;
    return Promise.all([listRun, searchText(state.text.query)]).then(() => state);
  }

  function applyFilters(dataset, filters, problems = []) {
    set({
      filters: Object.freeze({ ...filters }),
      queryText: problems.length > 0 ? state.queryText : formatExploreQuery(filters, dataset),
      problems: Object.freeze([...problems]),
    });
    return refresh();
  }

  /** Search with the text of the query bar. */
  function setQuery(text) {
    const dataset = EXPLORE_DATASETS[state.dataset];
    const parsed = parseExploreQuery(text, dataset);
    set({ queryText: String(text ?? '') });
    return applyFilters(dataset, parsed.filters, parsed.problems);
  }

  /** Set or clear one filter from the rail or a detail panel. An empty value clears it. */
  function setFilter(name, value) {
    const dataset = EXPLORE_DATASETS[state.dataset];
    const next = { ...state.filters };
    const trimmed = typeof value === 'string' ? value.trim() : '';
    if (trimmed === '') {
      delete next[name];
      return applyFilters(dataset, next);
    }
    const problem = checkExploreFilter(dataset, name, trimmed);
    if (problem) {
      set({ problems: Object.freeze([problem]) });
      return run();
    }
    next[name] = trimmed;
    return applyFilters(dataset, next);
  }

  function clearFilters() {
    return applyFilters(EXPLORE_DATASETS[state.dataset], {});
  }

  /** Switch what is searched. Filters the new dataset also has are carried; the rest are dropped by name. */
  function setDataset(id) {
    const dataset = exploreDataset(id);
    if (dataset.id === state.dataset) return Promise.resolve(state);
    const carried = {};
    for (const field of dataset.fields) {
      const value = state.filters[field.name];
      if (typeof value === 'string' && !checkExploreFilter(dataset, field.name, value)) carried[field.name] = value;
    }
    set({ dataset: dataset.id, windowPreset: exploreWindowPreset(dataset, state.windowPreset) });
    return applyFilters(dataset, carried);
  }

  function setWindow(preset) {
    const dataset = EXPLORE_DATASETS[state.dataset];
    const next = exploreWindowPreset(dataset, preset);
    if (next === state.windowPreset) return Promise.resolve(state);
    set({ windowPreset: next });
    return refresh();
  }

  /** Show or hide the requests a client made for itself. Off is the default and hides them. */
  function setIncludeClientGenerated(value) {
    const next = Boolean(value);
    if (next === state.includeClientGenerated) return Promise.resolve(state);
    set({ includeClientGenerated: next });
    return run();
  }

  /** Open one result. An event or finding is re-read on its own (Q9); a device or audit row is shown as held. */
  async function open(key) {
    const dataset = EXPLORE_DATASETS[state.dataset];
    const row = state.rows.find((r) => dataset.rowKey(r) === key) ?? null;
    const seq = ++detailSeq;
    if (dataset.detail === 'row') {
      if (!row) return state;
      if (dataset.id !== 'devices') {
        set({ detail: Object.freeze({ status: 'ready', key, row, record: null, submissionId: null }) });
        return state;
      }
      // A device row also lists every collector the device reported, read on its own.
      const held = { status: 'ready', key, row, record: null, submissionId: null };
      set({ detail: Object.freeze({ ...held, collectors: Object.freeze({ status: 'loading', result: null }) }) });
      const result = await ask(() => buildExploreCollectorsRequest(row.device));
      if (seq !== detailSeq) return state;
      set({ detail: Object.freeze({ ...held, collectors: Object.freeze({ status: result.isRefusal ? 'refused' : 'ready', result }) }) });
      return state;
    }
    // A linked record may not be on the page that is loaded; its id is still enough to read it.
    const submissionId = row?.submission_id ?? String(key).split('|')[0];
    set({ detail: Object.freeze({ status: 'loading', key, row, record: null, submissionId }) });
    const record = await ask(() => buildExploreRecordRequest({ submissionId, receivedAtHint: row?.received_at }));
    if (seq !== detailSeq) return state;
    const served = !record.isRefusal && record.data.length > 0;
    set({
      detail: Object.freeze({ status: served ? 'ready' : 'refused', key, row, record, submissionId }),
      shell: shellAfter(record),
    });
    // An event whose content is stored shows it: opening the record is asking to read it.
    if (served && content && String(eventView(record).tiles[0]?.value?.text) === 'uploaded') await retrieveContent();
    return state;
  }

  function close() {
    detailSeq += 1;
    if (state.detail.status !== 'closed') set({ detail: DETAIL_CLOSED });
    return state;
  }

  /** One hit, in the shape the renderer and the open action use. */
  function textHit(h) {
    return Object.freeze({
      submissionId: String(h.submission_id), snippet: String(h.snippet ?? ''),
      subject: h.subject ?? null, directory_name: h.directory_name ?? null,
      device: h.device ?? null, tool: h.tool ?? null, tool_name: h.tool_name ?? null,
      hostname: h.hostname ?? null,
    });
  }

  /**
   * Search prompt text, narrowed by the filters the rail holds and the window in force. It is not
   * a filter of the list above it: it is a separate, audited read of the content index, answered
   * with bounded snippets and a page cursor, and a hit is opened like any other event.
   *
   * The window is resolved once here and kept with the request, so every later page of the same
   * search repeats the window the first page was issued under.
   */
  async function searchText(query) {
    const text = String(query ?? '').trim();
    const seq = ++textSeq;
    if (text === '') {
      textRan = null;
      set({ text: TEXT_IDLE });
      return state;
    }
    const pageSize = state.text.pageSize ?? TEXT_IDLE.pageSize;
    const filters = textFiltersOf(state.filters);
    const ignored = Object.freeze(textIgnoredOf(state.filters));
    if (!content) {
      textRan = null;
      set({ text: Object.freeze({ ...TEXT_IDLE, status: 'refused', query: text, pageSize, ignored, problem: Object.freeze({ code: 'no_content_path', message: 'This page has no content path behind it.' }) }) });
      return state;
    }
    const window = state.windowPreset ? windowFor(state.windowPreset, now()) : null;
    textRan = Object.freeze({ query: text, limit: pageSize, ...filters, ...(window ? { window } : {}) });
    set({ text: Object.freeze({ ...TEXT_IDLE, status: 'loading', query: text, pageSize, ignored }) });
    const answer = await content.search(textRan);
    if (seq !== textSeq) return state;
    if (answer.state !== 'available') {
      set({ text: Object.freeze({ ...TEXT_IDLE, status: 'refused', query: text, pageSize, ignored, problem: Object.freeze(answer.error ?? { code: answer.state, message: 'The search was not served.' }) }) });
      return state;
    }
    set({
      text: Object.freeze({
        status: 'ready', query: text, problem: null, pageSize, ignored,
        moreProblem: null, loadingMore: false,
        hits: Object.freeze((answer.hits ?? []).map(textHit)),
        nextCursor: answer.next_cursor ?? null,
        truncated: Boolean(answer.truncated),
      }),
    });
    return state;
  }

  /** Fetch the next page of the prompt search on screen and append it. */
  async function loadMoreText() {
    const cursor = state.text.nextCursor;
    if (state.text.status !== 'ready' || !cursor || !textRan || state.text.loadingMore) return state;
    const seq = textSeq;
    set({ text: Object.freeze({ ...state.text, loadingMore: true, moreProblem: null }) });
    const answer = await content.search({ ...textRan, cursor });
    if (seq !== textSeq) return state;
    if (answer.state !== 'available') {
      // The hits already shown stay; the refusal says why the list stops here.
      set({ text: Object.freeze({ ...state.text, loadingMore: false, moreProblem: Object.freeze(answer.error ?? { code: answer.state, message: 'The search was not served.' }) }) });
      return state;
    }
    set({
      text: Object.freeze({
        ...state.text, loadingMore: false, moreProblem: null,
        hits: Object.freeze([...state.text.hits, ...(answer.hits ?? []).map(textHit)]),
        nextCursor: answer.next_cursor ?? null,
        truncated: Boolean(answer.truncated),
      }),
    });
    return state;
  }

  /** Change the number of hits a prompt search asks for per page, and restart it. */
  function setTextPageSize(size) {
    const n = Number(size);
    if (!Number.isInteger(n) || n <= 0 || n > 50) return Promise.resolve(state);
    if (state.text.status === 'idle') {
      set({ text: Object.freeze({ ...TEXT_IDLE, pageSize: n }) });
      return Promise.resolve(state);
    }
    set({ text: Object.freeze({ ...state.text, pageSize: n }) });
    return searchText(state.text.query);
  }

  function clearText() {
    textSeq += 1;
    textRan = null;
    if (state.text !== TEXT_IDLE) set({ text: TEXT_IDLE });
    return state;
  }

  /** Open a search hit: the event it belongs to, read on its own like any linked record. */
  async function openHit(submissionId) {
    if (state.dataset !== 'events') await setDataset('events');
    return open(String(submissionId));
  }

  /**
   * Retrieve the content of the open record. The vault decides; a refusal comes back with its
   * reason and is shown.
   */
  async function retrieveContent() {
    const detail = state.detail;
    if (detail.status !== 'ready' || !detail.submissionId || !detail.record) return state;
    const submissionId = detail.submissionId;
    const seq = ++contentSeq;
    const base = { ...CONTENT_IDLE, submissionId };
    if (!content) {
      set({ content: Object.freeze({ ...base, status: 'refused', problem: Object.freeze({ code: 'no_content_path', message: 'This page has no content path behind it.' }) }) });
      return state;
    }
    set({ content: Object.freeze({ ...base, status: 'loading' }) });
    // The retrieval request mints a single-use URL; the content itself is fetched from that URL,
    // never from this answer. The vault audits both halves.
    const answer = await content.retrieve({ event_ids: exploreEventIds(detail.record) });
    if (seq !== contentSeq || state.detail.submissionId !== submissionId) return state;
    if (answer.state === 'available' && typeof answer.retrieval_url === 'string' && answer.retrieval_url !== '') {
      const fetched = await content.readUrl(answer.retrieval_url);
      if (seq !== contentSeq || state.detail.submissionId !== submissionId) return state;
      if (fetched.state === 'available' && typeof fetched.content === 'string') {
        const split = exploreUserInput(fetched.content);
        set({
          content: Object.freeze({
            ...base, status: 'ready', typed: split.typed, kind: split.kind, full: fetched.content,
            bytes: fetched.content.length, grantId: answer.grant_id ?? null,
          }),
        });
        return state;
      }
      if (fetched.state === 'no_longer_available') {
        set({ content: Object.freeze({ ...base, status: 'gone', gone: Object.freeze({ state: fetched.state, reason: fetched.reason ?? null, receipt: fetched.receipt_ref ?? null }) }) });
        return state;
      }
      set({ content: Object.freeze({ ...base, status: 'refused', problem: Object.freeze(fetched.error ?? { code: 'refused', message: 'The retrieval URL refused the read.' }) }) });
      return state;
    }
    if (answer.state === 'refused' || answer.error) {
      set({ content: Object.freeze({ ...base, status: 'refused', problem: Object.freeze(answer.error ?? { code: 'refused', message: 'The retrieval was refused.' }) }) });
      return state;
    }
    // Content that is gone is an answer, not a failure: the state and its reason are what is shown.
    set({ content: Object.freeze({ ...base, status: 'gone', gone: Object.freeze({ state: answer.state, reason: answer.reason ?? null, receipt: answer.receipt_ref ?? null }) }) });
    return state;
  }

  /** Put retrieved content away without closing the record. */
  function hideContent() {
    contentSeq += 1;
    if (state.content !== CONTENT_IDLE) set({ content: CONTENT_IDLE });
    return state;
  }

  /** Put the page into the state a link describes, search, and open the linked result. */
  async function restore(hash) {
    const decoded = decodeExploreHash(hash);
    set({
      dataset: decoded.dataset.id,
      windowPreset: decoded.windowPreset,
      filters: decoded.filters,
      includeClientGenerated: decoded.includeClientGenerated,
      queryText: formatExploreQuery(decoded.filters, decoded.dataset),
      problems: decoded.problems,
    });
    await run();
    if (decoded.open && state.status === 'ready') await open(decoded.open);
    return state;
  }

  /** The link for the state on screen. */
  function hash() {
    return encodeExploreHash({
      dataset: EXPLORE_DATASETS[state.dataset],
      windowPreset: state.windowPreset,
      filters: state.filters,
      open: state.detail.key,
      includeClientGenerated: state.includeClientGenerated,
    });
  }

  /**
   * Export the current filtered events or findings list. The list on screen is re-issued as an
   * export: query-api generates a bounded CSV server-side and returns a single-use, short-lived
   * download link, which is handed to the browser. The audit row records the filters.
   */
  async function exportList() {
    const dataset = EXPLORE_DATASETS[state.dataset];
    if (!dataset.exportable || state.status !== 'ready' || !ran) return state;
    set({ export: Object.freeze({ status: 'exporting', problem: null, rowCount: null }) });
    if (!exportApi) {
      set({ export: Object.freeze({ status: 'refused', problem: Object.freeze({ code: 'no_export_path', message: 'This page has no export path behind it.' }), rowCount: null }) });
      return state;
    }
    const answer = await exportApi.list(buildExploreRequest({
      dataset,
      filters: state.filters,
      window: state.windowPreset ? windowFor(state.windowPreset, now()) : null,
      includeClientGenerated: state.includeClientGenerated,
    }));
    if (answer.state === 'available' && typeof answer.download_url === 'string' && answer.download_url !== '') {
      save(answer.download_url);
      set({ export: Object.freeze({ status: 'saved', problem: null, rowCount: answer.row_count ?? null }) });
      return state;
    }
    set({ export: Object.freeze({ status: 'refused', problem: Object.freeze(answer.error ?? { code: 'refused', message: 'The export was not served.' }), rowCount: null }) });
    return state;
  }

  return Object.freeze({
    get state() { return state; },
    run, loadMore, setQuery, setFilter, clearFilters, setDataset, setWindow, setIncludeClientGenerated, open, close, restore, hash,
    searchText, loadMoreText, setTextPageSize, clearText, openHit, retrieveContent, hideContent, exportList,
  });
}

/**
 * Boot the page into a document.
 *
 * Every read goes to the page's own origin. The page sends no tenant and no credential: its server
 * attaches the session's product token.
 *
 * @param {object} input
 * @param {Document} input.document
 * @param {object} [input.api]     a query api (createQueryApi); the page origin's otherwise
 * @param {object} [input.content] a content api (createContentApi); the page origin's otherwise
 * @param {object} [input.export]  a list-export api (createListExportApi); the page origin's otherwise
 */
export async function bootExplore({ document, api: given, content: givenContent, export: givenExport } = {}) {
  const el = (id) => document.getElementById(id);
  const collapsed = readCollapsed(document);
  const nav = el('nav');
  // The same navigation panel as the dashboard. A signed-in role may see fewer pages than the
  // shell names; the server decides, and the read path refuses the rest.
  const session = await loadSession();
  const allowed = allowedPageIds(session);
  if (nav) nav.innerHTML = renderNav(filterNavItems(shellNavItems({ page: 'index.html' }), allowed), 'explore', { collapsed: [...collapsed] });
  wireShell({ document, collapsed, session });
  const api = given ?? createQueryApi({ transport: httpTransport() });
  const content = givenContent ?? createContentApi({ transport: httpContentTransport() });
  const exportApi = givenExport ?? createListExportApi({ transport: httpExportTransport() });
  const textInput = el('x-text-query');
  let lastHash = null;

  /** Hand a download link to the browser as a file the server has already named. */
  const save = (url) => {
    const link = document.createElement('a');
    link.href = url;
    link.rel = 'noopener';
    document.body.appendChild(link);
    link.click();
    link.remove();
  };

  const explorer = createExplorer({ api, content, export: exportApi, save, onChange: paint });

  /** Replace a region only when its markup changed, so an untouched region keeps its focus. */
  const painted = new Map();
  function put(id, html) {
    if (painted.get(id) === html) return;
    painted.set(id, html);
    el(id).innerHTML = html;
  }

  /** The control that has focus, described so its replacement can be found after a repaint. */
  function focusMark() {
    const active = document.activeElement;
    if (!active || active === document.body) return null;
    if (active.id) return { id: active.id };
    return active.dataset?.act ? { data: { ...active.dataset } } : null;
  }

  function restoreFocus(mark) {
    if (!mark || (document.activeElement && document.activeElement !== document.body)) return;
    const match = mark.id
      ? el(mark.id)
      : [...document.querySelectorAll('[data-act]')].find((node) => Object.entries(mark.data).every(([k, v]) => node.dataset[k] === v));
    if (match) match.focus();
  }

  function paint(state) {
    const mark = focusMark();
    put('x-datasets', renderExploreDatasets(state));
    put('x-window', renderExploreWindow(state));
    put('x-problems', renderExploreProblems(state));
    put('x-text', renderExploreText(state));
    put('x-rail', renderExploreRail(state));
    put('x-summary', renderExploreSummary(state));
    put('x-results', renderExploreResults(state));
    el('x-results').setAttribute('aria-busy', String(state.status === 'loading'));
    put('x-detail', renderExploreDetail(state));
    el('x-detail').hidden = state.detail.status === 'closed';
    el('x-body').classList.toggle('x-has-detail', state.detail.status !== 'closed');
    // A prompt-text search takes over the results; the list and its filters come back when it is cleared.
    el('x-page').classList.toggle('x-text-active', state.text.status !== 'idle');
    restoreFocus(mark);
    const next = explorer.hash();
    if (next !== lastHash) {
      lastHash = next;
      try {
        document.defaultView.history.replaceState(null, '', next);
      } catch {
        document.location.hash = next;
      }
    }
  }

  function focusDetail() {
    const title = el('x-detail-title');
    if (title) title.focus();
  }

  async function openRow(key) {
    const pending = explorer.open(key);
    focusDetail();
    await pending;
  }

  function closeDetail() {
    const key = explorer.state.detail.key;
    explorer.close();
    const row = key ? [...document.querySelectorAll('.x-row')].find((r) => r.dataset.key === key) : null;
    if (row) row.focus();
  }

  el('x-text-form').addEventListener('submit', (event) => {
    event.preventDefault();
    explorer.searchText(textInput.value);
  });
  document.addEventListener('click', (event) => {
    const target = event.target.closest('[data-act]');
    if (!target) return;
    const { act } = target.dataset;
    if (act === 'dataset') explorer.setDataset(target.dataset.dataset);
    else if (act === 'window') explorer.setWindow(target.dataset.window);
    else if (act === 'include') explorer.setIncludeClientGenerated(target.getAttribute('aria-pressed') !== 'true');
    else if (act === 'toggle') {
      const on = target.getAttribute('aria-pressed') === 'true';
      explorer.setFilter(target.dataset.field, on ? '' : target.dataset.value);
    } else if (act === 'filter') explorer.setFilter(target.dataset.field, target.dataset.value);
    else if (act === 'clear') explorer.clearFilters();
    else if (act === 'run') explorer.run();
    else if (act === 'more') explorer.loadMore();
    else if (act === 'export') explorer.exportList();
    else if (act === 'open') openRow(target.dataset.key);
    else if (act === 'close') closeDetail();
    else if (act === 'hit') {
      const pending = explorer.openHit(target.dataset.submission);
      focusDetail();
      pending.then(focusDetail);
    } else if (act === 'text-more') explorer.loadMoreText();
    else if (act === 'text-clear') {
      textInput.value = '';
      explorer.clearText();
    } else if (act === 'retrieve') explorer.retrieveContent();
  });

  // The page size is a prompt-search control, rendered into #x-text rather than the rail.
  el('x-text').addEventListener('change', (event) => {
    if (event.target?.id === 'x-text-page') explorer.setTextPageSize(event.target.value);
  });

  // A filter is applied when it is committed: a choice from a list, or Enter in a text field.
  el('x-rail').addEventListener('change', (event) => {
    const field = event.target.dataset?.field;
    if (field) explorer.setFilter(field, event.target.value);
  });

  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape' && explorer.state.detail.status !== 'closed') {
      closeDetail();
      return;
    }
    const row = event.target.closest?.('.x-row');
    if (!row) return;
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      openRow(row.dataset.key);
    } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      const sibling = event.key === 'ArrowDown' ? row.nextElementSibling : row.previousElementSibling;
      if (sibling) {
        event.preventDefault();
        sibling.focus();
      }
    }
  });

  document.defaultView.addEventListener('hashchange', () => {
    if (document.location.hash !== lastHash) explorer.restore(document.location.hash);
  });

  await explorer.restore(document.location.hash);
  return { explorer };
}
