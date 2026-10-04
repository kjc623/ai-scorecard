package cli

// nodeProxyScript is the stdlib-only Node bootstrap the shim writes when NodeRequire is
// set. Node does not honour HTTP_PROXY/HTTPS_PROXY by default, so a plain environment
// routes its requests direct and proxy.tls never sees them (§4.5). This script installs
// an https.Agent subclass whose createConnection CONNECTs through HTTPS_PROXY and wraps
// the socket with tls.connect({servername, ca}), then makes it the global agent.
//
// It fails open: with no proxy configured it defers to the stock Agent, and it is loaded
// once via NODE_OPTIONS=--require. It is stdlib-only on purpose — nothing here is fetched
// at runtime, and the file must parse as plain JavaScript.
const nodeProxyScript = `// Shadow AI Capture — Node runtime shim (cli.shim).
//
// Node's HTTP stack ignores HTTP_PROXY/HTTPS_PROXY, so this bootstrap installs an
// https.Agent that CONNECTs through the configured proxy and trusts the device CA bundle,
// then makes it the global agent. Loaded via NODE_OPTIONS=--require. Stdlib only; fail
// open (no proxy configured -> the stock agent).
'use strict';

const https = require('https');
const tls = require('tls');
const net = require('net');
const fs = require('fs');

function proxyTarget() {
  const raw = process.env.HTTPS_PROXY || process.env.https_proxy ||
              process.env.HTTP_PROXY || process.env.http_proxy;
  if (!raw) return null;
  try {
    const u = new URL(raw);
    if (!u.hostname) return null;
    return {
      host: u.hostname,
      port: u.port ? Number(u.port) : (u.protocol === 'https:' ? 443 : 80),
    };
  } catch (_) {
    return null;
  }
}

function caBundle() {
  const path = process.env.NODE_EXTRA_CA_CERTS || process.env.SSL_CERT_FILE;
  if (!path) return undefined;
  try {
    return fs.readFileSync(path);
  } catch (_) {
    return undefined;
  }
}

const PROXY = proxyTarget();
const CA = caBundle();

class ShadowProxyAgent extends https.Agent {
  constructor(options) {
    super(options);
    this._proxy = PROXY;
    this._ca = CA;
  }

  createConnection(options, callback) {
    const proxy = this._proxy;
    const ca = this._ca;

    // Fail open: with no proxy configured, behave like the default agent.
    if (!proxy) {
      return super.createConnection(options, callback);
    }

    const socket = net.connect(proxy.port, proxy.host);
    const target = options.host + ':' + options.port;
    let upgraded = false;
    let buf = Buffer.alloc(0);

    socket.on('connect', () => {
      socket.write(
        'CONNECT ' + target + ' HTTP/1.1\r\n' +
        'Host: ' + target + '\r\n' +
        'Proxy-Connection: keep-alive\r\n\r\n'
      );
    });

    const onData = (chunk) => {
      buf = Buffer.concat([buf, chunk]);
      const idx = buf.indexOf('\r\n\r\n');
      if (idx === -1) return;
      const head = buf.slice(0, idx).toString('latin1');
      if (!/^HTTP\/1\.[01] 200/.test(head)) {
        socket.destroy(new Error('proxy CONNECT rejected: ' + head.split('\r\n')[0]));
        return;
      }
      socket.removeListener('data', onData);
      const rest = buf.slice(idx + 4);
      upgraded = true;

      const tlsSocket = tls.connect({
        socket: socket,
        servername: options.servername || options.host,
        ca: ca,
      });
      if (rest.length > 0) tlsSocket.unshift(rest);
      callback(null, tlsSocket);
    };

    socket.on('data', onData);
    socket.on('error', (err) => {
      if (!upgraded) callback(err);
    });
  }
}

https.globalAgent = new ShadowProxyAgent({ keepAlive: true });
`
