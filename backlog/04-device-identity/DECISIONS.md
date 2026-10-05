# 04. Device identity — decisions to confirm

This task reverses a deliberate design choice: `ops.device` held `hostname_hash`, not the hostname,
and the wire format was pseudonymous end to end (`docs/04` §2.1, `docs/06` §5.5). The task asks to
put a clear hostname — and, per the owner's answer, a real username — on the device. That is a
product and privacy decision, not a technical one, so it is recorded here before it is built, and
the outcome will go in an ADR under `docs/adr`.

Four questions. D1 is the one TASK.md says to raise; D2, D3 and D4 shaped the schema and the read.
The owner answered D1–D3 while this was written; D4 is a recommendation because the owner was not
sure.

---

## D1. Store the clear hostname, and gate it where? **Decided: clear, tenant setting, gated at send.**

The owner's starting suggestion, confirmed: clear hostname as a tenant-level setting, on by default.

**Options**

| | A. Clear, gated at the device (chosen) | B. Clear stored, gated at the read | C. Always clear | D. Keep hashed |
|---|---|---|---|---|
| Where the gate acts | the signed policy/config the device holds; when `clear` the device sends the hostname, when `hashed` it sends only `hostname_hash` | the server always stores clear and the setting only hides it from reads | none | none |
| Privacy property | real: an opted-out tenant never transmits or stores the clear name | weak: clear hostnames sit at rest even for tenants that opted out | reverses the design everywhere | no dashboard name (the task fails) |
| Storage | `ops.device.hostname` (clear) **or** `hostname_hash` | `ops.device.hostname` always | `hostname` | `hostname_hash` |

