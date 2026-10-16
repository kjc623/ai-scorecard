// render.js — view models to HTML. Pure: no DOM access, no fetching, no state.
//
// Every string that came from the API is escaped here. The dashboard renders tool fingerprints,
// rule titles, user references and case references, all of which are attacker-influenced text as
// far as the browser is concerned, and none of which is allowed to become markup.
//
// The one visual rule that matters more than the rest: a value the response does not carry renders
// as an absent marker, and a genuine zero renders as `0`. They are different CSS classes fed by
// different value kinds, so a stylesheet change cannot merge them by accident either.

import { formatBytes, formatCount, formatInstant, formatPercent, formatScore } from './format.js';
import { UNRECOGNISED_TOOL } from './vocab.js';

export { UNRECOGNISED_TOOL };

export function escapeHtml(value) {
  return String(value ?? '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

/**
 * Render one value that already has a kind. There is no branch here that turns `absent` into a
 * number, and none that turns a number into an absence.
 */
export function renderValue(value) {
  if (!value || typeof value !== 'object') return '<span class="v-absent">—</span>';
  switch (value.kind) {
    case 'number':
      return `<span class="v-number">${escapeHtml(value.text ?? formatCount(value.value))}</span>`;
    case 'vocab':
      return `<span class="v-vocab ${vocabClass(value.key ?? value.text)}">${escapeHtml(value.text)}</span>`;
    case 'absent':
    default:
      return '<span class="v-absent" title="Not carried by this response">—</span>';
  }
}

const MEASURE_KINDS = Object.freeze(['measure', 'measure-bytes', 'measure-score', 'count', 'share']);

/** A data bar behind a number: the same value, drawn, scaled to the largest value in its column. */
function dataBar(raw, max) {
  if (!(max > 0) || !(Number(raw) > 0)) return '';
  return `<span class="cell-bar" style="--w:${Math.max(2, Math.round((Number(raw) / max) * 100))}" aria-hidden="true"></span>`;
}

/** A vocabulary value as a chip, its key kept in the markup and its colour. */
function chip(value) {
  if (!value) return '';
  const key = String(value.key ?? '');
  return `<span class="v-vocab v-vocab-${escapeHtml(key.replace(/[^a-z_]/gi, ''))}" title="${escapeHtml(key)}">${escapeHtml(value.text ?? key)}</span>`;
}

/**
 * Two lines in one cell: the thing, then the facts a reader checks next. The first line is a
 * name, or a state chip when the state is the thing; an empty first line is an absence, not a blank.
 */
function renderStack(cell) {
  if (!cell || typeof cell !== 'object') return renderValue({ kind: 'absent' });
  const named = cell.primary !== null && cell.primary !== undefined && cell.primary !== '';
  const primary = named
    ? `<span class="cell-primary${cell.mono ? ' v-mono' : ''}">${escapeHtml(cell.primary)}</span>`
    : (cell.chip ? '' : '<span class="v-absent">—</span>');
  const secondary = cell.secondary ? `<span class="cell-secondary">${escapeHtml(cell.secondary)}</span>` : '';
  return `<span class="cell-stack"${cell.title ? ` title="${escapeHtml(cell.title)}"` : ''}>${primary}${chip(cell.chip)}${secondary}</span>`;
}

function renderCell(row, column, max = 0) {
  const { key, kind } = column;
  const raw = row[key];
  if (kind === 'stack') return renderStack(raw);
  if (kind === 'measure' || kind === 'measure-bytes' || kind === 'measure-score') {
    if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
    if (kind === 'measure-bytes') return `${dataBar(raw, max)}<span class="v-number">${escapeHtml(formatBytes(Number(raw)))}</span>`;
    if (kind === 'measure-score') return `<span class="v-number">${escapeHtml(formatScore(Number(raw)))}</span>`;
    return `${dataBar(raw, max)}<span class="v-number">${escapeHtml(formatCount(Number(raw)))}</span>`;
  }
  // A part out of its whole, the bar drawn against the whole rather than the column.
  if (kind === 'share') {
    if (!raw || raw.count === null || raw.count === undefined) return renderValue({ kind: 'absent' });
    const of = raw.of === null || raw.of === undefined ? '' : `<span class="cell-of"> of ${escapeHtml(formatCount(Number(raw.of)))}</span>`;
    return `${dataBar(raw.count, Number(raw.of))}<span class="v-number">${escapeHtml(formatCount(Number(raw.count)))}</span>${of}`;
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
  // A person's name opens their page; a person with no known name is listed by reference. A
  // column keyed on the reference takes the row's names: the directory's, else the account's.
  if (kind === 'person') {
    const ref = row.subject ? String(row.subject) : '';
    const named = key === 'subject' ? (row.directory_name ?? row.subject_name ?? null) : raw;
    const label = named === null || named === undefined || named === '' ? ref : String(named);
    if (!ref) return renderValue({ kind: 'absent' });
    return `<a class="cell-link" href="#person?subject=${encodeURIComponent(ref)}" title="User reference ${escapeHtml(ref)}">${escapeHtml(label)}</a>`;
  }
  if (kind === 'link') {
    return raw ? `<a class="cell-link" href="${escapeHtml(String(raw))}">${escapeHtml(column.linkLabel ?? 'Open')}</a>` : '';
  }
  if (kind === 'vocab') {
    // A vocabulary value is rendered even when it is a value people forget: `unknown` and
    // `never_reported` are answers, and a blank would be indistinguishable from a missing field.
    return raw === null || raw === undefined ? '<span class="v-vocab v-vocab-unknown">unknown</span>' : renderValue({ kind: 'vocab', text: raw, key: vocabKey(key, raw) });
  }
  if (raw === null || raw === undefined) return renderValue({ kind: 'absent' });
  // The tool column shows the name the catalogue gives the fingerprint, with the fingerprint on
  // hover. Beside an unrecognised tool the fingerprint is also shown, because it is the only thing
  // that tells two unrecognised tools apart. A row with no tool_name shows the fingerprint.
  if (key === 'tool') {
    const fingerprint = String(raw);
    const name = row.tool_name ? String(row.tool_name) : fingerprint;
    if (name === fingerprint) return `<span class="v-text v-mono" title="Tool fingerprint">${escapeHtml(fingerprint)}</span>`;
    const label = `<span class="v-text" title="Tool fingerprint ${escapeHtml(fingerprint)}">${escapeHtml(name)}</span>`;
    return name === UNRECOGNISED_TOOL ? `${label} <span class="v-mono v-tool-fp">${escapeHtml(fingerprint)}</span>` : label;
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
    const cls = (value) => vocabClass(vocabKey(column.key, value));
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

/**
 * A table. `groupBy` names a column whose value heads a run of rows instead of repeating in each;
 * `rowHref` names the column holding the address a whole row opens. A row that names a submission
 * opens it regardless: the event screen is reached from a row, not typed.
 */
export function renderTable(table) {
  const numeric = (c) => MEASURE_KINDS.includes(c.kind);
  const columns = table.columns.filter((c) => c.key !== table.groupBy);
  const maxima = new Map(columns.filter(numeric).map((c) => [
    c.key,
    Math.max(0, ...table.rows.map(({ row }) => Number(row[c.key]) || 0)),
  ]));
  const head = columns.map((c) => `<th scope="col"${numeric(c) ? ' class="num"' : ''}>${escapeHtml(c.label)}</th>`).join('');
  let group = null;
  const body = table.rows.map(({ row }) => {
    const cells = columns.map((c) => `<td${numeric(c) ? ' class="num"' : ''}>${renderCell(row, c, maxima.get(c.key))}</td>`).join('');
    let heading = '';
    if (table.groupBy) {
      const value = row[table.groupBy] === null || row[table.groupBy] === undefined ? '' : String(row[table.groupBy]);
      if (value !== group) {
        group = value;
        heading = `<tr class="row-group"><th scope="rowgroup" colspan="${columns.length}">${value === '' ? '<span class="v-absent">—</span>' : escapeHtml(value)}</th></tr>`;
      }
    }
    let href = null;
    if (typeof row.submission_id === 'string' && row.submission_id !== '') {
      const hint = row.received_at ? `&received_at_hint=${encodeURIComponent(row.received_at)}` : '';
      href = `#event?submission_id=${encodeURIComponent(row.submission_id)}${hint}`;
    } else if (table.rowHref && typeof row[table.rowHref] === 'string' && row[table.rowHref] !== '') {
      href = row[table.rowHref];
    }
    return heading + (href ? `<tr class="row-link" tabindex="0" data-href="${escapeHtml(href)}">${cells}</tr>` : `<tr>${cells}</tr>`);
  }).join('');
  const empty = table.rows.length === 0
    ? `<tr class="row-empty"><td colspan="${columns.length}">${escapeHtml(table.emptyText ?? 'No rows.')}</td></tr>`
    : '';
  const count = table.rows.length > 0 ? `<span class="block-count">${table.rows.length}</span>` : '';
  const more = table.href ? `<a class="block-link" href="${escapeHtml(table.href)}">${escapeHtml(table.moreLabel ?? 'View all')}</a>` : '';
  return `<section class="table-block card" id="${escapeHtml(blockId(table.title))}"><div class="card-core"><header class="block-head"><h3>${escapeHtml(table.title)}</h3>${renderInfo(table.info)}${count}${more}</header>`
    + (table.summary ? `<p class="block-summary">${escapeHtml(table.summary)}</p>` : '')
    + (table.breakdowns === false ? '' : renderBreakdowns(table))
    + `<div class="table-scroll"><table><thead><tr>${head}</tr></thead><tbody>${body}${empty}</tbody></table></div></div></section>`;
}

/** A series: one column per bucket, with the axis scaled to the largest value. */
export function renderSeries(series) {
  if (!series) return '';
  const open = (inner) => `<section class="series-block card" id="${escapeHtml(blockId(series.title))}"><div class="card-core"><header class="block-head"><h3>${escapeHtml(series.title)}</h3></header>`
    + `${series.summary ? `<p class="block-summary">${escapeHtml(series.summary)}</p>` : ''}${inner}</div></section>`;
  if (series.unavailable) return open(`<p class="series-unavailable">No chart: ${escapeHtml(series.reason)}</p>`);
  if (series.points.length === 0) return open('<p class="series-unavailable">No points.</p>');
  const max = Math.max(1, ...series.points.map((p) => (p.value.kind === 'number' ? p.value.value : 0)));
  // A point with parts is drawn as its parts stacked, first at the foot, and its tip names each.
  const stacked = series.points.some((p) => Array.isArray(p.parts));
  const bars = series.points.map((p, i) => {
    const label = formatInstant(p.bucket).slice(0, 10) || 'total';
    const delay = `--i:${Math.min(i, 24)}`;
    const height = Math.max(1, Math.round((p.value.value / max) * 100));
    const parts = (p.parts ?? []).filter((part) => part.value > 0);
    const tip = `${label} · ${formatCount(p.value.value)}${parts.length > 0 ? ` (${parts.map((part) => `${part.label} ${formatCount(part.value)}`).join(', ')})` : ''}`;
    const fill = stacked
      ? `<span class="bar-stack" style="--h:${height}">${parts.map((part) => `<span class="bar-seg ${vocabClass(part.key)}" style="flex-grow:${part.value}"></span>`).join('')}</span>`
      : `<span class="bar-fill" style="--h:${height}"></span>`;
    return `<div class="bar" style="${delay}" tabindex="0" data-tip="${escapeHtml(tip)}">${fill}<span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
  }).join('');
  const axis = `<div class="axis" aria-hidden="true"><span>${escapeHtml(formatCount(max))}</span><span>${escapeHtml(formatCount(Math.round(max / 2)))}</span><span>0</span></div>`;
  const legend = stacked
    ? `<ul class="dist-legend series-legend">${(series.points[0].parts ?? []).map((part) => `<li><span class="dist-dot ${vocabClass(part.key)}" aria-hidden="true"></span>${escapeHtml(part.label)}</li>`).join('')}</ul>`
    : '';
  return open(`<div class="chart">${axis}<div class="bars${series.points.length <= 3 ? ' bars-few' : ''}${stacked ? ' bars-stacked' : ''}">${bars}</div></div>${legend}`);
}

/**
 * The shape of a series in a small space: one line, a wash under it, a dot on the latest point.
 * The values are in its label for a reader who cannot see it; the chart below it carries them in full.
 */
export function renderSparkline(points, { width = 96, height = 28 } = {}) {
  if (!Array.isArray(points) || points.length < 2) return '';
  const values = points.map((p) => Math.max(0, Number(p.value) || 0));
  const max = Math.max(1, ...values);
  const pad = 3.5;
  const x = (i) => (pad + (i / (values.length - 1)) * (width - pad * 2)).toFixed(1);
  const y = (v) => (height - pad - (v / max) * (height - pad * 2)).toFixed(1);
  const line = `M${values.map((v, i) => `${x(i)},${y(v)}`).join(' L')}`;
  const floor = (height - pad).toFixed(1);
  const area = `${line} L${x(values.length - 1)},${floor} L${x(0)},${floor} Z`;
  const last = values.length - 1;
  const label = points.map((p, i) => `${formatInstant(p.bucket).slice(0, 10)}: ${formatCount(values[i])}`).join(', ');
  return `<svg class="spark" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="${escapeHtml(label)}">`
    + `<path class="spark-area" d="${area}"/><path class="spark-line" d="${line}"/>`
    + `<circle class="spark-end" cx="${x(last)}" cy="${y(values[last])}" r="3"/></svg>`;
}

const vocabClass = (key) => `v-vocab-${escapeHtml(String(key ?? '').replace(/[^a-z_]/gi, ''))}`;
/** A finding's severity is keyed apart from the other vocabularies: "high" is a serious state here, where elsewhere it is a strong signal. */
const vocabKey = (column, value) => (column === 'severity' ? `severity_${value}` : value);

/** The element id a block is reached by on its own page, from its title: a tile jumps to it. */
export const blockId = (title) => `b-${String(title ?? '').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')}`;

/** An (i) beside a label: what the figure counts, shown on hover or focus and read by assistive tech as it is. */
function renderInfo(text) {
  if (!text) return '';
  return '<span class="tip" tabindex="0"><svg class="tip-icon" viewBox="0 0 16 16" aria-hidden="true"><circle cx="8" cy="4.4" r="1.1" fill="currentColor"/><path d="M8 7v5.2" stroke="currentColor" stroke-width="1.6" stroke-linecap="round"/></svg>'
    + `<span class="tip-text" role="tooltip">${escapeHtml(text)}</span></span>`;
}

/** The attributes that make an element open a block on the same page: a link in all but address. */
const jumpAttrs = (title) => ` role="link" tabindex="0" data-jump="${escapeHtml(blockId(title))}"`;

/**
 * A block's frame: the same panel as a table, with its title, what it counts, its count and a link
 * to the whole. A block with a `cardHref` is itself the link, so it holds no other; one with a
 * `cardJump` opens the block it names on the same page.
 */
function blockFrame(kind, block, inner, { count = 0, classes = '' } = {}) {
  const id = ` id="${escapeHtml(blockId(block.title))}"`;
  const n = count > 0 ? `<span class="block-count">${count}</span>` : '';
  const note = block.note ? `<span class="table-note">${escapeHtml(block.note)}</span>` : '';
  const head = `<h3>${escapeHtml(block.title)}</h3>${renderInfo(block.info)}${n}${note}`;
  if (block.cardHref) {
    return `<a class="${kind}-block card card-link${classes}" href="${escapeHtml(block.cardHref)}"${id}><div class="card-core"><header class="block-head">${head}</header>${inner}</div></a>`;
  }
  if (block.cardJump) {
    return `<div class="${kind}-block card card-link${classes}"${jumpAttrs(block.cardJump)}${id}><div class="card-core"><header class="block-head">${head}</header>${inner}</div></div>`;
  }
  const more = block.href ? `<a class="block-link" href="${escapeHtml(block.href)}">${escapeHtml(block.moreLabel ?? 'View all')}</a>` : '';
  return `<section class="${kind}-block card${classes}"${id}><div class="card-core"><header class="block-head">${head}${more}</header>${inner}</div></section>`;
}

/**
 * Part-to-whole: the whole as a figure, one bar, and a legend that carries every part's count and
 * share in words. A part that counts nothing draws nothing; it stays in the legend as a zero.
 */
export function renderShare(block) {
  const parts = block.parts ?? [];
  const drawn = parts.filter((p) => Number(p.count) > 0);
  if (drawn.length === 0) return blockFrame('share', block, `<p class="block-empty">${escapeHtml(block.emptyText ?? 'Nothing to show.')}</p>`);
  const figure = `<p class="share-total"><span class="tile-value">${escapeHtml(formatCount(block.total))}</span>${block.totalLabel ? `<span class="share-total-label">${escapeHtml(block.totalLabel)}</span>` : ''}</p>`;
  const bar = drawn.map((p) => `<span class="dist-seg ${vocabClass(p.key)}" style="flex-grow:${Number(p.count)}" title="${escapeHtml(p.label)}: ${escapeHtml(formatCount(p.count))} (${escapeHtml(formatPercent(p.share))})"></span>`).join('');
  const legend = parts.map((p) => `<li><span class="dist-dot ${vocabClass(p.key)}" aria-hidden="true"></span>${escapeHtml(p.label)}<span class="dist-n">${escapeHtml(formatCount(p.count))}</span><span class="dist-pct">${escapeHtml(formatPercent(p.share))}</span></li>`).join('');
  const said = drawn.map((p) => `${p.label} ${formatCount(p.count)} (${formatPercent(p.share)})`).join(', ');
  return blockFrame('share', block, `<div class="share">${figure}<div class="dist-bar share-bar" role="img" aria-label="${escapeHtml(said)}">${bar}</div><ul class="dist-legend">${legend}</ul></div>`);
}

/** One fact beside a ranked item: the number first, then what it counts. */
function renderFact(fact) {
  const value = fact.value?.kind === 'number'
    ? `<span class="v-number">${escapeHtml(fact.value.text ?? formatCount(fact.value.value))}</span>`
    : renderValue({ kind: 'absent' });
  return `<span class="rank-fact">${value} ${escapeHtml(fact.label)}</span>`;
}

/**
 * A ranked list: each item a link to its own screen, its value drawn against the largest, its
 * share said in words, the facts beside it and the shape of its series. The list is its own
 * table view: every number is in the text.
 */
export function renderRanking(block) {
  const items = block.items ?? [];
  if (items.length === 0) return blockFrame('rank', block, `<p class="block-empty">${escapeHtml(block.emptyText ?? 'Nothing to rank.')}</p>`);
  const max = Math.max(1, ...items.map((item) => (item.value?.kind === 'number' ? item.value.value : 0)));
  // A window served in one bucket has no shape to draw, so the list has no trend column.
  const trends = items.some((item) => Array.isArray(item.trend) && item.trend.length > 1);
  const head = '<div class="rank-head" aria-hidden="true">'
    + `<span>${escapeHtml(block.itemLabel ?? 'Name')}</span><span></span><span class="rank-num">${escapeHtml(block.valueLabel ?? 'Value')}</span>`
    + `<span>${escapeHtml((block.metaLabels ?? []).join(' · '))}</span>${trends ? `<span>${escapeHtml(block.trendLabel ?? '')}</span>` : ''}</div>`;
  const rows = items.map((item, i) => {
    const value = item.value?.kind === 'number' ? item.value.value : null;
    const bar = value === null
      ? '<span class="rank-bar rank-bar-absent" aria-hidden="true"></span>'
      : `<span class="rank-bar" aria-hidden="true"><span class="rank-fill" style="--w:${Math.max(2, Math.round((value / max) * 100))}"></span></span>`;
    const number = value === null
      ? renderValue({ kind: 'absent' })
      : `<span class="v-number">${escapeHtml(item.value.text ?? formatCount(value))}</span>`
        + (typeof item.share === 'number' ? `<span class="rank-share">${escapeHtml(formatPercent(item.share))}</span>` : '');
    const name = `<span class="rank-name${item.mono ? ' v-mono' : ''}"${item.title ? ` title="${escapeHtml(item.title)}"` : ''}>${escapeHtml(item.label)}</span>`;
    const sub = item.sublabel ? `<span class="rank-sub${item.subMono ? ' v-mono' : ''}">${escapeHtml(item.sublabel)}</span>` : '';
    const tag = item.href ? 'a' : 'div';
    return `<li class="rank-item" style="--i:${Math.min(i, 24)}"><${tag} class="rank-link"${item.href ? ` href="${escapeHtml(item.href)}"` : ''}>`
      + `<span class="rank-who">${name}${chip(item.chip)}${sub}</span>${bar}<span class="rank-value">${number}</span>`
      + `<span class="rank-meta">${(item.meta ?? []).map(renderFact).join('')}</span>`
      + (trends ? `<span class="rank-trend">${renderSparkline(item.trend)}</span>` : '')
      + `</${tag}></li>`;
  }).join('');
  const summary = block.summary ? `<p class="block-summary">${escapeHtml(block.summary)}</p>` : '';
  return blockFrame('rank', block, `${summary}${head}<ol class="rank">${rows}</ol>`, { count: items.length, classes: trends ? '' : ' rank-flat' });
}

/** One stage of a funnel: what it counts, the count, its share of the stage before, and its change. */
function renderStage(stage) {
  return `<div class="funnel-step funnel-${escapeHtml(stage.key)}"><span class="funnel-label">${escapeHtml(stage.label)}</span>`
    + `<span class="funnel-value">${renderValue(stage.value)}</span>`
    + (stage.detail ? `<span class="funnel-detail">${escapeHtml(stage.detail)}</span>` : '')
    + (stage.change ? `<span class="funnel-change funnel-${escapeHtml(stage.change.direction)}">${escapeHtml(stage.change.text)}</span>` : '')
    + '</div>';
}

/**
 * A funnel: each stage narrows the one before it, and the last splits into outcomes, drawn as one
 * bar over them. Every number is in the text.
 */
export function renderFunnel(block) {
  if (block.emptyText) return blockFrame('funnel', block, `<p class="block-empty">${escapeHtml(block.emptyText)}</p>`);
  const arrow = '<span class="funnel-arrow" aria-hidden="true"></span>';
  const outcomes = block.outcomes ?? [];
  const drawn = outcomes.filter((o) => Number(o.count) > 0);
  const bar = drawn.length > 0
    ? `<div class="dist-bar funnel-bar" aria-hidden="true">${drawn.map((o) => `<span class="dist-seg ${vocabClass(o.key)}" style="flex-grow:${Number(o.count)}"></span>`).join('')}</div>`
    : '';
  return blockFrame('funnel', block, `<div class="funnel">${(block.stages ?? []).map(renderStage).join(arrow)}${arrow}`
    + `<div class="funnel-outcomes">${bar}<div class="funnel-split">${outcomes.map(renderStage).join('')}</div></div></div>`);
}

/** A block of a screen, by its kind. */
function renderBlock(block) {
  switch (block?.kind) {
    case 'ranking': return renderRanking(block);
    case 'funnel': return renderFunnel(block);
    case 'share': return renderShare(block);
    case 'series': return renderSeries(block);
    case 'table': return renderTable(block);
    default: return '';
  }
}

export function renderBanner(banner) {
  return `<div class="banner banner-${escapeHtml(banner.level)}" role="status">`
    + `<strong>${escapeHtml(banner.title)}</strong><span>${escapeHtml(banner.text)}</span></div>`;
}

/** Partial coverage is a fact about every figure on the page: said once, in one quiet line above them. */
const quiet = (banner) => banner.level === 'warning' && banner.about === 'coverage';

function renderStatusLine(banners) {
  const lines = banners.filter(quiet);
  if (lines.length === 0) return '';
  return `<p class="data-status" role="status">${lines.map((b) => `<span class="data-status-item"><strong>${escapeHtml(b.title)}</strong> ${escapeHtml(b.text)}</span>`).join('')}</p>`;
}

/** A tile's split: one bar and its counts, sized by the counts themselves. A part with an address opens the rows it counts. */
function renderSplit(split) {
  if (!Array.isArray(split) || split.length === 0) return '';
  const legend = (part) => {
    const text = `${escapeHtml(part.label)}<span class="dist-n">${escapeHtml(formatCount(part.count))}</span>`;
    return part.href && (Number(part.count) || 0) > 0 ? `<a class="dist-link" href="${escapeHtml(part.href)}">${text}</a>` : text;
  };
  // A part that counts nothing draws nothing and opens nothing; it stays in the legend as a zero.
  const drawn = split.filter((part) => (Number(part.count) || 0) > 0);
  return '<div class="dist-bar tile-split" aria-hidden="true">'
    + drawn.map((part) => `<span class="dist-seg ${vocabClass(part.key)}" style="flex-grow:${Number(part.count)}"></span>`).join('')
    + '</div><ul class="dist-legend">'
    + split.map((part) => `<li><span class="dist-dot ${vocabClass(part.key)}" aria-hidden="true"></span>${legend(part)}</li>`).join('')
    + '</ul>';
}

/**
 * A tile. One with an address is a link, unless its split already holds links: then the address
 * is a line of its own, because a link inside a link is not a thing a browser can follow. One with
 * a `jump` opens the block it names on the same page. Its `info` is the (i) beside its label.
 */
export function renderTile(t) {
  const nested = (t.split ?? []).some((part) => part.href && (Number(part.count) || 0) > 0);
  const wraps = Boolean(t.href) && !nested;
  const jumps = Boolean(t.jump) && !wraps && !nested;
  const tag = wraps ? 'a' : 'div';
  const open = wraps ? `<a class="tile card tile-link" href="${escapeHtml(t.href)}">`
    : jumps ? `<div class="tile card tile-link"${jumpAttrs(t.jump)}>`
      : '<div class="tile card">';
  const trend = t.trend ? `<span class="tile-spark">${renderSparkline(t.trend, { width: 160, height: 36 })}</span>` : '';
  // A tile that is itself the link says where it goes on its last line; one whose split holds the
  // links gets a link of its own.
  const more = !t.href ? ''
    : wraps ? (t.moreLabel ? `<span class="tile-more">${escapeHtml(t.moreLabel)}</span>` : '')
      : `<a class="tile-more" href="${escapeHtml(t.href)}">${escapeHtml(t.moreLabel ?? 'View all')}</a>`;
  return `${open}<div class="card-core"><span class="tile-label">${escapeHtml(t.label)}${renderInfo(t.info)}</span>`
    + `<span class="tile-figure"><span class="tile-value">${renderValue(t.value)}</span>${trend}</span>`
    + renderSplit(t.split)
    + (t.note ? `<span class="tile-note">${escapeHtml(t.note)}</span>` : '')
    + more
    + `</div></${tag}>`;
}

export function renderNeeds(view) {
  return `<section class="needs"><h2>${escapeHtml(view.title)}</h2><p>${escapeHtml(view.subtitle ?? 'This screen needs an input before it can ask its question.')}</p></section>`;
}

/**
 * The Users search bar. It is the one input a reader can type: there is no list of users to pick
 * from, so a user is reached by reference and shown on the same screen.
 */
function renderSearch(search) {
  if (!search) return '';
  return '<form class="needs-form search-form" data-screen="person" role="search">'
    + `<input name="subject" type="search" aria-label="Find a user" placeholder="Search by name, or enter a user reference" value="${escapeHtml(search.value ?? '')}" autocomplete="off" spellcheck="false">`
    + '<button type="submit">Search</button></form>';
}

/** A screen's filter fields, each typed: the form puts what is given in the address. */
function renderForm(form) {
  if (!form || !Array.isArray(form.fields) || form.fields.length === 0) return '';
  const fields = form.fields.map((f) => `<label class="form-field"><span class="filter-label">${escapeHtml(f.label)}</span>`
    + `<input class="dp-input" name="${escapeHtml(f.name)}" type="search" value="${escapeHtml(f.value ?? '')}" placeholder="${escapeHtml(f.placeholder ?? '')}" autocomplete="off" spellcheck="false"></label>`).join('');
  return `<form class="filter-form card" data-screen="${escapeHtml(form.screen)}" role="search"><div class="card-core">${fields}`
    + `<div class="form-actions"><button type="submit" class="btn btn-small">${escapeHtml(form.submit ?? 'Filter')}</button>`
    + (form.clearHref ? `<a class="btn btn-small btn-quiet" href="${escapeHtml(form.clearHref)}">Clear</a>` : '')
    + '</div></div></form>';
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

/** A destination's pages as tabs: links, the open one marked. */
export function renderTabs(tabs) {
  if (!tabs || !Array.isArray(tabs.items) || tabs.items.length === 0) return '';
  return `<nav class="page-tabs" aria-label="${escapeHtml(tabs.label ?? 'Pages')}">${tabs.items.map((t) => (
    `<a class="page-tab" href="${escapeHtml(t.href)}"${t.id === tabs.current ? ' aria-current="page"' : ''}>${escapeHtml(t.label)}</a>`
  )).join('')}</nav>`;
}

/**
 * A page's head: its title, the tools at its right (the window, an action), then its tabs when it
 * has them, and what the open page shows. `tools` and `badges` are HTML their caller escaped.
 */
export function renderPageHead({ title, eyebrow = null, tabs = null, question = null, subtitle = null, tools = '', badges = '' }) {
  const lede = (question ? `<p class="question">${escapeHtml(question)}</p>` : '')
    + (subtitle ? `<p class="subtitle">${escapeHtml(subtitle)}</p>` : '');
  const tabbed = Boolean(tabs && tabs.items?.length);
  return '<header class="screen-head"><div class="screen-title">'
    + (eyebrow ? `<span class="eyebrow">${escapeHtml(eyebrow)}</span>` : '')
    + `<h2>${escapeHtml(title)}${badges}</h2>`
    + (tabbed ? '' : lede)
    + '</div>'
    + (tools ? `<div class="screen-tools">${tools}</div>` : '')
    + '</header>'
    + (tabbed ? renderTabs(tabs) + (lede ? `<div class="screen-lede">${lede}</div>` : '') : '');
}

/**
 * A whole screen: the summary first (the lead blocks, the tiles, the filters, then the ranked and
 * shared blocks), the detail after (the chart and the tables). A page can add a section above the
 * summary and one below the tables, as `shell.before` and `shell.after`, HTML its caller escaped.
 * Partial coverage is one line under the title; a stale aggregate, a refusal or a state that
 * cannot say keeps its banner.
 */
export function renderScreen(view, shell = {}) {
  if (view.needsInput && !view.search) return renderNeeds(view);
  const status = renderStatusLine(view.banners ?? []);
  const banners = (view.banners ?? []).filter((b) => !quiet(b)).map(renderBanner).join('');
  const tiles = (view.tiles ?? []).length > 0 ? `<div class="tiles">${view.tiles.map(renderTile).join('')}</div>` : '';
  const lead = (view.lead ?? []).map(renderBlock).join('');
  const blocks = (view.blocks ?? []).map(renderBlock).join('');
  const tables = (view.tables ?? []).map(renderTable).join('');
  const filters = renderFilters(view.filters);
  const series = (view.series ?? []).map(renderSeries).join('');
  // The reading notes are one click away rather than under every table: they matter, and they are
  // the same on every visit. Where the screen reads from is one of them.
  const noteList = [...(view.notes ?? []), ...(view.sourceLabel ? [`Reads from ${view.sourceLabel}.`] : [])];
  const notes = noteList.length > 0
    ? `<details class="notes"><summary>About this data<span class="notes-count">${noteList.length}</span></summary><ul>${noteList.map((n) => `<li>${escapeHtml(n)}</li>`).join('')}</ul></details>`
    : '';
  const badges = (view.badges ?? []).length > 0 ? ` <span class="screen-badges">${view.badges.map(chip).join('')}</span>` : '';
  // A screen's own control, such as Teams' "+ Add a Team": HTML its caller rendered and escaped.
  const tools = (shell.tool ? `<div class="screen-tool">${shell.tool}</div>` : '')
    + (shell.presets ? `<div class="screen-switches">${renderPresets(shell.presets)}</div>` : '');
  const header = renderPageHead({
    title: shell.title ?? view.title, eyebrow: shell.eyebrow, tabs: shell.tabs, question: view.question, subtitle: view.subtitle, tools, badges,
  });
  const actions = (view.actions ?? []).length > 0
    ? `<nav class="screen-actions">${view.actions.map((a) => `<a class="btn btn-small" href="${escapeHtml(a.href)}">${escapeHtml(a.label)}</a>`).join('')}</nav>`
    : '';
  return `<article class="screen">${header}${renderSearch(view.search)}${actions}${status}${banners}${shell.before ?? ''}${renderForm(view.form)}${lead}${tiles}${filters}${blocks}${series}${tables}${shell.after ?? ''}${notes}</article>`;
}

/** Thin-line icons for the navigation: one stroke weight, no fills. */
const NAV_ICONS = Object.freeze({
  overview: '<circle cx="12" cy="12" r="8.5"/><path d="M12 12 16.5 8"/><path d="M12 3.5v2M20.5 12h-2M5.5 12h-2"/>',
  rules: '<path d="M10 6.5h10M10 12h10M10 17.5h10"/><path d="M4 6.5h2.5M4 12h2.5M4 17.5h2.5"/>',
  search: '<circle cx="11" cy="11" r="6.5"/><path d="m16 16 4.5 4.5"/>',
  usage: '<path d="M4 19.5V10M9.5 19.5v-15M15 19.5v-7M20.5 19.5V7"/>',
  settings: '<path d="M4 7h9M17 7h3M4 17h3M11 17h9"/><circle cx="15" cy="7" r="2"/><circle cx="9" cy="17" r="2"/>',
  archive: '<rect x="3.5" y="4.5" width="17" height="4" rx="1"/><path d="M5 8.5v10a1.5 1.5 0 0 0 1.5 1.5h11a1.5 1.5 0 0 0 1.5-1.5v-10"/><path d="M10 12.5h4"/>',
});

function navIcon(name) {
  const body = NAV_ICONS[name];
  return body ? `<svg class="nav-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.25" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>` : '';
}

/**
 * The navigation, as the contents of the page's nav element: one link per destination, the one
 * holding the open page marked. The destinations kept at the foot sit apart from the rest.
 */
export function renderNav(items, current) {
  let footed = false;
  return items.map((item) => {
    const active = item.id === current ? ' class="active" aria-current="page"' : '';
    const link = `<a href="${escapeHtml(item.href ?? `#${item.id}`)}"${active}>${navIcon(item.icon)}<span>${escapeHtml(item.label)}</span></a>`;
    if (!item.foot || footed) return link;
    footed = true;
    return `<span class="nav-spacer" aria-hidden="true"></span>${link}`;
  }).join('');
}
