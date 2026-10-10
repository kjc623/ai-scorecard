# Going live with an environment

The order an environment is brought up in, from an empty subscription to a managed device whose
events appear on the dashboard. `README.md` describes the resources, variables and secrets this
refers to. Steps marked **(once)** are done once per environment.

## 1. Prerequisites (once)

1. **Subscription and region.** A subscription with quota in the region for Container Apps
   (Consumption), PostgreSQL Flexible Server (`Standard_B2s` pre-prod, `Standard_D2ds_v5` prod),
   Application Gateway WAF_v2 and Front Door Premium. Register the providers `Microsoft.App`,
   `Microsoft.DBforPostgreSQL`, `Microsoft.KeyVault`, `Microsoft.Network`, `Microsoft.Cdn`,
   `Microsoft.ContainerRegistry`, `Microsoft.OperationalInsights`, `Microsoft.Insights`,
   `Microsoft.ManagedIdentity`, `Microsoft.Consumption`.
2. **Resource group** `rg-sac-<environment>-<region>`, e.g. `rg-sac-preprod-eastus`.
3. **Deployment identity.** An Entra application with a federated credential for this repository's
   GitHub environment (`repo:<org>/<repo>:environment:<environment>`), granted **Contributor** and
   **Role Based Access Control Administrator** on the resource group (the deployment creates role
   assignments for the workload identities).
4. **GitHub environment** `preprod` / `prod`. Require a reviewer on `prod`.
   - Variables: `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID`, `SAC_RESOURCE_GROUP`,
     the `SAC_*` variables in `README.md`, and `SAC_POLICY_PUBLIC_KEY` (step 3).
   - Secret `SAC_CLASSIFIER_SIGNING_KEY`: the classifier release signing seed, 32 random bytes in
     hex (`openssl rand -hex 32`). Keep it; devices verify classifier releases against its public half.
   - Secret `SAC_EXTENSION_SIGNING_KEY`: the PEM private key whose public half is `key` in
     `device/extension/manifest.json`. It fixes the extension's id; a different key is a different
     extension to every browser.
   - Optional code signing with Azure Trusted Signing: variables `SAC_SIGNING_ENDPOINT`,
     `SAC_SIGNING_ACCOUNT`, `SAC_SIGNING_PROFILE`, with the deployment identity granted the
     *Trusted Signing Certificate Profile Signer* role. Without them the MSI is built unsigned.
5. **DNS names** for `SAC_DEVICE_FQDN` and `SAC_ANALYST_FQDN`, and a TLS certificate for the device
   hostname from a public CA, as a PFX.
6. **The vendor Entra application** (one, multi-tenant), registered in your tenant:
   - Supported accounts: any organizational directory. Web redirect URIs
     `https://<analyst-fqdn>/callback` and `https://<analyst-fqdn>/onboard/entra/callback`.
   - App roles (value = name, for users/groups): `viewer`, `analyst`, `content_reader`, `admin`.
   - API permissions: delegated `openid`, `profile`, `email`, `offline_access`; application
     `DeviceManagementManagedDevices.Read.All` (the Intune check), `User.Read.All` and
     `GroupMember.Read.All` (reading the directory, Settings → Deployment), granted by each customer at
     consent. A customer that consented before the last two were added grants them again (Entra admin
     center → Enterprise applications → the app → Permissions → Grant admin consent).
   - No client secret: control-api authenticates as the app with its managed identity (step 5).
   - Set `SAC_ENTRA_APP_CLIENT_ID` to its client id.
7. **Intune licensing** for the test devices and the customer tenant.

## 2. Bootstrap deployment (once)

Run the **deploy** workflow for the environment with *bootstrap* checked. It creates the network,
registry, Key Vault, PostgreSQL, the Container Apps environment and the identities — no apps, jobs or
edges, because they need images and secrets that do not exist yet.

## 3. Secrets (once)

With your address in `SAC_KEYVAULT_ADMIN_IPS` and your object id in `SAC_KEYVAULT_ADMIN_PRINCIPALS`
(redeploy the bootstrap if you added them after step 2):

