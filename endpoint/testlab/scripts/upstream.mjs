// The "upstream" the device re-originates to: a Node https server on 0.0.0.0:443 presenting
// /pki/upstream-server.crt|key. It answers for /healthz and the Anthropic Messages API surface a
// coding agent calls (POST /v1/messages, POST /v1/messages/count_tokens), so there is a real TLS
// destination a real client trusts — via the upstream CA the device installs, not via anything the
// device CA mints.
//
// The Messages endpoint returns an Anthropic-shaped response (not `{"ok":true}`) so a real CLI can
// be run against it headless: an SSE stream when the request asks for `stream:true`, and a single
// JSON document otherwise. Every request is logged to stdout (captured in `docker compose logs
// upstream`), which is how run.mjs proves the *real* client — not a synthetic fetch — reached here.
import https from 'node:https';
import fs from 'node:fs';

const cert = fs.readFileSync('/pki/upstream-server.crt');
const key = fs.readFileSync('/pki/upstream-server.key');

const MODEL = 'claude-sonnet-4-5';

function readBody(req) {
  return new Promise((resolve) => {
    let data = '';
    req.on('data', (c) => (data += c));
    req.on('end', () => resolve(data));
  });
}

function anthropicId(prefix) {
  return `${prefix}_${Math.random().toString(16).slice(2, 10)}${Date.now().toString(16).slice(-8)}`;
}

// A minimal, valid Messages response (streaming or not), keyed to the request's `stream` flag and
// echoing back the requested model so the client does not see a model it did not ask for.
function respond(req, res, body) {
  let parsed = {};
  try {
    parsed = body ? JSON.parse(body) : {};
  } catch {
    /* a non-JSON body still gets a well-shaped response */
  }

  const msgId = anthropicId('msg');
  const model = parsed.model || MODEL;
  const text = 'hello from the fake upstream';

  if (parsed.stream === true) {
    res.writeHead(200, {
      'content-type': 'text/event-stream',
      'cache-control': 'no-cache',
      connection: 'keep-alive',
    });
    const send = (event, data) => {
      res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
    };
    send('message_start', {
      type: 'message_start',
      message: {
        id: msgId,
        type: 'message',
        role: 'assistant',
        model,
        content: [],
        stop_reason: null,
        stop_sequence: null,
        usage: { input_tokens: 10, output_tokens: 1 },
      },
    });
    send('content_block_start', {
      type: 'content_block_start',
      index: 0,
      content_block: { type: 'text', text: '' },
    });
    send('content_block_delta', {
      type: 'content_block_delta',
      index: 0,
      delta: { type: 'text_delta', text },
    });
    send('content_block_stop', { type: 'content_block_stop', index: 0 });
    send('message_delta', {
      type: 'message_delta',
      delta: { stop_reason: 'end_turn', stop_sequence: null },
      usage: { output_tokens: 5 },
    });
    send('message_stop', { type: 'message_stop' });
    res.end();
    return;
  }

  res.writeHead(200, { 'content-type': 'application/json' });
  res.end(
    JSON.stringify({
      id: msgId,
      type: 'message',
      role: 'assistant',
      model,
      content: [{ type: 'text', text }],
      stop_reason: 'end_turn',
      stop_sequence: null,
      usage: { input_tokens: 10, output_tokens: 5 },
    }),
  );
}

const server = https.createServer({ cert, key }, async (req, res) => {
  const path = new URL(req.url, 'https://api.anthropic.test').pathname;
  console.log(`upstream ${req.method} ${req.url} user-agent=${req.headers['user-agent'] ?? ''} ` +
    `anthropic-version=${req.headers['anthropic-version'] ?? ''} auth=${req.headers['x-api-key'] ?? req.headers.authorization ?? ''}`);

  if (path === '/healthz') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    return;
  }

  const body = await readBody(req);
  console.log(`upstream ${req.method} ${req.url} body=${body.slice(0, 512)}`);

  // Both GET (the lab's plain https.get check) and POST (a real coding agent) hit /v1/messages.
  if (path === '/v1/messages' && (req.method === 'POST' || req.method === 'GET')) {
    respond(req, res, body);
    return;
  }

  if (req.method === 'POST' && path === '/v1/messages/count_tokens') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ input_tokens: 10 }));
    return;
  }

  // Anything else (the API key header check, a malformed path, …) is a real API-shaped error, so a
  // client fails loudly and clearly rather than misparsing a silent `{"ok":false}`.
  res.writeHead(404, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ type: 'error', error: { type: 'not_found_error', message: `no such endpoint: ${req.method} ${req.url}` } }));
});

server.listen(443, '0.0.0.0', () => {
  console.log('upstream listening on 0.0.0.0:443 (api.anthropic.test)');
});
