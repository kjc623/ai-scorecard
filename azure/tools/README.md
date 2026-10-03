# `azure/tools/` — checking infrastructure without Azure

A zero-dependency static checker for the Bicep tree. It exists because there is no `az` CLI, no Bicep
compiler and no subscription on the machine this was written on — and "we could not check it" is not
the same as "there is nothing to check".

```
node azure/tools/check-infra.mjs          # the checks as a report; exit 1 on a finding
node --test azure/tools/index.mjs         # the suite, in test form
node --test azure/tools/check-infra.test.mjs   # the same suite, named directly
```

Name the file rather than the directory: `node --test azure/tools/` resolves the directory as a
module on Node 22.23.1 and fails with `MODULE_NOT_FOUND` before running anything, which is why
`index.mjs` exists. `tools/verify-all.mjs` discovers `check-infra.test.mjs` by name.

| File | Responsibility |
|---|---|
| `check-infra.mjs` | The checks |
| `check-infra.test.mjs` | The suite |
| `index.mjs` | Re-exports the suite so it can be run by name |
| `package.json` | Declares the `test` and `check` scripts |

## What it reads

`azure/**/*.bicep`, `azure/params/*.bicepparam`, `azure/inventory.json`, `azure/cost-model.md` and
`docs/05-platform-delivery.md`. It makes no network call and has no dependencies.

## What it proves, and what it cannot

It proves properties that are decidable by reading text: every parameter is typed and described, no
region or SKU is hard-coded outside a parameter file, no secret is in the repository, the private-only
resources declare it, `content-vault` is internal-only with no Front Door route, the database has no
public path and no firewall rules, the module layout matches §3.3, and every row of the §2 inventory
is implemented — diffed in **both** directions, so a module with no inventory row is as much a finding
as a row with no module.

It cannot tell you that a deployment works. That needs a subscription, and no environment has ever
been deployed. A green run here means the properties are **declared**, never that they are
**effective**.
