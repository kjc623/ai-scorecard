/**
 * predicate.js — "a user-authored payload sent to a generative endpoint", as one pure function
 * over a request record. No chrome.*, no clock, no I/O.
 *
 * The predicate runs before the mode is consulted for content: whether a request is a generative
 * submission is a question of shape (size, structure, content type), which is M0-grade
 * information. A negative match is counted (`skipped_not_generative`) and never emitted.
 *
 * Each signal contributes weight and the threshold favours recall, because the point is finding
 * tools nobody has enumerated; precision is recovered downstream. The weights are a calibration,
 * exported so the tuning is visible.
 *
 * A `messages`-shaped array is generative on its own (`structural`). Plain natural-language text is
 * not, because text alone is also the shape of a draft save: a plain-text request only matches when
 * the response contract agrees, which is what `classifyWithResponse()` decides.
 */

import { utf8Strict } from './codec.js';

export const SIGNAL_WEIGHT = Object.freeze({
  // Body structure
  body_messages_array: 0.45,
  body_messages_named_member: 0.10,
  body_role_discriminator: 0.10,
  body_content_payload: 0.10,
  body_prompt_member: 0.20,
  body_model_params: 0.20,
  body_tool_declarations: 0.25,
  body_system_member: 0.05,
  body_long_text: 0.25,
  body_many_fields: 0.10,
  /**
   * The chat shape found by a key scan instead of a parse: the body is a capped prefix or its bytes
   * are not valid UTF-8. Weaker evidence than a parsed array (no value was read), but it keeps
   * over-cap and binary bodies matchable.
   */
  body_chat_shape_from_key_scan: 0.25,
  // Method, content type, size
  request_post: 0.10,
  request_json_content_type: 0.10,
  request_non_trivial_size: 0.05,
  // Path — "low weight, evidence only"
  path_conversational_vocabulary: 0.10,
  // Destination: evidence, never sufficient; the bundle supplies these sets
  destination_sanctioned_set: 0.15,
  destination_seed_set: 0.10,
  destination_denied_set: 0.15,
  // Context
  context_automation_marker: 0.30,
  context_composer: 0.10,
});

export const DEFAULT_THRESHOLD = 0.8;
/** Below this a request is not held for its response contract. */
export const CANDIDATE_FLOOR = 0.25;
/** Strong enough to carry a request without response corroboration. */
export const STRUCTURAL_FLOOR = 0.8;
/** A text member must hold at least this much contiguous text to count as non-trivial. */
export const LONG_TEXT_CHARS = 180;
/** A body above this size is non-trivial. */
export const TRIVIAL_SIZE_BYTES = 32;

const ROLE_KEYS = ['role', 'author', 'speaker', 'sender', 'from'];
const ROLE_VALUES = new Set(['user', 'assistant', 'system', 'tool', 'function', 'model', 'human', 'ai', 'developer']);
const CONTENT_KEYS = ['content', 'text', 'parts', 'message', 'value', 'body'];
const PROMPT_KEYS = new Set(['prompt', 'prompts', 'input', 'inputs', 'query', 'question', 'instruction', 'instructions', 'user_message', 'userMessage', 'q']);
const SYSTEM_KEYS = new Set(['system', 'system_prompt', 'systemPrompt', 'preamble', 'context']);
const MODEL_PARAM_KEYS = new Set([
  'model', 'model_id', 'modelid', 'model_name', 'temperature', 'top_p', 'top_k', 'max_tokens', 'maxtokens',
  'max_output_tokens', 'max_completion_tokens', 'frequency_penalty', 'presence_penalty', 'stop', 'stop_sequences',
  'n', 'stream', 'seed', 'logprobs', 'response_format', 'reasoning_effort', 'modalities',
]);
const TOOL_KEYS = new Set([
  'tools', 'functions', 'tool_choice', 'toolchoice', 'function_call', 'functioncall', 'tool_config', 'mcp_servers',
]);
const PATH_VOCABULARY = /(^|[/_.-])(chat|chats|completion|completions|complete|generate|generation|generations|message|messages|conversation|conversations|assistant|assistants|prompt|prompts|responses|thread|threads|inference|invoke|agent|agents|reason|chatapi)([/_.\-?]|$)/i;

