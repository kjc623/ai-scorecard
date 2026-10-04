# 0013. No kernel-mode component and no Apple restricted entitlement in v1

Status: proposed
Date: 2026-10-02

## Context

Two environment facts constrain how far collection can reach, and both are stated in the brief as
boundaries to design against rather than problems to solve.

**Windows.** Brief E8: "Without a kernel-mode component, interception reaches only applications that
honour system proxy settings." Chromium and Electron clients do; many native applications do not. Closing
that gap requires a kernel driver, and brief §5.5 makes that commercially expensive in this segment: the
product will not qualify for Microsoft's security-vendor allowlist (E21), EV certificates no longer
bypass SmartScreen (E20), and a component that installs a root certificate and intermediates TLS
already resembles malware to other endpoint security products (E22). A kernel driver makes all three
worse and adds the largest possible blast radius to the component that already has the largest one.

**macOS.** Brief E18: restricted entitlements are "an approval gate with no published SLA" and can be
refused. Brief R2 names them a schedule gate. The intuitive reading is that macOS collection is blocked
until Apple approves something.

**Linux.** Linux is a supported endpoint platform. The same E8 boundary applies — a client that ignores
the system proxy and the `http_proxy`/`https_proxy` environment is not reached by a userspace proxy — and
the kernel facility that would close it is an **eBPF/TC hook** rather than a Windows WFP callout or an
Apple entitlement. It carries the same blast radius, the same review burden, and the same question of
whether a customer's security tooling treats it as an agent of interception, so v1 declines it for the
same reasons and treats it as a later coverage upgrade.

That reading is wrong for this design, and it is worth stating why in an ADR, because the difference
between "blocked on Apple" and "not blocked on Apple" is the difference between a launch date and a
queue position.

## Decision

**No kernel-mode component in v1.** The egress proxy is user-space. The resulting coverage boundary is
accepted, **measured**, and reported: a client that ignores the system proxy is excluded, recorded as a
named coverage gap with a reason (brief §5.2: "Detect the failure, exclude the process, and record that
coverage was not achieved for it"), and never left broken to preserve collection.

**No Apple restricted entitlement in v1.** The macOS design needs none:

| Requirement | v1 mechanism | Entitlement needed |
|---|---|---|
| Root certificate trust | MDM configuration profile (`com.apple.security.root`) into the System keychain, with trust set by the payload | none |
| System proxy | MDM configuration profile | none |
| Privileged background service | LaunchDaemon | none |
| Process and module enumeration | Unprivileged process listing | none |
| Loopback port occupation | User-space port binding (all three default ports are above 1024) | none |
| Document parsing isolation | Child process with a memory cap and hard timeout | none |

Endpoint Security would add exec telemetry for mode I and a Network Extension would extend coverage past
E8. Both are **coverage upgrades, not dependencies**. The applications are filed in week 1 anyway — they
are free to file, and their absence becomes binding the moment the upgrade is wanted — but **no launch
milestone depends on them**. This converts brief R2 from a launch blocker into an optional enhancement.

The same reasoning applies to Screen Recording on macOS, which brief E16 says can never be pre-granted by
MDM and which any approach depending on would break zero-touch deployment permanently. Nothing depends on
it, and mode C is best-effort by design.

## Alternatives considered

- **A kernel-mode network filter on Windows.** Rejected for v1 on blast radius, on the E21/E22 reputation
  problem, and because brief E8 explicitly frames the boundary as something to measure rather than
  eliminate. It remains the correct upgrade if measured coverage on a target customer's estate proves
  unacceptable.
- **Apple Endpoint Security as a dependency for mode I.** Rejected: it makes a launch milestone depend on
  an approval with no published SLA, and it would deliver metadata only (brief E15: "no file-content or
  payload read"), so it cannot improve content coverage at all.
- **A Network Extension content filter as a dependency.** Rejected for the same schedule reason. It is a
  genuine coverage improvement and is filed for, but not waited on.
- **An eBPF/TC hook on Linux.** Rejected for v1 on the same blast-radius and review grounds as the
  Windows kernel filter. It is the correct Linux coverage upgrade if measured coverage proves
  unacceptable, and it needs no third-party approval — which is why it is a candidate for a later release
  rather than a schedule risk.

## Consequences

Easier: the macOS launch path is MDM configuration plus a signed package, with no third party in the
critical path. The Windows component is user-space, so it is easier to sign, review and kill. Blast
radius is bounded by fail-open behaviour rather than by kernel correctness.

Harder, and stated plainly: **coverage is lower than it could be, and the product must say so.** Brief
E8's boundary is real. It is reported per provider and per device rather than averaged into a number that
looks acceptable, and the sales conversation has to survive a customer asking why a particular native
application is not covered.

We now maintain: the coverage measurement described in [01-collectors](../01-collectors.md) §15, the
exclusion artefact that brief R4 requires, and the two entitlement applications as open schedule items
rather than completed prerequisites.

Revisit if: measured coverage on a real estate shows the boundary costs more than the driver would, or if
Apple grants the entitlements early — in which case the upgrade lands sooner and this ADR is amended
rather than reversed.
