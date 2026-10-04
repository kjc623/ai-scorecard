// The "upstream" the device re-originates to: a Node https server on 0.0.0.0:443 presenting
// /pki/upstream-server.crt|key. It answers 200 for /v1/messages (the intercepted destination the
// lab's clients POST/GET) and 200 for /healthz, so there is a real TLS destination a real client
// trusts — via the upstream CA the device installs, not via anything the device CA mints.
import https from 'node:https';
import fs from 'node:fs';

const cert = fs.readFileSync('/pki/upstream-server.crt');
const key = fs.readFileSync('/pki/upstream-server.key');

const server = https.createServer({ cert, key }, (req, res) => {
  if (req.url === '/healthz' || req.url === '/v1/messages') {
    res.writeHead(200, { 'content-type': 'application/json' });
    res.end(JSON.stringify({ ok: true }));
    return;
  }
  res.writeHead(404, { 'content-type': 'application/json' });
  res.end(JSON.stringify({ ok: false }));
});

server.listen(443, '0.0.0.0', () => {
  console.log('upstream listening on 0.0.0.0:443 (api.anthropic.test)');
});
