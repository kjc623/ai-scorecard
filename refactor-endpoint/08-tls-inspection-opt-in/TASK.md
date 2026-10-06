# 08. TLS inspection becomes opt-in

## Problem

Today every device intercepts TLS by default:
- capture-core installs its per-device root in the machine trust store;
- the CLI shim points runtimes at the local proxy;
- on Windows, every signed-in user's Internet Settings get a proxy auto-config (PAC) URL;
- the proxy decrypts the hosts in `interception.seed_hosts`.

The owner has decided this becomes opt-in per tenant and off by default (`DECISIONS.md`,
`DESIGN.md` §10). Some of these components are built only at service start
(`cmd/capture-core/service.go`, the PAC around lines 379–400), so they can't be switched by
policy.

## Goal

With the tenant setting off (the default), a device runs no proxy, writes no proxy or CA
environment, sets no PAC, and has no root in the trust store. Turning the setting on brings all
four up on the next policy poll, without a restart; turning it off removes them.

## Scope

- **Database**:
  - `ops.tenant.tls_inspection boolean NOT NULL DEFAULT false`, audited on change like the other
    tenant settings.
  - The lab seed (`localdev/lab.mjs` or `localdev/seed.mjs`, wherever the tenants' settings are
    set) sets it `true` for the lab tenant, so a lab device keeps today's behaviour.
    Leave the sample tenant at the default.
- **control-api**:
  - `policyserve.Bundle.Interception` gains `Enabled bool` (`json:"enabled"`), filled from the
    column.
  - `PUT /admin/v1/settings/tls-inspection` with `{enabled}`, shown in `GET /admin/v1/settings`.
- **Device**:
  - `policy.Interception.Enabled`.
  - Make the TLS proxy, the CLI shim and the PAC `core.Toggled` providers (task 06), each
    returning `b.Interception.Enabled`.
  - The PAC becomes a registered provider: collector `egress_proxy` is taken, so give it its own
    collector code, `desktop_proxy`, with a `ref.collector` row.
  - The trust root's install and remove move with the TLS proxy's Start and Stop. Today the
    supervisor removes it at shutdown; with this change it is removed also when policy switches
    interception off.
  - The PAC's listen address is read from policy on each start, so a changed `pac_listen` takes
    effect on the next toggle. Empty means the PAC provider is disabled; fix the 8350 fallback to
    match the bundle's documented meaning.
- **Native-first exclusion** (`DESIGN.md` §10) is not in this task. It needs process attribution
  (task 09) and the catalog (task 14), and lands in task 27.
- **Dashboard**: a "TLS inspection" switch on the Settings page, with one sentence under it
  saying what turning it on installs on devices.
- **Tests**:
  - device: with a bundle whose `interception.enabled` is false, startup leaves the fake trust
    store empty, writes no shim profile and starts no proxy; toggling it on then off installs
    and removes all three;
  - control-api: compose and the PUT route;
  - dashboard: the switch.

## Done when

- The device tests above pass.
- On the reference VM, after deploying with `node localdev/testbed/deploy.mjs`:
  1. With the setting off for the lab tenant, all three are absent:
     - `invm.ps1 -Command 'certutil -store Root'` lists no Shadow AI Capture root;
     - `invm.ps1 -AsUser console -Command 'reg query "HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings" /v AutoConfigURL'`
       shows no PAC;
     - `invm.ps1 -Command '[Environment]::GetEnvironmentVariable("HTTPS_PROXY","Machine")'` is
       empty.
  2. Switching it on in the dashboard brings all three back within one policy poll (the default
     interval is 15 minutes; don't restart the service).
     - Run the same three commands repeatedly, and record the time from the change to each one
       appearing.
     - Run the PAC check with `-AsUser second` too: the second user's Internet Settings get the PAC
       as well.
     - Switching it off removes them within one poll.
  3. Afterwards, leave the setting on for the lab tenant.
- `node tools/accept.mjs` passes.
