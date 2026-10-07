# 00a. Pre-prod environment and test tenant

Needs:
- backlog tasks 25 (CI green) and 26 (template validated against the subscription) finished;
- the work on `backlog/15-device-layout` and `refactor-endpoint/plan` merged to `main`, because
  pre-prod deploys from `main`;
- the owner available for every step marked **owner**: they create things in Azure, DNS, Entra,
  GitHub and Intune that an agent may not.

## Problem

Every on-device check in this plan runs against Azure pre-prod, never the local lab. Pre-prod
doesn't exist yet: the subscription the owner's `az` is signed into has no `rg-sac-preprod-eastus`.
`azure/RUNBOOK.md` lists the sequence; most steps are owner actions.

## Goal

Pre-prod is deployed from `main` and passes `azure/RUNBOOK.md` §8. It has a test tenant, linked to
the reference VM's Entra tenant, that agents may create data in. The owner's own tenant
(`11111111-1111-1111-1111-111111111111`) stays untouched. The values later tasks need are recorded
in `refactor-endpoint/TESTBED.md`.

## Scope

The agent drives the checklist, runs only the read-only checks, and stops at each **owner** step
with exact instructions.

1. **Owner:** `azure/RUNBOOK.md` §1 (prerequisites) in full.
   - The device TLS certificate comes from a public CA, so devices need no extra trust profile.
   - Choose the subscription. If it isn't `f73f9bcb-…`, give the agent the id so `az` can be
     pointed at it read-only.
2. **Owner:** §2 bootstrap deployment (the deploy workflow with *bootstrap*), §3 secrets
   (`create-secrets.sh`, the device certificate import, `SAC_POLICY_PUBLIC_KEY`), and §4 deploy.
3. **Owner:** §5 DNS records and the Entra federated credential.
4. **Agent, read-only:** §8 checks, with `az containerapp list|show`, `az containerapp job execution list`
   and `curl` against the device and analyst hostnames, and `az monitor log-analytics query` for
   job logs.
   - Report each check's command and result.
   - Run no `az` command that creates, updates or deletes anything.
5. **Owner:** §6 test tenant.
   - Create it with
     `azure/scripts/tenant-admin.sh rg-sac-preprod-eastus tenant create --name "Endpoint Test" --ceiling m3 --actor <owner>`.
   - Invite the VM's Entra domain, consent, and sign in as admin.
   - Assign the console user and the second user (`TESTBED.md`) the `viewer` role, and the owner
     `admin`.

   The ceiling is `m3` so tasks can exercise every mode; each task sets the mode it needs on the
   Settings page.
6. **Owner:** from the dashboard, **Settings → Deployment → Download Intune package**, in the zip
   form (MSI plus tenant file).
   - Save its `ShadowAICapture.tenant.env` as `%USERPROFILE%\.sac-testbed\ShadowAICapture.tenant.env`
     on the PC. It holds the deployment key; it never goes in the repository.
   - The agent checks the file names the test tenant and the device endpoint, without printing the
     key.
7. **Owner:** fill in the environment rows of `TESTBED.md`:
   - subscription;
   - resource group;
   - device hostname;
   - analyst hostname;
   - test tenant id;
   - GitHub repository.
8. **Agent:** update `azure/RUNBOOK.md` only where a step turned out wrong or missing while
   following it. That is the one product file this task may change; keep the edit short and
   factual.

## Done when

- Every `azure/RUNBOOK.md` §8 check passes, and the report shows the commands and output.
- The owner confirms they are signed in to the dashboard as admin of the "Endpoint Test" tenant.
- The tenant file is saved, and `TESTBED.md`'s environment rows are filled in.
- `node tools/accept.mjs` passes on `main`.
