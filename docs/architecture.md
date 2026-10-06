# Architecture

Shadow AI Capture records what employees send to generative AI tools from company-managed devices
and makes it queryable by the company's security team. Customers have not bought enterprise AI, so no
vendor API exists to pull usage from: if the device does not observe it, the data does not exist.

The design turns on one property: **content is interpreted where it is observed and, by default,
does not leave the device.** Classification labels, a digest and dimensions cross the network; the
prompt itself crosses only when the backend grants the upload of that one event.

## The pieces

```
 managed device                                   Azure (one environment per residency region)
 ──────────────                                   ──────────────────────────────────────────────
 browser ── extension ─┐                           device edge                     browser edge
                       │ native messaging          Application Gateway (mTLS)      Front Door + WAF
 CLI tools ─ cli.shim ─┤                                 │                               │
 local models ─────────┤                                 ▼                               ▼
                       ▼                           ingest-api   control-api ◀──── dashboard server
                 capture-core (service) ──HTTPS──▶  (events)   (enrol, policy,      │      │
                 ├ classifier-host (child)                      grants, upload,     ▼      ▼
                 └ capture-spool (encrypted)                    sign-in, SCIM)  query-api  content-vault
                                                                     │              │      │
                                                   jobs: aggregate, expire          │      │
                                                         migrate, tenant-admin      ▼      ▼
                                                               └────────────▶ PostgreSQL ◀─┘
```

| Component | Where | What it does |
|---|---|---|
| `endpoint/capture-core` | Device, as a service | Observes AI traffic (`proxy.tls` for CLI tools routed by `cli.shim`, `proxy.loopback` for local model servers, and the browser extension through its native messaging host), applies policy, classifies through `classifier-host`, spools, and delivers events |
| `endpoint/classifier-host` | Device, child of capture-core | Deterministic rules (payment cards, identifiers, credentials, keys) over prompt text and documents, from a signed release |
| `endpoint/capture-spool` | Device | Encrypted, bounded, crash-safe queue between capture and delivery |
| `extension` | Device browsers (Chrome, Edge) | Observes requests to AI tools, extracts the prompt and the files attached to it, warns or blocks inline, and hands both to capture-core through the native messaging host |
| `installer` | Build | The Windows MSI (and macOS/Linux packages) that install the agent and register the native messaging host |
| `services/ingest-api` | Container app | The one write path for events: authenticates the device certificate, validates each envelope against the contract, writes through `ingest.record_event` |
| `services/control-api` | Container app | Device enrolment and certificates, signed policy delivery, content grants and upload, analyst sign-in and sessions, SCIM, tenant onboarding, tenant packages and extension updates |
| `services/content-vault` | Container app (internal) | The only component that can decrypt prompt content; stores it encrypted in PostgreSQL and serves approved retrievals and content search |
| `services/query-api` | Container app (internal) | The one read path: a closed query DSL compiled to parameterised SQL |
| `services/dashboard` | Container app | The analyst web app and its server: sign-in session, and the only forwarder of browser requests into the environment |
| `services/jobs` | Container Apps jobs | `aggregate` recomputes the dashboard's aggregates every five minutes; `expire` deletes data past its retention daily |
| `services/database` | PostgreSQL 16 | The schema, its invariant tests, and the `migrate` job that applies it |
| `services/platform` | Library | Managed-identity tokens and the PostgreSQL connection every Go service uses |
| `contracts` | Library | The event envelope schema and its generated Go binding |
| `azure` | Bicep | The deployment |
| `localdev` | Laptop | A local lab of the whole system |

## Collection modes

Each tenant has a ceiling mode, and signed policy narrows it per tool, data class and population.
The device resolves the mode before it reads anything.

| Mode | What leaves the device |
|---|---|
| M0 | Who, which tool, when, size, destination. No content is read |
| M1 | M0 plus classification labels and a content digest |
| M2 | M1 plus a minimised excerpt |
| M3 | M2, and the prompt is held on the device; it is uploaded only for events the backend grants |

## A device, end to end

1. **Install.** Intune installs the tenant package: the generic MSI plus a tenant file holding the
   tenant id, the device endpoint and the tenant's deployment key. The MSI installs the service,
   the classifier release, and the native messaging host registration for Chrome and Edge; Intune
   force-installs the extension from control-api's update manifest.
2. **Enrol.** capture-core generates a key pair, sends a CSR with the deployment key (and, if the
   tenant requires it, its Intune identifiers, which control-api checks with Microsoft Graph), and
   receives a certificate signed by the environment's device CA. Rotation re-enrols with the current
   certificate.
3. **Policy.** capture-core fetches the tenant's policy bundle, signed with the environment's policy
   key; the MSI pins the public half, so a device accepts only its own environment's policy.
