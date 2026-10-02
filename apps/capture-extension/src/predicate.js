/**
 * predicate.js — §8.2's "user-authored payload sent to a generative endpoint", as one pure
 * function over a request record. No chrome.*, no clock, no I/O: this is the unit under test.
 *
 * Discipline the caller must respect (§7.3): the predicate runs **before** the mode is
 * consulted for content, because whether a request is a generative submission is a shape
 * question — size, structure and content type are M0-grade information (§11.3's M0 row).
 * A negative match is counted (`skipped_not_generative`) and never emitted.
 *
 * Evidence, not verdict: each signal contributes weight and the threshold favours recall,
 * because the brief's problem is finding tools nobody has enumerated and precision is
 * recovered downstream (§8.2). Enforcement is a different, higher-threshold decision (§7.4).
 *
 * The weights below are this implementation's calibration of §8.2's table — the document
 * fixes the evidence and the asymmetry (recall over precision) but not the numbers. They are
 * exported so a reviewer can see exactly what was tuned, and so a bundle can override them.
 *
 * Two consequences of §7.5-Mode B worth stating: a `messages`-shaped array is accepted on its
 * own (`structural`), because that shape is already the "generative request structure" half;
 * plain natural-language text is not, because text alone is also the shape of a draft save.
 * A plain-text request can only reach a match when the response contract agrees — which is
 * why `classifyWithResponse()` exists and why `predicateResponse` is a first-class export.
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
  // Method, content type, size
  request_post: 0.10,
  request_json_content_type: 0.10,
  request_non_trivial_size: 0.05,
  // Path — "low weight, evidence only"
  path_conversational_vocabulary: 0.10,
  // Destination — "evidence, never sufficient" (§8.2); the bundle supplies these sets
  destination_sanctioned_set: 0.15,
  destination_seed_set: 0.10,
  destination_denied_set: 0.15,
  // Context (§8.2 last row, §7.5 Mode C)
  context_automation_marker: 0.30,
  context_composer: 0.10,
});

export const DEFAULT_THRESHOLD = 0.8;
/** Below this there is nothing to reclassify on: §7.2 defers classification, it does not schedule it for every byte. */
export const CANDIDATE_FLOOR = 0.25;
/** Strong enough to carry a request without response corroboration (§7.5 Mode B's first half). */
export const STRUCTURAL_FLOOR = 0.8;
/** A text member must hold at least this much contiguous text to count as "non-trivial" (§8.2 row 2). */
export const LONG_TEXT_CHARS = 180;
/** "A body above a trivial size" (§8.2 row 5). */
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

/** Cheap shape signature of the path: numeric ids and uuids collapse, so two sessions on one tool agree (§8.1 signal 2). */
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
    return new URL(url).pathname + (new URL(url).search ? '' : '');
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
 * §8.2's request-side set. Pure: same input, same output, no clock and no globals.
 *
 * @param {object} rec
 * @param {string} rec.url
 * @param {string} rec.method
 * @param {Record<string,string>} [rec.headers]
 * @param {number} [rec.size_bytes]
 * @param {Uint8Array|null} [rec.bytes]      the bytes actually held (may be a capped prefix)
 * @param {boolean} [rec.truncated]          true when `bytes` is a prefix of a larger payload
 * @param {string|null} [rec.form_text]      form bodies arrive parsed (E2)
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

  // Method, content type, size (§8.2 row 5)
  if (method === 'POST' || method === 'PUT' || method === 'PATCH') signal(signals, 'request_post', SIGNAL_WEIGHT.request_post, method);
  if (ct.json) signal(signals, 'request_json_content_type', SIGNAL_WEIGHT.request_json_content_type, ct.base);
  if (size > TRIVIAL_SIZE_BYTES) signal(signals, 'request_non_trivial_size', SIGNAL_WEIGHT.request_non_trivial_size, String(size));

  // Path: low weight, evidence only — never the decision (§2.1, C7)
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
    // A form upload is also how attachment capture is decided (§7.3): a file input's
    // filename travels as a form value, and that is a positive upload match.
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
    structure = 'unparsed';
    // A capped prefix of a larger payload still yields evidence: §5.3's whole point is that
    // the over-cap body is sized and hashed rather than read, so a cheap key scan beats nothing.
    const strict = utf8Strict(bytes);
    if (strict.ok && /"(messages|prompt|input|contents|system)"\s*:/.test(strict.text)) {
      signal(signals, 'body_prompt_member', SIGNAL_WEIGHT.body_prompt_member, 'unparsed_prefix');
    }
  }

  // Context
  const tabContext = Array.isArray(rec.tab_context) ? rec.tab_context : [];
  if (tabContext.includes('automation_marker')) signal(signals, 'context_automation_marker', SIGNAL_WEIGHT.context_automation_marker, 'header');
  if (tabContext.includes('active_composer')) signal(signals, 'context_composer', SIGNAL_WEIGHT.context_composer, 'composer');

  const score = round3(signals.reduce((n, s) => n + s.weight, 0));
  const structural = messages !== null || signals.some((s) => s.id === 'body_tool_declarations');
  const match = score >= threshold;

  return {
    match,
    score,
    threshold,
    signals,
    structural,
    structure,
    messages,
    path_signal: pathSignal,
    over_cap: Boolean(rec.truncated),
    upload_bearing: isUploadBearing({ headers, form: rec.form, ct }),
    attachment_candidates: rec.form ? formFileCandidates(rec.form) : [],
  };
}

