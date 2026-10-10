# control-api

The control plane. For devices: enrolment (a device certificate signed by the device CA), the signed
policy bundle, health reports, content-upload grants and the forwarding of granted content to
content-vault, the agent's own updates (the release build's signed statement and the MSI it
names, from the release this deployment runs), and the browser extension's update manifest and CRX. For people: the OpenID Connect
relying party for every customer identity provider, sessions, the short-lived product access tokens
query-api and content-vault verify, onboarding, the directory (SCIM provisioning, or a read of the
customer's Microsoft Entra ID through Microsoft Graph) and teams, and the admin API.
Devices authenticate with the certificate Application Gateway forwards in `X-Client-Cert`, verified
here against the device CA and matched to the device's live credential on every request.

## Routes

| Route | Caller | Authentication |
|---|---|---|
| `POST /v1/enrol` | device | deployment key (first enrolment) or current certificate (rotation) |
| `GET /v1/policy`, `POST /v1/health`, `POST /v1/content/grant`, `POST /v1/content` | device | device certificate |
| `GET /v1/agent/release`, `GET /v1/agent/package` | device | device certificate |
| `GET /v1/extension/updates.xml`, `GET /v1/extension/shadow-ai-capture.crx` | browser, through the dashboard server on the analyst hostname | none |
| `/internal/v1/auth/{begin,complete,token,revoke}` | dashboard server | `Bearer SAC_INTERNAL_TOKEN` |
| `/admin/v1/*` | dashboard server | product token, audience `sac-control`, role `admin` |
| `/scim/v2/*` | customer identity provider | SCIM bearer token |
| `/onboard/*`, `/.well-known/{jwks.json,openid-configuration}` | browser, verifiers | invite token / none |
| `GET /healthz`, `GET /readyz` | platform | none (`/readyz` includes a database round trip) |

## The directory

People and groups reach `ops.scim_user`, `ops.user_dim` and `ops.scim_group` in one of two ways,
both through the SCIM service (`internal/scim`), so a person has one record and one user reference
whichever way they arrived:

- **SCIM**: the customer's identity provider pushes them to `/scim/v2/*` with a token created on
  Settings → Deployment.
- **Microsoft Graph** (`internal/graphsync`): once an admin turns it on (Settings → Directory &
  teams), control-api reads the tenant's Entra directory with the vendor application's own token,
  an hour apart and on demand, and writes it through the same service. It needs the application
  permissions `User.Read.All` and `GroupMember.Read.All`, granted in the customer's tenant. It reads
  each member's name, UPN, object id, department, enabled flag and the parent of their on-premises
  distinguished name (their organisational unit); guests are skipped. Someone gone from a complete
  listing is retired, never deleted. Only the groups a team follows are read.

Teams (`/admin/v1/teams`, `internal/directoryadmin`) are made in the console with chosen members,
or follow a group, a department or an organisational unit; `ops.v_team_member` resolves each to its
current members, and `jobs aggregate` rolls usage up per team.

## Configuration

| Variable | Meaning |
|---|---|
| `SAC_PG_HOST`, `SAC_PG_PORT`, `SAC_PG_DATABASE`, `SAC_PG_USER` (`control-api`), `SAC_PG_SSLMODE` | PostgreSQL; without `SAC_PG_PASSWORD` (lab only) each connection uses the managed identity's Entra token (`AZURE_CLIENT_ID`) |
| `SAC_CA_CERT_PEM`, `SAC_CA_KEY_PEM` | device CA certificate and key (Key Vault `sac-device-ca-cert`, `sac-device-ca-key`) |
| `SAC_AUTH_ISSUER` | `iss` of product and service tokens; verifiers fetch `{issuer}/.well-known/jwks.json` |
| `SAC_SESSION_SIGNING_KEY_FILE` | P-256 key file that signs tokens; further PEM blocks are published for rotation |
| `SAC_INTERNAL_TOKEN` | the dashboard server's credential for `/internal/v1/auth/*` (at least 32 characters) |
| `SAC_PUBLIC_URL` | browser origin: sign-in returns to `{SAC_PUBLIC_URL}/callback`; onboarding and SCIM URLs; the extension manifest's CRX URL |
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
