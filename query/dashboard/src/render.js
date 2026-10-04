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
import { freshnessText, coverageText } from './states.js';
import { GAP_REASONS } from './vocab.js';

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

function renderCell(row, column) {
  const { key, kind } = column;
  const raw = row[key];
  if (kind === 'measure' || kind === 'measure-bytes' || kind === 'measure-score') {
    if (row.result_state === 'suppressed') return renderValue({ kind: 'suppressed', k: row.k });
    if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
    if (kind === 'measure-bytes') return `<span class="v-number">${escapeHtml(formatBytes(Number(raw)))}</span>`;
    if (kind === 'measure-score') return `<span class="v-number">${escapeHtml(formatScore(Number(raw)))}</span>`;
    return `<span class="v-number">${escapeHtml(formatCount(Number(raw)))}</span>`;
  }
  if (kind === 'count') {
    return raw === null || raw === undefined ? renderValue({ kind: 'absent' }) : `<span class="v-number">${escapeHtml(formatCount(Number(raw)))}</span>`;
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
  if (kind === 'vocab') {
    // A vocabulary value is rendered even when it is a value people forget: `unknown` and
    // `never_reported` are answers, and a blank would be indistinguishable from a missing field.
    return raw === null || raw === undefined ? '<span class="v-vocab v-vocab-unknown">unknown</span>' : renderValue({ kind: 'vocab', text: raw });
  }
  if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
  return `<span class="v-text">${escapeHtml(String(raw))}</span>`;
}

export function renderTable(table) {
  const head = table.columns.map((c) => `<th scope="col">${escapeHtml(c.label)}</th>`).join('');
  const body = table.rows.map(({ row }) => {
    const cls = row.result_state === 'suppressed' ? ' class="row-suppressed"' : '';
    const cells = table.columns.map((c) => `<td>${renderCell(row, c)}</td>`).join('');
    return `<tr${cls}>${cells}</tr>`;
  }).join('');
  const empty = table.rows.length === 0
    ? `<tr class="row-empty"><td colspan="${table.columns.length}">${escapeHtml(table.emptyText ?? 'No rows.')}</td></tr>`
    : '';
  const note = table.suppressedCells > 0
    ? `<p class="table-note">${escapeHtml(String(table.suppressedCells))} suppressed cell(s) in this table: a hatched cell means there was something and the number is withheld, never that the value is zero.</p>`
    : '';
  return `<section class="table-block"><h3>${escapeHtml(table.title)}</h3>`
    + `<table><thead><tr>${head}</tr></thead><tbody>${body}${empty}</tbody></table>${note}</section>`;
}

/**
 * A series. A suppressed bucket renders as a hatched column, not as a zero-height one, and the
 * axis is scaled to the largest publishable value so a suppressed bucket cannot be read off the
 * chart's proportions.
 */
export function renderSeries(series) {
  if (!series) return '';
  if (series.unavailable) {
    return `<section class="series-block"><h3>${escapeHtml(series.title)}</h3>`
      + `<p class="series-unavailable">No chart: ${escapeHtml(series.reason)}</p></section>`;
  }
  if (series.points.length === 0) {
    return `<section class="series-block"><h3>${escapeHtml(series.title)}</h3><p class="series-unavailable">No points.</p></section>`;
  }
  const max = Math.max(1, ...series.points.map((p) => (p.value.kind === 'number' || p.value.kind === 'floor' ? p.value.value : 0)));
  const bars = series.points.map((p) => {
    const label = formatInstant(p.bucket).slice(0, 10) || 'total';
    if (p.value.kind === 'floor') {
      return `<div class="bar bar-floor" title="${escapeHtml(label)}: ≥ ${escapeHtml(formatCount(p.value.value))} (${escapeHtml(String(p.value.suppressedCells))} suppressed)"><span class="bar-fill" style="height:100%"></span><span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
    }
    const height = Math.max(1, Math.round((p.value.value / max) * 100));
    return `<div class="bar" title="${escapeHtml(label)}: ${escapeHtml(formatCount(p.value.value))}"><span class="bar-fill" style="height:${height}%"></span><span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
  }).join('');
  return `<section class="series-block"><h3>${escapeHtml(series.title)}</h3><div class="bars">${bars}</div>`
    + `<p class="series-note">Scale tops out at ${escapeHtml(formatCount(max))}. Hatched columns are floors, not zeroes.</p></section>`;
}

export function renderBanner(banner) {
  return `<div class="banner banner-${escapeHtml(banner.level)}" role="status">`
    + `<strong>${escapeHtml(banner.title)}</strong><span>${escapeHtml(banner.text)}</span></div>`;
}

export function renderTile(t) {
  return `<div class="tile"><span class="tile-label">${escapeHtml(t.label)}</span>`
    + `<span class="tile-value">${renderValue(t.value)}</span>`
    + (t.note ? `<span class="tile-note">${escapeHtml(t.note)}</span>` : '')
    + '</div>';
}

/** The persistent coverage strip (§11.3): on every screen, with the enrolled denominator. */
export function renderStrip({ coverage, freshness }) {
  const cov = coverageText(coverage);
  const fresh = freshnessText(freshness);
  const gaps = (cov.gaps ?? []).map((g) => `<span class="gap gap-${escapeHtml(g.reason)}">${escapeHtml(g.reason)}: ${escapeHtml(formatCount(g.count))}</span>`).join('');
  return `<div class="strip strip-${escapeHtml(cov.state ?? 'unknown')}" role="status">`
    + `<span class="strip-item strip-coverage"><strong>Coverage</strong> ${escapeHtml(cov.text)}</span>`
    + `<span class="strip-item strip-freshness state-${escapeHtml(fresh.state)}"><strong>Freshness</strong> ${escapeHtml(fresh.text)}</span>`
    + (gaps ? `<span class="strip-gaps">${gaps}</span>` : '')
    + '</div>';
}

export function renderNeeds(view) {
  // The person screen is the one input a reader can type: there is no list of people to pick from.
  const form = view.id === 'person'
    ? '<form class="needs-form" data-screen="person"><input name="subject" type="text" aria-label="User reference" placeholder="u_4f21" autocomplete="off" spellcheck="false" required><button type="submit">Show</button></form>'
    : '';
  return `<section class="needs"><h2>${escapeHtml(view.title)}</h2><p>${escapeHtml(view.subtitle ?? 'This screen needs an input before it can ask its question.')}</p>${form}</section>`;
}

/** A whole screen. `shell` carries the strip, which no screen may omit. */
export function renderScreen(view, shell = {}) {
  if (view.needsInput) return renderNeeds(view);
  const banners = (view.banners ?? []).map(renderBanner).join('');
  const tiles = (view.tiles ?? []).length > 0 ? `<div class="tiles">${view.tiles.map(renderTile).join('')}</div>` : '';
  const tables = (view.tables ?? []).map(renderTable).join('');
  const series = (view.series ?? []).map(renderSeries).join('');
  const notes = (view.notes ?? []).length > 0
    ? `<section class="notes"><h3>How to read this</h3><ul>${view.notes.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></section>`
    : '';
  const header = `<header class="screen-head"><h2>${escapeHtml(view.title)}</h2>`
    + (view.question ? `<p class="question">${escapeHtml(view.question)}</p>` : '')
    + (view.subtitle ? `<p class="subtitle">${escapeHtml(view.subtitle)}</p>` : '')
    + (view.sourceLabel ? `<p class="source">Source: <code>${escapeHtml(view.sourceLabel)}</code></p>` : '')
    + '</header>';
  return `${renderStrip(shell)}<article class="screen">${header}${banners}${tiles}${series}${tables}${notes}</article>`;
}

/** The navigation, as the contents of the page's nav element: items under their group headings. */
export function renderNav(items, current) {
  const groups = new Map();
  for (const item of items) {
    const key = item.group ?? '';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(item);
  }
  return [...groups].map(([group, members]) => {
    const head = group ? `<p class="nav-head">${escapeHtml(group)}</p>` : '';
    const links = members.map((item) => {
      const active = item.id === current ? ' class="active" aria-current="page"' : '';
      const q = item.question ? `<span class="nav-q">Q${escapeHtml(String(item.question))}</span>` : '';
      return `<a href="#${escapeHtml(item.id)}"${active}><span>${escapeHtml(item.label)}</span>${q}</a>`;
    }).join('');
    return `<div class="nav-group">${head}${links}</div>`;
  }).join('');
}

/** The state gallery: pick a scenario, see every screen render it. */
export function renderGallery(scenarios, current) {
  return `<section class="gallery"><h3>State gallery</h3>`
    + `<p>Every result state the API documents, rendered by the same code the screens use. Pick one to force it across the dashboard.</p>`
    + `<div class="gallery-items">${scenarios.map((s) => `<a class="gallery-item${s.id === current ? ' active' : ''}" href="#gallery/${escapeHtml(s.id)}">${escapeHtml(s.label)}</a>`).join('')}</div>`
    + '</section>';
}

/** The reason vocabulary, rendered so an absent reason cannot look like a blank. */
export function renderGapReasons(coverage) {
  const present = coverage?.gap_reasons ?? {};
  return `<ul class="gap-list">${GAP_REASONS.map((reason) => {
    const count = present[reason];
    return `<li class="${count === undefined ? 'gap-none' : 'gap-some'}">${escapeHtml(reason)}: ${count === undefined ? 'none recorded' : escapeHtml(formatCount(count))}</li>`;
  }).join('')}</ul>`;
}