/** An "upload-bearing request" for §7.3: a multipart/form body, or a form carrying a filename. */
export function isUploadBearing({ headers, form, ct }) {
  const base = (ct && ct.base) || contentTypeClass(headers).base;
  if (base.startsWith('multipart/form-data')) return true;
  return formFileCandidates(form).length > 0;
}

/** Form values that carry a filename. A filename alone is not attachment capture (§7.3). */
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
 * §8.2's response row: "a streaming or completion-shaped response contract for the same
 * request". Signature only — status, content type, framing headers. Never the response body,
 * which is a non-goal (§1.2) and is not available on this route anyway.
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
 * §7.5 Mode B: the answer to "a draft save and a chat call from the same origin are separated
 * by their bodies rather than their paths" is a **conjunction** — a generative request
 * structure *and* a generative response contract. A `messages`-shaped array is the first half
 * on its own; anything weaker needs the response half.
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

/** §8.2's last row: an automation marker raises confidence that a run came from an agent (Mode C, §7.5). */
export function automationMarker(headers) {
  const h = headers || {};
  const strong = ['x-automation', 'x-agent', 'x-playwright', 'x-puppeteer', 'x-selenium', 'x-browser-agent', 'x-client-agent'];
  for (const k of strong) if (h[k] !== undefined) return { present: true, marker: k, strength: 'high' };
  const ch = String(h['sec-ch-ua'] || '') + String(h['user-agent'] || '');
  if (/headless|automation/i.test(ch)) return { present: true, marker: 'client_hints', strength: 'high' };
  return { present: false, marker: '', strength: 'none' };
}

/**
 * §8.1 signals 1-3, the subset this route can see, canonicalised (sorted) and shape-replaced.
 * Route-specific signals are excluded by construction — this function is given only
 * destination, path shape and body shape — so one tool seen through two routes yields one
 * vector (§8.3 items 1 and 2). The tenant-declared label never enters the derivation.
 */
export function shapeVector(req) {
  const r = predicateRequest(req, Number.POSITIVE_INFINITY);
  const host = hostOf(req.url) || String(req.host || '').toLowerCase();
  return {
    v: 1,
    destination: registeredDomain(host),
    method: String(req.method || 'GET').toUpperCase(),
    path_shape: r.path_signal || normalisePath(pathnameOf(req.url)),
    content_type: contentTypeClass(req.headers || {}).base,
    body_shape: r.structure,
    message_count_bucket: bucket(r.messages ? r.messages.count : 0),
    role_values: r.messages ? [...r.messages.role_values].sort() : [],
    has_model_params: r.signals.some((s) => s.id === 'body_model_params'),
    has_tool_declarations: r.signals.some((s) => s.id === 'body_tool_declarations'),
  };
}

function bucket(n) {
  if (!n) return '0';
  if (n === 1) return '1';
  if (n <= 3) return '2-3';
  if (n <= 10) return '4-10';
  return '11+';
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

/** §7.4's 150 ms classification target is the classifier's; this is the shape pass and it is pure arithmetic. */
export function predicateBudgetMs() {
  return 0;
}