**Decision: A.** The setting is `ops.tenant.device_identity`, a closed pair `clear` | `hashed`,
`NOT NULL DEFAULT 'clear'`. `hostname_hash` stays for the `hashed` case, so no existing semantics are
lost. The setting has to reach the device, and the design's proper channel — the signed policy bundle
(`ops.policy_bundle.scope_matrix`) — has **no writer in this build** (control-api README, "what is not
built yet" #2). It therefore travels on the two authenticated per-device responses that already exist:
`EnrolmentResponse.device_identity` and `HealthResponse.device_identity`, with the agent defaulting to
`clear` (matching "on by default") until the server tells it otherwise. When the bundle writer lands,
the setting should move into the signed bundle so it is covered by the signature; that is recorded in
the ADR.

**The delivery caveat:** a device that was provisioned while the tenant was `hashed` will, on its
*first* contact after the setting is turned on, have no prior value; defaulting to `clear` means it
starts sending the clear hostname without a round trip. A tenant that wants the opposite default can
set it in the deployment's agent profile. This is the honest bound of a server-driven setting with no
bundle writer, and it is stated in the report.

---

## D2. "Username at time of submission" — pseudonymous `user_ref`, or a real name? **Decided: real name, per submission.**

The committed `TASK.md` said *"primary user: the user reference most recently active on the device"*;
the working-tree edit changed it to *"username at time of submission"*, and the owner answered "real
username". This is a larger reversal than the hostname: `docs/04` A8 and `docs/06` A6 keep names off
the wire and resolve a person only through the encrypted `ops.user_dim.directory_object_id_enc`.

**Options**

| | A. Real username per submission (chosen) | B. `user_ref` only (committed task text) | C. Device-level current user only |
|---|---|---|---|
| What crosses the wire | a new `subject_name` field on the envelope, beside `user_ref` | nothing new; `user_ref` is already there | a name on the heartbeat only |
| Search shows | the name | the pseudonymous ref | the ref (name only on Devices) |
| Design impact | reverses A8/A6; contract + ingest + search change | none | small |

**Decision: A.** The real username is carried on each submission as `subject_name`, alongside the
existing pseudonymous `user_ref` — `user_ref` remains the key for policy scope, dedup, aggregates,
k-suppression and audit, so it cannot be replaced. The field is **optional**: a device that cannot
resolve a name (a background process, a headless host) omits it, and the read falls back to
`user_ref`. It is gated by the same `device_identity` setting as the hostname: when `hashed`, the
device sends neither clear hostname nor name.

Where the agent gets it: the interactive account name of the machine running the agent, from the
process environment / OS user for the submitting session where the platform allows, with an override
flag. The lab device uses the simulator. Attribution of a specific intercepted request to a specific
OS user is the part that cannot be fully proven here (the owner's Windows agent is out of the
harness's reach); what can be proven is the contract, the storage, the read and the dashboard.

**Write/read model.** `ingest.submission.subject_name` and `ingest.observation.subject_name` are
nullable. The device row carries the *most recent* user so the Devices list is a single read:
`ops.device.last_user_ref` and `ops.device.last_subject_name`, maintained monotonically by the ingest
acceptance path exactly as `last_seen_at` already is.

**Consequence the owner should see:** the device read now returns a subject reference and a name, so
by `docs/04` §5.2 row 2 the Devices read becomes a **subject-level read and is audited as served**
(`query.devices`). The existing test asserting the opposite is updated because the source's shape
changed, not to make a test pass.

---

## D3. Agent version, managed state and mode source. **Decided: reported on heartbeat/enrolment, stored on `ops.device`.**

- **Agent version** is already on both channels (`DeviceInfo.agent_version`, `HealthRequest.agent_version`).
- **Managed state** is reported by the agent on enrolment and heartbeat. There is no MDM resolver in
  the repository, so "resolved from MDM where available" is not buildable today; the agent's report
  is the honest source and it is stored on the existing `ops.device.managed_state`. When an MDM
  resolver exists it should win over the agent report; the column already carries the three-value
  vocabulary that distinction needs.
- They are written to dedicated `ops.device` columns (`agent_version`, `managed_state`) rather than
  dug out of `ops.collector_state.detail` jsonb, so `mart.v_device_liveness` stays a plain read.

---

## D4. Which single "collection mode in force" does the Devices column show? **Recommended: the device's effective base mode. Owner unsure; this is the default unless redirected.**

The signed bundle sets modes per tool, population, class and device, so a machine has no one mode.
The owner's mental model was "blanket metadata coverage for all devices, then raise it per device",
and noted M3 will be common. That model points at one value per device:

| | A. Effective base mode (recommended) | B. Widest mode in force | C. Last submission's mode | D. Tenant default only |
|---|---|---|---|---|
| Meaning | the mode that applies to this device when no narrower scope does: the device override if the bundle names this device, else the tenant default (`core.Resolve` with only `DeviceID` set) | the most permissive mode any scope on the device allows | what actually happened most recently | the tenant-wide default, ignoring per-device raises |
| Matches "blanket + raise per device" | yes | partly | no (a fact about the past) | no (ignores the raise) |
| Present-tense config | yes | yes | no | yes |

**Recommendation: A.** The agent computes it from the verified bundle it already holds
(`core.Resolve(bundle, ScopeQuery{DeviceID})`), reports it on the heartbeat, and it is stored in
`ops.device.collection_mode`. Per-tool differences are not shown; the column is labelled so it is not
read as "everything on this device is at this mode". Because it is one reported field, A and B are a
one-line change if the owner prefers B. **This is the one part of the task built on a recommendation
rather than a decision.** If the owner wants D — mode as pure tenant configuration, no per-device
raise — the column is filled from `ops.tenant` instead and the agent does not report it.

---

## What this means for the build

- `database/schema.sql`: `ops.tenant.device_identity`; `ops.device.{hostname,agent_version,collection_mode,last_user_ref,last_subject_name}`; `ingest.{observation,submission}.subject_name`; `record_event` writes it; `mart.v_device_liveness` exposes the device fields; grants; an in-place `MIGRATION.sql`.
- `contracts/event-envelope.schema.json` + regenerated types: optional `subject_name`.
- `endpoint/protocol`: `subject_name` on the envelope; `hostname`/`managed_state` on enrolment; `hostname`/`agent_version`/`collection_mode`/`managed_state` and `device_identity` on health.
- `endpoint/capture-core`: resolve hostname and username, report them, gate on the setting.
- `control-api`: store the enrolment and health device fields, return the setting.
- `ingest-api`: store `subject_name`; maintain the device's most-recent user.
- `query-api`: device read fields; search hit carries `hostname` and the name.
- `query/dashboard`: Devices hostname + User + Agent version + Mode; search `user | hostname | tool`.
- `docs/04`, `docs/06`, `contracts/README`, and a new ADR record the reversal.

**The agent side cannot be verified on the real device from the harness** (the MSI is installed on the
owner's Windows host); it is verified through the protocol/selftest suites and the device simulator,
and the report says exactly what the owner must run.