4. **Capture.** A provider observes a submission, the policy engine resolves its mode, classifier-host
   labels the prompt and any attached documents, and the event envelope goes into the spool.
   Attachment bytes stay on the device; their name, size, digest and labels travel in the envelope.
5. **Deliver.** The drainer sends batches to `/v1/events` over mutual TLS. Application Gateway
   terminates TLS and forwards the certificate; ingest-api verifies it against the device CA, checks
   the credential and the tenant's region, validates each envelope, and writes it idempotently.
   Health reports go to control-api the same way.
6. **Content (M3 only).** For a delivered M3 event the device asks control-api for a grant. A granted
   event's prompt is sent once to `/v1/content`; control-api checks the grant and hands the content to
   content-vault, which encrypts it and stores it.

## Data

One database with four schemas: `ref` (shared reference data), `ops` (tenants, devices, credentials,
identity, audit, content), `ingest` (immutable observations and the deduplicated submission per
logical prompt) and `mart` (aggregates and findings, rebuildable from `ingest`).

- **Tenant isolation is enforced by the database.** Every tenant-scoped table has row-level security
  enabled and forced; a session sees only the tenant it set with `app.tenant_id`, and a session that
  set none sees nothing.
- **One role per component.** Each workload's managed identity is a PostgreSQL login granted exactly
  one role (`sac_ingest`, `sac_control`, `sac_vault`, `sac_query`, `sac_ops`). Cross-tenant lookups
  that must happen before a tenant is known (a sign-in, a certificate, a SCIM token) are SECURITY
  DEFINER functions owned by a role nobody logs in as.
- **Observations are immutable.** Rows in `ingest.observation` and `ops.audit` cannot be updated, and
  observations can be deleted only by the retention path.
- **Aggregates are recomputed, not incremented**, so late events are absorbed and a rerun is harmless.
- **Retention**: every captured row carries `expires_at`; the `expire` job deletes what has passed it
  and writes an erasure receipt per tenant.

## Prompt content

content-vault encrypts each object with AES-256-GCM under a per-tenant key derived (HKDF-SHA256) from
the environment's content master key, which only content-vault can read from Key Vault. The
ciphertext lives in `ops.content`, which only content-vault's role can read. Master keys are a
versioned keyring: a new key encrypts new content, old keys keep old content readable until it
expires.

Retrieval needs a signed-in person with the `content_reader` role. content-vault writes the audit
entry first and serves the content once, through a short-lived single-use URL that the dashboard
server forwards; an optional case reference and justification are recorded with it. Search over
content is a per-tenant setting
(`disabled`, `attachment_names`, `full_text`); with `full_text` content-vault indexes prompt text in
`ingest.search_text` as it stores it.

## Product rules

These hold everywhere, and tests enforce them:

- A per-person figure covering fewer than five people is suppressed, never shown or exported.
- Nothing ranks people by volume: no leaderboard, no list of people sorted by usage.
- The dashboard never shows a fleet percentage; counts are shown with their denominator.
- A read that resolves to a person writes its audit entry in the same transaction that serves it.
- query-api cannot read prompt content or the content search index; only content-vault can.
- The dashboard loads nothing from another host: no CDN, no remote fonts.
- A collection path that is not working reports itself `degraded` or `absent`; missing data is never
  presented as zero.

## Identity and access

- **Devices** hold certificates from the environment's device CA, which control-api signs with and
  ingest-api and control-api verify against. Application Gateway is the only path to the
  device-facing apps, so the forwarded certificate cannot be supplied by anyone else.
- **People** sign in through the dashboard to control-api, which is the relying party for every
  customer's identity provider: the vendor's multi-tenant Entra application, or any OIDC provider.
  control-api issues short-lived product access tokens (ES256) that query-api and content-vault
  verify against its published keys. Roles are `viewer`, `analyst`, `content_reader`, `admin`.
- **Directory**: customers provision people and departments by SCIM; devices and the directory
  derive the same pseudonymous user reference from a per-tenant key, so neither sends a name.
- **Services** authenticate to Azure with managed identities. The dashboard server presents a shared
  internal token to control-api's internal sign-in API; control-api presents a short-lived service
  token to content-vault when it stores content.

## Deployment

One Azure environment per residency region: an internal Container Apps environment on a private
VNet, PostgreSQL Flexible Server (VNet-integrated, Entra-only), Key Vault (private endpoint,
per-secret access), Application Gateway WAF_v2 for devices and Front Door Premium for browsers.
`azure/README.md` lists the resources, `azure/RUNBOOK.md` the go-live sequence, and
`.github/workflows/` builds, tests and deploys.
