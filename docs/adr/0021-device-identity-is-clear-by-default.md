# 0021. Device identity is clear by default, gated per tenant at the device, and a real username rides beside `user_ref`

Status: proposed · Adds `ops.tenant.device_identity` and device-identity fields to the contract,
`ops.device`, `ingest` and the device read · Supersedes the "pseudonymous on the wire" position of
[docs/04](../04-dashboard-and-query.md) §2.1 / A8 and [docs/06](../06-security-and-threat-model.md)
§5.5 / A6 · Related: [0004](0004-observations-are-immutable-and-the-closed-envelope-is-the-record.md),
[0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md),
[0014](0014-content-search-is-a-per-tenant-capability.md)
Date: 2026-10-05

## Context

The Devices page and every search result identified a device only by UUID, and the device carried no
agent version, no mode and no managed state. `ops.device` held `hostname_hash` — and, as built, did
not even write it — because the design deliberately kept the clear hostname off the store and the
wire (§5.5 of `docs/06`; the schema comment on `hostname_hash`). The design's position on people was
stronger still: `user_ref` is pseudonymous end to end, a real name crosses only in the encrypted
`ops.user_dim.directory_object_id_enc`, and `docs/04` §2.1 records "names are not resolved in v1" as
assumption A8.

The product owner has reversed both positions for this product, for the dashboard to be useful: an
operator cannot act on `35beae1b-e366-465a-8517-58df42c88bdc`, and a search hit that names neither
machine nor person is one an analyst must open to understand. The task (`backlog/04-device-identity`)
asks for the hostname on the device and in search, and the owner asked for the real username at
submission time, not the pseudonymous reference.

## Decision

1. **A clear hostname is stored and shown, and a real username rides beside `user_ref`.**
   `ops.device.hostname` holds the clear machine name when the setting permits it;
   `hostname_hash` remains for when it does not. The event contract gains an optional
   `subject_name`, carried alongside — never instead of — `user_ref`, which stays the join key for
   policy scope, dedup, aggregates, k-suppression and audit. A device that cannot attribute an
   observation to a person omits `subject_name` and the read falls back to `user_ref`.

2. **The choice is per tenant, on by default, and it gates what the device sends.** The setting is
   `ops.tenant.device_identity`, the closed pair `clear` | `hashed`, `NOT NULL DEFAULT 'clear'`.
   When it is `hashed` the device sends only the hash and no name, so an opted-out tenant never
   transmits the clear value — the privacy property the original design was protecting is retained
   as an option rather than removed.

3. **The setting travels to the device in the enrolment and health responses**, the two
   authenticated per-device channels that exist today, because the design's proper channel — the
   signed policy bundle — has no writer in this build. When a bundle writer lands, the setting moves
   into the signed bundle so it is covered by the signature.

4. **The device's most recent user is materialised on `ops.device`** (`last_user_ref`,
   `last_subject_name`), maintained by the ingest acceptance path exactly as `last_seen_at` already
   is, so the Devices list stays a single read. Because that read now returns a subject reference and
   a name, it is subject-level and is audited as served (`docs/04` §5.2, `query.devices`).

5. **Agent version, managed state and the effective base collection mode** are reported on
   enrolment and heartbeat and stored in dedicated `ops.device` columns, so the device read is a
   plain read. There is no MDM resolver in this build, so managed state is the agent's report; the
   column already carries the vocabulary an MDM-resolved value would need, and an MDM resolver
   should win over the agent when one exists.

## Alternatives considered

- **Keep the hash and render a stable pseudonym.** Rejected by the owner: a device the operator
  cannot name is the problem the task exists to fix, and the hostname is what the operator's own
  inventory uses.
- **Store clear and gate only at the read.** Rejected: a tenant that opted out would still have
  clear hostnames and usernames at rest, so the setting would be a display filter rather than a
  privacy control.
- **Keep `user_ref` as the only subject field and resolve names through the directory.** Rejected
  by the owner: it needs a directory sync (task 06) that does not exist, and it would not give the
  name as-of-submission for a device whose user changed.
- **Gate the setting in the signed policy bundle.** This is the right channel and the owner's
  suggestion, but `ops.policy_bundle` has no writer in this build (control-api README, "what is not
  built yet" #2), so it cannot reach a device yet. The enrolment/health response is the interim; the
  move to the bundle is the follow-on named in decision 3.
- **Treat the device read as non-subject-level because it returns a device row.** Rejected: once the
  row names a person it is subject-level data whatever the table is, and the audit rule exists
  precisely so that a reclassification of this kind is explicit. The existing test that asserted the
  opposite is updated because the source's shape changed, not to make a test pass.

## Consequences

- `docs/04` A8 and `docs/06` A6 ("names are not resolved in v1", "the wire format is pseudonymous")
  no longer hold. Both are updated to point at this record. The residual risk the design accepted —
  a store compromise yields a behavioural dossier attributed to real people — is now larger, and is
  stated in `docs/06` rather than left implied.
- `subject_name` is optional and gated, so the pseudonymous path still exists for a tenant that sets
  `hashed`; the contract does not make a name mandatory on any kind.
- The agent must resolve the submitting account name. Attribution of a specific intercepted request
  to a specific OS user is platform-dependent; where it cannot be done the field is omitted, which
  is why the read falls back to `user_ref`.
- The Devices read is now audited, which was not true before and is a real cost on a frequently-read
  page. That is the price of showing a person, and it is the same price the design already charges
  any other subject-bearing read.

## What would change this decision

A tenant that requires pseudonymity sets `device_identity = 'hashed'`; the schema, the contract and
the read all support it without a code change. If the field proves not to be resolvable reliably on
managed devices, decision 1's username half should be withdrawn and the device-level
`last_user_ref` kept, which is the committed task text's smaller position.