```sh
azure/scripts/create-secrets.sh sac-preprod-eastus-kv
az keyvault certificate import --vault-name sac-preprod-eastus-kv --name sac-device-tls --file device.pfx
```

Set the printed policy public key as the GitHub variable `SAC_POLICY_PUBLIC_KEY`: the agent release
pins it, so devices accept only policy this environment signed. Remove your address from
`SAC_KEYVAULT_ADMIN_IPS` afterwards if you want the vault's public endpoint closed.

## 4. Deploy

Run the **deploy** workflow (without *bootstrap*). It builds the agent release (MSI and browser
extension) on Windows, builds and pushes the seven images, deploys the full template, approves Front
Door's Private Link connection to the environment, and runs the `migrate` job, which applies the
schema and creates each workload's database login. Every later release is the same run.

## 5. DNS and the Entra federated credential (once)

From the deployment outputs:

- `deviceEdgeIp` → an `A` record for the device hostname.
- `frontDoorEndpoint` → a `CNAME` for the analyst hostname, and `analystDomainValidationToken` → a
  `TXT` record `_dnsauth.<analyst-fqdn>`. Front Door issues the certificate once the record validates.
- `entraFederatedCredential` → on the vendor Entra app: Certificates & secrets → Federated
  credentials → Add → *Other issuer*, with that issuer, subject and audience.

## 6. The first customer tenant

```sh
azure/scripts/tenant-admin.sh rg-sac-preprod-eastus tenant create --name "Contoso" --ceiling m1 --actor you@vendor.com
azure/scripts/tenant-admin.sh rg-sac-preprod-eastus tenant invite --tenant <tenant-id> --domain contoso.com --actor you@vendor.com
```

Each prints the command's output: the new tenant id, then the invite link. A Global or Cloud Application Administrator of the customer tenant opens
the link, consents, and signs in once, becoming that tenant's admin. In the customer's Entra
*Enterprise applications*, set *Assignment required* and assign people to the four roles.

Directory sync: in the dashboard, **Settings → Deployment → SCIM → Create token**; in the customer's
Entra create a non-gallery enterprise application for provisioning with Tenant URL
`https://<analyst-fqdn>/scim/v2` and the token, map `objectId → externalId`, scope it to assigned
users and groups, and start provisioning.

## 7. Devices (Intune)

1. Dashboard → **Settings → Deployment → Download Intune package** (the generic MSI wrapped with the
   tenant's deployment key).
2. Intune → Apps → Windows → *Windows app (Win32)*: install `msiexec /i "ShadowAICapture.msi" /qn`,
   uninstall `msiexec /x {product-code} /qn`, detection by MSI product code and version, x64,
   Windows 10 21H2 or later; assign *Required* to a device group.
3. Browser extension: a Settings catalog profile for **Google Chrome** and **Microsoft Edge** →
   *Extensions* → *Configure the list of force-installed apps and extensions* with
   `<extension-id>;https://<analyst-fqdn>/v1/extension/updates.xml` (the id is in the release's
   `release.json`). The update URL is on the analyst hostname, not the device hostname: the
   browsers' extension downloader cannot answer the device edge's client certificate request. The
   MSI registers the native messaging host the extension talks to.

## 8. Verify

- Every container app is *Running* and ready; `content-vault` and `query-api` show internal ingress.
- `curl https://<device-fqdn>/v1/health` reaches control-api; `curl https://<device-fqdn>/admin/v1/`
  is refused at the edge.
- `curl https://<analyst-fqdn>/v1/extension/updates.xml` answers 200 with an update manifest whose
  `codebase` is `https://<analyst-fqdn>/v1/extension/shadow-ai-capture.crx`.
- The `migrate` job's last execution succeeded; a second run logs nothing to apply.
- On a managed device the app installs, the deployment key's enrolment count rises and the device
  appears on the dashboard's Devices page by hostname.
- After prompts from the device: submissions on Tools, Users and Data classes; a prompt with a test
  card number raises a finding.
- `aggregate` runs every five minutes and `expire` daily without errors in `ContainerAppConsoleLogs`.
- No alert is firing in the environment's action group.
