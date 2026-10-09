# Pre-prod on Fly.io and Supabase

The order pre-prod is brought up in, from empty accounts to a managed device whose events appear on
the dashboard. Every step is the owner's; steps marked **(once)** are done once. The commands use
the app prefix `sac-preprod`; the scripts take another from `FLY_APP_PREFIX`.

## The environment

| App | Runs | Reachable |
|---|---|---|
| `sac-preprod-edge` | `services/edge`: TLS with the client certificate forwarded, only the device API | Port 443, raw TCP, on a dedicated IPv4 and an IPv6 address: the device hostname |
| `sac-preprod-dashboard` | `services/dashboard`, which also forwards `/onboard/*` to control-api | HTTPS on the analyst hostname, with a Fly.io-managed certificate |
| `sac-preprod-ingest-api`, `-control-api`, `-content-vault`, `-query-api` | The four APIs | Only on the organization's private network, at `http://<app>.internal:8080` |
| `sac-preprod-jobs` | One machine: supercronic runs `jobs aggregate`, `erase` and `expire` (`fly/jobs/crontab`) | Nowhere |
| `sac-preprod-migrate` | No machine: each deploy runs `migrate` once, as its release command | Nowhere |

Each app has one machine in `iad` (Ashburn, Virginia). PostgreSQL is a Supabase project on the Pro
plan in East US (North Virginia): the database `shadow`, owned by the administrator `sac_admin`, and
one login per component. The apps connect through Supabase's session pooler, which is reachable
over IPv4 and supports prepared statements, as `<login>.<project-ref>`, with TLS required.
`fly/<component>/fly.toml` holds each app's settings; the deploy workflow passes the ones that differ
per environment. Each secret is set only on the apps that read it:

