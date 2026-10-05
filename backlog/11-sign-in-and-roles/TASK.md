# 11. Sign-in and roles

Depends on: nothing. Tasks 12 and 13 need it.

## Problem

There is no login. The dashboard's server (`query/dashboard/tools/serve.mjs`) adds a development
principal header (`x-sac-dev-tenant`, `x-sac-dev-actor`) to every request, and `query-api` trusts it
when `SAC_DEV_TRUST_PRINCIPAL=1`. Every action in the audit trail is attributed to
`analyst@lab.test`. Since the approval step on content retrieval was removed, anyone who can reach
the Search page can read any stored prompt.

## Goal

- Sign-in through the customer's identity provider with OpenID Connect, Microsoft Entra ID first.
- The session establishes tenant and actor. `query-api` and the vault receive them from a verified
  token, never from a client-supplied header. The development header remains only behind its flag,
  for the lab.
- Roles, enforced server-side in `query-api` and the vault, with the dashboard hiding what a role
  cannot use.
- The audit trail records the real actor.

## Read first

- `docs/06-security-and-threat-model.md`: principals, and the roles the design already names.
- `docs/04-dashboard-and-query.md` on who may see subject-level data.
- `query/query-api/src/http/server.js`: `principalOf`.
- `vault/content-vault/internal/auth`.
- `query/dashboard/src/shell.js`.

## Raise before building

The owner's starting proposal for roles is below. It is not the design's list. Reconcile it with
`docs/06`, put the result to the owner, and stop for confirmation.

- viewer: aggregates only, no per-person data
- analyst: events, findings, people
- content reader: may read stored prompt content and search prompt text
- admin: settings, sanction decisions, exports

Session storage must not add a third-party dependency without saying so.

## Done when

The agent verifies, on the device-auth lab running a local OIDC stand-in:

- An unauthenticated request to the dashboard or to `/v1/query` is refused.
- Signed in as a viewer, Search cannot be opened and content cannot be read; signed in as a content
  reader, both work. Each is observed in the browser, which means `tools/observe.mjs` has to be
  able to carry a session; add that.
- The audit trail shows each user's own identity.
- Tests cover each role boundary on the server.

The owner verifies:

- Sign-in against a real Microsoft Entra ID tenant. Say in the report what configuration it needs.
