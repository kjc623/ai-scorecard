# capture-core — the endpoint agent

`capture-core` is the privileged device process of
[docs/01-collectors.md §3.1](../../docs/01-collectors.md): one static Go binary per platform that
hosts the collection providers, the policy engine, the spool, the browser's native-messaging host and
the device-to-cloud drain. On Windows the same binary hosts the service itself (`--service`).

Its job is to **wire** components. §4.1's provider contract, §11.2's mode gate, §13.2's bundle
verification and §3.5's startup and shutdown order all live in the packages below; the binary
resolves configuration, builds the graph and drives the supervisor. Policy is data it loads, never
logic it contains — a new interception scope or loopback port is a signed bundle change, not a
release.

## Packages

| Package | Responsibility |
|---|---|
| [core/](core/README.md) | The agent's spine: the provider contract and one coverage row per route, mode resolution and the content gate, envelope minting, counter and health assembly, and §3.5's ordering as data. |
| [policy/](policy/README.md) | The signed bundle: its shape, the four-cause verification chain, and the rule that a rejected bundle never widens what the device enforces. |
| [dedup/](dedup/README.md) | The device half of [docs/02 §4](../../docs/02-ingest-and-transport.md): `content_digest`, the `dedup_key` ladder and the canonicalisation they are pinned to. |
| [proxy/tlsproxy/](proxy/README.md) | `proxy.tls` — the egress interceptor, the only provider that reads content outside the browser, and therefore the one that must fail open. |
| [proxy/loopback/](proxy/README.md) | `proxy.loopback` — the broker that holds a local inference server's port, forwards to the relocated upstream and observes the plaintext bodies passing through it. |
| [detect/](detect/README.md) | `proc.detect` — the process and model detector: the cheapest provider, the only one that fails open by doing nothing. |
| [classifierlink/](classifierlink/README.md) | The capture-core side of the local socket to `classifier-host`, and the failure contract that keeps a classifier outage from failing a submission. |
| [drain/](drain/README.md) | The device-to-cloud drain (ADR 0020): enrolment, the DPoP/x509 transport, batching, settling and backoff. |
| [credential/](credential/README.md) | The sealed per-device credential at rest: the issued leaf (or registered DPoP key) plus the never-exported private key. |
| [dpop/](dpop/README.md) | The device side of the compact-JWS contract (RFC 7515 ES256, RFC 9449 DPoP). |
| [cmd/capture-core/](cmd/capture-core/README.md) | The binary: flags, subcommands, service deployment, and the self test that is the endpoint's end-to-end evidence. |

## How a submission flows

A provider hands `core.Pipeline` an observation: metadata obtainable without reading content, a lazy
`ContentReader`, the route's extractor, and the policy decision if there is one. The pipeline
resolves the effective mode from the bundle in force — the most restrictive of the tool,
population, device, class-ceiling and tenant-default contributions — and only then decides whether
the reader is ever called. At M0 it is not: the outcome is a refusal, not an empty body, because an
empty body and a forbidden body are different facts.

If the mode permits reading, the pipeline asks `classifierlink` for labels, computes the canonical
digest through `dedup` (wire `canon` in as the normaliser), mints an envelope the closed contract
will accept, and appends it to the spool. Every step that degrades — an unavailable classifier, an
extraction failure, an over-cap body, a missing canonicaliser — is recorded as `confidence: degraded`
with a named reason rather than being reported as "nothing found".

## Build and test

From `endpoint/capture-core`, with the offline prefix the repository's gates use:

```powershell
$env:GOCACHE="$PWD\..\..\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
go build ./...
go test ./...
```

`run.ps1` wraps that with `gofmt`, `vet`, the binary build and `-Selftest`; from the repository root,
`node tools/verify-all.mjs` runs this module with the rest of the device tier. The module consumes
`device/protocol`, `device/capture-spool` and `device/canon` through local `replace` directives —
those are Go module paths, not directories, and nothing here redefines their shapes.

## What it deliberately does not do

- **No policy of its own.** No compiled-in host list, port map, threshold or mode. Everything policy
  lives in the verified bundle, and with no valid bundle the device is at M0.
- **No `cli.shim`.** Modes E (CLI) and G are unrouted: the step exists in §3.5's order and is
  recorded as skipped, so the route has no coverage row rather than a healthy-looking empty one.
- **The device-to-cloud drain is opt-in, and the health channel is still file-only.** With
  `--device-endpoint` unset (the default) nothing is sent to a server and the shutdown drain reports
  what is still spooled. With it set, [drain/](drain/README.md) enrols the device (or loads the
  sealed credential), obtains a DPoP token or presents the x509 leaf, and POSTs `/v1/events` batches
  oldest-first with full-jitter backoff, settling each record from the per-event outcome. This path
  is proven end-to-end against the local auth lab; health is still a file, not `POST /v1/health`,
  which `control-api` does not yet serve.
- **No M3 content store.** An M3 observation is refused rather than emitted without the content it
  says it holds; the local store and grant-bound retrieval are not implemented here.
- **No platform facilities wired.** The system proxy, the OS trust store and DPAPI/Keychain key
  sealing are interfaces; this build wires none of them, so `proxy.tls` reports `degraded` with a
  named detail instead of health. Process enumeration is an interface in `detect` with one partial
  implementation: `--proc-detect` wires a Windows `tasklist` enumerator that sees image names and
  PIDs only, and on any other platform the route is not started.
- **Enrolment is real, identity is still flags.** The device generates its keypair, POSTs
  `POST /v1/enrol` (a PKCS#10 CSR in `x509` mode, the public JWK plus a proof in `dpop` mode), and
  seals the issued credential beside the spool. The envelope identity (`--tenant-id`, `--device-id`)
  remains configuration; the credential's hardware-identity seed is `--mdm-id` when set, and falls
  back to hashing the device identity (an ASSUMPTION, not a hardware binding).

Deployment, the full flag list and the self test's assertions are in
[cmd/capture-core/README.md](cmd/capture-core/README.md).
