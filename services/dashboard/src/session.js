// session.js — what the navigation hides for a signed-in role.
//
// The page never holds a token and never reads one: its server does, and it answers GET /session
// with the actor, the tenant and the page ids the roles may open (server/session.mjs decides those,
// so the rule lives in one place). The fetch itself lives in transport.js, the package's one
// network seam; this module is the pure part — which nav items survive it.

/**
 * The nav ids a session may show, or null when no session was read. The navigation is then left
 * whole: hiding is a convenience, and the server refuses what a role may not use either way.
 */
export function allowedPageIds(session) {
  return Array.isArray(session?.pages) ? new Set(session.pages) : null;
}

/** Keep only the nav items the session may show. `null` allowed means keep every item. */
export function filterNavItems(items, allowed) {
  if (!allowed) return items;
  return items.filter((item) => allowed.has(item.id));
}

/** May a page be opened? The same rule as the navigation, for an address typed or linked directly. */
export function mayOpen(allowed, pageId) {
  return !allowed || allowed.has(pageId);
}
