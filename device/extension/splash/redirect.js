/**
 * redirect.js — the splash page a redirect rule sends a person to instead of the web app they
 * opened: the rule's message and a button to its link.
 *
 * The page's address names the rule and the host the person tried to open. The message and link
 * come from the service worker, out of the bundle in force, so the page shows only what a rule
 * says. Every value is set as text, and the button is offered only for an https link.
 */

/**
 * Fill the page from what the rule says. `spec` is null when the rule is not a redirect in force;
 * the page then says the app is not available, with no way on.
 *
 * @param {Document} document
 * @param {{message: string, link: string}|null} spec
 * @param {string} from  the host the person tried to open
 */
export function renderSplash(document, spec, from) {
  const el = (id) => document.getElementById(id);
  el('sp-from').textContent = from ? `You tried to open ${from}.` : '';
  el('sp-message').textContent = spec && spec.message ? spec.message : 'Your organisation does not allow this app.';
  const link = spec && typeof spec.link === 'string' && spec.link.startsWith('https://') ? spec.link : '';
  const go = el('sp-go');
  if (!link) {
    go.hidden = true;
    go.removeAttribute('href');
    el('sp-link').textContent = '';
    return;
  }
  go.setAttribute('href', link);
  go.textContent = `Continue to ${hostOf(link)}`;
  go.hidden = false;
  el('sp-link').textContent = link;
}

/** The rule id and host the page's address carries. */
export function splashParams(search) {
  const q = new URLSearchParams(search);
  return { rule_id: q.get('rule') || '', from: q.get('from') || '' };
}

function hostOf(link) {
  try {
    return new URL(link).hostname;
  } catch {
    return link;
  }
}

// Runs in the extension's page only, never under `node --test`.
if (typeof chrome !== 'undefined' && chrome.runtime && typeof chrome.runtime.sendMessage === 'function' && typeof document !== 'undefined') {
  const { rule_id, from } = splashParams(location.search);
  chrome.runtime.sendMessage({ type: 'capture_redirect_spec', rule_id })
    .then((answer) => renderSplash(document, answer && answer.ok ? answer : null, from))
    .catch(() => renderSplash(document, null, from));
}
