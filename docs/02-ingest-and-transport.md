# Shadow AI Capture — Ingest and Transport

**Status:** proposed · **Owns:** the device→cloud channel, the ingest write path, the grant and content
paths, and the **normative deduplication specification in §4** · **Read with:**
[00-architecture](00-architecture.md) (the master document) ·
[01-collectors](01-collectors.md) · [03-data-platform](03-data-platform.md) ·
[04-dashboard-and-query](04-dashboard-and-query.md) · [06-security-and-threat-model](06-security-and-threat-model.md) ·
[database/schema.sql](../database/schema.sql) · [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json)

Requirement references are the brief's own (`brief §4.3`, `R9`) or the master document's decisions and
constraints (`D5`, `C12`, `E3`, `Q3`). Anything the brief does not say is marked **ASSUMPTION** and
indexed in Appendix C.

---

## 1. Scope

This document specifies how bytes move between a managed device and the cloud, and what the cloud does
with them. Three invariants are owned here, and everything below exists to hold them:

| # | Invariant | Source |
|---|---|---|
| I1 | No prompt text and no attachment bytes cross the network without a per-event decision the backend made and can audit | brief §1, §4.4; C14; master §4.2 step 8 |
| I2 | Every accepted observation is counted **exactly once per logical submission**, every collapse is explainable in terms of which route produced what, and no observation is silently discarded | brief §4.1, §4.3; R9; C12 |
| I3 | No failure path reports success: every rejection, gap, undercount and unreconcilable observation has a machine-readable surface | brief §7; C22–C25; R11 |

**In scope:** transport and device authentication (§2); the grant state machine (§3); deduplication —
normative (§4); the six device-facing APIs (§5); the idempotent write path (§6); validation and reason
codes (§7); batching, backpressure and retry (§8); the health channel (§9); grant issuance and upload
(§10); the analyst retrieval path (§11); tenant and region enforcement at the edge (§12); the wire-level
failure catalogue (§13).

**Out of scope, specified elsewhere:** collector internals, spool format, provider behaviour
([01-collectors](01-collectors.md)); `mart` aggregates, retention execution and reconciliation jobs
([03-data-platform](03-data-platform.md)); dashboard and query shapes
([04-dashboard-and-query](04-dashboard-and-query.md)); key hierarchy and threat model
([06-security-and-threat-model](06-security-and-threat-model.md)).

**ASSUMPTION:** §4's canonicalisation is a versioned contract (`sac-canon-1`). Changing any step in
§4.2 after the first device ships splits the fleet into two digest populations, so it is treated as a
schema change with a conformance test, not as a tuning exercise.

---

## 2. Transport and authentication

### 2.1 The channel

| Property | Value | Source / why |
|---|---|---|
| Hostname | Per-region service FQDN on a **custom domain** (e.g. `ingest.eu.example.com`) | Application Gateway terminates the device TLS connection; the device credential names it — the certificate's SAN in `x509` mode, the token audience in `dpop` mode (Appendix B) |
| Protocol | TLS 1.3 minimum; TLS 1.2 refused | Cipher agility without configuration drift; the device population is ours, not the customer's browser |
| 0-RTT / early data | **Refused** | Early data is replayable and every endpoint here writes |
| Renegotiation | Refused | No legitimate need; it is an attack surface |
| Mutual auth | A per-device credential is **required**, and it is one of two production modes: **`x509`** (client certificate) or **`dpop`** (RFC 9449 proof of possession). The edge forwards; the origin authenticates (ADR 0020) | brief §4.2 "one-shot, mutually authenticated"; master §4.2 step 6 |
| Pinning | In `x509` mode the device pins the **issuing CA set** (not a leaf), delivered in the MDM enrolment profile. In `dpop` mode the device holds the keypair and no CA pins | Leaf pinning breaks on every rotation; CA pinning survives it |
| Payload | Envelope JSON only (§5.3). Content never travels on this channel | I1; master §4.2 step 5 |

**The edge, and why it is a filter rather than the authority.** Application Gateway `WAF_v2` is the
public device ingress (ADR 0020 decision 1). Its listener runs in **passthrough** mode by default: it
requests a client certificate when one is presented and forwards it as PEM in `X-Client-Cert`, set by a
rewrite from the `{var_client_certificate}` server variable, and does not validate the chain. The origin
re-validates the chain against its configured trust bundle and, decisively, the **per-device credential
status** on every request (§2.3); in `dpop` mode there is no certificate at the edge at all and the
origin verifies the token, the per-request proof and the replay store. The edge is a *filter*, never the
authority. Strict mode — the edge validating against an uploaded CA chain — is an optional hardening for
certificate-only tenants, not the default, because one regional gateway serves tenants with different
credential modes. This replaces the Front Door Premium mTLS premise: that feature is in **preview**, its
revocation check is OCSP-only, its support for our Private Link origin is denied by Azure's own
documentation, and as an L7 proxy it always terminates TLS, so the origin could never verify the device
handshake under it (ADR 0020; Appendix B). Front Door remains in front of the analyst surface
(`/analyst/*`) and is not on the device path.

