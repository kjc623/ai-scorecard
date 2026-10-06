# 26. Validate the template against a real subscription

Needs: the owner's subscription, the `rg-sac-preprod-eastus` resource group, and an `az login` with
Contributor and Role Based Access Control Administrator on it (`azure/RUNBOOK.md` step 1). Nothing is
created by this task.

## Problem

`azure/main.bicep` compiles cleanly and `azure/tools/check-infra.mjs` checks its security
properties, but Azure has never seen it. API versions, property combinations (Container Apps
workload profiles, PostgreSQL Entra administrators, Application Gateway per-path WAF policies, Front
Door Private Link origins, per-secret Key Vault role assignments) and naming rules are only proven by
Azure's own validation.

## Goal

`az deployment group validate` and `az deployment group what-if` succeed for both deployment shapes
of `preprod`:

- bootstrap (`SAC_BOOTSTRAP=true`, no image tag), and
- full (`SAC_BOOTSTRAP=false`, `SAC_IMAGE_TAG` set to any tag; images need not exist for validation).

## Scope

- Set the `SAC_*` environment variables the parameter file reads (`azure/README.md`) to real or
  plausible values supplied by the owner; never commit them.
- Fix every error in `azure/main.bicep` or `azure/modules/` with the smallest correct change, keeping
  `az bicep build` warning-free and `check-infra` passing. If a fix changes a security property
  (network exposure, identity, ingress), ask the owner first.
- Read the what-if output for surprises (resources you did not expect, or expected ones missing) and
  report them.
- Out of scope: `az deployment group create`, and changing `prod.bicepparam` beyond what the same
  fixes require.

## Done when

- Both `validate` runs succeed, and both `what-if` runs complete; the report lists the resources each
  would create.
- `node tools/accept.mjs --only bicep,static` passes.
