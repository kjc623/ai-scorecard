/**
 * submission.js — the request a catalogued web app sends when a person submits a prompt, and the
 * prompt it carries.
 *
 * The predicate finds submissions to tools nobody has enumerated by their shape. A web app the
 * catalog names is known by more than its shape: the request it sends for a prompt has a fixed
 * method and path, and the typed text sits in one place in its body. Such a request is a submission
 * whatever it scores, and what is stored is the typed text rather than the payload around it, which
 * is the client's conversation state, settings and tokens.
 *
 * A request to the app that does not match, or whose body does not hold the prompt where the app
 * puts it, is left to the predicate.
 */

import { utf8Strict } from './codec.js';

/**
 * app_key -> how the app's prompt request is recognised and where the prompt is in it.
 * `path` is matched against the URL's path; `prompt` reads the body, normalised by request-body.js,
 * and returns the typed text or null.
 */
export const SUBMISSIONS = Object.freeze({
  claude_web: {
    path: /^\/api\/organizations\/[^/]+\/chat_conversations\/[^/]+\/completion$/,
    prompt: (body) => stringMember(jsonOf(body), 'prompt'),
  },
  gemini_web: {
    path: /\/assistant\.lamda\.BardFrontendService\/StreamGenerate$/,
    // `f.req` is a JSON array whose second element is itself JSON text; that inner array starts
    // with the message, whose first element is the typed text.
    prompt: (body) => {
      const outer = parseJson(formValue(body, 'f.req'));
      const inner = Array.isArray(outer) && typeof outer[1] === 'string' ? parseJson(outer[1]) : null;
      const message = Array.isArray(inner) ? inner[0] : null;
      return Array.isArray(message) ? nonEmpty(message[0]) : null;
    },
  },
  perplexity_web: {
    path: /^\/rest\/sse\/perplexity_ask$/,
    prompt: (body) => stringMember(jsonOf(body), 'query_str'),
  },
});

/**
 * The prompt of a catalogued app's submission, or null when the request is not one.
 *
 * @param {string} toolFingerprint  the request's tool, `app:<app_key>` when the catalog names it
 * @param {{url: string, method: string}} detail
 * @param {import('./request-body.js').NormalisedBody} body
 * @returns {string|null}
 */
export function knownSubmission(toolFingerprint, detail, body) {
  const app = String(toolFingerprint || '').startsWith('app:') ? toolFingerprint.slice(4) : null;
  const recipe = app && Object.hasOwn(SUBMISSIONS, app) ? SUBMISSIONS[app] : null;
  if (!recipe || String(detail.method || '').toUpperCase() !== 'POST' || !body || body.truncated_reason) return null;
  let path;
  try {
    path = new URL(detail.url).pathname;
  } catch {
    return null;
  }
  return recipe.path.test(path) ? recipe.prompt(body) : null;
}

function jsonOf(body) {
  if (body.source !== 'raw') return null;
  const text = utf8Strict(body.bytes);
  return text.ok ? parseJson(text.text) : null;
}

function formValue(body, name) {
  const values = body.form && body.form[name];
  return Array.isArray(values) && values.length === 1 ? values[0] : null;
}

function parseJson(text) {
  if (typeof text !== 'string') return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function stringMember(obj, key) {
  return obj && typeof obj === 'object' && !Array.isArray(obj) ? nonEmpty(obj[key]) : null;
}

function nonEmpty(v) {
  return typeof v === 'string' && v.trim() !== '' ? v : null;
}
