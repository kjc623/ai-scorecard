# Shadow AI Capture

Records what employees send to generative AI tools from company-managed devices and makes it
queryable by the company's security team.

Content is interpreted where it is observed and, by default, does not leave the device:
classification labels, a digest and dimensions cross the network, and a prompt is uploaded only when
the backend grants that one event. `docs/architecture.md` describes the system.

## Repository

| Path | What it is |
|---|---|
| `device/` | The device agent: `capture-core` (the service), `classifier-host`, `capture-spool`, `protocol`, and cross-module integration tests |
| `device/extension/` | The Chrome/Edge extension |
| `device/installer/` | The Windows MSI, macOS package and Linux package |
| `services/ingest-api/` | The event write path |
| `services/control-api/` | Enrolment, policy, content grants and upload, sign-in, SCIM, tenant onboarding |
| `services/content-vault/` | Encrypted prompt storage, retrieval and content search |
| `services/query-api/`, `services/dashboard/` | The read API and the analyst web app |
| `services/edge/` | The device entry point where there is no Application Gateway: TLS with the client certificate forwarded, and only the device API reachable |
| `services/jobs/` | The scheduled `aggregate` and `expire` jobs |
| `services/database/` | The PostgreSQL schema, its invariant tests, and the `migrate` job |
| `services/platform/` | Managed-identity tokens and the PostgreSQL connection shared by the Go services |
| `contracts/` | The event envelope schema and its generated Go binding |
| `azure/` | The deployment (Bicep), its runbook and scripts |
| `fly/` | The pre-prod deployment on Fly.io and Supabase, its runbook and scripts |
| `localdev/` | A local lab of the whole system |
| `tools/` | The acceptance gate and the cross-component checks |

## Build and test

Go 1.27, Node 22, Docker (for the database gate) and the Azure CLI with Bicep (for the template
check). One command runs every gate that can run on the host:

```
node tools/accept.mjs
```

Each component's README says how to build and test it on its own.

## Deploy

`.github/workflows/ci.yml` runs the gates each change can affect; `.github/workflows/deploy.yml` builds the
agent release and the images and deploys them to pre-prod on Fly.io and Supabase. `fly/RUNBOOK.md`
takes pre-prod from empty accounts to a managed device whose events appear on the dashboard;
`azure/RUNBOOK.md` does the same for an Azure environment.

## Run locally

`localdev/README.md`.
