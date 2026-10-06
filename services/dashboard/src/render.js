// render.js — view models to HTML. Pure: no DOM access, no fetching, no state.
//
// Every string that came from the API is escaped here. The dashboard renders tool fingerprints,
// rule titles, user references and case references, all of which are attacker-influenced text as
// far as the browser is concerned, and none of which is allowed to become markup.
//
// The one visual rule that matters more than the rest: a suppressed value renders as a hatched
// marker carrying k, and a genuine zero renders as `0`. They are different CSS classes fed by
// different value kinds, so a stylesheet change cannot merge them by accident either.

import { formatBytes, formatCount, formatInstant, formatScore } from './format.js';

export function escapeHtml(value) {
  return String(value ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/**
 * Render one value that already has a kind. There is no branch here that turns `suppressed` into a
 * number, and none that turns a number into a suppression.
 */
export function renderValue(value) {
  if (!value || typeof value !== 'object') return '<span class="v-absent">—</span>';
  switch (value.kind) {
    case 'number':
      return `<span class="v-number">${escapeHtml(value.text ?? formatCount(value.value))}</span>`;
    case 'floor':
      return `<span class="v-floor" title="A floor: ${escapeHtml(String(value.suppressedCells ?? 0))} cell(s) suppressed">≥ ${escapeHtml(String(value.text ?? formatCount(value.value)).replace(/^≥\s*/, ''))}</span>`;
    case 'suppressed':
      return `<span class="v-suppressed" title="Fewer than k = ${escapeHtml(String(value.k ?? 5))} distinct subjects">suppressed<span class="v-k">k=${escapeHtml(String(value.k ?? 5))}</span></span>`;
    case 'vocab':
      return `<span class="v-vocab v-vocab-${escapeHtml(String(value.text).replace(/[^a-z_]/gi, ''))}">${escapeHtml(value.text)}</span>`;
    case 'absent':
    default:
      return '<span class="v-absent" title="Not carried by this response">—</span>';
  }
}

const MEASURE_KINDS = Object.freeze(['measure', 'measure-bytes', 'measure-score', 'count']);

/**
 * A data bar behind a number: the same value, drawn. It is scaled to the largest publishable
 * value in its column, so a suppressed cell has no length to read off.
 */
function dataBar(raw, max) {
  if (!(max > 0) || !(Number(raw) > 0)) return '';
  return `<span class="cell-bar" style="--w:${Math.max(2, Math.round((Number(raw) / max) * 100))}" aria-hidden="true"></span>`;
}

function renderCell(row, column, max = 0) {
  const { key, kind } = column;
  const raw = row[key];
  if (kind === 'measure' || kind === 'measure-bytes' || kind === 'measure-score') {
    if (row.result_state === 'suppressed') return renderValue({ kind: 'suppressed', k: row.k });
    if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
    if (kind === 'measure-bytes') return `${dataBar(raw, max)}<span class="v-number">${escapeHtml(formatBytes(Number(raw)))}</span>`;
    if (kind === 'measure-score') return `<span class="v-number">${escapeHtml(formatScore(Number(raw)))}</span>`;
    return `${dataBar(raw, max)}<span class="v-number">${escapeHtml(formatCount(Number(raw)))}</span>`;
  }
  if (kind === 'count') {
    return raw === null || raw === undefined ? renderValue({ kind: 'absent' }) : `${dataBar(raw, max)}<span class="v-number">${escapeHtml(formatCount(Number(raw)))}</span>`;
  }
  if (kind === 'instant') {
    return raw ? `<span class="v-time">${escapeHtml(formatInstant(raw))}</span>` : renderValue({ kind: 'absent' });
  }
  if (kind === 'device-clock') {
    return raw
      ? `<span class="v-time v-device-clock" title="The device clock: possibly skewed, never normalised into server time">${escapeHtml(formatInstant(raw))}</span>`
      : renderValue({ kind: 'absent' });
  }
  if (kind === 'seconds') {
    return raw === null || raw === undefined ? renderValue({ kind: 'absent' }) : `<span class="v-number">${escapeHtml(formatCount(Number(raw)))} s</span>`;
  }
  if (kind === 'status') {
    // A state said in plain words. The vocabulary value it stands for stays in the markup.
    const value = String(raw ?? 'unknown');
    return `<span class="v-vocab v-vocab-${escapeHtml(value.replace(/[^a-z_]/gi, ''))}" title="${escapeHtml(value)}">${escapeHtml(row[`${key}_text`] ?? value)}</span>`;
  }
  if (kind === 'ago') {
    return raw
      ? `<span class="v-text" title="${escapeHtml(formatInstant(raw))}">${escapeHtml(row[`${key}_ago`] ?? formatInstant(raw))}</span>`
      : '<span class="v-absent">Never</span>';
  }
  if (kind === 'device-name') {
    // The hostname is the device name; the UUID is the identity and stays available on hover, and
    // is the fallback label when no hostname was recorded (a 'hashed' tenant, or an old device).
    const uuid = row.device ? String(row.device) : '';
    if (raw === null || raw === undefined || raw === '') {
      return uuid
        ? `<span class="v-text" title="${escapeHtml(uuid)}"><span class="v-mono">${escapeHtml(uuid.slice(0, 8))}…</span></span>`
        : renderValue({ kind: 'absent' });
    }
    return `<span class="v-text" title="${escapeHtml(uuid)}">${escapeHtml(String(raw))}</span>`;
  }
  if (kind === 'link') {
    return raw ? `<a class="cell-link" href="${escapeHtml(String(raw))}">${escapeHtml(column.linkLabel ?? 'Open')}</a>` : '';
  }
  if (kind === 'vocab') {
    // A vocabulary value is rendered even when it is a value people forget: `unknown` and
    // `never_reported` are answers, and a blank would be indistinguishable from a missing field.
    return raw === null || raw === undefined ? '<span class="v-vocab v-vocab-unknown">unknown</span>' : renderValue({ kind: 'vocab', text: raw });
  }
  if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
  // The tool column resolves the raw, behaviour-derived fingerprint to a name at read time. Both
  // are shown: the name is what a person reads, and the fingerprint is the stable identity behind
  // it, so an "Unrecognised tool" is never mistaken for a tool the product knows and merely
  // labelled badly. A row that carries no tool_name falls back to the fingerprint.
  if (key === 'tool') {
    const fingerprint = String(raw);
    const name = row.tool_name ? String(row.tool_name) : fingerprint;
    if (name === fingerprint) return `<span class="v-text v-mono" title="Tool fingerprint">${escapeHtml(fingerprint)}</span>`;
    return `<span class="v-text" title="Tool fingerprint ${escapeHtml(fingerprint)}">${escapeHtml(name)}</span>`
      + ` <span class="v-mono v-tool-fp">${escapeHtml(fingerprint)}</span>`;
  }
  // A person reference opens that person's series: the one screen that takes it.
  if (key === 'subject') return `<a class="cell-link" href="#person?subject=${encodeURIComponent(String(raw))}">${escapeHtml(String(raw))}</a>`;
  return `<span class="v-text">${escapeHtml(String(raw))}</span>`;
}

/**
 * How the rows on screen split across a vocabulary column, drawn as one segmented bar. It counts
 * the rows this response carried and says so; it is a picture of the table beneath it, not a
 * figure for the fleet.
 */
function renderBreakdowns(table) {
  if (table.rows.length < 2) return '';
  const blocks = [];
  for (const column of table.columns) {
    if (column.kind !== 'vocab' || blocks.length >= 3) continue;
    const counts = new Map();
    for (const { row } of table.rows) {
      const value = row[column.key] === null || row[column.key] === undefined ? 'unknown' : String(row[column.key]);
      counts.set(value, (counts.get(value) ?? 0) + 1);
    }
    if (counts.size < 2 || counts.size > 6) continue;
    const parts = [...counts].sort((a, b) => b[1] - a[1]);
    const cls = (value) => `v-vocab-${escapeHtml(value.replace(/[^a-z_]/gi, ''))}`;
    blocks.push(
      `<div class="dist"><span class="dist-label">${escapeHtml(column.label)}</span>`
      + `<div class="dist-bar" role="img" aria-label="${escapeHtml(parts.map(([v, n]) => `${v}: ${n}`).join(', '))}">`
      + parts.map(([value, n], i) => `<span class="dist-seg dist-c${i} ${cls(value)}" style="flex-grow:${n}" title="${escapeHtml(value)}: ${n} of ${table.rows.length} rows shown"></span>`).join('')
      + '</div><ul class="dist-legend">'
      + parts.map(([value, n], i) => `<li><span class="dist-dot dist-c${i} ${cls(value)}" aria-hidden="true"></span>${escapeHtml(value)}<span class="dist-n">${n}</span></li>`).join('')
      + '</ul></div>',
    );
  }
  return blocks.length > 0 ? `<div class="dists">${blocks.join('')}</div>` : '';
}

export function renderTable(table) {
  const numeric = (c) => MEASURE_KINDS.includes(c.kind);
  const maxima = new Map(table.columns.filter(numeric).map((c) => [
    c.key,
    Math.max(0, ...table.rows.map(({ row }) => (row.result_state === 'suppressed' ? 0 : Number(row[c.key]) || 0))),
  ]));
  const head = table.columns.map((c) => `<th scope="col"${numeric(c) ? ' class="num"' : ''}>${escapeHtml(c.label)}</th>`).join('');
  const body = table.rows.map(({ row }) => {
    const cells = table.columns.map((c) => `<td${numeric(c) ? ' class="num"' : ''}>${renderCell(row, c, maxima.get(c.key))}</td>`).join('');
    if (row.result_state === 'suppressed') return `<tr class="row-suppressed">${cells}</tr>`;
    // A row that names a submission opens it: the event screen is reached from a row, not typed.
    if (typeof row.submission_id === 'string' && row.submission_id !== '') {
      const hint = row.received_at ? `&received_at_hint=${encodeURIComponent(row.received_at)}` : '';
      return `<tr class="row-link" tabindex="0" data-href="#event?submission_id=${encodeURIComponent(row.submission_id)}${hint}">${cells}</tr>`;
    }
    return `<tr>${cells}</tr>`;
  }).join('');
  const empty = table.rows.length === 0
    ? `<tr class="row-empty"><td colspan="${table.columns.length}">${escapeHtml(table.emptyText ?? 'No rows.')}</td></tr>`
    : '';
  const note = table.suppressedCells > 0
    ? `<p class="table-note"><span class="legend-swatch" aria-hidden="true"></span>${escapeHtml(String(table.suppressedCells))} suppressed<span class="sr"> cell(s) in this table: a hatched cell means there was something and the number is withheld, never that the value is zero.</span></p>`
    : '';
  const count = table.rows.length > 0 ? `<span class="block-count">${table.rows.length}</span>` : '';
  const more = table.href ? `<a class="block-link" href="${escapeHtml(table.href)}">View all</a>` : '';
  return `<section class="table-block card"><div class="card-core"><header class="block-head"><h3>${escapeHtml(table.title)}</h3>${count}${note}${more}</header>`
    + (table.breakdowns === false ? '' : renderBreakdowns(table))
    + `<div class="table-scroll"><table><thead><tr>${head}</tr></thead><tbody>${body}${empty}</tbody></table></div></div></section>`;
}

/**
 * A series. A suppressed bucket renders as a hatched column, not as a zero-height one, and the
 * axis is scaled to the largest publishable value so a suppressed bucket cannot be read off the
 * chart's proportions.
 */
export function renderSeries(series) {
  if (!series) return '';
  const open = (inner) => `<section class="series-block card"><div class="card-core"><header class="block-head"><h3>${escapeHtml(series.title)}</h3></header>${inner}</div></section>`;
  if (series.unavailable) return open(`<p class="series-unavailable">No chart: ${escapeHtml(series.reason)}</p>`);
  if (series.points.length === 0) return open('<p class="series-unavailable">No points.</p>');
  const max = Math.max(1, ...series.points.map((p) => (p.value.kind === 'number' || p.value.kind === 'floor' ? p.value.value : 0)));
  const bars = series.points.map((p, i) => {
    const label = formatInstant(p.bucket).slice(0, 10) || 'total';
    const delay = `--i:${Math.min(i, 24)}`;
    if (p.value.kind === 'floor') {
      return `<div class="bar bar-floor" style="${delay}" tabindex="0" data-tip="${escapeHtml(label)} · ≥ ${escapeHtml(formatCount(p.value.value))} (${escapeHtml(String(p.value.suppressedCells))} suppressed)"><span class="bar-fill" style="--h:100"></span><span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
    }
    const height = Math.max(1, Math.round((p.value.value / max) * 100));
    return `<div class="bar" style="${delay}" tabindex="0" data-tip="${escapeHtml(label)} · ${escapeHtml(formatCount(p.value.value))}"><span class="bar-fill" style="--h:${height}"></span><span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
  }).join('');
  const hasFloor = series.points.some((p) => p.value.kind === 'floor');
  const axis = `<div class="axis" aria-hidden="true"><span>${escapeHtml(formatCount(max))}</span><span>${escapeHtml(formatCount(Math.round(max / 2)))}</span><span>0</span></div>`;
  return open(
    `<div class="chart">${axis}<div class="bars${series.points.length <= 3 ? ' bars-few' : ''}">${bars}</div></div>`
    + (hasFloor ? '<p class="series-note"><span class="legend-swatch" aria-hidden="true"></span>Hatched columns are floors, not zeroes.</p>' : ''),
  );
}

export function renderBanner(banner) {
  return `<div class="banner banner-${escapeHtml(banner.level)}" role="status">`
    + `<strong>${escapeHtml(banner.title)}</strong><span>${escapeHtml(banner.text)}</span></div>`;
}

/** A tile's two-part split: one bar and its two counts, sized by the counts themselves. */
function renderSplit(split) {
  if (!Array.isArray(split) || split.length === 0) return '';
  return '<div class="dist-bar tile-split" aria-hidden="true">'
    + split.map((part) => `<span class="dist-seg v-vocab-${escapeHtml(part.key)}" style="flex-grow:${Math.max(0, Number(part.count) || 0)}"></span>`).join('')
    + '</div><ul class="dist-legend">'
    + split.map((part) => `<li><span class="dist-dot v-vocab-${escapeHtml(part.key)}" aria-hidden="true"></span>${escapeHtml(part.label)}<span class="dist-n">${escapeHtml(formatCount(part.count))}</span></li>`).join('')
    + '</ul>';
}

export function renderTile(t) {
  const tag = t.href ? 'a' : 'div';
  return `<${tag} class="tile card${t.href ? ' tile-link' : ''}"${t.href ? ` href="${escapeHtml(t.href)}"` : ''}><div class="card-core"><span class="tile-label">${escapeHtml(t.label)}</span>`
    + `<span class="tile-value">${renderValue(t.value)}</span>`
    + renderSplit(t.split)
    + (t.note ? `<span class="tile-note">${escapeHtml(t.note)}</span>` : '')
    + `</div></${tag}>`;
}

export function renderNeeds(view) {
  return `<section class="needs"><h2>${escapeHtml(view.title)}</h2><p>${escapeHtml(view.subtitle ?? 'This screen needs an input before it can ask its question.')}</p></section>`;
}

/**
 * The Users search bar. It is the one input a reader can type: there is no list of people to pick
 * from, so a person is reached by reference and shown on the same screen.
 */
function renderSearch(search) {
  if (!search) return '';
  return '<form class="needs-form search-form" data-screen="person" role="search">'
    + `<input name="subject" type="search" aria-label="User reference" placeholder="Enter a user reference, such as u_4f21" value="${escapeHtml(search.value ?? '')}" autocomplete="off" spellcheck="false">`
    + '<button type="submit">Search</button></form>';
}

/** A screen's filters: one labelled row of links each, above the table they narrow. */
function renderFilters(filters) {
  if (!Array.isArray(filters) || filters.length === 0) return '';
  return `<div class="filter-bar">${filters.map((f) => (
    `<div class="filter"><span class="filter-label">${escapeHtml(f.label)}</span>${renderPresets(f, f.label)}</div>`
  )).join('')}</div>`;
}

/** A segmented control of links: the window presets, or a screen's own switch. The choice is in the address. */
function renderPresets(presets, label = 'Window') {
  if (!presets || !Array.isArray(presets.items) || presets.items.length === 0) return '';
  return `<div class="seg" role="group" aria-label="${escapeHtml(presets.label ?? label)}">${presets.items.map((p) => (
    `<a class="seg-item" href="${escapeHtml(p.href)}"${p.id === presets.current ? ' aria-current="true"' : ''}>${escapeHtml(p.label)}</a>`
  )).join('')}</div>`;
}

/** A whole screen. Degraded coverage and stale data are said by the banners under the title. */
export function renderScreen(view, shell = {}) {
  if (view.needsInput && !view.search) return renderNeeds(view);
  const banners = (view.banners ?? []).map(renderBanner).join('');
  const tiles = (view.tiles ?? []).length > 0 ? `<div class="tiles">${view.tiles.map(renderTile).join('')}</div>` : '';
  const tables = (view.tables ?? []).map(renderTable).join('');
  const filters = renderFilters(view.filters);
  const series = (view.series ?? []).map(renderSeries).join('');
  // The reading notes are one click away rather than under every table: they matter, and they are
  // the same on every visit.
  const notes = (view.notes ?? []).length > 0
    ? `<details class="notes"><summary>About this data<span class="notes-count">${view.notes.length}</span></summary><ul>${view.notes.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`
    : '';
  const header = '<header class="screen-head"><div class="screen-title">'
    + (shell.eyebrow ? `<span class="eyebrow">${escapeHtml(shell.eyebrow)}</span>` : '')
    + `<h2>${escapeHtml(view.title)}</h2>`
    + (view.question ? `<p class="question">${escapeHtml(view.question)}</p>` : '')
    + (view.subtitle ? `<p class="subtitle">${escapeHtml(view.subtitle)}</p>` : '')
    + '</div><div class="screen-tools"><div class="screen-switches">'
    + renderPresets(shell.switch)
    + renderPresets(shell.presets)
    + '</div>'
    + (view.sourceLabel ? `<p class="source" title="Where this screen reads from">Source <code>${escapeHtml(view.sourceLabel)}</code></p>` : '')
    + '</div></header>';
  return `<article class="screen">${header}${renderSearch(view.search)}${banners}${tiles}${series}${filters}${tables}${notes}</article>`;
}

/** Thin-line icons for the navigation: one stroke weight, no fills. */
const NAV_ICONS = Object.freeze({
  overview: '<circle cx="12" cy="12" r="8.5"/><path d="M12 12 16.5 8"/><path d="M12 3.5v2M20.5 12h-2M5.5 12h-2"/>',
  search: '<circle cx="11" cy="11" r="6.5"/><path d="m16 16 4.5 4.5"/>',
  usage: '<path d="M4 19.5V10M9.5 19.5v-15M15 19.5v-7M20.5 19.5V7"/>',
  collection: '<rect x="4" y="5" width="16" height="11" rx="1.5"/><path d="M9 20h6M12 16v4"/>',
  governance: '<path d="M12 3.5 5 6.5v5c0 4.3 2.9 7.4 7 9 4.1-1.6 7-4.7 7-9v-5z"/><path d="m9.2 12 2 2 3.6-4"/>',
  settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
});

function navIcon(name) {
  const body = NAV_ICONS[name];
  return body ? `<svg class="nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>` : '';
}

/**
 * The navigation, as the contents of the page's nav element. An item with no group is a top-level
 * link; the rest sit under their group, which folds. A group holding the current screen is open,
 * so the reader is never somewhere the navigation does not show.
 */
export function renderNav(items, current, options = {}) {
  const collapsed = new Set(options.collapsed ?? []);
  const link = (item, nested) => {
    const active = item.id === current ? ' class="active" aria-current="page"' : '';
    const title = item.question ? ` title="Question ${escapeHtml(String(item.question))}"` : '';
    return `<a href="${escapeHtml(item.href ?? `#${item.id}`)}"${active}${title}>${nested ? '' : navIcon(item.icon)}<span>${escapeHtml(item.label)}</span></a>`;
  };
  const out = [];
  const groups = new Map();
  for (const item of items) {
    if (!item.group) {
      out.push({ html: link(item, false) });
      continue;
    }
    if (!groups.has(item.group)) {
      const entry = { group: item.group, icon: item.groupIcon, members: [] };
      groups.set(item.group, entry);
      out.push(entry);
    }
    groups.get(item.group).members.push(item);
  }
  return out.map((entry) => {
    if (entry.html) return entry.html;
    const holdsCurrent = entry.members.some((m) => m.id === current);
    const open = holdsCurrent || !collapsed.has(entry.group);
    return `<details class="nav-group" data-group="${escapeHtml(entry.group)}"${open ? ' open' : ''}>`
      + `<summary>${navIcon(entry.icon)}<span>${escapeHtml(entry.group)}</span><svg class="nav-chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="m9 6 6 6-6 6"/></svg></summary>`
      + `<div class="nav-items">${entry.members.map((m) => link(m, true)).join('')}</div></details>`;
  }).join('');
}
