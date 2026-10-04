# endpoint/testlab — capture-core as a Linux appliance

The headline deliverable for the endpoint: a container that runs `capture-core` on Linux as an
appliance-style device, and proves — with a real HTTPS client and **no `--insecure`** — that

1. **interception** works: `proxy.tls` terminates `api.anthropic.test:443`, re-originates to the real
   upstream, and records the observation to the spool;
2. **OS trust** works: `--trust-install` installs the per-device CA into the Linux trust store
   (`/usr/local/share/ca-certificates/sac-device-ca.crt` + `update-ca-certificates`), the client
   trusts the minted leaf through the *system* store, and `--trust-remove-on-stop` removes it again;
3. **cli.shim** works: `/etc/profile.d/shadow-ai-capture.sh` exports the proxy and CA-bundle
   variables (both cases, plus `NODE_USE_ENV_PROXY=1`), the `https` runtime is routed through the
   proxy by the shim's `NODE_OPTIONS=--require node-proxy.cjs` bootstrap, the **`fetch`** runtime
   (the path Claude Code's SDK uses) is routed by `NODE_USE_ENV_PROXY`, both trust the device CA via
   `NODE_EXTRA_CA_CERTS`, and the generated `claude-sac` launcher hands the same environment to the
   agent it execs.

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
openssl bash`, with `build/` at `/app/bin` and `scripts/` at `/app/scripts`):

| service | what it does |
|---|---|
| `pki` | a one-shot that mints the **upstream** CA and a server cert for `api.anthropic.test` into the `/pki` named volume (the device CA itself is minted separately, by `sac-bundle` inside the device container). |
| `upstream` | a Node `https` server on `0.0.0.0:443` presenting `/pki/upstream-server.crt\|key`, network-aliased `api.anthropic.test`. It returns `200 {"ok":true}` for `/v1/messages` and `/healthz`. |
| `device` | the appliance. Its entrypoint installs the upstream CA into the OS trust store (so `proxy.tls` trusts the upstream it re-originates to), runs `sac-bundle`, and starts `capture-core` with the trust/CA/shim/provider flags. |

The device's entrypoint and the exact `capture-core` argv are in
[`scripts/device-entry.sh`](scripts/device-entry.sh). It holds the container alive after
`capture-core` exits, so `run.mjs` can `exec` the trust-removal assertions.

## The checks (`run.mjs`)

* **(a) curl** — `curl --proxy "$HTTPS_PROXY" -X POST https://api.anthropic.test/v1/messages` returns
  `200` with **no `--insecure`**: the system trust store proves the device CA was installed.
* **(b) OS trust** — `/usr/local/share/ca-certificates/sac-device-ca.crt` exists and its subject is
  trusted in `/etc/ssl/certs/ca-certificates.crt` (asserted with `openssl verify -CAfile`).
* **(c) Node `https`** — `node scripts/node-request.mjs` after sourcing the profile: a plain
  `https.get` routed through the proxy by the shim's `NODE_OPTIONS` bootstrap, trusting the device CA
  via `NODE_EXTRA_CA_CERTS`. The script also prints the peer certificate's issuer, so the check
  asserts the leaf was minted by the device CA (i.e. the request was genuinely intercepted, not a
  direct connection that happened to succeed).
* **(c2) Node `fetch`** — `node scripts/node-fetch-request.mjs`, the path Claude Code's SDK uses.
  Node 24 honours `NODE_USE_ENV_PROXY=1` from the profile. A `200` proves interception: the upstream
  CA is not in Node's bundled roots, so a direct connection would fail verification.
* **(c3) launcher** — the generated `claude-sac` is run with a stub `claude` on `PATH`; the check
  asserts the child received `HTTPS_PROXY`, `NODE_USE_ENV_PROXY` and `NODE_EXTRA_CA_CERTS`.
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
