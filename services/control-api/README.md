# control-api

The control plane. For devices: enrolment (a device certificate signed by the device CA), the signed
policy bundle, health reports, content-upload grants and the forwarding of granted content to
content-vault, the agent's own updates (the release build's signed statement and the MSI it
names, from the release this deployment runs), and the browser extension's update manifest and CRX. For people: the OpenID Connect
relying party for every customer identity provider, sessions, the short-lived product access tokens
query-api and content-vault verify, onboarding, the directory (SCIM provisioning, or a read of the
customer's Microsoft Entra ID through Microsoft Graph) and teams, and the admin API. For tenants:
the pull of AI providers' own records of their people's prompts, from the providers' admin APIs,
and the intake of those records from the logs of providers that have no API to read.
Devices authenticate with the certificate Application Gateway forwards in `X-Client-Cert`, verified
here against the device CA and matched to the device's live credential on every request.

## Routes

| Route | Caller | Authentication |
|---|---|---|
| `POST /v1/enrol` | device | deployment key (first enrolment) or current certificate (rotation) |
| `GET /v1/policy`, `POST /v1/health`, `POST /v1/content/grant`, `POST /v1/content` | device | device certificate |
| `GET /v1/agent/release`, `GET /v1/agent/package` | device | device certificate |
| `POST /v1/diagnostics` | device, answering a diagnostics request its health response named | device certificate |
| `GET /v1/extension/updates.xml`, `GET /v1/extension/shadow-ai-capture.crx` | browser, through the dashboard server on the analyst hostname | none |
| `/internal/v1/auth/{begin,complete,token,revoke}` | dashboard server | `Bearer SAC_INTERNAL_TOKEN` |
| `/admin/v1/*` | dashboard server | product token, audience `sac-control`, role `admin` |
| `/scim/v2/*` | customer identity provider | SCIM bearer token |
| `POST /intake/v1/{provider}/{connection}` | the customer's cloud, delivering a provider's log | the connection's intake token |
| `/onboard/*`, `/.well-known/{jwks.json,openid-configuration}` | browser, verifiers | invite token / none |
| `GET /healthz`, `GET /readyz` | platform | none (`/readyz` includes a database round trip) |

## The directory

People and groups reach `ops.scim_user`, `ops.user_dim` and `ops.scim_group` in one of two ways,
both through the SCIM service (`internal/scim`), so a person has one record and one user reference
whichever way they arrived:

- **SCIM**: the customer's identity provider pushes them to `/scim/v2/*` with a token created on
  Settings → Identity Providers.
- **Microsoft Graph** (`internal/graphsync`): once an admin turns it on (Settings → Identity
  Providers), control-api reads the tenant's Entra directory with the vendor application's own token,
  an hour apart and on demand, and writes it through the same service. It needs the application
  permissions `User.Read.All` and `GroupMember.Read.All`, granted in the customer's tenant. It reads
  each member's name, UPN, object id, department, enabled flag and the parent of their on-premises
  distinguished name (their organisational unit); guests are skipped. Someone gone from a complete
  listing is retired, never deleted. Only the groups a team follows are read.

Teams (`/admin/v1/teams`, `internal/directoryadmin`) are made in the console from any mix of people
an admin picks and groups, departments, organisational units and other teams they include;
`ops.v_team_member` resolves each to its current members, and `jobs aggregate` rolls usage up per
team. A team cannot include a team that already includes it.

## Provider integrations

`internal/providers` reads the prompts a tenant's people sent an AI provider from that provider's
admin API, so a prompt sent from a device without the agent is still seen. An admin connects each
provider on the console's Settings → Integrations page (`/admin/v1/providers`), which lists every
row of `ref.provider` with the tools its records are filed under and the tenant's connection to it:

| Provider | Credential | Read from | Filed under |
|---|---|---|---|
| ChatGPT Enterprise | the workspace ID and a Compliance API key: an Admin key with the Conversation messages permission, which a workspace owner grants | the Compliance Logs Platform's `CONVERSATION_MESSAGE` log files | `chatgpt_web` |
| Claude Enterprise | the organization ID and a Compliance API key (`read:compliance_user_data`) | the Compliance API's chats and Claude Code and Cowork sessions | `claude_web`, `claude_code`, `claude_desktop` |
| Microsoft 365 Copilot | none: the vendor Entra application, with `AiEnterpriseInteraction.Read.All` consented in the customer's tenant and an active Entra connection | Microsoft Graph's `getAllEnterpriseInteractions`, per licensed person | `m365_copilot` |

A key is sealed under `SAC_DIRECTORY_KEY` and never returned or audited; disconnecting forgets it.
Each active connection is pulled right after it is connected, on demand (Pull now) and on its
schedule: every hour unless an admin chooses every 15 or 30 minutes, 6 hours or a day on its card
(`PUT /admin/v1/providers/{provider}/schedule`, `ops.provider_connection.pull_every_minutes`). The
schedule is kept with the connection, not the process: once a minute the runner pulls each
connection whose last pass started its schedule ago, on whichever replica takes the connection's
lock first, so a restart or a second replica does not pull it more often. Each pass reads from where
the last one stopped; the first reads the last 7 days. Every record of a person's
prompt is classified with the rules and model the agent ships, under the tenant's collection mode,
attributed by the user reference a device derives from the same UPN or object id, and recorded
through `ingest.record_provider_event` with the route `provider.api`; at M3 its text is stored in
content-vault. A record of a prompt a device also captured merges into the device's submission. A
provider's record is never enforced. A failed pass records one of a closed set of codes
(`ops.provider_connection.last_error`), which the page explains. No setting turns the pull on: it
uses the database login, `SAC_DIRECTORY_KEY`, `SAC_CONTENT_VAULT_URL` and, for Microsoft 365
Copilot, the Entra application (`SAC_ENTRA_CLIENT_ID`).

### The log intake

Some providers have no API to read, but write their record of each prompt to a log in the
customer's own cloud. The customer forwards that log to the connection's intake URL,
`{SAC_PUBLIC_URL}/intake/v1/{provider}/{connection}`, which Front Door, or the dashboard server
where it is the only public origin, passes to control-api untouched. Connecting such a provider
issues the connection's intake token, shown once beside the URL; it is stored only as its SHA-256
(`ops.provider_connection.intake_token_hash`), `ops.tenant_for_intake` resolves a delivery's tenant
from it before any tenant is known, and connecting again replaces it. Every delivery presents it:

| Provider | Log | Delivered by | The token is | Filed under |
|---|---|---|---|---|
| Gemini Code Assist | the Gemini for Google Cloud request log, with prompt logging turned on | a Cloud Logging sink to a Pub/Sub topic, and a push subscription to the URL with authentication on | the push subscription's audience, so Google's signed ID token carries it | `gemini_code_assist`, and requests from the Gemini CLI `gemini_cli` |
| Amazon Bedrock | model invocation logging to CloudWatch Logs | a subscription filter to an Amazon Data Firehose stream whose destination is an HTTP endpoint at the URL, GZIP on | the endpoint's access key | `aws_bedrock` |
| Perplexity Enterprise | the audit log | its webhook, at the URL | the bearer token | `perplexity_web` |

A delivery is recorded before it is answered, in the form its sender expects, so a sender that does
not hear success sends it again and the event ids derived from the provider's own ids record it
once. One delivery is at most 16 MiB as sent and 64 MiB inflated. An entry that is not the
provider's log is passed over and counted; a delivery its transport cannot read is refused and
recorded on the connection as `delivery_malformed`. A Bedrock request by an assumed role whose
session name, or an IAM user whose name, is an email address is attributed to that person;
otherwise to the role or user, as an account. A Bedrock request body over 100 KB is logged only to
S3 and arrives without its text. The intake reads no setting of its own beyond
`SAC_PUBLIC_URL`.

## Configuration

| Variable | Meaning |
|---|---|
| `SAC_PG_HOST`, `SAC_PG_PORT`, `SAC_PG_DATABASE`, `SAC_PG_USER` (`control-api`), `SAC_PG_SSLMODE` | PostgreSQL; without `SAC_PG_PASSWORD` (lab only) each connection uses the managed identity's Entra token (`AZURE_CLIENT_ID`) |
| `SAC_CA_CERT_PEM`, `SAC_CA_KEY_PEM` | device CA certificate and key (Key Vault `sac-device-ca-cert`, `sac-device-ca-key`) |
| `SAC_AUTH_ISSUER` | `iss` of product and service tokens; verifiers fetch `{issuer}/.well-known/jwks.json` |
| `SAC_SESSION_SIGNING_KEY_FILE` | P-256 key file that signs tokens; further PEM blocks are published for rotation |
| `SAC_INTERNAL_TOKEN` | the dashboard server's credential for `/internal/v1/auth/*` (at least 32 characters) |
| `SAC_PUBLIC_URL` | browser origin: sign-in returns to `{SAC_PUBLIC_URL}/callback`; onboarding, SCIM and provider intake URLs; the extension manifest's CRX URL |
| `SAC_AUTH_REDIRECT_URIS` | optional further exact sign-in redirect URIs (the lab's second dashboard origin) |
| `SAC_DIRECTORY_KEY` | base64 32-byte key that seals stored secrets and the tenant user-reference keys |
| `SAC_PUBLIC_DEVICE_ENDPOINT` | device origin written into tenant packages |
| `SAC_POLICY_SIGNING_KEY_FILE`, `SAC_POLICY_SIGNING_KEY_ID` | Ed25519 policy key and the key id devices pin (default `policy-key-1`) |
| `SAC_CONTENT_VAULT_URL` | content-vault's internal address; uploads carry a service token (`aud sac-vault`, `svc control-api`) |
| `SAC_REGION` | Azure region; a tenant pinned elsewhere is refused |
| `SAC_ENTRA_CLIENT_ID`, `SAC_ENTRA_FIC=managed` | the vendor Entra app, authenticated by the managed identity; unset disables Entra sign-in and the Intune check |
| `SAC_ENTRA_CLIENT_SECRET` | lab only: the Entra app's secret, for a lab pointed at a real Entra tenant |
| `SAC_AUTH_ALLOW_INSECURE_IDP` | lab only: admits the lab's http test identity provider; never set in production |
| `SAC_HTTP_ADDR`, `SAC_AGENT_RELEASE_DIR` | defaults `0.0.0.0:8080` and `/opt/sac/agent-release` |

## Tenant administration

The tenant-admin job runs the same image with the same database login:

    control-api tenant create --name "Contoso" --region eastus --ceiling m1 --actor alice@vendor
    control-api tenant invite --tenant <id> --domain contoso.com --expires 168h --actor alice@vendor

`create` prints the tenant id; `invite` prints the one-time onboarding URL (built on
`SAC_PUBLIC_URL`). `--region` defaults to `SAC_REGION`.

## Build and test

    docker build -f services/control-api/Dockerfile --build-context agent-release=<dir> -t control-api .
    cd services/control-api && gofmt -l . && go vet ./... && go test ./...

Live database tests run when `SAC_TEST_PG_DSN` names a database with `services/database/schema.sql` applied
(as a superuser that can `SET ROLE sac_control`); otherwise they skip.