const JSON_CONTENT_TYPES = ['application/json', 'application/vnd.api+json', 'text/json', 'application/x-ndjson', 'application/jsonl'];

/** Cheap shape signature of the path: numeric ids and uuids collapse, so two sessions on one tool agree. */
export function normalisePath(pathname) {
  return String(pathname || '')
    .replace(/\/[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/gi, '/:uuid')
    .replace(/\/\d+/g, '/:n')
    .replace(/\/[0-9a-f]{16,}/gi, '/:hex')
    .slice(0, 512);
}

export function hostOf(url) {
  try {
    return new URL(url).host;
  } catch {
    return '';
  }
}

export function pathnameOf(url) {
  try {
    return new URL(url).pathname;
  } catch {
    return '';
  }
}

function contentTypeClass(headers) {
  const raw = String((headers && (headers['content-type'] || headers['Content-Type'])) || '').toLowerCase();
  const base = raw.split(';')[0].trim();
  if (!base) return { raw, base, json: false, form: false };
  return {
    raw,
    base,
    json: JSON_CONTENT_TYPES.some((t) => base === t || base.endsWith('+json')),
    form: base === 'application/x-www-form-urlencoded' || base.startsWith('multipart/form-data'),
  };
}

/** Cheap key-name scan for a payload that could not be parsed. Values are never read. */
function byteKeyScan(bytes, lossy) {
  if (!bytes || bytes.byteLength === 0) return null;
  const text = lossy
    // A replacement character cannot create or destroy an ASCII key name, so the shape is still
    // readable even though the payload is not — and nothing else is taken from this reading.
    ? new TextDecoder('utf-8', { fatal: false }).decode(bytes)
    : (() => {
        const strict = utf8Strict(bytes);
        return strict.ok ? strict.text : null;
      })();
  if (text === null) return null;
  return new Set([...text.matchAll(/"([A-Za-z_][A-Za-z0-9_]*)"\s*:/g)].map((m) => m[1]));
}

function tryParseJson(bytes) {
  if (!bytes) return null;
  const strict = utf8Strict(bytes);
  if (!strict.ok) return null;
  const text = strict.text;
  if (!text) return null;
  const first = text.trimStart()[0];
  if (first !== '{' && first !== '[') return null;
  try {
    return JSON.parse(text);
  } catch {
    return null;
  }
}

function longestString(node, best = { len: 0, path: '' }, path = '$', depth = 0) {
  if (depth > 12 || node === null || node === undefined) return best;
  if (typeof node === 'string') {
    if (node.length > best.len) return { len: node.length, path };
    return best;
  }
  if (Array.isArray(node)) {
    for (let i = 0; i < node.length && i < 64; i++) best = longestString(node[i], best, `${path}[${i}]`, depth + 1);
    return best;
  }
  if (typeof node === 'object') {
    for (const [k, v] of Object.entries(node)) best = longestString(v, best, `${path}.${k}`, depth + 1);
    return best;
  }
  return best;
}

function keySetDeep(node, depth = 0, out = new Set(), path = '$') {
  if (depth > 12 || node === null || typeof node !== 'object') return out;
  if (Array.isArray(node)) {
    for (let i = 0; i < node.length && i < 64; i++) keySetDeep(node[i], depth + 1, out, `${path}[${i}]`);
    return out;
  }
  for (const [k, v] of Object.entries(node)) {
    out.add(k);
    keySetDeep(v, depth + 1, out, `${path}.${k}`);
  }
  return out;
}

function objectMemberCount(node) {
  if (!node || typeof node !== 'object' || Array.isArray(node)) return 0;
  return Object.keys(node).length;
}

/** The chat shape: an ordered array of objects each carrying a role discriminator and a content-like payload. */
function detectMessagesArray(node, path = '$', depth = 0, out = null) {
  if (depth > 10 || node === null || typeof node !== 'object') return out;
  if (Array.isArray(node)) {
    const objects = node.filter((x) => x && typeof x === 'object' && !Array.isArray(x));
    const scalars = node.length - objects.length;
    if (objects.length >= 1 && scalars === 0) {
      const withRole = objects.filter((o) => Object.keys(o).some((k) => ROLE_KEYS.includes(k)));
      const withContent = objects.filter((o) => Object.keys(o).some((k) => CONTENT_KEYS.includes(k)));
      if (withRole.length === objects.length && withContent.length === objects.length) {
        const roleValues = new Set();
        for (const o of objects) {
          for (const k of ROLE_KEYS) {
            if (typeof o[k] === 'string') roleValues.add(o[k].toLowerCase());
          }
        }
        return {
          path,
          count: objects.length,
          role_values: [...roleValues],
          role_like: [...roleValues].some((v) => ROLE_VALUES.has(v)),
        };
      }
    }
    for (let i = 0; i < node.length && i < 32; i++) {
      const r = detectMessagesArray(node[i], `${path}[${i}]`, depth + 1, out);
      if (r) return r;
    }
    return out;
  }
  for (const [k, v] of Object.entries(node)) {
    const r = detectMessagesArray(v, `${path}.${k}`, depth + 1, out);
    if (r) return r;
  }
  return out;
}

function signal(list, id, weight, detail) {
  list.push({ id, weight, detail: detail === undefined ? '' : detail });
  return list;
}

/**
 * The request-side evidence. Pure: same input, same output, no clock and no globals.
 *
 * @param {object} rec
 * @param {string} rec.url
 * @param {string} rec.method
 * @param {Record<string,string>} [rec.headers]
 * @param {number} [rec.size_bytes]
 * @param {Uint8Array|null} [rec.bytes]      the bytes actually held (may be a capped prefix)
 * @param {boolean} [rec.truncated]          true when `bytes` is a prefix of a larger payload
 * @param {string|null} [rec.form_text]      form bodies arrive parsed
 * @param {{sanctioned?: Set<string>|string[], seed?: Set<string>|string[], denied?: Set<string>|string[]}} [rec.bundle]
 * @param {string[]} [rec.tab_context]       e.g. ['active_composer', 'automation_marker']
 * @param {number} [threshold]
 */
export function predicateRequest(rec, threshold = DEFAULT_THRESHOLD) {
  const signals = [];
  const headers = rec.headers || {};
  const ct = contentTypeClass(headers);
  const bytes = rec.bytes || null;
  const size = Number.isFinite(rec.size_bytes) ? rec.size_bytes : bytes ? bytes.byteLength : 0;
  const method = String(rec.method || '').toUpperCase();

  // Method, content type, size
  if (method === 'POST' || method === 'PUT' || method === 'PATCH') signal(signals, 'request_post', SIGNAL_WEIGHT.request_post, method);
  if (ct.json) signal(signals, 'request_json_content_type', SIGNAL_WEIGHT.request_json_content_type, ct.base);
  if (size > TRIVIAL_SIZE_BYTES) signal(signals, 'request_non_trivial_size', SIGNAL_WEIGHT.request_non_trivial_size, String(size));

  // Path: low weight, evidence only, never the decision
  let pathSignal = null;
  try {
    const u = new URL(rec.url);
    const normalised = normalisePath(u.pathname);
    if (PATH_VOCABULARY.test(u.pathname) || PATH_VOCABULARY.test(normalised)) {
      signal(signals, 'path_conversational_vocabulary', SIGNAL_WEIGHT.path_conversational_vocabulary, normalised);
      pathSignal = normalised;
    }
  } catch {
    /* an unparseable URL contributes no path evidence */
  }

  // Destination sets: "evidence, never sufficient"
  const host = hostOf(rec.url);
  const registered = registeredDomain(host);
  for (const [key, weight, id] of [
    ['sanctioned', SIGNAL_WEIGHT.destination_sanctioned_set, 'destination_sanctioned_set'],
    ['seed', SIGNAL_WEIGHT.destination_seed_set, 'destination_seed_set'],
    ['denied', SIGNAL_WEIGHT.destination_denied_set, 'destination_denied_set'],
  ]) {
    const set = rec.bundle && rec.bundle[key];
    if (!set) continue;
    const list = set instanceof Set ? [...set] : set;
    if (list.includes(host) || list.includes(registered)) signal(signals, id, weight, host);
  }

  // Body
  const parsedForm = rec.form_text || null;
  const json = parsedForm ? null : tryParseJson(bytes);
  let messages = null;
  let structure = 'none';

  if (parsedForm) {
    structure = 'form';
    const names = Object.keys(rec.form || {});
    if (names.some((n) => PROMPT_KEYS.has(n))) signal(signals, 'body_prompt_member', SIGNAL_WEIGHT.body_prompt_member, 'form');
    if (names.some((n) => MODEL_PARAM_KEYS.has(n))) signal(signals, 'body_model_params', SIGNAL_WEIGHT.body_model_params, 'form');
    // A file input's filename travels as a form value, which is a positive upload match.
    if (names.length >= 3) signal(signals, 'body_many_fields', SIGNAL_WEIGHT.body_many_fields, `forms:${names.length}`);
    const longest = Math.max(0, ...Object.values(rec.form || {}).flat().map((v) => (typeof v === 'string' ? v.length : 0)));
    if (longest >= LONG_TEXT_CHARS) signal(signals, 'body_long_text', SIGNAL_WEIGHT.body_long_text, `chars:${longest}`);
  } else if (json !== null && typeof json === 'object') {
    structure = Array.isArray(json) ? 'array' : 'object';
    messages = detectMessagesArray(json);
    if (messages) {
      signal(signals, 'body_messages_array', SIGNAL_WEIGHT.body_messages_array, `count:${messages.count}`);
      if (messages.role_like) signal(signals, 'body_role_discriminator', SIGNAL_WEIGHT.body_role_discriminator, messages.role_values.join(','));
      signal(signals, 'body_content_payload', SIGNAL_WEIGHT.body_content_payload, messages.path);
    }
    const keys = keySetDeep(json);
    if (keys.has('messages')) signal(signals, 'body_messages_named_member', SIGNAL_WEIGHT.body_messages_named_member, 'messages');
    if ([...keys].some((k) => PROMPT_KEYS.has(k))) signal(signals, 'body_prompt_member', SIGNAL_WEIGHT.body_prompt_member, 'json');
    if ([...keys].some((k) => MODEL_PARAM_KEYS.has(k))) signal(signals, 'body_model_params', SIGNAL_WEIGHT.body_model_params, 'json');
    if ([...keys].some((k) => TOOL_KEYS.has(k))) signal(signals, 'body_tool_declarations', SIGNAL_WEIGHT.body_tool_declarations, 'json');
    if ([...keys].some((k) => SYSTEM_KEYS.has(k))) signal(signals, 'body_system_member', SIGNAL_WEIGHT.body_system_member, 'json');
    const longest = longestString(json);
    if (longest.len >= LONG_TEXT_CHARS) signal(signals, 'body_long_text', SIGNAL_WEIGHT.body_long_text, `chars:${longest.len}`);
    if (objectMemberCount(Array.isArray(json) ? json[0] : json) >= 4) {
      signal(signals, 'body_many_fields', SIGNAL_WEIGHT.body_many_fields, 'json');
    }
  } else if (bytes && bytes.byteLength > 0) {
    // Bytes that did not parse as JSON: a capped prefix, or bytes that are not valid UTF-8. The
    // strict decoder decides which.
    structure = utf8Strict(bytes).ok ? 'unparsed' : 'binary';
  }

  // Body structure from a key scan rather than a parse, so over-cap and binary bodies can still
  // match (and are then reported degraded).
  if (structure === 'unparsed' || structure === 'binary') {
    const scanDetail = structure === 'binary' ? 'binary_key_scan' : 'unparsed_prefix';
    const keys = byteKeyScan(bytes, structure === 'binary');
    if (keys) {
      if (keys.has('messages') || keys.has('contents')) {
        signal(signals, 'body_messages_named_member', SIGNAL_WEIGHT.body_messages_named_member, scanDetail);
      }
      if ([...keys].some((k) => ROLE_KEYS.includes(k))) {
        signal(signals, 'body_role_discriminator', SIGNAL_WEIGHT.body_role_discriminator, scanDetail);
      }
      if ([...keys].some((k) => CONTENT_KEYS.includes(k))) {
        signal(signals, 'body_content_payload', SIGNAL_WEIGHT.body_content_payload, scanDetail);
      }
      if ([...keys].some((k) => PROMPT_KEYS.has(k))) {
        signal(signals, 'body_prompt_member', SIGNAL_WEIGHT.body_prompt_member, scanDetail);
      }
      if ([...keys].some((k) => MODEL_PARAM_KEYS.has(k))) {
        signal(signals, 'body_model_params', SIGNAL_WEIGHT.body_model_params, scanDetail);
      }
      if ([...keys].some((k) => TOOL_KEYS.has(k))) {
        signal(signals, 'body_tool_declarations', SIGNAL_WEIGHT.body_tool_declarations, scanDetail);
      }
      // A member named `messages`, a role discriminator and a content payload together is the chat
      // shape, seen without a parse.
      const roleLike = [...keys].some((k) => ROLE_KEYS.includes(k));
      const contentLike = [...keys].some((k) => CONTENT_KEYS.includes(k));
      const messagesNamed = keys.has('messages') || keys.has('contents');
      if (messagesNamed && roleLike && contentLike) {
        signal(signals, 'body_chat_shape_from_key_scan', SIGNAL_WEIGHT.body_chat_shape_from_key_scan, scanDetail);
      }
    }
  }

  // Context
  const tabContext = Array.isArray(rec.tab_context) ? rec.tab_context : [];
  if (tabContext.includes('automation_marker')) signal(signals, 'context_automation_marker', SIGNAL_WEIGHT.context_automation_marker, 'header');
  if (tabContext.includes('active_composer')) signal(signals, 'context_composer', SIGNAL_WEIGHT.context_composer, 'composer');

  const score = round3(signals.reduce((n, s) => n + s.weight, 0));
  const structural = messages !== null
    || signals.some((s) => s.id === 'body_tool_declarations')
    || signals.some((s) => s.id === 'body_chat_shape_from_key_scan');
  const match = score >= threshold;

  // The metadata-only counterpart of `match`, for the lane where no body was requested (an M0
  // destination). It decides whether the request is worth an identity-and-volume observation, the
  // most the route reports at M0. The size is unknown on that lane, so a conversational path stands
  // in for it; a false positive costs one uninteresting event.
  const conversationalPath = signals.some((s) => s.id === 'path_conversational_vocabulary');
  const metadata_candidate =
    method !== 'GET' &&
    (ct.json || ct.form || conversationalPath) &&
    (size > TRIVIAL_SIZE_BYTES || conversationalPath);

  return {
    match,
    metadata_candidate,
    score,
    threshold,
    signals,
    structural,
    structure,
    messages,
    path_signal: pathSignal,
    over_cap: Boolean(rec.truncated),
    upload_bearing: isUploadBearing({ headers, form: rec.form || null, ct }),
    attachment_candidates: rec.form ? formFileCandidates(rec.form) : [],
  };
}

/** An upload-bearing request: a multipart/form body, or a form carrying a filename. */
export function isUploadBearing({ headers, form, ct }) {
  const base = (ct && ct.base) || contentTypeClass(headers).base;
  if (base.startsWith('multipart/form-data')) return true;
  return formFileCandidates(form).length > 0;
}

/** Form values that carry a filename. A filename alone is not attachment capture. */
export function formFileCandidates(form) {
  if (!form) return [];
  const names = ['file', 'files', 'filename', 'file_name', 'upload', 'attachment', 'attachments', 'document'];
  const out = [];
  for (const [k, values] of Object.entries(form)) {
    for (const v of values) {
      if (typeof v !== 'string' || v === '') continue;
      if (names.includes(k.toLowerCase()) || /\.(pdf|docx?|xlsx?|pptx?|csv|txt|md|json|zip|png|jpe?g|gif|eml|msg)$/i.test(v)) {
        out.push({ field: k, name: v });
      }
    }
  }
  return out;
}

const STREAMING_CONTRACTS = ['text/event-stream', 'application/x-ndjson', 'application/stream+json', 'application/jsonl'];
const COMPLETION_HOOKS = ['x-request-id', 'openai-processing-ms', 'openai-version', 'x-ratelimit-limit-tokens', 'anthropic-ratelimit-tokens-limit'];

/**
 * The response-side evidence: a streaming or completion-shaped response contract for the same
 * request. Signature only (status, content type, framing headers); never the response body.
 */
export function predicateResponse(res) {
  const signals = [];
  const headers = (res && res.headers) || {};
  const ct = contentTypeClass(headers);
  const status = Number((res && res.status) || 0);
  if (STREAMING_CONTRACTS.some((t) => ct.base === t || ct.base.startsWith(t))) {
    signal(signals, 'response_streaming_contract', 0.30, ct.base);
  }
  if (ct.json && (status === 200 || status === 201)) signal(signals, 'response_json_ok', 0.05, `${status} ${ct.base}`);
  for (const h of COMPLETION_HOOKS) {
    if (headers[h] !== undefined) {
      signal(signals, 'response_completion_hook', 0.10, h);
      break;
    }
  }
  if (headers['transfer-encoding'] === 'chunked') signal(signals, 'response_chunked_framing', 0.05, 'chunked');
  const score = round3(signals.reduce((n, s) => n + s.weight, 0));
  return { score, signals, streaming: signals.some((s) => s.id === 'response_streaming_contract') };
}

/**
 * A draft save and a chat call on the same origin differ by body, not path, so a match is a
 * conjunction: a generative request structure and a generative response contract. A
 * `messages`-shaped array is enough on its own; anything weaker needs the response half.
 *
 * @returns {{match: boolean, score: number, response_score: number, structural: boolean, reason: string, signals: any[]}}
 */
export function classifyWithResponse(req, res, threshold = DEFAULT_THRESHOLD) {
  const r = predicateRequest(req, threshold);
  const s = res ? predicateResponse(res) : { score: 0, signals: [], streaming: false };
  const structuralMatch = r.score >= STRUCTURAL_FLOOR && r.structural;
  const combined = round3(r.score + s.score);
  if (structuralMatch) {
    return { match: true, score: combined, response_score: s.score, structural: true, reason: 'request_structure', signals: [...r.signals, ...s.signals] };
  }
  if (r.score >= CANDIDATE_FLOOR && combined >= threshold) {
    return { match: true, score: combined, response_score: s.score, structural: false, reason: 'request_plus_response_contract', signals: [...r.signals, ...s.signals] };
  }
  return { match: false, score: combined, response_score: s.score, structural: r.structural, reason: r.match ? 'structure_without_response_contract' : 'below_threshold', signals: [...r.signals, ...s.signals] };
}

/** eTLD+1 approximation. A suffix list is not shipped: this is a stable label for a fingerprint, never a security decision. */
export function registeredDomain(host) {
  const h = String(host || '').toLowerCase().replace(/:\d+$/, '');
  if (!h || /^\d+(\.\d+){3}$/.test(h) || h === 'localhost' || h.startsWith('[')) return h;
  const parts = h.split('.');
  if (parts.length <= 2) return h;
  const twoLevel = new Set(['co.uk', 'org.uk', 'ac.uk', 'gov.uk', 'com.au', 'co.jp', 'com.br', 'co.nz', 'com.cn', 'co.in']);
  const last2 = parts.slice(-2).join('.');
  return twoLevel.has(last2) ? parts.slice(-3).join('.') : lastTwo(parts);
}

function lastTwo(parts) {
  return parts.slice(-2).join('.');
}

function round3(n) {
  return Math.round(n * 1000) / 1000;
}
