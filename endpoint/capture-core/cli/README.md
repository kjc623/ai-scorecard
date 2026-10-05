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

- a CA bundle from the bundle's `interception.root_ca_pem` (mode 0600). On Windows the device root
  is followed by every certificate in the machine's `ROOT` store (`roots_windows.go`); see "The
  bundle replaces a trust list" below;
- a managed profile exporting `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` (both cases),
  `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `CURL_CA_BUNDLE`,
  `NODE_USE_ENV_PROXY=1`, and (when `node_require`) `NODE_OPTIONS=--require <node-proxy.cjs>`;
- on Linux a machine-environment file beside the profile; on macOS a marker-guarded loader line in
  `/etc/zshenv`; on Windows a `shim.cmd` and `setx /M` for each variable (a non-admin failure is
  degraded, not fatal), deleted again from the machine environment when the provider stops. The
  binary wires `trust.ExecRunner` as the command runner; before it did, the service logged "no
  runner configured; machine environment not set", wrote the files, and no process ever inherited
  them;
- when the bundle sets `cli_shim.node_require`, a stdlib-only `node-proxy.cjs` and
  `NODE_OPTIONS=--require <path>`. The bootstrap subclasses `https.Agent` so `createConnection`
  issues a CONNECT to the proxy and honours `NO_PROXY`; it does **not** pass a `ca` option, because
  the device CA already reaches Node through `NODE_EXTRA_CA_CERTS` and replacing the trust store
  would break the public destinations the proxy blind-tunnels.

**Two client paths, both covered, and neither requires the user to change how they launch a
tool.** The shim is transparent: it configures the *machine/user environment* and the OS trust
store (in production these are delivered by MDM/GPO — docs/05 §6), so the user opens a terminal and
runs `claude` as usual, and a new login session inherits the proxy and CA. Claude Code's `fetch`
path is covered on Node 24+ by `NODE_USE_ENV_PROXY` and on older runtimes by Claude Code's own
`HTTPS_PROXY` support; clients using the `http`/`https` stack are covered by the `--require`
bootstrap. `endpoint/testlab` exercises both from a plain login shell, with no wrapper and no manual
export. (As with any environment change, a process that was already running when the shim started
does not pick it up until it is restarted; that is standard for env-based configuration, and the
health row's `shim_not_inherited` detail exists so a missing inheritance is visible rather than
silent.)

## The bundle replaces a trust list

`NODE_EXTRA_CA_CERTS` adds to a runtime's roots. `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE` and
`CURL_CA_BUNDLE` **replace** them. The proxy intercepts only the destinations in scope and
blind-tunnels everything else with its real certificate, so a bundle holding the device root alone
makes every out-of-scope HTTPS request fail verification in any runtime that reads one of those
three. On Windows that was observed: with the machine environment set, Python `requests` could not
reach `pypi.org` (and so neither could `pip`), while the intercepted host worked.

So on Windows the bundle is the device root followed by the machine `ROOT` store, read through
`CertOpenSystemStore`. With that, `requests` reached an out-of-scope host and the intercepted one.
Two limits, both stated rather than hidden. Windows fetches rarely-used roots on demand, so the
store holds the roots present now, not every root Windows would trust; a destination under a root
the machine has never needed will still fail in those runtimes until something else pulls it in.
And the bundle is written when the provider starts, so a root added later is not in it until the
service restarts.

On Linux and macOS the bundle is still the device root alone. `endpoint/testlab` does not show the
problem because everything its clients reach is the intercepted upstream; the same replacement
applies there to an out-of-scope destination and is not fixed.

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

Three of these fail on Windows and did before the Windows work above:
`TestStartWritesFilesWithContentAndPermissions` (asserts a POSIX `0600` mode), `TestNodeProxyScript`
and `TestCounters` (both assert the Linux profile shape). `roots_windows.go` has no unit test: it
reads the real machine store, and its evidence is the installed service on Windows 11, where the
bundle held 58 certificates.