| Secret | Apps | Set by |
|---|---|---|
| `SAC_CA_CERT_PEM` (device CA certificate) | ingest-api, control-api | `create-secrets.sh` |
| `SAC_CA_KEY_PEM`, `SAC_DIRECTORY_KEY`, `SAC_ENTRA_CLIENT_SECRET` | control-api | `create-secrets.sh` |
| `SESSION_SIGNING_KEY`, `POLICY_SIGNING_KEY` (files at `/mnt/secrets/sac-session-signing-key`, `/mnt/secrets/sac-policy-signing-key`) | control-api | `create-secrets.sh` |
| `SAC_INTERNAL_TOKEN` | control-api, dashboard | `create-secrets.sh` |
| `SAC_CONTENT_KEYS` | content-vault | `create-secrets.sh` |
| `SAC_CURSOR_KEY` | query-api | `create-secrets.sh` |
| `EDGE_TLS_CERT_PEM`, `EDGE_TLS_KEY_PEM` (the device hostname's certificate) | edge | `create-secrets.sh` |
| `SAC_PG_PASSWORD` | ingest-api, control-api, content-vault, query-api, jobs, each its own; migrate, the administrator's | `setup-database.sh` |
| `SUPABASE_CA_CERT` (a file at `/etc/sac/supabase-ca.crt`, which query-api verifies the database by) | query-api | `setup-database.sh` |

## 1. Accounts and names (once)

1. **Fly.io.** An organization with a payment method. Install `flyctl` and sign in
   (`fly auth login`).
2. **Supabase.** An organization on the Pro plan, and a project in **East US (North Virginia)**
   with a strong database password; keep the password. From the project:
   - the project ref (Project settings → General);
   - the session pooler's host: **Connect** → **Session pooler**, the host of that string. Copy it:
     it cannot be worked out from the region;
   - the server's CA certificate: the project's **Database → Settings** page
     (`/dashboard/project/<ref>/database/settings`), SSL configuration, **Download certificate**.
     Turn **Enforce SSL on incoming connections** on while there: every connection here uses TLS.
3. **The apps**, and their public addresses. `fly deploy` allocates none, so the two public apps
   get theirs here: the edge a dedicated IPv4 (billed monthly; raw TCP needs a dedicated address)
   and an IPv6, the dashboard a shared IPv4 and an IPv6 (Fly.io issues its certificate against the
   IPv6):

   ```sh
   for c in edge ingest-api control-api content-vault query-api dashboard jobs migrate; do
     fly apps create "sac-preprod-$c" --org <org>
   done
   fly ips allocate-v4 --app sac-preprod-edge
   fly ips allocate-v6 --app sac-preprod-edge
   fly ips allocate-v4 --shared --app sac-preprod-dashboard
   fly ips allocate-v6 --app sac-preprod-dashboard
   ```

4. **DNS names** for the device hostname (`SAC_DEVICE_FQDN`) and the analyst hostname
   (`SAC_ANALYST_FQDN`), and a TLS certificate for the device hostname from a public CA, so devices
   need no extra trust: the chain (leaf first) and the unencrypted private key, both PEM.
5. **The vendor Entra application**, as `azure/RUNBOOK.md` §1 step 6 with the analyst hostname,
   except its credential: under *Certificates & secrets* create a **client secret** and keep its
   value; control-api authenticates as the application with it. No federated credential.
6. **Intune licensing**, as `azure/RUNBOOK.md` §1 step 7.

## 2. Database (once)

With `psql` and `openssl` on the path (Git Bash, macOS or Linux):

```sh
fly/scripts/setup-database.sh <project-ref> <session-pooler-host> prod-ca-2021.crt
```

It asks for the project's database password (or reads `SUPABASE_DB_PASSWORD`), creates `sac_admin`,
the five component logins and the database `shadow` with generated passwords, and stages each
password on its app; nothing is printed. It refuses to run once any of them exists. In Git Bash,
write the certificate's path in Windows form with forward slashes (`C:/Users/<you>/prod-ca-2021.crt`):
`psql` is a Windows program and does not understand `/c/Users/...`.

If an app's log says `password authentication failed` for its login after the deploy, the password
Fly.io holds for it does not match the server's. Give the login a new one and set it on the app;
the app restarts with it:

```sh
openssl rand -hex 32                                  # keep a copy in the password manager
psql "host=<pooler-host> port=5432 dbname=postgres user=postgres.<ref> sslmode=verify-full sslrootcert=<ca.crt>" \
  -c "ALTER ROLE \"query-api\" PASSWORD '<new password>'"
printf 'SAC_PG_PASSWORD=%s\n' '<new password>' | fly secrets import --app sac-preprod-query-api
```

## 3. Secrets (once)

```sh
fly/scripts/create-secrets.sh <new-key-directory> device-fullchain.pem device.key
```

It asks for the Entra client secret, generates the device CA, the session and policy signing keys,
the internal token, the directory, cursor and content keys, and stages each on its apps with the
device certificate. An existing secret is never replaced. Then:

- Set the printed policy public key as the GitHub variable `SAC_POLICY_PUBLIC_KEY` (step 4): the
  agent release pins it, so devices accept only policy this environment signed.
- **Keep a copy of every file in the key directory outside Fly.io** (a password manager or offline
  storage), then delete the directory. Fly.io never shows a secret's value again, and pre-prod's
  enrolled devices and stored content survive a move of the services only if these keys do.

## 4. The GitHub environment `preprod` (once)

- Secret `FLY_API_TOKEN`: an organization deploy token,
  `fly tokens create org --org <org> --name "github deploy"`.
- Variables: `FLY_ORG` (the organization's slug), `FLY_APP_PREFIX` (`sac-preprod`),
  `SAC_DEVICE_FQDN`, `SAC_ANALYST_FQDN`, `SAC_ENTRA_APP_CLIENT_ID` (the Entra application's client
  id), `SAC_PG_HOST` (the session pooler's host), `SUPABASE_PROJECT_REF`, `SAC_POLICY_PUBLIC_KEY`.
- Secrets `SAC_CLASSIFIER_SIGNING_KEY` and `SAC_EXTENSION_SIGNING_KEY`, and the optional code
  signing variables, as `azure/RUNBOOK.md` §1 step 4. Without code signing the MSI is unsigned.

## 5. Deploy

Merge to `main`, or run the **deploy** workflow. It builds the agent release (artifact
`agent-release`, version `1.0.<run number>`) on Windows, pushes the eight images to Fly.io's
registry tagged with the commit, runs `migrate` in the migrate app (its output is in the run's log,
and a failure stops the run), then deploys each app with that commit's image. Every later release is
the same run. It allocates no public addresses: §1 step 3 did.

## 6. DNS and the dashboard's certificate (once)

Neither hostname may sit behind a proxy such as Cloudflare's orange cloud: the edge terminates TLS
itself and needs the device's client certificate, and Fly.io validates the analyst hostname against
the dashboard's own address. Create every record as plain DNS.

- `fly ips list --app sac-preprod-edge`: an `A` record (the dedicated IPv4) and an `AAAA` record for
  the device hostname.
- `fly certs add <analyst-fqdn> --app sac-preprod-dashboard`, then
  `fly certs setup <analyst-fqdn> --app sac-preprod-dashboard` prints the records for the analyst
  hostname: an `A` (the shared IPv4) and an `AAAA`. `fly certs check <analyst-fqdn> --app
  sac-preprod-dashboard` shows when the certificate is issued, usually within minutes of the records
  resolving.

## 7. The agents' read-only token (once)

`fly tokens create readonly --org <org> --name "agents read-only"`, saved at the path
`refactor-endpoint/TESTBED.md` names. It reads status, machines, addresses and logs; it is never
committed.

## 8. The first tenant

```sh
fly/scripts/tenant-admin.sh tenant create --name "Contoso" --ceiling m1 --actor you@vendor.com
fly/scripts/tenant-admin.sh tenant invite --tenant <tenant-id> --domain contoso.com --actor you@vendor.com
```

Each runs control-api's command in its machine and prints the output: the new tenant id, then the
invite link. Consent and roles are as `azure/RUNBOOK.md` §6. Pre-prod does not serve `/scim/v2/*` or
`/.well-known/*` to browsers, so it has no directory sync.

## 9. Devices (Intune)

As `azure/RUNBOOK.md` §7, with the device hostname.

## 10. Verify

1. Every app's machines are started and pass their checks: `fly status --app <app>` and
   `fly machine list --app <app>` for each app; the migrate app has no machine.
2. Only the edge and the dashboard have public addresses: `fly ips list --app <app>` lists nothing
   for the other six.
3. `curl https://<device-fqdn>/v1/health` reaches control-api (which answers a GET with 405, not
   the edge's 403), and `curl https://<device-fqdn>/admin/v1/` is refused by the edge with 403.
4. `curl -sS -o /dev/null -w '%{http_code}\n' https://<analyst-fqdn>/signin` answers 200 with a
   certificate curl accepts, and the page is the dashboard's sign-in.
5. The deploy run's `migrate` job succeeded (`gh run view <run-id> --log`: `"schema up to date"`), and
   the next run's says `"applied":0`.
6. `fly logs --app sac-preprod-jobs` shows `pass complete` for `aggregate` (every five minutes),
   `erase` (every minute) and `expire` (daily at 02:30 UTC), with no `pass failed` or
   `tenant failed`.
7. On a managed device the app installs, the deployment key's enrolment count rises, and the device
   appears on the dashboard's Devices page by hostname.
8. After prompts from the device: submissions on Tools, Users and Data classes, and a prompt with a
   test card number raises a finding.

## Later

- **Renewing the device certificate:**
  `fly secrets unset --stage EDGE_TLS_CERT_PEM EDGE_TLS_KEY_PEM --app sac-preprod-edge`, then
  `create-secrets.sh` with the new files (it keeps everything else and generates nothing), then
  `fly secrets deploy --app sac-preprod-edge`.
- **A secret staged after the first deploy** reaches the running machines with
  `fly secrets deploy --app <app>`, or with the next deploy.
