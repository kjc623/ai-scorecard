# control-api

The control plane. For devices: enrolment (a device certificate signed by the device CA), the signed
policy bundle, health reports, content-upload grants and the forwarding of granted content to
content-vault, and the browser extension's update manifest and CRX. For people: the OpenID Connect
relying party for every customer identity provider, sessions, the short-lived product access tokens
query-api and content-vault verify, onboarding, SCIM provisioning and the deployment admin API.
Devices authenticate with the certificate Application Gateway forwards in `X-Client-Cert`, verified
here against the device CA and matched to the device's live credential on every request.

## Routes

| Route | Caller | Authentication |
|---|---|---|
| `POST /v1/enrol` | device | deployment key (first enrolment) or current certificate (rotation) |
| `GET /v1/policy`, `POST /v1/health`, `POST /v1/content/grant`, `POST /v1/content` | device | device certificate |
| `GET /v1/extension/updates.xml`, `GET /v1/extension/shadow-ai-capture.crx` | browser | none |
| `/internal/v1/auth/{begin,complete,token,revoke}` | dashboard server | `Bearer SAC_INTERNAL_TOKEN` |
| `/admin/v1/*` | dashboard server | product token, audience `sac-control`, role `admin` |
| `/scim/v2/*` | customer identity provider | SCIM bearer token |
| `/onboard/*`, `/.well-known/{jwks.json,openid-configuration}` | browser, verifiers | invite token / none |
| `GET /healthz`, `GET /readyz` | platform | none (`/readyz` includes a database round trip) |

## Configuration

| Variable | Meaning |
|---|---|
| `SAC_PG_HOST`, `SAC_PG_PORT`, `SAC_PG_DATABASE`, `SAC_PG_USER` (`control-api`), `SAC_PG_SSLMODE` | PostgreSQL; without `SAC_PG_PASSWORD` (lab only) each connection uses the managed identity's Entra token (`AZURE_CLIENT_ID`) |
| `SAC_CA_CERT_PEM`, `SAC_CA_KEY_PEM` | device CA certificate and key (Key Vault `sac-device-ca-cert`, `sac-device-ca-key`) |
| `SAC_AUTH_ISSUER` | `iss` of product and service tokens; verifiers fetch `{issuer}/.well-known/jwks.json` |
| `SAC_SESSION_SIGNING_KEY_FILE` | P-256 key file that signs tokens; further PEM blocks are published for rotation |
| `SAC_INTERNAL_TOKEN` | the dashboard server's credential for `/internal/v1/auth/*` (at least 32 characters) |
| `SAC_PUBLIC_URL` | browser origin: sign-in returns to `{SAC_PUBLIC_URL}/callback`; onboarding and SCIM URLs |
| `SAC_AUTH_REDIRECT_URIS` | optional further exact sign-in redirect URIs (the lab's second dashboard origin) |
| `SAC_DIRECTORY_KEY` | base64 32-byte key that seals stored secrets and the tenant user-reference keys |
| `SAC_PUBLIC_DEVICE_ENDPOINT` | device origin written into tenant packages and the extension manifest |
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
