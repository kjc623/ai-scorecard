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
 * app_key -> the app's prompt requests: how each is recognised and where its prompt is. `path` is
 * matched against the URL's path; `prompt` reads the body, normalised by request-body.js, and
 * returns the typed text or null.
 */
export const SUBMISSIONS = Object.freeze({
  claude_web: [
    {
      // A Connect RPC with a protobuf body: the action's message (field 2) holds the typed text in
      // its field 3. The same method carries other actions, which have no field 2.
      path: /^\/claudeai-rpc\/[^/]+\.ConversationService\/PerformAction$/,
      prompt: (body) => (body.source === 'raw' ? protoText(body.bytes, [2, 3]) : null),
    },
    {
      path: /^\/api\/organizations\/[^/]+\/chat_conversations\/[^/]+\/completion$/,
      prompt: (body) => stringMember(jsonOf(body), 'prompt'),
    },
  ],
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
  const recipes = app && Object.hasOwn(SUBMISSIONS, app) ? [SUBMISSIONS[app]].flat() : [];
  if (!recipes.length || String(detail.method || '').toUpperCase() !== 'POST' || !body || body.truncated_reason) return null;
  let path;
  try {
    path = new URL(detail.url).pathname;
  } catch {
    return null;
  }
  const recipe = recipes.find((r) => r.path.test(path));
  return recipe ? recipe.prompt(body) : null;
}

/**
 * The UTF-8 text at a path of field numbers in a protobuf message, each step the first field of
 * that number, or null when the bytes are not such a message.
 */
export function protoText(bytes, fields) {
  let value = bytes;
  for (const field of fields) {
    value = protoField(value, field);
    if (value === null) return null;
  }
  const text = utf8Strict(value);
  return text.ok ? nonEmpty(text.text) : null;
}

/** The bytes of the first length-delimited field numbered `field`, or null. */
function protoField(bytes, field) {
  let i = 0;
  const varint = () => {
    let n = 0;
    for (let shift = 0; shift < 70; shift += 7) {
      if (i >= bytes.length) return -1;
      const b = bytes[i++];
      n += (b & 0x7f) * 2 ** shift;
      if (b < 0x80) return n;
    }
    return -1;
  };
  while (i < bytes.length) {
    const key = varint();
    if (key < 0) return null;
    const wire = key % 8;
    if (wire === 0) {
      if (varint() < 0) return null;
    } else if (wire === 1 || wire === 5) {
      i += wire === 1 ? 8 : 4;
    } else if (wire === 2) {
      const len = varint();
      if (len < 0 || i + len > bytes.length) return null;
      if (Math.floor(key / 8) === field) return bytes.subarray(i, i + len);
      i += len;
    } else {
      return null;
    }
  }
  return null;
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
