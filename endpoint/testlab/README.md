# endpoint/testlab — capture-core as a Linux appliance

The headline deliverable for the endpoint: a container that runs `capture-core` on Linux as an
appliance-style device, and proves — with a real HTTPS client and **no `--insecure`** — that

1. **interception** works: `proxy.tls` terminates `api.anthropic.test:443`, re-originates to the real
   upstream, and records the observation to the spool;
2. **OS trust** works: `--trust-install` installs the per-device CA into the Linux trust store
   (`/usr/local/share/ca-certificates/sac-device-ca.crt` + `update-ca-certificates`), the client
   trusts the minted leaf through the *system* store, and `--trust-remove-on-stop` removes it again;
3. **cli.shim** works **transparently**: a plain login shell inherits the proxy and CA environment
   (`/etc/profile.d/shadow-ai-capture.sh`, both cases, plus `NODE_USE_ENV_PROXY=1`) with no wrapper
   and no manual export; the `https` runtime is routed by the `NODE_OPTIONS=--require node-proxy.cjs`
   bootstrap, the **`fetch`** runtime (the path Claude Code's SDK uses) by `NODE_USE_ENV_PROXY`, and
   both trust the device CA via `NODE_EXTRA_CA_CERTS`.

## Run it

```sh
# from the repository root, with the offline Go environment the gates use
export GOCACHE=/workspace/.tools/gocache GOPROXY=off GOTOOLCHAIN=local GOFLAGS=-mod=mod

node endpoint/testlab/run.mjs            # build binaries + image, up, assert, down -v
node endpoint/testlab/run.mjs --keep     # same, but leave the lab running for inspection
node endpoint/testlab/run.mjs --down     # tear down (volume included)
```

`run.mjs` compiles the two Linux binaries (or you can do it by hand):

```sh
cd endpoint/capture-core
GOOS=linux GOARCH=amd64 go build -o ../testlab/build/capture-core ./cmd/capture-core
GOOS=linux GOARCH=amd64 go build -o ../testlab/build/sac-bundle ./cmd/sac-bundle
```

## What the lab is

Three services share one image, `sac/testlab-endpoint:dev` (`node:24-alpine` + `ca-certificates curl
openssl bash`, with `build/` at `/app/bin`, `scripts/` at `/app/scripts`, and the **real Claude Code
terminal CLI** installed globally via `npm i -g @anthropic-ai/claude-code --allow-scripts=…`):

| service | what it does |
|---|---|
| `pki` | a one-shot that mints the **upstream** CA and a server cert for `api.anthropic.test` into the `/pki` named volume (the device CA itself is minted separately, by `sac-bundle` inside the device container). |
| `upstream` | a Node `https` server on `0.0.0.0:443` presenting `/pki/upstream-server.crt\|key`, network-aliased `api.anthropic.test`. It answers the Anthropic Messages surface (`POST/GET /v1/messages`, `POST /v1/messages/count_tokens`) with an Anthropic-shaped response — an SSE stream when `stream:true`, a JSON document otherwise — and logs every request (method, url, user-agent, api key) to `docker compose logs upstream`, which is how `run.mjs` proves the *real* CLI reached here. |
| `device` | the appliance. Its entrypoint installs the upstream CA into the OS trust store (so `proxy.tls` trusts the upstream it re-originates to), runs `sac-bundle`, and starts `capture-core` with the trust/CA/shim/provider flags. |

The device's entrypoint and the exact `capture-core` argv are in
[`scripts/device-entry.sh`](scripts/device-entry.sh). It holds the container alive after
`capture-core` exits, so `run.mjs` can `exec` the trust-removal assertions.

## The checks (`run.mjs`)

* **(a) curl** — `curl --proxy "$HTTPS_PROXY" -X POST https://api.anthropic.test/v1/messages` returns
  `200` with **no `--insecure`**: the system trust store proves the device CA was installed.
* **(b) OS trust** — `/usr/local/share/ca-certificates/sac-device-ca.crt` exists and its subject is
  trusted in `/etc/ssl/certs/ca-certificates.crt` (asserted with `openssl verify -CAfile`).
* **(c) Node `https`** — `node scripts/node-request.mjs` run in a **login shell** (`sh -lc`), exactly
  as a user would launch a tool: the shell sources `/etc/profile.d`, so no wrapper and no manual
  export are involved. A plain `https.get` is routed by the shim's `NODE_OPTIONS` bootstrap and
  trusts the device CA via `NODE_EXTRA_CA_CERTS`. The script also prints the peer certificate's
  issuer, so the check asserts the leaf was minted by the device CA (genuinely intercepted).
* **(c2) Node `fetch`** — `node scripts/node-fetch-request.mjs` from the same login shell; this is
  the path Claude Code's SDK uses. Node 24 honours `NODE_USE_ENV_PROXY=1` from the profile. A `200`
  proves interception: the upstream CA is not in Node's bundled roots, so a direct connection would
  fail verification.
* **(c3) transparency** — `sh -lc env` (a new login shell) contains `HTTPS_PROXY`,
  `NODE_USE_ENV_PROXY` and `NODE_EXTRA_CA_CERTS` with nothing the user did.
* **(d2) the real Claude Code CLI** — `claude -p "hello"` run inside the device from a login shell
  (which sources `/etc/profile.d`) with `ANTHROPIC_API_KEY=sk-ant-test` and
  `ANTHROPIC_BASE_URL=https://api.anthropic.test`, so the actual coding agent targets the fake
  upstream. `--bare --permission-prompts none --tools ""` keeps it fully headless and tool-free.
  This is the proof that the *real* CLI — not a curl or a synthetic fetch — is captured by
  `proxy.tls`: it asserts the CLI exits `0` and prints the upstream's response, that the `proxy.tls`
  `observed`/`emitted` counters advance for its request, that the spool gains a non-empty segment,
  that the `cli.shim` row stays `healthy`, and that the upstream log contains the CLI's own
  `user-agent=claude-cli/2.1.289` on `POST /v1/messages`. It also prints `claude --version` and
  `node -v`.
* **(d) health** — the health channel's `proxy.tls` row reports `observed>=1` and `emitted>=1`, and
  the `cli.shim` row reports `state=healthy`.
* **(e) spool** — the spool directory holds a non-empty segment.
* **(f) removal** — `kill -TERM $(cat /state/capture-core.pid)`; after it exits the device CA file
  is gone and its subject is no longer in the system bundle.

## Windows / macOS parity

The same binary flags drive trust on Windows and macOS; only the platform half behind `trust/`
differs (`certutil -addstore` on Windows, `security add-trusted-cert` on macOS). Those command
constructions are **unit-tested** in `endpoint/capture-core/trust/` (the `Runner` seam scripts the
exact argv without touching a real store), but they are **not run here**: this lab exercises the
Linux path for real, which is the one platform a Linux host can actually mutate the trust store of.

**Windows is exercised elsewhere, for real.** The lab MSI ([installer/README.md](../../installer/README.md),
`node installer/lab-msi.mjs`) installs the agent as a service on a Windows 11 host with
`--trust-install`, `--trust-remove-on-stop` and `--cli-shim` set. There the device root was
installed into the machine `Root` store when the service started, the machine environment carried
the proxy and CA variables, the real Claude Code CLI was intercepted from a new process, and after
an uninstall the root, the variables and the files were gone. That is one host and a manual run,
not a repeatable lab like this one: nothing asserts it in CI. It also showed two things this lab
cannot, because here every client reaches only the intercepted upstream: the Windows shim was
never setting the machine environment (no command runner was wired), and a CA bundle holding only
the device root breaks every out-of-scope destination for runtimes that read `SSL_CERT_FILE` or
`REQUESTS_CA_BUNDLE`. Both are described in [capture-core/cli/README.md](../capture-core/cli/README.md).

**macOS is still unit tests only.** Nothing has run `security add-trusted-cert`, the `/etc/zshenv`
loader line, or the PKG.

**Windows smoke-test caveat.** Windows `curl.exe` uses Schannel, which checks revocation and fails
closed with `CRYPT_E_NO_REVOCATION_CHECK` for a locally trusted per-device CA that publishes no
CRL/OCSP. Pass `curl.exe --ssl-no-revoke` (or set the equivalent revocation policy in the client).
That is a client revocation setting, not a proxy or trust-store failure. Also avoid PowerShell
mangling the JSON body: write it to a file and use `--data-binary "@body.json"`.

## Notes and residual gaps

* **`cli_shim.node_require` comes from the generator.** `sac-bundle --node-require` (default true)
  makes the bundle carry `cli_shim.node_require`, so `cli.shim` writes `node-proxy.cjs` and sets
  `NODE_OPTIONS=--require`. `--shim-managed-dir /state/shim` pins the managed directory the bundle
  names, matching `--shim-dir`.
* **No classifier host.** The lab runs rules-only (no `--classifier-address`), so after the first
  *classified* (POST) observation `proxy.tls` honestly reports `degraded` with
  `detail=classifier_unavailable` (§4.2: it never claims healthy while a named capability is
  unavailable). The startup row is `healthy` (bound + end-to-end canary probe passed), which is what
  the readiness wait asserts; the `observed`/`emitted` counters still prove interception happened.
* **No system proxy / no DPAPI / no Keychain.** As `cmd/capture-core/README.md` documents, the system
  proxy is an interface with no implementation in this build, and the spool/CA keys are `0600` files
  that report unsealed. The lab therefore sets `HTTPS_PROXY` via the shim's own profile, which is
  exactly what `cli.shim` is for.
* **Generated material is never committed.** The upstream PKI lives in a named volume, and the device
  CA/key and the spool live inside the device container; `build/` (the host-compiled binaries) is
  gitignored.
* **Claude Code 2.1.289 is a native binary, not Node.** `npm i -g @anthropic-ai/claude-code`
  installs the npm wrapper plus a platform-native optional dependency (`…-linux-x64-musl` on
  alpine); the postinstall copies that self-contained ~240MB ELF binary over `bin/claude.exe`. It is
  a Bun-based binary, so `NODE_OPTIONS`/`NODE_EXTRA_CA_CERTS`/`NODE_USE_ENV_PROXY` do **not** apply to
  it. The lab therefore proves that the real CLI is captured by the parts of the shim that do apply:
  it honours `HTTPS_PROXY` from the process env itself (no `~/.claude/settings.json` needed), and it
  trusts the device CA through the OS trust store (`--trust-install`) **or** `SSL_CERT_FILE`
  (either alone suffices; both are set here). It proxies *all* HTTPS — including `api.anthropic.com`
  telemetry — through `HTTPS_PROXY`, so nothing is bypassed.
* **The image build needs npm network access.** The Go binaries compile offline (`GOPROXY=off`), but
  the `npm i -g @anthropic-ai/claude-code` step in the `Dockerfile` downloads the npm wrapper plus
  the ~240MB native binary from the registry. If the build runs air-gapped, that layer fails; the
  offline fallback is the SDK/fetch path already exercised by checks **(c)/(c2)**.
