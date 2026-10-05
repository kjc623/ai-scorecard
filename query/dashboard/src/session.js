// session.js — what the navigation hides for a signed-in role.
//
// The page never holds a token and never reads one: its server does, and it answers GET /session
// with the actor, the tenant and the page ids the roles may open (tools/session.mjs decides those
// server-side, so the rule lives in one place). The fetch itself lives in transport.js, the
// package's one network seam; this module is the pure part — the decision about which nav items
// survive it.
//
// Sample mode — the file opened from disk — has no server and gets `null`, which means "show
// everything"; that is the sample transport's own state gallery, not a signed-in session.

/**
 * The nav ids a session may show, or null for "all" (sample mode). A session with no `pages`
 * is treated as all, so a server that predates this field does not blank the navigation.
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
