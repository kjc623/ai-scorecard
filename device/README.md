# device

Everything that runs on or is installed onto a managed device: the capture agent, the browser
extension that hands it submissions, and the installers that put both in place. The agent observes
AI submissions (the local TLS proxy for CLI tools, the loopback broker for local inference servers,
the browser extension through native messaging), classifies them on the device, applies the tenant's
signed collection policy, spools them encrypted and delivers them to the device edge over mutual TLS.

| Component | What it is |
|---|---|
| `protocol` | The shapes the device components and the device edge agree on: enrolment, policy, events, health, content upload, native messaging, classifier frames, spool records. |
| `capture-core` | The agent binary (`cmd/capture-core`) and its packages: `core` (pipeline, mode gate, envelope, supervisor), `policy`, `proxy/tlsproxy`, `proxy/loopback`, `cli` (CLI trust shim), `trust`, `drain` (enrolment, delivery, policy fetch, health, content upload), `credential`, `contentstore`, `state`, `hostinfo`, `dedup`, `classifierlink`, `attachments`. |
| `capture-spool` | The encrypted, bounded, crash-safe single-writer spool. |
| `classifier-host` | The on-device classifier, run by capture-core as a child process on stdio. |
| `extension` | The Chrome/Edge extension that observes browser submissions to AI tools and hands them, with their attachments, to capture-core through the native messaging host. |
| `installer` | The Windows MSI, macOS package and Linux package that install the agent and register the native messaging host. |
| `integration` | Cross-component tests, including contract acceptance of every envelope the device emits. |

## Running in production

The installer registers one service (Windows service `ShadowAICapture`, launchd
`com.shadowaicapture.capture-core`, systemd `shadow-ai-capture.service`) that runs:

```
capture-core --config-file <vendor capture-core.env> --config-file <tenant.env>
```

Each file is `KEY=VALUE` lines; a later file wins, and a command-line flag wins over both. The
tenant file comes from the customer's MDM with the deployment package.

| Key | Flag | Meaning |
|---|---|---|
| `SAC_STATE_DIR` | `--state-dir` | Protected state directory (required). Holds the spool, `spool.key`, `credential.sealed`, `content/`, `content.key`, `policy/`, `device-ca/`, `health.json` and, for the Windows service, `capture-core.log`. capture-core gives it a protected DACL (SYSTEM, Administrators) on Windows and mode 0700 owned by root elsewhere. |
| `SAC_TENANT_ID` | `--tenant-id` | Tenant id (required; tenant file). |
| `SAC_DEVICE_ENDPOINT` | `--device-endpoint` | Device edge base URL, https with no path (required; tenant file). |
| `SAC_DEPLOYMENT_KEY` | `--deployment-key` | Tenant deployment key; enrols the device (tenant file). |
| `SAC_CA_FILE` | `--ca-file` | PEM CA set trusted for the device endpoint in addition to the system roots. |
| `SAC_POLICY_KEY` | `--policy-key` | Hex Ed25519 key the policy bundle must verify under. Without it the device runs at M0. |
| `SAC_POLICY_KEY_ID` | `--policy-key-id` | Key id the bundle must name (default `policy-key-1`). |
| `SAC_CLASSIFIER_RELEASE` | `--classifier-release` | Signed classifier release directory; set with the key below. |
| `SAC_CLASSIFIER_PUBKEY` | `--classifier-pubkey` | Hex Ed25519 key the classifier release must verify under. |
| `SAC_DEVICE_IDENTITY` | `--device-identity` | `clear` or `hashed` until the server states the tenant's setting (default `clear`). |
| `SAC_LOG_LEVEL` | `--log-level` | `debug`, `info`, `warn`, `error` (default `info`). Logs are JSON on standard error. |

The CLI trust shim writes the CA bundle and environment profile that command-line runtimes read to
a directory users can read, outside the state directory: `C:\ProgramData\ShadowAICapture\cli`
(Windows), `/var/db/shadow-ai-capture` (macOS, with `state/` inside it, root only) and
`/etc/shadow-ai-capture` plus `/etc/profile.d/shadow-ai-capture.sh` (Linux). The directory is the
agent's own default, never taken from the policy bundle.

On first start the agent enrols with the deployment key (a CSR; the edge returns a device
certificate), fetches the signed policy bundle, mints its per-device interception CA, and starts the
providers. TLS inspection is a tenant setting, off by default: only while the bundle's
`interception.enabled` is true does the agent run the TLS proxy, write the CLI trust shim, set the
Windows desktop-app PAC and keep its CA in the trust store, and a policy change starts or removes
them without a restart. It rotates the certificate, presenting the current
one, two thirds of the way through its validity. `capture-core --print-config` shows the resolved
configuration and the enrolment; `--version` the build.

## The browser's native messaging host

Chrome and Edge start the registered host with the extension's origin (`chrome-extension://<id>/`)
as the first argument; the installers register `capture-core` itself. In that mode it is a relay:
it connects to the running service at `\\.\pipe\ShadowAICapture.native` (Windows; SYSTEM and
Administrators full control, interactive users read and write, remote clients refused) or
`/var/run/shadow-ai-capture/native.sock` (macOS and Linux; directory 0755 root, socket 0666), checks
that the endpoint belongs to the service (pipe owned by SYSTEM or Administrators, socket peer uid
0), and copies Chromium native-messaging frames (4-byte little-endian length, then one JSON
message, at most 1 MiB) between its stdin/stdout and the endpoint. The service names the connecting
process's account (the pipe's client process token, `SO_PEERCRED`, `LOCAL_PEERCRED`) and attributes
that browser user's observations to them.

Attachment bytes arrive ahead of their observation (manifest, contiguous chunks, completion). The
service refuses the manifest with `mode_forbids_read` when policy reads no content, holds at most
32 MiB per attachment and 64 MiB per connection in memory, and drops bytes whose observation does
not name them by digest within two minutes. The observation's bytes are classified under its mode
(documents go to classifier-host's isolated parser child), the labels join the event's, and the
bytes are discarded: the envelope carries only the name, size and digest, and nothing is written
to the spool or the content store.

## Build and test

```
cd device/capture-core && go build -ldflags "-X main.version=1.2.3" ./cmd/capture-core
for m in protocol capture-spool capture-core integration; do (cd device/$m && gofmt -l . && go vet ./... && go test ./...); done
```
