// The fetch client the lab uses to prove the Claude Code path.
//
// Claude Code (and the Anthropic SDK it uses) call global fetch/undici, which does NOT read
// HTTP(S)_PROXY unless NODE_USE_ENV_PROXY is set (Node 24+). This client contains no proxy code of
// its own: routing comes from the shim profile's HTTPS_PROXY + NODE_USE_ENV_PROXY, and trust comes
// from NODE_EXTRA_CA_CERTS. A direct connection would fail, because the upstream CA the proxy
// re-originates to is not in Node's bundled root store.
const url = 'https://api.anthropic.test/v1/messages';

try {
  const res = await fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ messages: [{ role: 'user', content: 'hello from fetch' }] }),
  });
  const body = await res.text();
  console.log(`statusCode ${res.status}`);
  console.log(`peerIntercepted ${res.status === 200}`);
  if (body.trim()) console.log(body.trim());
  process.exit(res.status === 200 ? 0 : 1);
} catch (err) {
  console.error(`fetch error: ${err.message}`);
  process.exit(1);
}
