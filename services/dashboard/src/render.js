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
      return `<span class="v-vocab v-vocab-${escapeHtml(String(value.text).replace(/[^a-z_]/gi, ''))}">${escapeHtml(value.text)}</span>`;
    case 'absent':
    default:
      return '<span class="v-absent" title="Not carried by this response">—</span>';
  }
}

const MEASURE_KINDS = Object.freeze(['measure', 'measure-bytes', 'measure-score', 'count']);

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
    return raw === null || raw === undefined ? '<span class="v-vocab v-vocab-unknown">unknown</span>' : renderValue({ kind: 'vocab', text: raw });
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
  return `<section class="table-block card"><div class="card-core"><header class="block-head"><h3>${escapeHtml(table.title)}</h3>${count}${more}</header>`
    + (table.breakdowns === false ? '' : renderBreakdowns(table))
    + `<div class="table-scroll"><table><thead><tr>${head}</tr></thead><tbody>${body}${empty}</tbody></table></div></div></section>`;
}

/** A series: one column per bucket, with the axis scaled to the largest value. */
export function renderSeries(series) {
  if (!series) return '';
  const open = (inner) => `<section class="series-block card"><div class="card-core"><header class="block-head"><h3>${escapeHtml(series.title)}</h3></header>${inner}</div></section>`;
  if (series.unavailable) return open(`<p class="series-unavailable">No chart: ${escapeHtml(series.reason)}</p>`);
  if (series.points.length === 0) return open('<p class="series-unavailable">No points.</p>');
  const max = Math.max(1, ...series.points.map((p) => (p.value.kind === 'number' ? p.value.value : 0)));
  const bars = series.points.map((p, i) => {
    const label = formatInstant(p.bucket).slice(0, 10) || 'total';
    const delay = `--i:${Math.min(i, 24)}`;
    const height = Math.max(1, Math.round((p.value.value / max) * 100));
    return `<div class="bar" style="${delay}" tabindex="0" data-tip="${escapeHtml(label)} · ${escapeHtml(formatCount(p.value.value))}"><span class="bar-fill" style="--h:${height}"></span><span class="bar-label">${escapeHtml(label.slice(5))}</span></div>`;
  }).join('');
  const axis = `<div class="axis" aria-hidden="true"><span>${escapeHtml(formatCount(max))}</span><span>${escapeHtml(formatCount(Math.round(max / 2)))}</span><span>0</span></div>`;
  return open(`<div class="chart">${axis}<div class="bars${series.points.length <= 3 ? ' bars-few' : ''}">${bars}</div></div>`);
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

/** A block's frame: the same panel as a table, with its title, count and link to the whole. */
function blockFrame(kind, block, inner, { count = 0, classes = '' } = {}) {
  const n = count > 0 ? `<span class="block-count">${count}</span>` : '';
  const note = block.note ? `<span class="table-note">${escapeHtml(block.note)}</span>` : '';
  const more = block.href ? `<a class="block-link" href="${escapeHtml(block.href)}">${escapeHtml(block.moreLabel ?? 'View all')}</a>` : '';
  return `<section class="${kind}-block card${classes}"><div class="card-core"><header class="block-head"><h3>${escapeHtml(block.title)}</h3>${n}${note}${more}</header>${inner}</div></section>`;
}

/** Part-to-whole: one bar, and a legend that carries every part's count and share in words. */
export function renderShare(block) {
  const parts = (block.parts ?? []).filter((p) => Number(p.count) > 0);
  if (parts.length === 0) return blockFrame('share', block, `<p class="block-empty">${escapeHtml(block.emptyText ?? 'Nothing to show.')}</p>`);
  // One part is the whole: a sentence says so, and a bar of one colour is not drawn.
  if (parts.length === 1 && parts[0].phrase) {
    return blockFrame('share', block, `<p class="share-one">All <span class="v-number">${escapeHtml(formatCount(parts[0].count))}</span> submissions went to ${escapeHtml(parts[0].phrase)}.</p>`);
  }
  const bar = parts.map((p) => `<span class="dist-seg ${vocabClass(p.key)}" style="flex-grow:${Math.max(0, Number(p.count) || 0)}" title="${escapeHtml(p.label)}: ${escapeHtml(formatCount(p.count))} (${escapeHtml(formatPercent(p.share))})"></span>`).join('');
  const legend = parts.map((p) => `<li><span class="dist-dot ${vocabClass(p.key)}" aria-hidden="true"></span>${escapeHtml(p.label)}<span class="dist-n">${escapeHtml(formatCount(p.count))}</span><span class="dist-pct">${escapeHtml(formatPercent(p.share))}</span></li>`).join('');
  const said = parts.map((p) => `${p.label} ${formatPercent(p.share)}`).join(', ');
  return blockFrame('share', block, `<div class="share"><div class="dist-bar share-bar" role="img" aria-label="${escapeHtml(said)}">${bar}</div><ul class="dist-legend">${legend}</ul></div>`);
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
  return blockFrame('rank', block, `${head}<ol class="rank">${rows}</ol>`, { count: items.length, classes: trends ? '' : ' rank-flat' });
}

/** A block of a screen, by its kind. */
function renderBlock(block) {
  switch (block?.kind) {
    case 'ranking': return renderRanking(block);
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
 * is a line of its own, because a link inside a link is not a thing a browser can follow.
 */
export function renderTile(t) {
  const nested = (t.split ?? []).some((part) => part.href && (Number(part.count) || 0) > 0);
  const wraps = Boolean(t.href) && !nested;
  const tag = wraps ? 'a' : 'div';
  const trend = t.trend ? `<span class="tile-spark">${renderSparkline(t.trend, { width: 160, height: 36 })}</span>` : '';
  // A tile that is itself the link says where it goes on its last line; one whose split holds the
  // links gets a link of its own.
  const more = !t.href ? ''
    : wraps ? (t.moreLabel ? `<span class="tile-more">${escapeHtml(t.moreLabel)}</span>` : '')
      : `<a class="tile-more" href="${escapeHtml(t.href)}">${escapeHtml(t.moreLabel ?? 'View all')}</a>`;
  return `<${tag} class="tile card${wraps ? ' tile-link' : ''}"${wraps ? ` href="${escapeHtml(t.href)}"` : ''}><div class="card-core"><span class="tile-label">${escapeHtml(t.label)}</span>`
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
 * The Users search bar. It is the one input a reader can type: there is no list of people to pick
 * from, so a person is reached by reference and shown on the same screen.
 */
function renderSearch(search) {
  if (!search) return '';
  return '<form class="needs-form search-form" data-screen="person" role="search">'
    + `<input name="subject" type="search" aria-label="Find a person" placeholder="Search by name, or enter a user reference" value="${escapeHtml(search.value ?? '')}" autocomplete="off" spellcheck="false">`
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

/**
 * A whole screen: the summary first (tiles, then the ranked and shared blocks), the detail after
 * (the chart, the filters, the tables). Partial coverage is one line under the title; a stale
 * aggregate, a refusal or a state that cannot say keeps its banner.
 */
export function renderScreen(view, shell = {}) {
  if (view.needsInput && !view.search) return renderNeeds(view);
  const status = renderStatusLine(view.banners ?? []);
  const banners = (view.banners ?? []).filter((b) => !quiet(b)).map(renderBanner).join('');
  const tiles = (view.tiles ?? []).length > 0 ? `<div class="tiles">${view.tiles.map(renderTile).join('')}</div>` : '';
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
  const header = '<header class="screen-head"><div class="screen-title">'
    + (shell.eyebrow ? `<span class="eyebrow">${escapeHtml(shell.eyebrow)}</span>` : '')
    + `<h2>${escapeHtml(view.title)}${badges}</h2>`
    + (view.question ? `<p class="question">${escapeHtml(view.question)}</p>` : '')
    + (view.subtitle ? `<p class="subtitle">${escapeHtml(view.subtitle)}</p>` : '')
    + '</div><div class="screen-tools"><div class="screen-switches">'
    + renderPresets(shell.switch)
    + renderPresets(shell.presets)
    + '</div></div></header>';
  const actions = (view.actions ?? []).length > 0
    ? `<nav class="screen-actions">${view.actions.map((a) => `<a class="btn btn-small" href="${escapeHtml(a.href)}">${escapeHtml(a.label)}</a>`).join('')}</nav>`
    : '';
  return `<article class="screen">${header}${renderSearch(view.search)}${actions}${status}${banners}${tiles}${blocks}${series}${filters}${tables}${notes}</article>`;
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
