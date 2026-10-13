/**
 * socket-submission.js — the prompt a catalogued web app sends over a WebSocket, read from the
 * frames the page puts on the wire.
 *
 * `webRequest` sees a WebSocket open and never its frames, so a tool that streams its prompt over a
 * socket (Microsoft Copilot, Microsoft 365 Copilot) is invisible to the request lanes: the handshake
 * is all they observe. The main-world hook (`content/ws-hook.js`) wraps `WebSocket.prototype.send`
 * for the Copilot socket hosts and hands each outgoing frame here; this module recognises the socket
 * by its host and path and reads the typed text out of the frame, exactly as `submission.js` reads a
 * known app's request body.
 *
 * A socket no recipe names, or a frame that is not a chat turn (a handshake, a ping, a metrics or a
 * stop frame), yields null and nothing is observed.
 */

/** The socket hosts the recipes below read. content/ws-hook.js posts frames for these; its test drives each one. */
export const SOCKET_HOSTS = Object.freeze(['copilot.microsoft.com', 'substrate.office.com']);

/** The SignalR record separator: one WebSocket message may carry several frames, each ending in it. */
const RS = '\u001e';

/**
 * app_key -> how its chat socket is recognised and where the typed text sits.
 *   host   matches the socket's host (an exact host or a leading-dot suffix);
 *   path   matches the socket's path;
 *   prompt reads the frame text the page sent and returns the typed prompt, or null.
 */
export const SOCKET_SUBMISSIONS = Object.freeze({
  // Consumer Microsoft Copilot: wss://copilot.microsoft.com/c/api/chat. The page sends one JSON
  // frame per turn, { "event": "send", "content": [{ "type": "text", "text": ... }], ... }.
  microsoft_copilot: {
    host: /(^|\.)copilot\.microsoft\.com$/,
    path: /^\/c\/api\/chat/,
    prompt: (text) => classicPrompt(text),
  },
  // Microsoft 365 Copilot (BizChat / Office web): wss://substrate.office.com/m365Copilot/Chathub/...
  // SignalR over WebSocket. The chat turn is a type-4 invocation to target "chat"; the typed text is
  // arguments[0].message.text. The handshake, metrics, ping and stop frames carry no such message.
  m365_copilot: {
    host: /(^|\.)substrate\.office\.com$/,
    path: /^\/m365Copilot\/Chathub/i,
    prompt: (text) => signalrPrompt(text),
  },
});

/**
 * The prompt of a catalogued app's chat frame, with the app it belongs to, or null when the socket
 * is not one we read or the frame is not a chat turn.
 *
 * @param {string} url  the socket's URL, as the page opened it
 * @param {string} data the frame the page sent (a string; binary frames are not chat turns here)
 * @returns {{app_key: string, prompt: string}|null}
 */
export function socketSubmission(url, data) {
  if (typeof data !== 'string' || data === '') return null;
  let host;
  let path;
  try {
    const u = new URL(url);
    host = u.hostname.toLowerCase().replace(/\.$/, '');
    path = u.pathname;
  } catch {
    return null;
  }
  for (const [app_key, recipe] of Object.entries(SOCKET_SUBMISSIONS)) {
    if (recipe.host.test(host) && recipe.path.test(path)) {
      const prompt = recipe.prompt(data);
      return prompt === null ? null : { app_key, prompt };
    }
  }
  return null;
}

/** The classic consumer frame: the text parts of a `send` event's content array. */
function classicPrompt(data) {
  for (const obj of frames(data)) {
    if (!obj || obj.event !== 'send' || !Array.isArray(obj.content)) continue;
    const text = obj.content
      .filter((part) => part && part.type === 'text' && typeof part.text === 'string')
      .map((part) => part.text)
      .join('')
      .trim();
    if (text !== '') return text;
  }
  return null;
}

/** The SignalR chat turn: arguments[0].message.text of a type-4 invocation to target "chat". */
function signalrPrompt(data) {
  for (const obj of frames(data)) {
    if (!obj || obj.type !== 4 || obj.target !== 'chat' || !Array.isArray(obj.arguments)) continue;
    const message = obj.arguments[0] && obj.arguments[0].message;
    const text = message && typeof message.text === 'string' ? message.text.trim() : '';
    if (text !== '') return text;
  }
  return null;
}

/** The JSON objects in a frame payload: one per record-separated chunk, bad chunks skipped. */
function* frames(data) {
  for (const chunk of data.split(RS)) {
    if (chunk === '') continue;
    try {
      yield JSON.parse(chunk);
    } catch {
      /* a chunk that is not JSON is not a chat turn */
    }
  }
}
