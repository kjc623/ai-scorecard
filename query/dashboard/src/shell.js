// shell.js — what every page of the dashboard shares around its content: the navigation's
// shape, the reader's stored preferences, and the controls in the navigation panel.
//
// The dashboard (app.js) and the Explore page (explore-app.js) both boot through this, so the
// sidebar is one thing: the same groups, the same folding, the same theme and data-source switch.

/**
 * The navigation: five destinations, with the three usage screens under one group. Every other
 * screen is reached from a row, a switch on one of these, or the footer.
 */
export const NAV_GROUPS = Object.freeze([
  { group: null, icon: 'overview', ids: ['posture'], labels: { posture: 'Overview' } },
  { group: 'Usage', icon: 'usage', ids: ['tools', 'teams', 'person'], labels: { tools: 'Tools & data classes', teams: 'Teams', person: 'Users' } },
  { group: null, icon: 'collection', ids: ['devices'], labels: { devices: 'Devices' } },
  { group: null, icon: 'governance', ids: ['audit'], labels: { audit: 'Audit trail' } },
]);

const NAV_FOLDED = Object.freeze(NAV_GROUPS.filter((g) => g.folded).map((g) => g.group));

/**
 * The navigation as renderNav takes it. `page` prefixes each screen link, so a sibling page links
 * back into the dashboard; the Explore page sits second, as a top-level link, on both.
 */
export function shellNavItems({ page = '', query = '', questionOf = () => null } = {}) {
  const items = NAV_GROUPS.flatMap(({ group, icon, ids, labels }) => ids.map((id) => ({
    id, label: labels[id], question: questionOf(id), group, icon, groupIcon: icon, href: `${page}#${id}`,
  })));
  return [
    items[0],
    { id: 'explore', label: 'Search', href: `explore.html${query}`, icon: 'search', group: null },
    ...items.slice(1),
  ];
}

/** The group a screen sits under, for the eyebrow above its title. */
export function groupOf(screenId) {
  return NAV_GROUPS.find((g) => g.ids.includes(screenId))?.group ?? null;
}

/** Local preferences. Storage can be unavailable (a private window, a file:// page): then nothing is kept. */
export function readPref(document, key, fallback) {
  try {
    return document.defaultView?.localStorage?.getItem(key) ?? fallback;
  } catch {
    return fallback;
  }
}

export function writePref(document, key, value) {
  try {
    document.defaultView?.localStorage?.setItem(key, value);
  } catch {
    // Not kept. The page still works.
  }
}

/**
 * The shell's own controls: which navigation groups are folded, the theme, the data-source switch,
 * the mobile menu, and a row that opens the record it names. Every lookup is optional, so a test
 * document with none of these elements boots the same way.
 */
export function wireShell({ document, live, collapsed }) {
  const el = (id) => (typeof document.getElementById === 'function' ? document.getElementById(id) : null);
  const html = document.documentElement;

  // Theme: follow the system unless the reader chose. The choice is the only thing stored.
  const theme = readPref(document, 'sac.theme', '');
  if (html?.dataset && (theme === 'light' || theme === 'dark')) html.dataset.theme = theme;

  const source = el('source');
  if (source) {
    source.innerHTML = `<a class="seg-item" href="${live ? '.' : '#'}"${live ? '' : ' aria-current="true"'} data-source="sample">Sample</a>`
      + `<a class="seg-item" href="${live ? '#' : '?transport=live'}"${live ? ' aria-current="true"' : ''} data-source="live">Live</a>`;
  }

  if (typeof document.addEventListener !== 'function') return;

  // A folded group stays folded across screens and visits.
  document.addEventListener('toggle', (event) => {
    const group = event.target?.dataset?.group;
    if (!group) return;
    if (event.target.open) collapsed.delete(group);
    else collapsed.add(group);
    writePref(document, 'sac.nav.collapsed', [...collapsed].join('|'));
  }, true);

  document.addEventListener('click', (event) => {
    const target = event.target;
    if (typeof target?.closest !== 'function') return;
    if (target.closest('#theme')) {
      const dark = html.dataset.theme
        ? html.dataset.theme === 'dark'
        : document.defaultView?.matchMedia?.('(prefers-color-scheme: dark)').matches;
      html.dataset.theme = dark ? 'light' : 'dark';
      writePref(document, 'sac.theme', html.dataset.theme);
      return;
    }
    if (target.closest('#menu')) {
      document.body.classList.toggle('menu-open');
      return;
    }
    // Choosing a destination closes the mobile menu.
    if (target.closest('.nav a')) document.body.classList.remove('menu-open');
    const sourceLink = target.closest('[data-source]');
    if (sourceLink && sourceLink.getAttribute('aria-current') !== 'true') {
      // Switching data source reloads the page on the same screen.
      event.preventDefault();
      const base = document.location.pathname;
      document.location.assign(`${base}${sourceLink.dataset.source === 'live' ? '?transport=live' : ''}${document.location.hash}`);
      return;
    }
    const row = target.closest('tr[data-href]');
    if (row && !target.closest('a, button')) document.location.hash = row.dataset.href;
  });

  document.addEventListener('keydown', (event) => {
    const row = event.target?.closest?.('tr[data-href]');
    if (row && (event.key === 'Enter' || event.key === ' ')) {
      event.preventDefault();
      document.location.hash = row.dataset.href;
    }
    if (event.key === 'Escape') document.body?.classList?.remove('menu-open');
  });
}

/** The navigation groups the reader has folded; the rarely needed ones start folded. */
export function readCollapsed(document) {
  return new Set(String(readPref(document, 'sac.nav.collapsed', NAV_FOLDED.join('|'))).split('|').filter(Boolean));
}
