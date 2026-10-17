// format.js — presentation of a value that already has a state.
//
// Nothing here decides whether a value exists. An absent measure is not a number and never reaches
// these functions as one; see states.js for that decision.

/** A count with its noun: "1 person", "3 people". */
export function plural(value, one, many) {
  return `${formatCount(value)} ${Number(value) === 1 ? one : many}`;
}

/** Group thousands. Deterministic, locale-independent, so a test can assert it. */
export function formatCount(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  const sign = value < 0 ? '-' : '';
  const digits = Math.abs(Math.round(value)).toString();
  return sign + digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

/** A byte count in the unit a human reads, always with its unit and never with false precision. */
export function formatBytes(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let v = Math.abs(value);
  let unit = 0;
  while (v >= 1024 && unit < units.length - 1) {
    v /= 1024;
    unit += 1;
  }
  const rendered = unit === 0 ? String(Math.round(v)) : v.toFixed(v < 10 ? 1 : 0);
  return `${value < 0 ? '-' : ''}${rendered} ${units[unit]}`;
}

/** A share of a whole as a whole percentage; a share too small to round to one says so rather than reading as nothing. */
export function formatPercent(share) {
  if (typeof share !== 'number' || !Number.isFinite(share)) return '—';
  const pct = share * 100;
  if (pct > 0 && pct < 1) return '<1%';
  return `${Math.round(pct)}%`;
}

/** A rate per 1,000 to one decimal; a rate too small to round to a tenth says so rather than reading as none. */
export function formatRate(rate) {
  if (typeof rate !== 'number' || !Number.isFinite(rate)) return '—';
  if (rate > 0 && rate < 0.05) return '<0.1';
  return rate >= 1000 ? formatCount(rate) : rate.toFixed(1);
}

/** A score is a proportion; two decimals is all the information a MaxScore has. */
export function formatScore(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  return value.toFixed(2);
}

/** An instant, to the minute, in UTC and labelled as such. */
export function formatInstant(iso) {
  if (!iso) return '—';
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return '—';
  return `${new Date(ms).toISOString().slice(0, 16).replace('T', ' ')}Z`;
}

const MONTHS = Object.freeze(['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']);

/** An instant as a reader scans it in a list, "Oct 16 14:33", in UTC; the full form belongs on hover. */
export function formatWhen(iso) {
  if (!iso) return '—';
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return '—';
  const d = new Date(ms);
  const two = (n) => String(n).padStart(2, '0');
  return `${MONTHS[d.getUTCMonth()]} ${d.getUTCDate()} ${two(d.getUTCHours())}:${two(d.getUTCMinutes())}`;
}

/** A label list, summarised without losing the classes that are present. */
export function formatLabels(labels) {
  if (!Array.isArray(labels) || labels.length === 0) return '—';
  return labels
    .map((l) => (typeof l === 'string' ? l : `${l.class}${typeof l.score === 'number' ? ` (${formatScore(l.score)})` : ''}`))
    .join(', ');
}

/** A duration between two instants, in the largest unit that fits. */
export function formatDuration(fromIso, toIso) {
  if (!fromIso || !toIso) return '—';
  const ms = Date.parse(toIso) - Date.parse(fromIso);
  if (!Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 60_000) return `${Math.round(ms / 1000)} s`;
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)} min`;
  if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)} h`;
  return `${Math.round(ms / 86_400_000)} d`;
}
