# 19. Tell people why sign-in is refused for a closed tenant

## Problem

control-api refuses to mint a product token when a tenant is not `active` or its `read_enabled` is
false: sign-in and token refresh answer 403 with the code `tenant_closed`
(`control/control-api/internal/identity/identity.go`, `CodeTenantClosed`). The dashboard server shows
a generic sign-in failure, and a session whose refresh is refused mid-use just stops working.

## Goal

- The sign-in page says the organisation's access is suspended and to contact their administrator,
  when the refusal is `tenant_closed`.
- When a refresh is refused with `tenant_closed` during a session, the server ends the session and
  shows the same message, instead of failing API calls one by one.

## Scope

- `query/dashboard/server/` (the sign-in flow, the session refresh, the sign-in page messages).
- Tests in `query/dashboard/test/` with a stubbed control-api answering `tenant_closed` on sign-in and
  on refresh.
- Out of scope: control-api (its behaviour is settled), other error codes.

## Done when

- The tests pass and cover both paths.
- On the lab, with the sample tenant's `read_enabled` set to false by the superuser (and set back
  afterwards), signing in to it shows the message (observed in the browser). Never change the owner's
  tenant.
