# cli — the CLI trust shim (`cli.shim`)

`cli.shim` is docs/01-collectors.md §4.5: the provider that makes modes E (CLI) and G capturable
through `proxy.tls` by configuring the shell environment CLI tools launch from (E10). It observes
nothing directly and holds no ports. It writes files and environment entries, and it **fails open
by passing every command through untouched**.

Its two jobs, per §4.5's table:

| Runtime | Trust (E9) | Proxy (E9) | What the shim sets |
|---|---|---|---|
| Go | OS trust store — works unmodified | standard proxy variables | proxy variables only |
| Node.js | bundled CA list — needs an added bundle path | does **not** read proxy variables by default | `NODE_EXTRA_CA_CERTS` plus a bootstrap that gives Node's HTTP stack the CONNECT proxy |
| Python (`requests`) | `certifi` bundle — needs a CA bundle path | standard proxy variables | `REQUESTS_CA_BUNDLE` plus proxy variables |

## What it writes

- a CA bundle from the bundle's `interception.root_ca_pem` (mode 0600);
- a managed profile exporting `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`, `NODE_EXTRA_CA_CERTS`,
  `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and `CURL_CA_BUNDLE`;
- on Linux a machine-environment file beside the profile; on macOS a marker-guarded loader line in
  `/etc/zshenv`; on Windows a `shim.cmd` and `setx /M` for each variable (a non-admin failure is
  degraded, not fatal);
- when the bundle sets `cli_shim.node_require`, a stdlib-only `node-proxy.cjs` and
  `NODE_OPTIONS=--require <path>`. The bootstrap subclasses `https.Agent` so `createConnection`
  issues a CONNECT to the proxy; it does **not** pass a `ca` option, because the device CA already
  reaches Node through `NODE_EXTRA_CA_CERTS` and replacing the trust store would break the public
  destinations the proxy blind-tunnels. That is the "explicit proxy configuration Node's HTTP stack
  needs".

The files are staged under the bundle's `cli_shim.managed_dir`, the `--shim-dir` flag, or a per-OS
default. Start is transactional: a failure removes everything it wrote, because a half-written
profile is worse than none.

## Health is §4.5's three checks

Healthy requires all three, and never happens without a root CA:

1. the profile exists with the expected content → else `degraded detail=shim_profile_missing`;
2. the CA bundle parses and contains the root → else `degraded detail=shim_ca_bundle_unreadable`;
3. where an `EnvProbe` is supplied, a child inherited the environment → else
   `degraded detail=shim_not_inherited`.

With no `EnvProbe` the third check is skipped rather than passed: a provider with no way to read an
environment must not fail health for it, and the gap is named in the README rather than hidden.

A kill switch (`mode: disable`) for `cli.shim` **or** `proxy.tls` removes the managed files and
reports `absent detail=killed`, because the proxy it feeds has stopped enforcing. The trust store is
never touched by this provider — `trust` owns that.

## Tests

`cli_test.go` generates a real root certificate, drives the provider against `t.TempDir()` and a fake
`Runner`, and covers: file content and permissions, the CA bundle containing the root, all three
degraded causes, the environment-inherited check, the kill switch removing files and reporting
`absent/killed`, `Start`/`Stop` idempotence, the Node bootstrap's shape, and the counters. A
compile-time assertion pins `*Provider` to `core.Provider`.
