// shell.js — what every page of the dashboard shares around its content: the navigation's
// shape, the reader's stored preferences, and the controls in the navigation panel.
//
// The dashboard (app.js) and the Explore page (explore-app.js) both boot through this, so the
// sidebar is one thing: the same destinations, the same theme switch, and the same line saying who
// is signed in, with its sign-out button.

import { escapeHtml } from './render.js';

/**
 * The navigation: four destinations and Settings, each holding its pages as tabs under its title.
 * Archive holds the pages still being built, and only pre-production offers it. Settings and Archive
 * sit at the foot of the panel. Every other screen is reached from a row or a tile on one of these.
 */
export const SECTIONS = Object.freeze([
  section('overview', 'Overview', 'overview', [['posture', 'Overview']]),
  section('usage', 'Usage', 'usage', [['tools', 'Tools'], ['teams', 'Teams'], ['person', 'Users']]),
  section('search', 'Search', 'search', [['explore', 'Search']], { href: 'explore.html' }),
  section('policy', 'Policy', 'rules', [['rules', 'Rules'], ['labels', 'Labels'], ['sanction', 'Tools'], ['activity', 'Activity']]),
  section('archive', 'Archive', 'archive', [['insights', 'Insights']], { foot: true }),
  section('settings', 'Settings', 'settings', [['settings', 'Collection'], ['devices', 'Devices'], ['directory', 'Identity Providers'], ['audit', 'Audit trail']], { foot: true }),
]);

function section(id, label, icon, pages, { href = null, foot = false } = {}) {
  return Object.freeze({
    id, label, icon, href, foot,
    pages: Object.freeze(pages.map(([page, title]) => Object.freeze({ id: page, label: title }))),
  });
}

/** The destination a page belongs to. An event is a record Search opens. */
export function sectionOf(pageId) {
  const id = pageId === 'event' ? 'explore' : pageId;
  return SECTIONS.find((s) => s.pages.some((p) => p.id === id)) ?? null;
}

/**
 * The navigation as renderNav takes it: one link per destination the session is offered, to the
 * first of its pages the session may open. `allowed` null keeps every page. `page` prefixes each
 * link, so a sibling page links back into the dashboard.
 */
export function shellNavItems({ page = '', allowed = null } = {}) {
  return SECTIONS.flatMap((s) => {
    const open = s.pages.filter((p) => !allowed || allowed.has(p.id));
    if (open.length === 0) return [];
    return [Object.freeze({
      id: s.id, label: s.label, icon: s.icon, foot: s.foot,
      pages: Object.freeze(s.pages.map((p) => p.id)),
      href: s.href ?? `${page}#${open[0].id}`,
    })];
  });
}

/**
 * The tabs of the destination a page belongs to, as the page header shows them: the pages the
 * session may open, each a link. Null for a destination with one page, which needs no tabs.
 */
export function tabsFor(pageId, { allowed = null, hrefOf = (id) => `#${id}` } = {}) {
  const s = sectionOf(pageId);
  if (!s || s.pages.length < 2) return null;
  const items = s.pages.filter((p) => !allowed || allowed.has(p.id)).map((p) => Object.freeze({ id: p.id, label: p.label, href: hrefOf(p.id) }));
  return items.length > 1 ? Object.freeze({ label: s.label, current: pageId, items: Object.freeze(items) }) : null;
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
 * The shell's own controls: who is signed in, the theme, the mobile menu, and a row that opens the
 * record it names. Every lookup is optional, so a test document with none of these elements boots
 * the same way.
 */
export function wireShell({ document, session = null }) {
  const el = (id) => (typeof document.getElementById === 'function' ? document.getElementById(id) : null);
  const html = document.documentElement;

  const who = el('who');
  if (who) who.innerHTML = renderWho(session);

  // Theme: follow the system unless the reader chose. The choice is the only thing stored.
  const theme = readPref(document, 'sac.theme', '');
  if (html?.dataset && (theme === 'light' || theme === 'dark')) html.dataset.theme = theme;

  if (typeof document.addEventListener !== 'function') return;

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
