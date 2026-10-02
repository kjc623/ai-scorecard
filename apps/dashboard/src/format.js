// format.js — presentation of a value that already has a state.
//
// Nothing here decides whether a value exists. A suppressed cell is not a number and never reaches
// these functions as one; see states.js for that decision.

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

/** A score is a proportion; two decimals is all the information a MaxScore has. */
export function formatScore(value) {
  if (typeof value !== 'number' || !Number.isFinite(value)) return '—';
  return value.toFixed(2);
}

/** A share, with the denominator the caller already has. Never a bare percentage. */
export function formatShare(numerator, denominator) {
  if (typeof numerator !== 'number' || typeof denominator !== 'number' || denominator <= 0) return '—';
  return `${Math.round((numerator / denominator) * 100)}%`;
}

/** An age in words: "3 min ago". `state` says whether that age is a fault. */
export function formatAge(fromIso, now = Date.now()) {
  if (!fromIso) return 'never';
  const ms = now - Date.parse(fromIso);
  if (!Number.isFinite(ms)) return 'unknown';
  if (ms < 0) return 'in the future';
  const seconds = Math.round(ms / 1000);
  if (seconds < 90) return `${seconds} s ago`;
  const minutes = Math.round(seconds / 60);
  if (minutes < 90) return `${minutes} min ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours} h ago`;
  return `${Math.round(hours / 24)} d ago`;
}

/** An instant, to the minute, in UTC and labelled as such. */
export function formatInstant(iso) {
  if (!iso) return '—';
  const ms = Date.parse(iso);
  if (!Number.isFinite(ms)) return '—';
  return `${new Date(ms).toISOString().slice(0, 16).replace('T', ' ')}Z`;
}

/** A day bucket, for a chart axis. */
export function formatDay(iso) {
  if (!iso) return '—';
  return String(iso).slice(0, 10);
}

/** A uuid, shortened for a table. Never shortened for a detail header. */
export function shortId(id, length = 8) {
  if (typeof id !== 'string') return '—';
  return id.length <= length ? id : `${id.slice(0, length)}…`;
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
