# cli — the CLI trust shim (`cli.shim`)

`cli.shim` is docs/01-collectors.md §4.5: the provider that makes modes E (CLI) and G capturable
through `proxy.tls` by configuring the shell environment CLI tools launch from (E10). It observes
nothing directly and holds no ports. It writes files and environment entries, and it **fails open
by passing every command through untouched**.

Its two jobs, per §4.5's table:

| Runtime | Trust (E9) | Proxy (E9) | What the shim sets |
|---|---|---|---|
| Go | OS trust store — works unmodified | standard proxy variables | proxy variables only |
| Node.js | bundled CA list — needs an added bundle path | `fetch`/undici ignores proxy env below Node 24; the `https` stack needs a bootstrap | `NODE_EXTRA_CA_CERTS`, `NODE_USE_ENV_PROXY=1`, and a `--require` bootstrap for the `https` stack |
| Python (`requests`) | `certifi` bundle — needs a CA bundle path | standard proxy variables | `REQUESTS_CA_BUNDLE` plus proxy variables |

## What it writes

- a CA bundle from the bundle's `interception.root_ca_pem` (mode 0600);
- a managed profile exporting `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (both cases),
  `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`,
  `NODE_USE_ENV_PROXY=1`, and (when `node_require`) `NODE_OPTIONS=--require <node-proxy.cjs>`;
- a **launcher** (`claude-sac` / `claude-sac.cmd`) that sets the same environment and execs
  `claude`. This is what guarantees capture: a coding agent started from an IDE, from a
  freshly-opened shell, or from Explorer does not depend on the machine environment having been
  re-read after `setx /M`;
- on Linux a machine-environment file beside the profile; on macOS a marker-guarded loader line in
  `/etc/zshenv`; on Windows a `shim.cmd` and `setx /M` for each variable (a non-admin failure is
  degraded, not fatal);
- when the bundle sets `cli_shim.node_require`, a stdlib-only `node-proxy.cjs` and
  `NODE_OPTIONS=--require <path>`. The bootstrap subclasses `https.Agent` so `createConnection`
  issues a CONNECT to the proxy and honours `NO_PROXY`; it does **not** pass a `ca` option, because
  the device CA already reaches Node through `NODE_EXTRA_CA_CERTS` and replacing the trust store
  would break the public destinations the proxy blind-tunnels.

**Two client paths, both covered.** Claude Code (and the Anthropic SDK) use global `fetch`, which
does **not** read `HTTP(S)_PROXY` below Node 24 — hence `NODE_USE_ENV_PROXY=1` on Node 24+, and
Claude Code's own `HTTPS_PROXY` support on older runtimes. Clients using the `http`/`https` stack are
covered by the `--require` bootstrap. `endpoint/testlab` exercises both an `https.get` client and a
`fetch` client, plus the launcher, end to end.

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
