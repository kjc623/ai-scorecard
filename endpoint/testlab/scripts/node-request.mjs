// The Node client the lab uses to prove the cli.shim Node path end-to-end.
//
// It makes a plain https request to the intercepted destination and contains no proxy code of its
// own. Everything that routes it through proxy.tls and trusts the device CA comes from the shim's
// environment, sourced by the caller from /etc/profile.d/shadow-ai-capture.sh:
//   * NODE_OPTIONS=--require node-proxy.cjs installs the CONNECT-through-proxy agent; and
//   * NODE_EXTRA_CA_CERTS points at the device CA bundle, so Node trusts the minted leaf.
//
// It prints the status code and the peer certificate's issuer (which is the device CA when the
// request was intercepted, and the upstream CA when it went direct), and exits non-zero unless the
// status is 200.
import https from 'node:https';

const url = 'https://api.anthropic.test/v1/messages';
let socket = null;

const req = https.get(url, (res) => {
  socket = res.socket || socket;
  let body = '';
  res.on('data', (chunk) => (body += chunk));
  res.on('end', () => {
    const cert = socket && typeof socket.getPeerCertificate === 'function' ? socket.getPeerCertificate() : null;
    console.log(`statusCode ${res.statusCode}`);
    console.log(`peerIssuer ${cert?.issuer?.CN ?? ''}`);
    console.log(`peerSubject ${cert?.subject?.CN ?? ''}`);
    if (body.trim()) console.log(body.trim());
    process.exit(res.statusCode === 200 ? 0 : 1);
  });
});
req.on('socket', (s) => {
  if (!socket) socket = s;
});
req.on('error', (err) => {
  console.error(`request error: ${err.message}`);
  process.exit(1);
});
