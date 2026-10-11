// shell.js — what every page of the dashboard shares around its content: the navigation's
// shape, the reader's stored preferences, and the controls in the navigation panel.
//
// The dashboard (app.js) and the Explore page (explore-app.js) both boot through this, so the
// sidebar is one thing: the same groups, the same folding, the same theme switch, and the same line
// saying who is signed in, with its sign-out button.

import { escapeHtml } from './render.js';

/**
 * The navigation: six destinations, with the three usage screens under one group and the admin's
 * settings under another. Every other screen is reached from a row or a switch on one of these. A
 * signed-in role sees only the items it may use (session.js).
 */
export const NAV_GROUPS = Object.freeze([
  { group: null, icon: 'overview', ids: ['posture'], labels: { posture: 'Overview' } },
  { group: 'Usage', icon: 'usage', ids: ['tools', 'teams', 'person'], labels: { tools: 'Tools', teams: 'Teams', person: 'Users' } },
  { group: null, icon: 'collection', ids: ['devices'], labels: { devices: 'Devices' } },
  { group: null, icon: 'governance', ids: ['audit'], labels: { audit: 'Audit trail' } },
  { group: 'Settings', icon: 'settings', ids: ['settings', 'deployment', 'directory'], labels: { settings: 'Settings', deployment: 'Deployment', directory: 'Directory & teams' } },
]);

const NAV_FOLDED = Object.freeze(NAV_GROUPS.filter((g) => g.folded).map((g) => g.group));

/**
 * The navigation as renderNav takes it. `page` prefixes each screen link, so a sibling page links
 * back into the dashboard; the Explore page sits second, as a top-level link, on both.
 */
export function shellNavItems({ page = '', questionOf = () => null } = {}) {
  const items = NAV_GROUPS.flatMap(({ group, icon, ids, labels }) => ids.map((id) => ({
    id, label: labels[id], question: questionOf(id), group, icon, groupIcon: icon, href: `${page}#${id}`,
  })));
  return [
    items[0],
    { id: 'explore', label: 'Search', href: 'explore.html', icon: 'search', group: null },
    ...items.slice(1),
  ];
}

/** The group a screen sits under, for the eyebrow above its title. */
export function groupOf(screenId) {
  return NAV_GROUPS.find((g) => g.ids.includes(screenId))?.group ?? null;
}

const ROLE_LABELS = Object.freeze({ viewer: 'Viewer', analyst: 'Analyst', content_reader: 'Content reader', admin: 'Admin' });

/**
 * Who is signed in, for the navigation panel: the actor, their roles, and a sign-out button that
 * POSTs to the page's own server.
 */
export function renderWho(session) {
  if (!session || typeof session !== 'object') return '';
  const roles = (Array.isArray(session.roles) ? session.roles : []).map((r) => ROLE_LABELS[r] ?? r).join(', ');
  return `<p class="who-text"><span class="who-name" title="${escapeHtml(session.actor ?? '')}">${escapeHtml(session.actor ?? '')}</span>`
    + `<span class="who-role">${escapeHtml(roles)}</span></p>`
    + '<form class="who-form" method="post" action="/signout"><button type="submit" class="who-out">Sign out</button></form>';
}

/** Local preferences. Storage can be unavailable (a private window): then nothing is kept. */
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
 * The shell's own controls: who is signed in, which navigation groups are folded, the theme, the
 * mobile menu, and a row that opens the record it names. Every lookup is optional, so a test
 * document with none of these elements boots the same way.
 */
export function wireShell({ document, collapsed, session = null }) {
  const el = (id) => (typeof document.getElementById === 'function' ? document.getElementById(id) : null);
  const html = document.documentElement;

  const who = el('who');
  if (who) who.innerHTML = renderWho(session);

  // Theme: follow the system unless the reader chose. The choice is the only thing stored.
  const theme = readPref(document, 'sac.theme', '');
  if (html?.dataset && (theme === 'light' || theme === 'dark')) html.dataset.theme = theme;

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
    const row = target.closest('tr[data-href]');
    if (row && !target.closest('a, button')) follow(document, row.dataset.href);
  });

  document.addEventListener('keydown', (event) => {
    const row = event.target?.closest?.('tr[data-href]');
    if (row && (event.key === 'Enter' || event.key === ' ')) {
      event.preventDefault();
      follow(document, row.dataset.href);
    }
    if (event.key === 'Escape') document.body?.classList?.remove('menu-open');
  });
}

/** Open what a row names: a screen of this page by its hash, or a sibling page by its address. */
function follow(document, href) {
  if (String(href).startsWith('#')) document.location.hash = href;
  else document.location.href = href;
}

/** The navigation groups the reader has folded; the rarely needed ones start folded. */
export function readCollapsed(document) {
  return new Set(String(readPref(document, 'sac.nav.collapsed', NAV_FOLDED.join('|'))).split('|').filter(Boolean));
}