**Origin reachability.** The origin must be reachable only through the edge: the container app
environment takes no public ingress, and Application Gateway reaches it over the VNet, with the source
restricted to the gateway subnet (master §4.1 diagram). A forwarded `X-Client-Cert` is trusted **only**
behind that lock — the origin re-validates the chain regardless, but the lock is what makes the
forwarded header meaningful, because the platform documents that an origin reachable from the internet
can be called directly and bypass the edge entirely. This is a correctness requirement of the
authentication design, not a hardening nicety (ADR 0019's intent, kept).

### 2.2 The per-device credential

One credential per device, per tenant, and it is one of two kinds: an **`x509`** client certificate or a
**`dpop`** RFC 9449 keypair (ADR 0020 decision 4). The private key is generated on the device and never
exported — enrolment carries a PKCS#10 CSR in `x509` mode and the public JWK plus a proof of possession
in `dpop` mode, never key material. The certificate carries `device_id` as subject CN, the tenant in an
organisational attribute, `clientAuth` EKU, and the regional service FQDNs in SAN.
`public_key_thumbprint` on `ops.device_credential` is the **single transport binding for both modes**:
SHA-256 over the certificate's SubjectPublicKeyInfo for `x509`, and the RFC 7638 JWK thumbprint for
`dpop`, in the same base64url spelling. `credential_type` records which, so one gateway can serve a
mixed fleet.

Why per-device rather than per-tenant: brief §4.2 requires device identity to be "revocable per device",
and revocation granularity is the whole point — a stolen credential must cost one device, not a fleet.
Per-device credentials are also what make every write attributable, which §9, §11 and C30 depend on.

| Lifecycle step | Behaviour | Source |
|---|---|---|
| **Issue** | MDM delivers the tenant package: the generic agent and a tenant file naming the service FQDN and the tenant's reusable **deployment key** (the lab profile carries a short-lived, single-use enrolment token instead; §5.1). Device calls `POST /v1/enrol` with the key or token: in `dpop` mode with its public JWK and a proof of possession, which registers the key; in `x509` mode either with a CSR the tenant intermediate CA signs, or, for a customer-issued tenant, by presenting the certificate the customer's PKI/MDM already issued, which the origin verifies against the tenant's `device_ca_pem` and registers ([ADR 0022](adr/0022-customer-issued-device-certificates-are-registered-not-signed.md)). Either response returns the credential, `device_id`, tenant and region | brief §4.2; D4 (MDM-delivered profile); ADR 0020 decision 3; ADR 0022 |
| **Re-image** | Re-enrolment with the same hardware identity returns the **existing** `device_id` and a fresh credential; no duplicate device row is created | C11; brief §4.2 |
| **Rotate** | At 60 days of a 90-day life, the device re-enrols authenticated with its **current** credential and receives a replacement. Old and new are both accepted for a 7-day overlap, so a failed rotation never locks a device out | **ASSUMPTION:** duration and overlap are operational parameters, not measured. The overlap is enforced at the origin's credential store, not at the edge, so it does not depend on the edge's CA list |
| **Expiry watch** | The server knows every `not_after` it issued. A daily job alarms on credentials inside 14 days with no rotation, and the device reports `credential_not_after` in health (§9) | C24; avoids the E19-class known-date outage |
| **Revoke** | An operator sets `revoked_at` (with a reason) on the credential in `ops.device_credential`, or on the device in `ops.device`. The **origin** checks status on every request — no cache, because the volume is 0.14 events/s mean and ≤500 events/s worst case (master §1.4), which a point lookup absorbs | brief §4.2 "a revoked device is rejected and marked accordingly" |
| **Emergency (tenant-wide, `x509` only)** | Replacing the tenant's CA in the origin's trust bundle invalidates every certificate that tenant's CA issued. It is a manual, audited, two-person action with an obvious blast radius; it does not touch a `dpop` credential, which is revoked row by row instead | ADR 0020: the origin is the authority, so the CA bundle is replaced there rather than at the edge |

**Revocation is not erasure.** A revoked device's already-ingested observations remain subject to the
retention and erasure paths (brief §3.4). Revocation stops the flow; it does not rewrite history, and
the two must not be conflated in the console.

### 2.3 What happens to an in-flight batch when the credential is revoked

The credential status check is performed twice: once at request admission (cheap rejection) and once
**inside the write transaction, before commit** (authoritative). A batch whose device is revoked while
the batch is being validated therefore fails with `401 revoked_device` and the transaction rolls back:
nothing is written, so there is no partial batch to reconcile (I2). The device stops sending, records
the rejection locally, and **retains its spool** — the data is not discarded because the device may be
re-enrolled. Operator-visible signal: the device record flips to `revoked` with the actor and time, its
`last_seen_at` freezes, and the device appears in the "devices not reporting" answer (brief §3.6
question 7) as `revoked`, not as `absent` — the liveness job must not overwrite a known cause with an
unknown one (brief §3.2's list of states that must never be merged).

### 2.4 Why collectors hold no database credential

| Reason | Consequence if violated |
|---|---|
| The device is untrusted (master §3, Alternative B treats it as such). A DB credential on 5,000 endpoints is 5,000 opportunities for fleet-wide data access | One compromised laptop reads every tenant's events |
| Tenant must come from the **authenticated principal**, never from the request body (C32; master §4.3). A DB client chooses its own session tenant | Cross-tenant reads become a configuration accident |
| §4.3 requires validation against a versioned schema; D8 makes `kind` a closed registry the server enforces, which is what makes R7 structural | A collector defect ships raw process telemetry straight into the store |
| Revocation must take effect on a device that may be offline for days; a network-edge credential check does that, a database credential cannot | A decommissioned device keeps writing |
| The database has no public listener and the ingest service holds the only writer identity | Bypass of every control in this document |

So: `ingest-api` writes `ingest.*` under its own managed identity with forced row-level security; the
device's only capability is to call six endpoints as itself.

---

## 3. The grant and content path at a glance

brief §4.4 inverts the usual design: content does not arrive and get judged, it **stays put until a
judgement arrives**. The device detects the policy match; the backend decides; only a granted decision
produces anything the device can upload with.

```
 DEVICE                                EDGE + control-api              content-vault
 ──────                                ──────────────────              ─────────────
 provider observes submission
   │
   ├─ policy match (rule id, class, severity)          ── content held locally;
   │                                                      content state = local_only
   ▼
 [requested] ── POST /v1/content/grant ──► [evaluating] ── inputs: mode, budget,
   │   (event_id, mode, digest, size)          │          retention class, case ref
   │                                           │
   │                    ┌──────────────────────┴───────────────────────┐
   │                    ▼                                              ▼
   │              [granted]                                     [denied(reason)]
   │       upload URL (1 object, ≤T)
   │       wrapped key + object key
   │                    │                                              │
   ▼                    ▼                                              ▼
 [uploading] ── PUT ciphertext ──► [staged] ── finaliser verifies ──► [uploaded]
   │                                  │        size + raw digest          │
   │  no PUT before T                 │        grant id in metadata        │
   └──────────────► [expired] ────────┴── mismatch / no live grant ──► object deleted
                                                                          │
                          denied / expired ⇒ content state stays local_only
```

**Grant decisions** (`ops.grant.decision`, a closed enum). The bracketed labels in the diagram are steps
of the flow; the stored values are these five:

| `decision` | Meaning | Exits to |
|---|---|---|
| `pending` | Decision in flight for one `event_id` (the diagram's *requested* and *evaluating*) | `granted`, `denied` |
| `granted` | Decision recorded; upload URL and object key issued; `upload_expires_at` set | `expired`, `voided`; a verified upload leaves the grant `granted` and creates the object row |
| `denied` | Terminal, with one of the four reasons in `denial_reason` (§10.2) — a denial without a reason is refused by a check constraint. Content stays on the device | terminal |
| `expired` | `upload_expires_at` passed with no verified object. Staged bytes deleted | terminal |
| `voided` | Invalidated before completion: credential revoked, tenant suspended, digest mismatch, or a second write attempted | terminal |

There is no `uploaded` grant decision. A verified upload — correct size, matching raw digest, grant id in
metadata — is recorded as a row in `ops.content_object`, which exists only once something has actually
been uploaded and whose own `state` is the narrower `uploaded · shredded`.

The four-value content state (`not_captured · local_only · uploaded · shredded`, master §5.4) lives on
`ingest.submission.content_state`. It is a *different* axis and is never collapsed into the grant
decision: a grant may be `denied` while the content is `local_only`, and an `expired` grant leaves it
`local_only` rather than `not_captured` — the device still holds the content and the distinction is
exactly the one brief §3.2 forbids merging.

**As built:** the device half and the decision half exist and run end to end in the local auth lab.
`capture-core` holds M3 content in a sealed local store (`contentstore`), asks for a grant once the
event is delivered, seals the object under the key the grant carries and makes the one upload.
`control-api` decides (`internal/content`), and its finaliser verifies the upload's size and declared
digest against the live grant before `content-vault` records the object. `ingest.record_event()` still
inserts every submission as `not_captured` and the envelope carries no content-state marker
([ADR 0017](adr/0017-the-m3-content-state-marker-is-device-local.md)); `control-api` moves the
submission to `local_only` when the device first asks for a grant — the request is the evidence that
content is held — and to `uploaded` when an upload is finalised. Three things differ from the design
above and are named rather than hidden: an `expired` or `voided` grant is not swept by a job (a new
request simply decides again, and a digest mismatch voids the grant at finalise); the staged-object
store is the lab's `contentlab`, not Blob storage; and these writes need database grants the schema
does not yet give `sac_control` (a read of `ingest.observation`, the `content_state` update, and the
`ops.usage_daily` write), which the lab does not exercise because it connects as the database owner.

---

## 4. Deduplication — normative

> This section is the contract referenced by master §5.2 and by
> [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json) for `dedup_key`.
> It is implemented independently on the endpoint and at ingest and is covered by a conformance test
> using recorded traffic from two routes observing one submission (Q3).

### 4.1 The problem

One employee action is visible to more than one provider. A prompt typed into `chatgpt.com` is seen by
`ext.web_request` (the serialised body) and by `proxy.tls` (the same body after decryption). A coding
agent is seen by `cli.shim` and by `proxy.tls`. Attachment content is read by `ext.page_context` and
its name only by `ext.web_request` (E3). Each provider emits its own envelope with its own `event_id`;
the schema is explicit that they "collapse later through `dedup_key`, not here".

The product consequence of getting this wrong is stated as a risk, not a nicety: R9 — "double-counting
where collection paths overlap **inflates every number the customer sees**". The complementary
requirement is brief §4.1's: where two routes overlap the higher-fidelity one wins and **the record
must carry which route produced it**, so a challenged number can be explained rather than defended.

**Why hashing raw bytes cannot work.** SHA-256 over the observed octets produces a different value for
every one of the following, all of which are the *same* submission:

| Difference | Why it happens |
|---|---|
| `{"prompt":"caf\u00e9"}` vs `{"prompt":"café"}` | JSON escaping vs UTF-8 literals; precomposed vs decomposed characters |
| `{"a":1,"b":2}` vs `{"b":2,"a":1}` | JSON key order is not semantically meaningful |
| `multipart/form-data; boundary=A1B2` vs `boundary=Z9Y8` | The browser picks a random boundary per request |
| Body with `\n\n` vs DOM text with a single blank line | The compose box and the serialised body differ in whitespace |
| `Content-Length` 4096 vs 4102 | Escaping changes the byte count while the text is identical |
| The same request re-sent by the client after a perceived failure | A new request id, new framing, same text |

Raw-byte hashing therefore does not deduplicate; it produces one logical submission per route per
retry, which is precisely the inflation R9 warns about. Deduplication must be computed over a
**canonical form derived from the user-authored content**, and the derivation must be identical in a Go
proxy parsing a provider's JSON body and a JavaScript extension reading a DOM node.

### 4.2 The canonical form

Extraction is route-specific and is *not* part of this contract; everything after extraction is.
A route produces exactly two things: a text string (possibly empty) and an ordered list of attachments.

**Canonicalisation steps, in order. A conforming implementation performs every step, in this order.**

| # | Step | Rule and justification |
|---|---|---|
| C1 | **Segment selection** | Select the **user-authored** segment: the final user-role turn of the submitted request, or the composed payload itself where the surface composes one. Exclude system/developer prompts, tool schemas, retrieval context, re-sent conversation history and anything the provider's client adds. This rule is what lets a proxy reading `messages[]` and an extension reading a compose box select the same characters. A route that cannot identify the boundary must **not guess** — it degrades to §4.5. Multiple authored segments are joined, in authored order, by a single U+0020 after C5. |
| C2 | **Decode** | Bytes → Unicode string using the declared charset (UTF-8 default); strip a leading BOM; replace ill-formed sequences with U+FFFD. Escape and entity processing happens in extraction, not here, so both routes arrive with the same characters. |
| C3 | **Unicode normalisation: NFC** | Not NFKC/NFKD. Compatibility folding merges distinct content (full-width vs ASCII, ligatures, superscripts), which would both inflate false merges and destroy the record's value as evidence. NFC is sufficient for the real cross-route variance: combining marks vs precomposed characters. |
| C4 | **Strip controls and invisibles** | Remove C0 controls **except TAB, LF and CR** (which C5 folds), DEL and C1 (U+007F–U+009F), zero-width and joiners (U+200B–U+200D), bidi marks and overrides (U+200E, U+200F, U+202A–U+202E), word joiner and invisible operators (U+2060–U+2064), and U+FEFF. **This step is what guarantees U+001E and U+001F cannot occur in any field**, which is what makes the separator in C9 unambiguous without escaping. |
| C5 | **Whitespace collapsing** | CRLF and CR → LF; then every run of whitespace (TAB, LF, U+0020, U+00A0, U+1680, U+2000–U+200A, U+2028, U+2029, U+202F, U+205F, U+3000) → a single U+0020; then trim leading and trailing whitespace. A compose box and a serialised body legitimately differ in trailing newlines and line endings; the digest is an identity, not a rendering. |
| C6 | **No case folding, no stemming, no punctuation stripping** | These merge submissions that are not the same submission and reduce the record's evidentiary value. The digest identifies content; it does not measure similarity. |
| C7 | **Attachment canonicalisation** | Per attachment: `name` = basename only, NFC, whitespace-collapsed, control-stripped, **not** case-folded, empty string when unknown; `media_type` = declared type lowercased with parameters (charset, boundary) removed, `application/octet-stream` when absent; `size_bytes` = exact octet count; `content_digest` = `sha256:` over the **raw attached octets** as the user selected them, or the literal `~` when the route is structurally unable to read the bytes (E3). The attachment records are then **sorted** by `(name, content_digest, size_bytes)`, because a multipart body's order and a page's file list order are not the same fact. |
| C8 | **Exclude non-content material** | Never hashed: `event_id`, `tenant_id`, `device_id`, `user_ref`, `tool_fingerprint`, `direction`, `kind`, `prompt_kind`, `occurred_at`, `received_at`, `monotonic_offset_ms`, `source`, `confidence`, `collection_mode`, `labels`, `classifier_version`, `policy_decision`, `content_excerpt`, HTTP method/path/query/framing/headers/cookies, multipart boundaries, JSON key order and escaping, compression, TLS record structure, retry counters and `batch_id`. Note the deliberate exclusion of the payload's byte size: escaping and framing make the same prompt 4096 bytes to one route and 4102 to another, so size is a *surrogate* input (§4.5) and never a Tier-T input. |

**C1's output is also the classifier's input.** The device hands the classifier the segment C1 selected,
not the whole captured body: the body additionally carries the client's system prompt, tool definitions and
re-sent history, and classifying those gave a plain question a label taken from a tool schema. Only when C1
cannot identify an authored boundary does the whole body go to the classifier, and that record is already
`confidence: degraded`.

**The device also records a `prompt_kind`.** Not every user-role message is a person's: clients send their
own requests (titling, summarisation, telemetry) that carry instructions in a user-role turn. The device
decides `user` | `client_generated` | `unknown` from request shape and C1's output, defaulting to `user`, and
the envelope carries it. It is metadata about the shape of the request, not part of the canonical form, so it
is excluded from the digest (C8). A `client_generated` submission is stored and auditable but is not indexed
for prompt-text search; see [01-collectors](01-collectors.md) §9.8.

**C9 — Digest input layout.** Fields are joined by U+001F INFORMATION SEPARATOR ONE, which C4
guarantees cannot appear inside any field. The input is UTF-8; no length prefixes are needed because no
field can contain the separator.

```
digest_input  = "sac-canon-1" ␟ "T" ␟ text ␟ att* ␟ "END"
att           = "A" ␟ name ␟ media_type ␟ decimal(size_bytes) ␟ content_digest
␟             = U+001F
text          = the C1–C6 output; present even when empty
content_digest= "sha256:" + 64 lowercase hex, or "~" when the bytes were unreadable
```

`content_digest` (envelope field) = `sha256:` + hex(SHA-256(UTF-8(digest_input))).

Example — one prompt, one attachment, computed identically by both routes:

```
sac-canon-1␟T␟Summarise the attached contract.␟A␟msa.pdf␟application/pdf␟184320␟sha256:9f2c…␟END
```

A proxy that parses the provider's JSON body reaches this by taking the last user-role turn and reading
the attachment by name and size; an extension reading the page reaches it by taking the compose box's
text and digesting the file object it read before the request was constructed (E3). Both then run the
same C3–C9 steps over the same two inputs.

### 4.3 The time bucket

| Property | Value |
|---|---|
| Width | **300 s (5 minutes)** |
| Function | `bucket_start = floor(occurred_at_utc / 300 s) × 300 s`, rendered as RFC3339 UTC |
| Clock | The **device's own wall clock**, uncorrected, as carried in `occurred_at` |
| **ASSUMPTION:** | The 300 s width is a chosen parameter, not a measured one; it is justified against both failure directions below and can only be changed with a canonicalisation version bump. |

**Why the device's wall clock and nothing else.** Three candidate clocks exist and only one works:

| Candidate | Why not |
|---|---|
| Server `received_at` | A retry, an outage flush or a two-day spool backlog delivers the *same* submission hours apart, so receive-time buckets systematically fail to collide in exactly the scenario R9 is about. The device must also mint `dedup_key` before it has any receive time. Server time is authoritative for **display** (brief §3.6) — the bucket is not a display fact. |
| `monotonic_offset_ms` | Its origin is arbitrary and per-process (schema), so two providers on one device cannot agree on a monotonic instant. It is retained for intra-device ordering, which is what it is for. |
| Device wall clock (`occurred_at`) | The only clock two processes on one device share. Both observers stamp at interception, milliseconds apart. The bucket is about **one device's own observation of one event**, which is precisely the frame this clock provides. |

Absolute device skew does not need correcting in the key: it shifts every event on that device equally,
so both routes shift together and still collide. Skew is measured and reported per device (C26) and is
never normalised into the key, because two routes would then have to agree on the correction —
reintroducing the disagreement the key exists to remove.

**Both failure directions.**

| Width | One submission seen by two routes, but split across a boundary | One user sending byte-identical content twice, merged into one |
|---|---|---|
| 60 s | ~3.3% of overlapping pairs at a 2 s inter-route gap; ~0.08% at 50 ms | negligible |
| **300 s** | **~0.7% at a 2 s gap; ~0.017% at 50 ms** | rare: identical canonical content, same user, same tool, same attachment set, inside five minutes |
| 3600 s | ~0.06% at a 2 s gap | material: "send it again" after an error is normal user behaviour within the hour |

**ASSUMPTION:** the inter-route gap is ≤50 ms typically and ≤2 s pathologically (an NTP step or a
scheduler stall between the two observations). No measurement exists yet; Q3's conformance test
produces it, and the reconciliation report (§4.5) measures the residual directly.

Two mitigations are designed to make the narrow-side risk bounded rather than
probabilistic-and-forgotten. A straddled pair is to be repaired by the daily reconciler: same tenant,
device, tool, `kind`, content digest and adjacent buckets with different routes is an identity-proving
match, so the later row is marked as superseded by the earlier one — nothing is deleted, the merge is
idempotent and re-runnable, and `mart` excludes superseded rows. The wide-side risk is bounded by the
bucket and is to be surfaced: a second observation of identical content from the *same* route inside
one bucket is indistinguishable from a duplicate, so it is flagged as a possible undercount on the
submission rather than silently merged (§4.4).

**As built:** `ingest.submission` has no supersession column and no possible-undercount flag, and the
reconciler is not implemented. A straddled pair therefore remains two submission rows, and a same-route
repeat inside one bucket folds into the existing row, raising `observation_count` and nothing else.

### 4.4 Route fidelity ranking

Fidelity is **what a route can prove about the submission**, not how much ground it covers. Coverage is
reported separately (C23, R11) and is not a fidelity property.

**Lower rank wins.** The rank is `ref.route_fidelity.fidelity_rank`, unique per route.

| Route | Brief §2 modes | Rank | Why this rank |
|---|---|---|---|
| `ext.page_context` | A, B, C | **10** | Reads the composition surface and the attachment **file objects** before the request is constructed. The only route that can digest attachment *contents* at all (E3), and the only one that sees the user-authored payload before any client-side serialisation. The canonical source: it sees what the user composed, not what the transport did with it. |
| `cli.shim` | E, G | **20** | Call-site capture from the managed shell environment (E9, E10): structured tool arguments rather than a parsed HTTP body, so no reconstruction is involved. A tool that ignores the environment is invisible, but that is a coverage fact, not a fidelity one. |
| `proxy.loopback` | F | **30** | Plaintext HTTP on loopback with nothing to decrypt and no framing to guess (E11), so the body is unambiguous. It observes an API call rather than a composition surface: client-assembled context and inlined attachment text are indistinguishable from typed text at this vantage point. Coverage is narrow. |
| `ext.web_request` | A, B, C | **40** | The exact serialised body the browser sent (E2). Accurate for the wire form, which may differ from the composed form in encoding and whitespace; attachments are filenames only (E3). Requires provider-specific body parsing. |
| `proxy.tls` | D, E, G, H | **50** | The same body class as loopback, but reached through interception, so the prompt is reconstructed from a provider-specific serialisation and its availability depends on trust-store and QUIC configuration (E6, E7). Authoritative for the runtimes no extension can reach. |
| `ext.dom` | A, B, C | **60** | The rendered compose surface, used where the request is not observable: attachments are chips (name and size), and browser-agent and canvas surfaces may render outside normal DOM structure (R5). Broad and cheap; the weakest content-capable route. |
| `proc.detect` | I | **70** | Establishes that a model ran and nothing more: the schema forbids `content_digest` on `model_detection`, and the row is stored with `yields_content = false`. It cannot participate in content-derived identity at all and appears here only to keep the ranking total. |

Ranks are stored in `ref.route_fidelity` (`source`, `fidelity_rank`, `yields_content`; master §5.3),
not compiled into services.

**The stored comparison is the route rank alone.** `ingest.submission.winning_fidelity` is the
`fidelity_rank` of `winning_source`, denormalised so the write path can compare without a join. The
material tier of §4.5 is not folded into it: tier separation is done by the keys instead. A record
without a content digest can only ever match a submission row that has no exact key (§4.5), so a
surrogate observation can never win a field on a row a content-reading route has already identified.
`kind` is part of both keys, so observations of different kinds never contend.

**As built:** `ingest-api` also carries a composite ordering (`tier_rank × 1000 + route_rank`, with
equal values broken by the smallest `event_id`) in `internal/dedup` as `Fidelity` and `Beats`. Nothing
in the request path uses it; the merge decision is the store's.

**The rule on collision.** Where two observations resolve to the same submission row:

1. The strictly lower `fidelity_rank` wins the contested fields — `content_digest`, `labels`,
   `classifier_version`, `confidence`, `collection_mode`, `size_bytes` — and becomes `winning_source`.
   Fields are replaced wholesale and never blended, because a blend of two observations would
   correspond to neither.
2. The logical record records **every** route that observed it, in `observed_routes`, and counts every
   observation in `observation_count`. Both survive in `ingest.observation`, which is append-only.
3. The count is one logical submission. It is neither inflated by the extra route nor reduced by
   dropping the losing observation — the loser's route set is the evidence that a blind spot on one
   route was covered by another, which is what a customer challenging coverage is asking about.
4. At equal rank — which, ranks being unique, means the same route again — the first-seen observation
   stays the winner.

**Winner selection does not depend on arrival order across routes.** The winning fields are those of
the lowest-ranked route that observed the submission; routes are a set union; the observation count is
a sum. That is what makes retries, out-of-order delivery and idempotent backfill free (C28). The fields
that are not contested — `policy_action`, `policy_rule_id`, `decided_locally` and `received_at` — are
set by the first observation to arrive and are not rewritten by a later one.

**A later-arriving lower-fidelity observation** changes no winning field. It appends its route to
`observed_routes` if it is not already there and increments `observation_count`. A genuine second
submission of identical content from the same route inside one bucket cannot be distinguished from a
duplicate, and is absorbed the same way (§4.3).

**A later-arriving higher-fidelity observation upgrades the record in place**: the contested fields are
replaced and `winning_source` and `winning_fidelity` move to the better route. A row that had no exact
key adopts the arriving observation's `dedup_key` together with its `content_digest`, and its
`merge_confidence` is promoted to `high` (§4.5). The submission keeps a **surrogate `submission_id`
minted at first insert and never changed**, so findings and analyst review state (`ops.finding_review`,
keyed by `(tenant_id, submission_id, rule_id)`) are not orphaned by an upgrade. `mart` is recomputed
from `ingest` over a lookback window (master §4.2 step 10), so an upgrade is absorbed by the next
aggregation run rather than requiring a repair.

### 4.5 The unreconcilable case

Some routes genuinely cannot produce canonical text, and one mode is forbidden from reading content at
all:

| Case | Why there is no canonical text | Source |
|---|---|---|
| Browser-agent canvas UIs | Content is rendered outside normal DOM structures; a text read is not obtainable reliably | R5; brief §2 mode C ("best effort on content") |
| Established WebSocket sessions | Only the handshake is observable; messages on the connection are not | E4 |
| **Mode M0** | Reading content is not permitted at all, so there is no digest to compute | brief §1.1 ("M0 requires no access to content at all") |
| Mode I, embedded on-device models | No prompt is reachable by any mechanism | brief §2 mode I |
| Attachments whose bytes a route cannot obtain | The network layer reports a filename, never bytes | E3 |

**The tension, resolved explicitly.** brief §4.1's envelope requires a normalised content digest for
dedup, while brief §1.1's M0 forbids reading content. M0 therefore cannot produce a content digest —
at all, ever, by construction. The contract already encodes the resolution (the schema forbids
`content_digest` at M0 and describes its `dedup_key` as weaker), and this document makes the
consequence explicit rather than letting it appear as a missing number:

> **At M0 the "normalised content digest" input to `dedup_key` is replaced by a payload-shape
> surrogate.** Dedup is correspondingly weaker, the record carries that fact, and the resulting
> uncertainty is reported as a bounded reconciliation delta — never as a silent merge and never as a
> missing event.

**The key ladder.** `dedup_key` is *always* present and always a `sha256:` value (the schema requires
it on every record), derived from the strongest material the mode and route permit:

```
dedup_input   = "sac-dedup-1" ␟ tenant_id ␟ device_id ␟ tool_fingerprint ␟ direction ␟ kind
                ␟ bucket_start ␟ tier ␟ material
tier          = "T" (content_digest) | "S" (surrogate) | "R" (rollup) | "D" (detection)
material      = content_digest                                      (Tier T)
              | decimal(size_bytes) ␟ decimal(attachment_count) ␟ names_digest   (Tier S)
              | window_start ␟ window_end                           (Tier R)
              | detection_basis                                     (Tier D)
names_digest  = sha256 of the sorted, canonicalised attachment names, or "~" if none known
```

For a `usage_rollup` the window **is** the bucket (`bucket_start = window_start`), because the record
already describes a period rather than an instant and the schema forbids the content and size fields
that would otherwise supply material.

Tenant, device and tool are in the key as well as the content: the same text sent by two people, or
from two devices, is two submissions (brief §4.1). The tier is **not** a wire field — the envelope is
frozen — so ingest recomputes it and stores it as `ingest.observation.dedup_tier`:

| Record | Recomputed tier |
|---|---|
| `kind = usage_rollup` | `R` |
| `kind = model_detection` | `D` |
| `kind = prompt`, `content_digest` present, all attachments readable or none present | `T`-A |
| `kind = prompt`, `content_digest` present, one or more attachments unreadable (E3) | `T`-B |
| `kind = prompt`, no `content_digest` (M0, or a route that could not read text) | `S` |

**The store-enforced merge keys.** `ingest.submission` carries two keys, and the store decides which one
an observation is filed under:

- **Exact key** — `dedup_key`, the device's value, kept only when `kind = prompt` and a `content_digest`
  is present (Tier T). Nullable on the submission.
- **Weak key** — `dedup_weak_key`, derived **on the server** by `ingest.weak_dedup_key()` so that two
  collectors written independently produce identical values. Not null on every submission:

```
dedup_weak_key = "sha256:" + hex(sha256(tenant | device | tool | kind | floor(occurred_at / 300 s) | size_bytes))
                 fields joined by "|"; size_bytes is 0 when the record carries none
```

It is the brief §4.1 formula with the digest term replaced by the payload size. `kind` is in it because
rollups and detections carry no size at all: without it a `usage_rollup` and a `model_detection` for the
same tool in the same bucket would collapse into one record, which would be a fabrication rather than a
merge.

Two partial unique indexes enforce the ladder, and the asymmetry is the point: `(tenant_id, dedup_key)`
is unique among rows that **have** an exact key, and `(tenant_id, dedup_weak_key)` is unique only among
rows that have **none**. The two cover disjoint sets of rows, so they can never disagree. Exact keys
never merge with each other, so two confident but different digests stay two rows; the weak key binds
only rows with no exact key, so it can never pull a confident row into a merge it does not belong in.

**Merge policy — what may collapse and what may not.**

| Match | Merge? | `merge_confidence` | Rationale |
|---|---|---|---|
| Exact ↔ exact, equal `dedup_key` | Yes | `high` | Identical canonical form in the same bucket: same text, same attachment material |
| Exact ↔ exact, different `dedup_key` (including Tier T-A ↔ Tier T-B) | **No** | `high` on each row | Two confident digests that differ stay two rows — canonicalisation divergence is a defect that must be visible rather than something the database papers over |
| Exact arriving on a weak-only row with the same weak key | Yes — the row is **adopted** and takes the exact key and its digest | promoted to `high` | This is what stops a submission seen at M0 by one route and with content by another from counting twice. Same device, tool, kind, bucket and size |
| Weak ↔ weak, same weak key (any routes) | Yes | `low` | Two routes that could not read content agree on device, tool, kind, bucket and size. The residual uncertainty stays flagged on the row |
| Weak arriving when only an exact row exists | **No** — a new weak-only row | `low` | The weak lookup matches only rows with no exact key. Both are stored, both are counted, and the pair is a reconciliation candidate |
| `usage_rollup` ↔ `usage_rollup`, same device, tool and bucket | Yes | `low` | A re-sent rollup folds into one logical row. The submission carries no `submission_count` or `bytes_total`; each rollup's own counts stay on its immutable observation row |
| `model_detection` ↔ `model_detection`, same device, tool and bucket | Yes | `low` | The fact recorded is "a model ran", which is per device, tool and bucket, not per invocation. `detection_basis` is not part of the weak key |

`merge_confidence` has two values, `high` and `low`: a row is `low` for exactly as long as it has no
exact key.

**As built:** the text ladder that would merge a Tier T-A and a Tier T-B observation of one submission
— a coarser key over the canonical prompt text alone — is not implemented. The store has no
prompt-digest column, so the two observations compute different `dedup_key` values (one carries
`sha256:…` for the attachment, the other `~`) and remain two submissions.

**Two invariants hold the line.**

- **I2a — never discarded.** Every accepted observation is a row in `ingest.observation`, append-only.
  No merge deletes, no path reports `duplicate` for an `event_id` that was not previously accepted, and
  an observation with non-reconcilable material is stored and counted even when it cannot be merged.
- **I2b — never silently merged.** A merge happens only through one of the two named keys above,
  derived from stated material. Where identity is not proven, the system stores both, reports both, and
  refuses to guess.

**The reconciliation report.** The scheduled `reconciler` emits one line per tenant per day, recorded in
`ops.reconciliation_run.checks`, and the dashboard shows it next to any count it affects:

```
reconciliation(tenant, day) =
  observations_total, submissions_total, merged_pairs,
  merged_low, non_reconcilable_observations, same_route_repeats,
  unexplained_delta, candidate_pairs[]
```

- `observations_total − submissions_total` must be fully explained by `merged_pairs`, `merged_low` and
  `same_route_repeats`.
- Anything left over appears as **"N observations with non-reconcilable digests"**, with a drill-down
  listing `event_id`, route, tier, device, tool and bucket for each — a Tier-S canvas-UI observation
  beside a Tier-T proxy observation of the same action, for example.
- A submission with an outstanding candidate pair is marked and its bucket is displayed with an
  explicit **±N awaiting reconciliation** band rather than a bare number.

This is the difference between a visible gap and a wrong number: the customer sees the count, the
routes behind it, the merges that produced it, and the small residue that could not be proven either
way — and can check the arithmetic.

---

## 5. The six device-facing APIs

Six endpoints and no more: each additional device-facing endpoint is another thing to authenticate,
version and keep compatible with clients on machines nobody controls (master §5.1). This count was five
until ADR 0020. DPoP is now a first-class production mode, and a DPoP device needs a token endpoint to
exchange a proof of possession for a short-lived, sender-constrained access token; that endpoint is
`POST /v1/token` (§5.6). The invariant the old count protected still holds — there is no unbounded,
bulk or device-initiated push path, and every endpoint is authenticated, versioned and enumerated here —
but it was never a claim that the number could not change. It was a claim that adding one is a
deliberate, reviewable act, which is why the sixth is argued rather than slipped in. Paths are
`/v1`-prefixed; the body carries `schema_version`, which is the *document* version, independent of the
API major version.

**Versioning rules, common to all six.** A server accepts a closed set of `schema_version` values it
advertises in the policy bundle, so a device never guesses. An unknown value is rejected with
`unsupported_schema_version` and the supported list in `detail` — an explicit, diagnosable failure
rather than a partial parse. Because the envelope schema sets `additionalProperties: false`, any new
field is a schema change: it is introduced as a new `schema_version` accepted alongside the old one for
the fleet's update window, and a required field can only arrive with `/v2`. Every response carries
`server_time` so the device can measure clock offset (C26).

**Common error envelope** (HTTP status + body): `401` bad/expired/revoked credential; `403` tenant
suspended or region mismatch; `404` unknown resource; `409` conflicting state; `413` oversize; `429`
rate limited with `retry_after_s`; `503` unavailable with `retry_after_s`. Bodies are
`{ "error": { "code": "...", "detail": {...}, "server_time": "..." } }`. Codes are the closed set in §7.

### 5.1 `POST /v1/enrol` — control-api

| Facet | Specification |
|---|---|
| Purpose | One-shot enrolment; issues the per-device credential and returns identity, region, the current policy ETag and the tenant's user-reference key (brief §4.2, C11) |
| Auth | Pluggable bootstrap (ADR 0020 decision 3), exactly one of: a short-lived, tenant-scoped, single-use **enrolment token** (the lab's, minted per device); a per-tenant, reusable **deployment key** (`sacdk_<tenant>.<secret>`) carried by the tenant package an MDM delivers to every device, since an MDM cannot hand each device its own token; or the **current** device credential on re-enrolment (a forwarded `x509` certificate, or a `dpop` access token plus a fresh proof). A deployment key is a shared secret, so a tenant whose devices Intune manages (`ops.tenant.device_verification = 'intune'`) also requires `attestation`, which `control-api` looks up in the customer's own Intune (below). The edge forwards, the origin authenticates, and the tenant comes from the token, the key or the credential — never the body |
| Request | `{ schema_version, enrolment_token? \| deployment_key?, mode, csr?, jwk?, device: { os, os_version, agent_version, hostname? \| hostname_hash?, managed_state?, mdm_id?, hardware_identity_hash }, attestation?: { intune_device_id?, entra_device_id?, serial_number? }, claimed_region? }` — `dpop` carries the public JWK and a proof of possession and no CSR. `x509` is one of two issuances ([ADR 0022](adr/0022-customer-issued-device-certificates-are-registered-not-signed.md)): **product-issued** carries a PKCS#10 CSR and no JWK, which `control-api` signs; **customer-issued** carries no CSR, and the device's certificate — issued by the customer's PKI/MDM (Intune Cloud PKI, ADCS, Jamf) — is presented on the transport and forwarded by the edge as `X-Client-Cert`, which `control-api` verifies against the tenant's `device_ca_pem` and **registers, never signs**. The tenant decides which applies (from the bootstrap credential); the request does not name it. The hostname is in clear or hashed per the tenant's device-identity setting (ADR 0021); no username and no directory identifier. `attestation` is read from the operating system, never configured, and an absent field is an absent fact |
| Response | `200` `{ device_id, tenant_id, region, reenrolled: bool, credential: { mode, cert_pem?, chain_pem?, not_after?, jwk? }, policy_etag, device_identity?, user_ref_key?, schema_version, server_time }` — the certificate fields are present in `x509` mode, the JWK in `dpop` mode. `user_ref_key` is the tenant's 32-byte user-reference key (base64url, unpadded), with which the device derives each person's pseudonymous `user_ref` the same way the directory side does ([03](03-data-platform.md) §3.3) |
| Errors | `400` schema violation or a body that does not match the declared mode; `401` bad bootstrap credential — for a deployment key, `deployment_key_invalid` or `deployment_key_revoked`; `403` `revoked_device` (a revoked device may **not** re-enrol into a fresh identity — revocation is not bypassable by re-imaging), tenant inactive, or `device_not_managed` when the tenant's Intune check refuses, with `detail.reason` naming which check failed; `409` the hardware identity already belongs to another tenant; `410` enrolment token or deployment key expired (`deployment_key_expired`); `429` `rate_limited` when one deployment key enrols faster than its limit; `401` `invalid_client_certificate` when a customer-issued tenant presents no certificate or one that does not chain to its device trust anchor (ADR 0022); `503` when the Intune check itself could not be made (retryable) |
| Idempotency | Store-enforced unique on `(tenant_id, hardware_identity_hash)`. Re-enrolment returns the **existing** `device_id` with `reenrolled: true` and HTTP 200, never a duplicate row (C11). Under the Intune check one Intune managed-device id is one product device, per tenant, so a re-imaged device returns the same `device_id` and a copied package cannot mint a second identity behind a real device's identifiers. **As built:** `control-api` implements `POST /v1/enrol` for both modes, and `ops.device.hardware_identity_hash` exists with the per-tenant partial unique index that makes re-enrolment idempotent. With no configured MDM id (a tenant package configures none; the lab profiles do), the agent seeds the hardware identity from the SMBIOS system UUID and serial, falling back to the serial and then the operating system's install id; before this, every device of a package-installed tenant hashed to the same identity. Two limits remain: the column is nullable, so a device that supplies no hardware identity gets no idempotency; and the §2.2 rotation overlap is not implemented — a rotation revokes the previous credential in the same transaction that inserts the new one |
| Versioning | `schema_version` admitted from the advertised set; the credential format is versioned by the CSR's signature algorithm, not by the path |
| **As built: the deployment key** | Minted by the admin's package download (Settings → Deployment, [05](05-platform-delivery.md) §6.1), one new key per download, stored only as `sha256:<hex>` in `ops.deployment_key` with its label, creator, optional expiry, `enrolment_count` and `last_used_at`; revocable from the same page. Each key is rate-limited in process memory (default 5 enrolments/s sustained, a burst of 300; a limit per replica, not fleet-wide). The Intune check calls Microsoft Graph `deviceManagement/managedDevices/{intune_device_id}` in the customer's Entra tenant with the vendor app's app-only token — `DeviceManagementManagedDevices.Read.All` is the only Graph application permission the product asks for — and requires the device to exist, be managed, match `serial_number` case-insensitively and match `entra_device_id` when both sides state one. It establishes that the identifiers name a real managed device of that customer, not that the caller is that device: the identifiers are not secrets. A refusal is audited (`device.enrol_refused`) so the admin can see a copied package being tried. The single-use enrolment token keeps working unchanged |
| **ASSUMPTION:** | The tenant package (service FQDN and deployment key beside a generic, code-only MSI) is delivered by MDM. The master doc assumes MDM delivery of trust and proxy configuration (D4); this reuses that channel rather than inventing one |

### 5.2 `GET /v1/policy` — control-api

| Facet | Specification |
|---|---|
| Purpose | Signed, versioned policy bundle: classifier version, effective collection mode per scope, retention class, spool bounds, per-collector feature state (brief §4.2) |
| Auth | Device credential (a forwarded certificate, or a `dpop` access token and proof, exactly as for `/v1/health`); tenant and device from the credential |
| Request | No body. `If-None-Match: "<bundle_version>"` |
| Response | `200` `{ bundle_version, signed_bundle: { key_id, algorithm, payload, signature }, not_after?, upgrade_required, schema_version, server_time }` with `ETag`; **`304`** with no body when unchanged (C10). `signed_bundle` is the stored envelope byte for byte |
| Errors | `401` revoked/expired; `403` tenant suspended; `404` no bundle exists for this tenant — the device falls to **M0**, never to "no policy means no restriction" (C10); `429`, `503` |
| Idempotency | Naturally idempotent; polling is free and expected. The server treats the ETag as the only version authority |
| Versioning | `bundle_version` is monotonic per tenant. `upgrade_required` is set when the device is below the bundle's `min_agent_version`; the device keeps its current bundle and reports `degraded` after the deadline |
| Note | Signature verification happens on the device. Ingest's obligation is narrower and absolute: **only signed bundles are ever served**, and a verification failure retains the previous bundle rather than producing an empty policy (C10, [01-collectors](01-collectors.md)) |
| **As built:** | `control-api` serves the endpoint (`internal/policyserve`) and is the writer of `ops.policy_bundle`. A tenant's bundle is composed from the database — the tenant's ceiling as the default collection mode, the tool catalogue's TLS hosts as the interception scope, the servable classifier release — signed with the vendor's Ed25519 policy key (`SAC_POLICY_SIGNING_KEY_FILE`; its public half is the trust anchor the generic MSI pins, not tenant data), and stored with its exact signed bytes in `ops.policy_bundle.signed_envelope`. A new version is minted only when that composition or the signing key changes, on the first poll after the change, so a device's ETag stays valid until something it would enforce changes; the version is monotonic per tenant. `404 no_policy_bundle` when no classifier release is servable and no bundle exists; `403 unknown_tenant` for an inactive tenant; `503` when no policy key is configured. `not_after` and `upgrade_required` are not set yet. The agent fetches the bundle after enrolment when no bundle file is configured, verifies it under the pinned key, caches the last verified bundle on disk so a restart enforces it before the network answers, and polls with `If-None-Match` |

### 5.3 `POST /v1/events` — ingest-api

| Facet | Specification |
|---|---|
| Purpose | Deliver a batch of 1–500 envelopes with a per-event outcome (brief §4.3, C12) |
| Auth | Device credential; tenant, device and region from the authenticated principal. A body `tenant_id` that disagrees is rejected, never honoured (§12) |
| Request | Below. `Content-Type: application/json`, `Content-Encoding: gzip` optional |
| Response | `200` with one result per event, in request order. Shape below |
| Errors | `400` the batch envelope itself is unparseable, `event_count` disagrees with `events`, or the count is outside 1–500; `401`/`403` per §5; **`413` oversize** (body or per-event cap); `429`; `503`. A batch that parses always returns `200`, even when every event inside it is rejected — per-event outcomes are the contract |
| Idempotency | Per event, enforced by the store on `(tenant_id, event_id)` (§6). A replay of an accepted event returns `duplicate`. A replay of the same `batch_id` returns `duplicate_batch` at batch level; the device then re-sends with a fresh `batch_id` and receives exact per-event outcomes — which is precisely the recovery for a gateway timeout after commit (§13) |
| Versioning | `schema_version` per envelope; the server validates against the named version and reports `supported` on rejection |
| **ASSUMPTION:** | Caps: request body ≤ 8 MiB compressed and ≤ 32 MiB decompressed (decompression-bomb guard), single envelope ≤ 256 KiB. An envelope is 1–2 KB in the sizing model (brief §3.1), so these are generous by two orders of magnitude |

```json
{ "schema_version": "1.0", "batch_id": "7f1c9a3e-...", "device_sent_at": "2026-10-02T14:03:11Z",
  "event_count": 3, "events": [ { "...": "deviceSubmission per contracts/event-envelope.schema.json" } ] }
```

```json
{ "schema_version": "1.0", "batch_id": "7f1c9a3e-...", "received_at": "2026-10-02T14:03:12.480Z",
  "server_time": "2026-10-02T14:03:12.480Z", "counts": { "accepted": 1, "duplicate": 1, "rejected": 1 },
  "results": [
    { "event_id": "…", "outcome": "accepted",  "submission_id": "…", "dedup_tier": "T", "won_fields": true },
    { "event_id": "…", "outcome": "duplicate", "submission_id": "…", "first_received_at": "2026-10-02T13:58:02Z" },
    { "event_id": "…", "outcome": "rejected",  "reason": "mode_violation",
      "detail": { "pointer": "/events/2/content_digest", "expected": "absent when collection_mode is m0",
                  "presence_map": ["schema_version", "event_id", "tenant_id", "device_id", "user_ref",
                                   "kind", "occurred_at", "monotonic_offset_ms", "source",
                                   "collection_mode", "dedup_key"] } } ] }
```

`received_at` is stamped once per request, by the gateway, and is identical for every event in a batch:
it is the batch's receive time. On a retry it is **not** re-stamped for events already accepted — the
stored row keeps its first-accepted value, because a retry is not a second receipt.

### 5.4 `POST /v1/health` — control-api

| Facet | Specification |
|---|---|
| Purpose | Keyed upsert of collector state (C23) — deliberately **not** an event stream (§9, D5) |
| Auth | Device credential; `device_id` and tenant from the certificate, never the body |
| Request | Per-collector state: `state`, `version`, `last_success_at`, permission state per required permission, spool depth and dropped count (C22), `policy_bundle_version`, `signature_ok`, `clock_offset_ms`, `credential_not_after`, `kill_switch_state` |
| Response | `200` `{ acked_at, server_time, next_report_after_s }` — cadence is server-driven so the fleet can be slowed without shipping code |
| Errors | `400`, `401`, `403`, `413`, `429` (health is throttled harder than events: one row per device, so there is no value in high frequency) |
| Idempotency | Upsert into `ops.collector_state` on `(tenant_id, device_id, collector)` — one row per device per collector — guarded so a stale report cannot overwrite a newer one: `WHERE excluded.last_report_at > existing.last_report_at`. Replays are free and harmless |
| Versioning | `schema_version` admitted from the advertised set; the per-collector array is open within a version so a new collector can report without a schema change — a new collector is *additive*, while a new field on the envelope is not (§5 versioning rules) |
| **ASSUMPTION:** | Report interval 15 min per device; minimum accepted interval 60 s. D5's arithmetic (hourly health across 5,000 devices is ~44M rows/year, ten times the prompt events) holds a fortiori at any interval, because the channel is an upsert and stores one row per device per collector |

### 5.5 `POST /v1/content/grant` — control-api, then `PUT` to blob storage

| Facet | Specification |
|---|---|
| Purpose | Ask the backend to decide whether one event's content may be uploaded, and receive the means to do it (C14) |
| Auth | Device credential. The `event_id` must be an observation of **that** device and tenant, otherwise `404 unknown_event` — a device cannot request a grant for someone else's event |
| Request | `{ schema_version, event_id, collection_mode, content_digest, size_bytes, attachment_count, raw_size_bytes, policy_rule_id }` — no case reference, no tenant, no device id |
| Response | `200` `{ grant_id, state: "granted" \| "denied", reason?, expires_at, upload: { method: "PUT", url, headers }, key: { object_key_b64, wrapped_key_b64, key_id, alg: "A256GCM" }, max_bytes }` |
| Errors | `400` schema; `401`/`403`; `404` unknown event; `409` grant already consumed for this event; `410` previous grant expired (a new request is required); `413` declared size above the tenant budget cap; **`422` mode violation** — a device holding an M0/M1 event has no content to upload and the request is refused, not silently denied; `429`; `503` |
| Idempotency | Unique on `(tenant_id, event_id)` for live grants: a repeat request while a grant is live returns the same `grant_id` and the same upload URL. A grant is **single-use** — one object, one write — so a second upload attempt is refused by the storage layer and by the finaliser (§10) |
| Versioning | As common rules. The upload URL is opaque and must be used verbatim; its shape is not part of the contract |
| Denial is not an error | A denial is a successful decision about a well-formed request and returns `200` with `state: "denied"`. Returning 4xx would inflate device error rates, confuse retry logic, and hide the one number the operator most needs to see: the denial reason mix |
| **As built:** | `control-api` serves the endpoint (`internal/content`, wire types in `endpoint/protocol/content.go`). It decides `mode_not_permitted`, `retention_expired` and `over_budget`; **`not_policy_relevant` is never produced**, because the tenant retention criteria it is decided on have no stored form, so every M3 event is treated as relevant. `410` is not returned: a request after an expired grant is decided afresh. The upload credential is a URL signed with a key shared with the storage layer (one object, one grant, an expiry of at most 15 minutes), not a storage user-delegation SAS; the object key and its wrapped form come from `content-vault`, and the response's `upload.headers` carry the wrapped key so the finaliser can record it. Disabled unless a vault URL and the signing key are configured |

### 5.6 `POST /v1/token` — control-api

The DPoP bootstrap endpoint ADR 0020 adds. It is device-facing, and it is the sixth endpoint; §5's
preamble states why the count changed.

| Facet | Specification |
|---|---|
| Purpose | Exchange a proof of possession for a short-lived, sender-constrained DPoP access token (RFC 7523 assertion plus RFC 9449 DPoP) |
| Auth | The device's **registered key**: a compact ES256 JWS `assertion` whose `sub` is `device_id` and whose `tenant_id` names the tenant, plus a `DPoP` header proof bound to `htm`/`htu` with a `jti`. Both are verified against `ops.device_credential.public_key_jwk`, and the proof key must equal the registered key. No enrolment token is accepted here |
| Request | `{ grant_type, assertion, device_id? }` with the `DPoP` header |
| Response | `200` `{ access_token, token_type: "DPoP", expires_in, server_time }`. `token_type` is always `DPoP`; there is no bearer fallback, because a token replayable from any host would break ADR 0005's sender-constraint |
| Errors | `400` malformed assertion or proof; `401` bad/expired/revoked credential, a proof key that does not match the registered key, or a replayed `jti`; `403` tenant suspended or region mismatch; `429`; `503` |
| Idempotency | Not idempotent by design: each call mints a token with its own `jti`, but the assertion and the proof are single-use within their validity windows, so a replay is refused rather than reissuing |
| Versioning | `schema_version` admitted from the advertised set, as the other endpoints; the token format is versioned by the JWS `typ` (`at+jwt`) and the claim set |
| **ASSUMPTION:** | Access-token lifetime 900 s (15 min) and proof clock tolerance ±30 s are chosen, not measured: short enough that a leaked token is bounded, long enough to cover a batch drain. The `jti` replay window is 5 min |
| **As built:** | `control-api` serves the endpoint, issuing `ES256` / `at+jwt` tokens bound to `cnf.jkt`; `ingest-api` verifies them and persists the proof `jti` in `ops.dpop_replay`. Two limits remain: the token endpoint verifies the proof `jti` is present but does not persist it, and `ops.dpop_replay` is granted to `sac_ingest` but not yet to `sac_control` |

---

## 6. The write path and idempotency

Four tables in `ingest`. Three are on this write path, and one of those carries no content; the fourth,
`ingest.search_text`, is the content search index, written by `content-vault` and specified in
[03-data-platform](03-data-platform.md) §13:

| Table | Key | Role |
|---|---|---|
| `ingest.observation` | `(tenant_id, event_id)` | One row per **observation**, append-only. Two routes observing one submission produce two rows. This is the idempotency surface |
| `ingest.submission` | `(tenant_id, submission_id)`, plus two store-enforced partial unique indexes: `(tenant_id, dedup_key)` where an exact key is present, `(tenant_id, dedup_weak_key)` where it is not | One row per **logical submission**, folded with the fidelity tie-break, carrying `observed_routes`. `mart` aggregates are derived from this table, so a count is never inflated by overlapping routes (R9) |
| `ingest.rejected` | `(tenant_id, rejected_id)` with TTL | Quarantine for validation failures: rejection detail, field-presence map, content-stripped envelope (§7) |

**Transaction boundary.** One transaction per batch. Validation (§7) is performed entirely in memory
before the transaction opens, so the transaction contains only writes. The service sets the session
tenant (`app.tenant_id`, transaction-local) and then:

1. Credential status re-checked inside the transaction (§2.3) — a revocation between admission and
   commit rejects the whole batch with nothing written.
2. One call to `ingest.record_event(envelope jsonb, received_at timestamptz)` per accepted event, in
   request order. **The write is not implemented in the service.** The function owns idempotency, the
   dedup decision, the fidelity tie-break and the retention date, so the transport layer cannot
   implement the tie-break differently from the storage layer
   ([ADR 0001](adr/0001-one-validating-write-path-collectors-hold-no-database-credential.md),
   [03-data-platform](03-data-platform.md) §4). Inside it:
   - `INSERT INTO ingest.observation … ON CONFLICT (tenant_id, event_id) DO NOTHING`. A row count of
     zero means this `event_id` was already accepted, and the outcome is `duplicate`. This is brief
     §4.3's "idempotency is per event key and enforced by the store": it is a unique constraint, not a
     prior `SELECT`.
   - The submission is resolved in order: an existing row with this observation's exact `dedup_key`;
     failing that, an existing row with the same `dedup_weak_key` and **no** exact key, which is
     adopted; failing that, a new row with a fresh `submission_id`.
   - On a match the row is updated unconditionally for route recording — `observed_routes` gains the
     route, `observation_count` increments, the occurrence window widens and `expires_at` only ever
     extends — and the contested fields are replaced only when the arriving route's `fidelity_rank` is
     strictly lower than `winning_fidelity` (§4.4). A lower-fidelity observation that loses the
     tie-break is therefore still recorded as a route that saw the submission.
   - It returns `inserted`, `merged` or `duplicate` with the `submission_id`.
3. The service reads back what the store decided rather than forming a second opinion: the first
   `received_at` for a duplicate, and `winning_source` for a merge, which is what `won_fields` reports.
4. `INSERT INTO ingest.rejected …` for each rejected event that has a quarantine code (§7), in the same
   transaction.

A write error aborts the batch and is retryable; a validation error is a per-event outcome and never
aborts the batch. The device therefore never observes a partial commit: either the batch committed and
the response describes every event, or nothing committed and a retry is free.

**What the response reports.** Per event: `accepted`, `duplicate` (with the first `received_at`) or
`rejected` (with `reason` and `detail`); for accepted events, the `submission_id` it merged into, the
`dedup_tier` the server recomputed, and whether this observation won the contested fields. Plus batch
counts. Duplicates are **counted and reported**, never silently dropped and never double-counted
(brief §4.3).

**Idempotency is enforced by the store, not by a check in application code** (brief §4.3, C12). No
service reads-then-writes to decide whether an event is new: the unique constraint on
`(tenant_id, event_id)` decides receipt, and the two partial unique indexes on the submission bound
merging. Those two cannot disagree, because they cover disjoint sets of rows (§4.5). The only
check-then-act in the design
is the `duplicate_batch` guard, and a race there is benign — both racers fall through to the event-key
constraint and both events report as duplicates, which is the correct answer anyway.

**Ingest latency does not depend on aggregation latency** (C13). This transaction writes `ingest.*` and
nothing else; `mart` is recomputed from the immutable table by the scheduled aggregator over a lookback
window (master §4.2 step 10), so a slow or failed aggregation run cannot slow a device. Event-level
reads see the row as soon as the commit lands — which is how the "<60 s visible" target (brief §8) is
met with the aggregate path off the critical path entirely.

---

## 7. Validation and rejection

Validation runs against the versioned schema named in `schema_version`, in the ingest service, before
the write transaction (§6). The reason codes are a **closed set**; a new code is a contract change.

| Code | Meaning | Retryable by the device? |
|---|---|---|
| `schema_violation` | The envelope fails the named schema. `detail.pointer` is the JSON Pointer, `detail.expected` the violated constraint | No — an agent defect; fix and redeploy |
| `unsupported_schema_version` | `schema_version` outside the server's advertised set. `detail.supported` lists the accepted values | No — the device must be updated |
| `unknown_kind` | `kind` outside the closed registry `prompt · usage_rollup · model_detection`. **This is the mechanism that makes R7 structural**: a collector defect cannot start shipping raw process telemetry, because no kind exists for it (D8) | No |
| `unknown_tenant` | The authenticated principal resolves to a tenant that is unknown or inactive | No |
| `tenant_mismatch` | Body `tenant_id` disagrees with the authenticated principal | No — always a defect or an attack (§12) |
| `revoked_device` | Credential revoked. Rejects the whole batch, not one event (§2.3) | No |
| `region_mismatch` | The receiving deployment is not the tenant's pinned region | No — fails closed (§12) |
| `mode_violation` | A content-derived field on a record whose mode forbids it: `content_digest`, `labels`, `classifier_version` or `confidence` on an M0 prompt; `content_excerpt` at M0 or M3; `labels` missing at M1+ | No — evidence of a collector defect, and on M0 evidence that the device read content it was not permitted to read, which is why it is rejected rather than ignored |
| `duplicate_batch` | `batch_id` already seen from this device within the replay window | Yes, with a **fresh** `batch_id` |
| `oversize` | Batch outside 1–500 events, request body above the cap, or a single envelope above the cap | No |

**§4.3's diagnosability requirement without storing content.** brief §4.3 requires "per-reason rejection
detail, so an agent defect is diagnosable from the server side without access to the device". That is
`ingest.rejected`, and beside the device, the receive time and the expiry it holds exactly three things:

| Column | Content | Why it is safe |
|---|---|---|
| `reason_code` + `detail` | Reason code, JSON Pointer, violated constraint, expected/actual **shape** only (type, pattern, enum) | Never echoes the offending value, which could be content |
| `field_presence` | The sorted list of field **names** present on the rejected envelope, plus the ones required and missing | Field names are contract vocabulary, not content |
| `envelope_redacted` | The envelope with `content_excerpt`, `content_digest` and `attachments` removed entirely. Timestamps, identifiers and the declared mode are kept | Identifiers and dimensions only: no prompt text, no attachment bytes, no filename and no digest. Two check constraints refuse a row whose redacted envelope carries any of those keys, so the rule does not depend on the caller |

`reason_code` carries the wire vocabulary above with two exceptions and three additions. `tenant_mismatch`
is never quarantined, because a cross-tenant body has no honest tenant to file the row under, and
`duplicate_batch` is batch-level and has no per-event envelope. Three storage-only codes have no wire
equivalent: `malformed_json`, `dedup_key_mismatch` and `internal_error`.

Consequences, stated so they are not discovered later: an operator can see *which* field failed, on
*which* device, agent version and collector, and can reproduce the defect from a synthetic record —
without the server ever holding the content that caused it. `content_excerpt` is never stored here even
at M2, because the excerpt is content and the quarantine exists to diagnose a defect, not to keep a copy.

**ASSUMPTION:** `ingest.rejected` TTL is 14 days and is not suspended by holds (a hold protects
collected data, and this table holds no collected content). The value is the `quarantine` row of
`ref.retention_class` — a policy setting read at write time, not a constant in the service, which falls
back to 30 days only if that row is missing. It is meant to be long enough to diagnose a collector
defect and short enough not to accumulate. Reads of `ingest.rejected` resolve to a subject and therefore write an audit entry as they
are served (C30).

---

## 8. Batching, backpressure and retry

| Property | Rule | Source |
|---|---|---|
| Batch size | **1–500 events per request.** The batch is formed by the spool drain, not by the collector, so ordering, pacing and backpressure have one owner | brief §4.3 |
| Ordering | Observations are sent oldest-first. The device does not reorder to group by tool or route: a batch is a slice of the spool | — |
| Retry | Exponential backoff with **full jitter**, base 1 s, factor 2, cap 300 s, unbounded attempts while the data is spooled | brief §4.3 "retries must be free"; §7 requires the counter, not the attempt count |
| **ASSUMPTION:** | The backoff parameters are chosen, not measured; they exist to spread a 5,000-device flush, not to satisfy a measured service limit | |
| Retryable vs terminal | `429`, `503`, timeouts and connection failures are retryable. Every reason code in §7 is terminal **for that event** except `duplicate_batch`. A terminal rejection is recorded locally in the spool's rejection counters and removed from the head, so one poison batch cannot block the queue behind it | I3; brief §7 |
| Poison payloads | A batch that fails `schema_violation` or `oversize` repeatedly is dropped after local recording, and the device reports a rejection count in its next health report. The server-side copy of what failed is `ingest.rejected` (§7), so dropping locally loses nothing an operator needs | brief §7 |
| Multi-day outage | The spool fills to its cap; on overflow the **oldest** entries are dropped, `dropped_total` is incremented, and the counter is reported in the next health report. An undercount is visible to the operator, never silent | C22; brief §7 |
| **ASSUMPTION:** | Default spool cap 250 MB per device, configurable per tenant through the policy bundle (brief §7 requires a configurable cap and states no value). At 1–2 KB per envelope, that is ~10⁵ events, far beyond any plausible outage backlog, so the cap is a disk-safety bound rather than a routine limit | |
| Reconnect after an outage | On the first success after a failure the device waits a random 0–300 s and then paces batches with jitter, so 5,000 devices do not flush in the same second. The server is sized for the resulting burst — master §1.4's worst case of 50–500 events/s is 2 requests/s at 500 events per batch | master §1.4 |
| Batch-level pacing | At most one in-flight `/v1/events` request per device. Concurrency multiplies the burst and buys nothing at 0.14 events/s mean | master §1.4 |
| Aggregation decoupling | Ingest writes only `ingest.*`; aggregation is asynchronous (§6). A device's round trip is never gated on a rollup | C13 |
| Clock reconciliation | Every response carries `server_time`. The device computes `clock_offset_ms` and reports it in health (§9). The event-stream difference `received_at − occurred_at` is reported separately as **delivery lag**, because for a spooled event it measures the backlog, not the clock. The two are never conflated and never normalised away | C26; brief §7 |
| **ASSUMPTION:** | Clock-skew flag threshold is ±300 s, one bucket width. It is a reporting threshold, not a correctness one: the bucket is relative to the device's own clock (§4.3), so a skewed device still deduplicates correctly and is flagged rather than rejected. A skew large enough to invalidate the client certificate fails the TLS handshake first, which is a time-sync runbook, not a re-enrolment | |

---

## 9. The health channel

**Why an upsert and not an event stream.** D5 records the arithmetic and this document applies it:
health emitted as events at hourly cadence across 5,000 devices is ~44M rows/year — ten times the
prompt events the product exists to collect, and exactly the failure brief §3.1 warns about ("if
per-process telemetry is emitted raw … it will dominate the entire system"). Health is also
*current-state* information: nobody asks what a device's spool depth was at 03:00 last Tuesday, and
answering it is not worth an unbounded data category. So `POST /v1/health` upserts one row per device
per collector into `ops.collector_state`, keyed `(tenant_id, device_id, collector)` (C23,
[ADR 0011](adr/0011-collector-health-is-a-keyed-operational-channel-not-an-event-stream.md)), and only
a daily per-device rollup enters `mart`, as `mart.agg_device_period`.

**Fields reported.** Device: `reported_at`, `agent_version`, `clock_offset_ms`, `credential_not_after`,
`policy_bundle_version`, `signature_ok`, `kill_switch_state`, spool `depth_events`, `spool_bytes`,
`oldest_spooled_at`, `dropped_total`, `rejected_total`. Per collector: `name`, `version`, `state`
(`healthy · degraded · absent · tampered`), `last_success_at`, permission state per required permission
(granted/denied/not-applicable), and any named coverage gap (E8's "detect the failure, exclude the
process, and record that coverage was not achieved").

The per-collector fields are columns of `ops.collector_state`: `state`, `version`, `permissions`,
`last_success_at`, `last_report_at`, `spool_depth`, `spool_capacity`, `spool_dropped_total` and
`error_code`. The device-level fields have no dedicated column; the row's `detail` JSON document is the
only place the schema can carry them.

**Two facts, never merged.** brief §3.2 forbids merging `healthy`/`degraded`/`absent`/`tampered`, and
there are two independent ways a device can be unhealthy: it *said so*, or it *stopped talking*. Storing
one merged enum would destroy the distinction, so the two facts live in two places:

| Where | Values | Populated by |
|---|---|---|
| `ops.collector_state.state` | `healthy · degraded · absent · tampered` | The device's own report, per collector (C24: tamper is **reported**, never inferred from absent events) |
| `mart.v_device_liveness.liveness` | `revoked · never_reported · stale · reporting` | Derived at read time from `ops.device`: `revoked_at`, then `last_seen_at` compared to now |

**How a device that has stopped reporting becomes a record rather than an absence.** A device row exists
from enrolment, so silence is always a row that has gone quiet, never a missing row. The liveness view
reports a device that has never been heard from as `never_reported` and one whose `last_seen_at` is
older than **24 h** as `stale`, while the last reported `ops.collector_state` rows stay intact and
marked as of their `last_report_at`. The dashboard's answer to brief §3.6 question 7 — "the state of
every device's collection, including devices not reporting" — is then a count over rows, not an
inference from an empty event list. Two properties follow from keeping the facts apart: a revoked
device reads `revoked` and is never rewritten to `stale` (§2.3), and a device whose last report was
`tampered` keeps `state = tampered` on its collector row while its liveness moves to `stale`, so the
operator sees "it reported tampering, then stopped" rather than losing one of the two facts.

**ASSUMPTION:** stale at 24 h. The threshold is chosen to be visible within one working day without
alarming on a laptop that is simply shut overnight; it is a server-side parameter, held in the view, and
adjustable without a device release.

**As built.** `POST /v1/health` is served by `control-api` (`internal/health`, `internal/httpapi`): it
authenticates the device credential, validates the collector vocabulary against `ref.collector`, and
upserts `ops.collector_state` keyed `(tenant, device, collector)` in one transaction with the device's
`last_seen_at`. The stale-report guard is the `ON CONFLICT ... DO UPDATE ... WHERE
last_report_at < EXCLUDED.last_report_at`, so a retry or an out-of-order replay is a no-op rather than a
regression. The device-level fields the schema has no column for (`agent_version`, `clock_offset_ms`,
`policy_bundle_version`, `signature_ok`, `kill_switch_state`, `credential_not_after`, the counters and
the spool bytes) travel in the row's `detail` jsonb; `state`, `version`, `permissions`,
`last_success_at`, `spool_depth`, `spool_capacity`, `spool_dropped_total` and `error_code` are columns.

The device reports collector names, not route names, and **maps each route to its `ref.collector`
code** in the agent before sending (`proxy.tls` → `egress_proxy`, `proxy.loopback` →
`loopback_broker`, `proc.detect` → `process_detector`, the browser routes → `capture_extension`); a
name outside that vocabulary is refused, so §4.3's "the collector name must come from `ref.collector`"
holds on the wire. The classifier host is a component rather than a collection route and reports its
own row. The heartbeat is sent on the agent's existing health-channel interval even when the spool is
empty, which is the point of the channel: an idle device reports rather than merely stopping.

The batch path is also a device-activity signal. `ingest-api` stamps `ops.device.last_seen_at` (only
forward, never backwards) in the same transaction that accepts a batch, so a streaming device is
`reporting` without waiting for a health report. The two writers cannot disagree about liveness: the
stamp is monotonic in both.

---

## 10. Grant issuance and content upload

### 10.1 Decision inputs

`control-api` decides on: the **effective collection mode** for the event's scope (C1, C3); the
**per-tenant content budget** — consumed bytes against the tenant's ceiling and the retention class the
object would occupy (the only unbounded cost in the system, brief §3.1); the **retention class** for
that data class and mode; and, when present, a **case reference**, which raises the object's retention
class and pins it to a hold-eligible scope. The device supplies none of these and cannot influence any
of them; it supplies the event identity, the mode it believes applies, the digest, the sizes, and the
policy rule that matched. All four inputs are server-side state read in the tenant's row-level-security
session.

### 10.2 The four denial reasons

| Reason | Returned when | Device behaviour |
|---|---|---|
| `mode_not_permitted` | The event's scope is M0 or M1, where content is not kept at all | Content is not uploaded; local handling per policy |
| `not_policy_relevant` | The match does not meet the tenant's retention criteria (low severity, allowlisted class, sanctioned tool with logging-only policy) | Same |
| `over_budget` | The tenant's content budget or the object's retention class would be exceeded | Same; the budget consumption is visible to the operator, so a denial is explained by a number, not by silence |
| `retention_expired` | The event is older than the retention class permits for its content, so an upload would create data the system must immediately delete | Same |

Denials are terminal for that event and are counted by reason. A device that keeps re-requesting a grant
for a denied event is rate limited, and repeated requests for the same event are refused by the live-grant
uniqueness rule (§5.5) rather than generating decisions in a loop.

### 10.3 The upload credential, and why it is scoped the way it is

On `granted`, `control-api` returns a **pre-authorised write credential for exactly one object**:
one blob path, one write, no read, no list, no delete, TTL ≤ 15 minutes, minted through a storage
user-delegation credential rather than a storage account key.

| Property | Why |
|---|---|
| One object, one path | Blast radius of a leaked grant is one object, and the object is already referenced by the event it belongs to |
| Write-only | The device has no reason to read back what it just encrypted; read capability would let a stolen grant exfiltrate another object's ciphertext |
| No list, no delete | Enumeration is the capability that turns one leaked credential into a corpus |
| ≤15 min, single use | A grant that outlives its decision is an upload path that exists without a live decision |
| **ASSUMPTION:** | 15 minutes is a chosen TTL. It must exceed the device's encrypt-and-upload time for the largest permitted object (50–500 GB/year is per tenant, not per object) and be short enough that an abandoned grant closes quickly |

### 10.4 Key material

Per object: a random 256-bit AES-GCM key, minted and wrapped by `content-vault` for that grant
([ADR 0006](adr/0006-content-is-ciphertext-under-per-object-keys-wrapped-by-a-per-tenant-key.md)).
`control-api` makes the decision and relays the vault's answer; it mints no key, holds no unwrap right
and has no grant on `ops.content_object`. The key is returned to the device in the plaintext
`key.object_key_b64` field of the grant response and stored server-side **only** in its wrapped form, in
`ops.content_object.wrapped_dek`, written when the finaliser records the verified object. The object key is wrapped by the per-tenant key (C15), and where the customer supplies
that key it lives in Key Vault / Managed HSM under their control: destroying it destroys the content,
which is how tenant offboarding and customer-held-key mode shred content (D1). The plaintext object key
in the response is not a disclosure to the device — the device already holds the plaintext it is about
to encrypt — and the response is `Cache-Control: no-store`, delivered only over the authenticated
mTLS channel, to a device that has already been granted the object.

The upload declares `raw_digest` (SHA-256 of the exact bytes uploaded) and `grant_id` in the object's
metadata. Both are verified by the finaliser before the object is promoted: a mismatch — same grant,
different bytes — voids the grant rather than storing an object nobody can identify. `raw_digest` is
distinct from the envelope's `content_digest`: the former proves the artefact, the latter identifies the
submission, and retrieval returns the former so an analyst can verify the bytes they were granted.

### 10.5 Why per-event and single-use, and the structural argument

A grant is issued for one `event_id`, decided on server-side state, consumed by one object. There is no
endpoint that accepts content, no bulk grant, no tenant-wide upload credential, and no device-wide
storage credential anywhere in the design.

Therefore brief §1.1's requirement that it "must not be possible to configure the system into an
'upload everything' state" **holds by construction rather than by a configuration check**. An
"upload everything" state would require a decision granting every event, and there is no path that
produces one: the only way content reaches storage is a blob path minted by a decision about a specific
event, evaluated against mode, budget and retention. A configuration mistake can deny too much; it
cannot permit an upload path that does not exist. The backstop is the finaliser, which deletes staged
objects with no live grant or a failing digest check — so even a leaked upload URL does not deposit an
object the system will keep.

---

## 11. Content retrieval

The analyst path is reached through `query-api` (master §5.1) and is specified here only at the
transport and authorisation level; the dashboard's shapes are in
[04-dashboard-and-query](04-dashboard-and-query.md). The retrieval endpoint itself is served by
`content-vault`, on its internal-only ingress.

**As built:** `content-vault` serves `POST /v1/content/retrieval`, which authorises one read and mints
a single-use retrieval URL, and `GET /v1/content/retrieval/{tenant}/{grant}`, which serves the content
that URL names. `query-api` forwards the analyst's side of it (`src/http/content.js`):
`POST /v1/content/retrieval` relays the minted URL and no content byte, and `POST /v1/content-search`
forwards a prompt-text search. It decides nothing; the four-eyes rule, the single-use grant, the search
tier and the audit rows are the vault's. The dashboard's Explore page is the surface for both, and its
web tier forwards the minted URL straight to the vault, so content never transits `query-api`. Two
things differ from the table below. The vault reads the stored ciphertext itself from blob storage,
presenting a storage credential: a managed-identity access token in a deployment, a shared bearer
against the lab's stand-in, which refuses a read without one. And the retrieval request is
authenticated by the signed-in person's product access token, which `query-api` forwards and the vault
verifies itself, refusing an `X-Sac-*` header that disagrees with it (`403 principal_mismatch`); the
development principal header is honoured only when no token issuer is configured, in the lab. The URL
itself is the capability, and the grant it names is single-use and short-lived. The second approver is a name
recorded on the request when the caller gives one — the vault refuses an approver who is the requester,
but nothing requires a second person to approve.

| Step | Requirement | Source |
|---|---|---|
| Request | `POST /v1/content/retrieval` with `event_id`, `case_reference`, `justification`, and `second_approver`, recorded as a single-use row in `ops.retrieval_grant` | C16; brief §4.4 |
| Authentication | The customer's identity provider, through `control-api`'s sign-in, which mints the product access token the vault verifies; tenant from the token, never the body; forced row-level security | master §4.3; [06](06-security-and-threat-model.md) §4.1 |
| Four eyes | A **second approver distinct from the requester** must be recorded on the request. Q9 leaves open whether that is a customer-side or vendor-side role and for which tenants; the field is required either way so the API does not change when Q9 closes | C16; Q9 |
| **Audit before serve** | The audit entry — actor, time, case reference, second approver, event, object, outcome — is written and **committed before any content byte is read**. If the fetch then fails, the entry stands. The asymmetry is deliberate: recording an access that returned nothing is safe; serving content with no record is not | C16; C30; brief §3.6 |
| Serving | `content-vault` unwraps the object key, and the response carries a short-lived, single-use retrieval URL plus `raw_digest` and `expires_at`. Content does not transit `query-api`'s response body | master §4.1 |
| `no_longer_available` | An expired, erased or shredded record returns **`200` with an explicit result**, never `404` and never an empty body: `{ state: "no_longer_available", reason: "retention_expired" \| "erasure" \| "hold_released" \| "tenant_offboarded" \| "key_unavailable", receipt_ref }`, with the erasure or retention receipt linked. `key_unavailable` is the Key Vault outage case: metadata and dashboards keep working and the outage is alerted separately from data loss | C17; master §5.4, §4.4; brief §4.4 |

**Why `content-vault` is not reachable from devices or browsers** (D7): it is the only component that
can unwrap content keys, so it has internal ingress only and no user-facing endpoint. Devices reach the
content path exclusively through `control-api`'s grant decision and then write ciphertext straight to
blob storage; browsers reach content exclusively through `query-api`, which calls the vault over the
internal network under its own service identity. A device credential is not accepted by `query-api`
(different hostname, different auth), and a device cannot exchange its credential for a vault call. The
split follows "who can decrypt", not "which nouns exist" — which is why there is a separate service at
all.

---

## 12. Tenant and region enforcement at the edge

| Control | Rule |
|---|---|
| Tenant resolution | From the **authenticated principal** — for devices the credential's tenant (the certificate's tenant attribute in `x509` mode, the signed token's `tenant_id` claim in `dpop`), and for analysts the product access token's `sac_tenant`, which `control-api` sets from its own mapping of the customer's identity provider (Entra `tid` or exact OIDC issuer), never from a claim the provider chooses. Never from the request body. A body `tenant_id` that disagrees is rejected `tenant_mismatch`, not reconciled |
| Session tenant | `ingest-api` and `control-api` set the tenant on the database session from the principal before any statement; row-level security is forced and every application role is a non-owner without `BYPASSRLS`, so a session with no tenant reads zero rows rather than all rows (C32; master §4.3) |
| Region | Pinned per tenant and **enforced at ingest, failing closed**. Regional service FQDNs make the mapping mechanical: a device pointed at the wrong region's endpoint is rejected `region_mismatch` rather than written cross-region. A device's `claimed_region` is advisory and ignored for enforcement | **master Q1 (ASSUMPTION):** the brief does not state residency requirements; the master document assumes enterprise buyers in this segment will require in-region storage and pins region per tenant |
| Edge filtering | Application Gateway `WAF_v2` + WAF: managed rule sets, request size limits aligned with §5.3's caps, per-device and per-tenant rate limits, and `x509` / `dpop` admission with authoritative validation at the origin (§2.1). Strict mode additionally validates `x509` chains at the edge, but never as the authority |
| Rate limits | Per device: events as a request-rate cap with a burst allowance sized to a post-outage flush; health at a hard minimum interval (§5.4); grants at a low per-device rate, because a device that needs many grants in a minute is a defect or an attack. Per tenant: a ceiling that bounds one compromised tenant's blast on shared infrastructure |
| **ASSUMPTION:** | Concrete limit values are operational parameters set with the first design-partner deployment. They are not measured, and they are deliberately held in Application Gateway and WAF configuration rather than in device code so they can be changed without a release |
| Identity over IP | Authentication is credential-based — an `x509` certificate or a `dpop` proof — and not IP-based; IP reputation and geo rules are **not** used to admit or refuse devices. Endpoint traffic comes from remote and hybrid workers on residential and carrier-grade-NAT addresses (brief §5.5: 70–85% management coverage, most users off any corporate egress), where IP-based decisions produce false outages and no security benefit |
| WAF visibility | The WAF inspects envelopes at the edge. At M0 and M1 an envelope contains no content by construction; at M2 it contains a minimised excerpt of at most 2048 characters, which therefore *does* transit the edge and is in scope for the edge's data handling. At M3 no excerpt is permitted on the wire at all (schema) — M3's content path is the approved retrieval path, not the ingest path |
| Bot protection | Application Gateway's bot-management rule set must not be applied to the device endpoints: the clients are automation by design, and an authenticated device presenting a valid `x509` credential or DPoP proof is not a bot problem. Device traffic is admitted on the credential; bot rules apply to the analyst-facing surface |
| Origin lock | Origin ingress is private and Application Gateway-only (§2.1). Without it, a forwarded certificate is advisory: the platform documents that the origin can otherwise be reached directly, bypassing the edge's check entirely |

---

## 13. Wire-level failure catalogue

| Failure | Device behaviour | Operator-visible signal |
|---|---|---|
| **TLS failure** (handshake refused, untrusted chain, pinning mismatch, protocol below 1.3, clock so far off that the credential is not yet valid or already expired) | Do not retry in a loop: back off, record locally, report `tls_failure` with the peer's TLS alert in the next health report | Device-auth failure metrics at the edge (§2.1) break down by SNI and error; `collector_state = degraded` fleet-wide if it is a CA or clock problem. Distinguish "our CA rotated and the device is stale" from "this device's clock is wrong" |
| **Clock skew beyond tolerance** | Events continue to be accepted; the device reports `clock_offset_ms`; the bucket is unaffected because it is device-relative (§4.3) | Per-device skew from the health report, reported and never normalised away (C26); `ops.collector_state` has no dedicated skew column, so the row's `detail` document is the only place the schema can hold it. A skew large enough to break certificate validity windows shows up first as a TLS failure |
| **Partial batch accepted** | Cannot happen by contract: per-event outcomes are the response, and the transaction is all-or-nothing (§6). The device acts on each event's outcome independently — `accepted` is removed, `duplicate` is removed, `rejected` is recorded and removed, retryable failures stay | `counts` per batch in ingest metrics; a rising `rejected` share per reason code, per agent version |
| **Gateway timeout after commit** | The device does not know whether the batch landed. It re-sends **with a new `batch_id`**; the event-key constraint makes every already-committed event return `duplicate` and the rest `accepted`. No local state is discarded on a timeout | Duplicate-to-accepted ratio in ingest metrics; a spike means a gateway or timeout-configuration problem, not data loss |
| **Duplicate batch replay** (same `batch_id` twice) | Whole batch rejected `duplicate_batch`; the device mints a fresh `batch_id` and re-sends, receiving exact per-event outcomes | Batch-level rejections counted by code; a single device repeating this is a defective agent, many devices are a spool-drain bug |
| **Credential expired mid-batch** | Exactly the revocation path (§2.3): `401`, nothing written, spool retained, sending stops. If the credential is merely *expired* rather than revoked, the device attempts rotation via `POST /v1/enrol` before giving up; a **revoked** device cannot re-enrol (§5.1) | Device marked `revoked`, or an expiry alarm inside 14 days (§2.2). Retained spool depth is visible, so held-back data is a number, not a suspicion |
| **WAF block** | The device sees a `403` from the edge that does not match the API's error envelope. It must not treat an unrecognised `403` as a decision: back off, record locally, report | WAF logs correlated with device id from the certificate; a WAF rule that blocks device traffic while analyst traffic is unaffected is diagnosable only if the device reports the anomaly, which is why it must |
| **Edge rejects a certificate before authentication** (Application Gateway strict mode returns `400` for a certificate outside the uploaded CA chain, or a listener misconfiguration) | The device sees a bare `400` from the edge that does not match the API's error envelope. It must not treat an unrecognised edge `400` as a decision: back off, record locally, report — exactly as for the WAF `403` | Edge `400` rate by SNI and error; a strict-mode CA mismatch is a fleet-wide `degraded` signal, distinguished from a malformed request by the absence of an envelope |
| **Region mismatch** | `403 region_mismatch`. The device does not fall back to another region's endpoint — falling back would defeat the pin. It reports the mismatch and stops content-reading until the policy bundle tells it the correct endpoint | A named error on the device record, plus a tenant-level alert. A tenant whose devices are all hitting the wrong region indicates a DNS or enrolment-profile error, not device misbehaviour |

---

## Appendix A — Store-side fields this document requires

The wire contract is frozen; these live only in the database, in
[database/schema.sql](../database/schema.sql), which is the authority for their shape.

| Location | Field | Purpose |
|---|---|---|
| `ingest.observation` | `received_at`, `ingested_at`, `expires_at`, beside one column per envelope field (`source` is the route) | The immutable record of what each route saw, with its materialised retention. The observation carries no merge key of its own beyond the device's `dedup_key` |
| `ingest.submission` | `submission_id` (surrogate, immutable), `dedup_key` (nullable; unique with `tenant_id` where present), `dedup_weak_key` (unique with `tenant_id` where `dedup_key` is null), `winning_source`, `winning_fidelity`, `observed_routes[]`, `observation_count`, `merge_confidence` (`high · low`), `content_state`, `shredded_reason` | The two-table model's logical row and its explainability |
| `ingest.rejected` | `reason_code`, `detail`, `field_presence`, `envelope_redacted`, `expires_at` | §7, with TTL |
| `ref.route_fidelity` | `source`, `fidelity_rank` (lower wins), `yields_content` | §4.4, referenced by the master doc §5.3 |
| `ops.grant` | `decision`, `denial_reason`, `object_id`, `upload_expires_at`, `case_reference`, `approved_by` | §3, §10 |
| `ops.content_object` | `state` (`uploaded · shredded`), `shredded_reason`, `shredded_at`, `ciphertext_sha256` (the upload's `raw_digest`), `wrapped_dek`, `kek_id`, `kek_version` | §3, §10, §11 |
| `ops.retrieval_grant` | `principal`, `case_reference`, `second_approver`, `raw_digest`, `expires_at`, `used_at`, `used_by` | §11: the single-use redemption record |
| `ops.device` | `hardware_identity_hash` (nullable, with a per-tenant partial unique index) | §5.1: C11's idempotency key, so re-enrolment after a re-image returns the existing `device_id` |
| `ops.device_credential` | `credential_type` (`x509 · dpop`), `public_key_thumbprint` (the single binding for both modes), `public_key_jwk` (`dpop` only), `revoked_at` | §2.2, §5.1: the per-device revocable credential and the one transport binding |
| `ops.enrolment_token` | `token_hash` (SHA-256, tenant-leading key), `hardware_identity_hash?`, `expires_at`, `used_at`, `revoked_at` | §5.1: the bootstrap credential, stored only as its hash |
| `ops.dpop_replay` | `(tenant_id, jti)`, `seen_at`, `expires_at`, swept once expired | §5.6: the bounded DPoP replay window, deliberately not an audit log |

## Appendix B — Platform facts relied on

From Microsoft Learn, *Mutual TLS authentication with Application Gateway* and the Application Gateway
`WAF_v2` / `Standard_v2` documentation, which §2.1, §2.2 and §12 are built against. This appendix
**supersedes the Front Door Premium mTLS facts it previously held**; Front Door is now the analyst
ingress only, and no device-authentication claim here depends on it (ADR 0020).

- Application Gateway **`WAF_v2` / `Standard_v2`** supports client-certificate authentication on the
  listener in **General Availability** (since February 2023). A listener either **requests** a client
  certificate and forwards it for the origin to validate (passthrough, the default here) or **requires
  and validates** it against an uploaded CA chain (strict mode, optional hardening for certificate-only
  tenants).
- In passthrough mode a **rewrite rule** sets a header — here `X-Client-Cert` — from the
  `{var_client_certificate}` server variable, PEM-encoded. The origin trusts it only when it is
  reachable solely through the gateway (private VNet, source restricted to the gateway subnet), and
  re-validates the chain against its own trust bundle regardless: the edge is a filter, the origin is
  the authority.
- Strict mode's uploaded CA chain is a manual artefact with no automatic rotation, so it is reserved
  for certificate-only tenants; a gateway that serves a mixed `x509` / `dpop` fleet runs passthrough,
  because the origin is the authority for both modes.
- **Historical (Front Door).** The prior version of this appendix cited *Mutual TLS authentication in
  Azure Front Door (preview)* (<https://learn.microsoft.com/en-us/azure/frontdoor/mutual-tls>): mTLS
  in preview, OCSP-only revocation, up to two CA certificates with no auto-rotation, a custom-domain
  SAN requirement, and an origin that had to be restricted to Front Door traffic because mTLS was
  otherwise bypassable. ADR 0019 and §2.1 as first written rested on those facts. ADR 0020 replaced
  them: Azure's Front Door Private Link page states it does not support client/mutual authentication
  for private-link origins, and Front Door is an L7 proxy that always terminates TLS, so the origin
  could never verify the device handshake behind it.

Azure Blob's shared-access-signature model — scoped, time-bounded, permission-limited access — is the
mechanism behind §10.3's single-object write credential, in its user-delegation form so that no storage
account key exists anywhere in the flow.

## Appendix C — Assumptions

| # | Assumption | Where |
|---|---|---|
| A1 | §4.2 canonicalisation is a versioned contract (`sac-canon-1`); changing it after first ship splits the fleet | §1 |
| A2 | 300 s bucket width is a chosen parameter, justified against both failure directions | §4.3 |
| A3 | Inter-route observation gap is ≤50 ms typical, ≤2 s pathological | §4.3 |
| A4 | Device credential life 90 days, rotation at 60 days, 7-day overlap | §2.2 |
| A5 | The tenant package (FQDN, the tenant's deployment key, and the credential mode, beside the generic MSI) is delivered by MDM | §5.1 |
| A6 | Batch caps: 8 MiB compressed, 32 MiB decompressed, 256 KiB per envelope | §5.3 |
| A7 | Health report interval 15 min; minimum accepted interval 60 s | §5.4 |
| A8 | `ingest.rejected` TTL 14 days (the `quarantine` retention class), not suspended by holds | §7 |
| A9 | Retry backoff base 1 s, factor 2, cap 300 s, full jitter | §8 |
| A10 | Default spool cap 250 MB per device, configurable per tenant | §8 |
| A11 | Clock-skew flag threshold ±300 s, reporting only | §8 |
| A12 | Liveness: stale at 24 h | §9 |
| A13 | Grant TTL ≤15 min, single object, single write | §10.3 |
| A14 | Rate-limit values are operational parameters set with the first deployment | §12 |
| A15 | DPoP access-token lifetime 900 s, proof clock tolerance ±30 s, `jti` replay window 5 min | §5.6 |
