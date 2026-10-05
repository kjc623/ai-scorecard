# 06 — Security, Cryptography and Threat Model

**Status:** proposed · **Cloud:** Microsoft Azure · **Data class:** regulated personal data

This document states the trust model, the cryptography, the threat analysis and the security invariants
for Shadow AI Capture. Everything in it derives from *Shadow AI Capture — Product Requirements &
Engineering Context* (hereafter **the brief**) or from the master document
([00-architecture](00-architecture.md)) and its decisions **D1–D8**. Requirement references use the
brief's numbering (`§4.4`, `R10`); decisions use the master document's (`D6`, `C32`). A claim neither
supports is marked **ASSUMPTION** and collected in §14.1, and a mechanical consequence of a cited
requirement is marked **derived from** so a reviewer can check the derivation. An unlabelled invented
fact is a defect in this document.

**One notation, because a reversal needs one.** The master document's **D6** was *"no server-side
full-text search over content, in any key mode"*. It has been reversed: content search is a required
product capability, the master document's D6 has been rewritten, and **ADR 0014 supersedes ADR 0008**.
Where this document needs to distinguish the rewritten decision from the decision it replaced it writes
**D6′** for the current one and **D6** for the historical one — for example "the index is new under D6′"
against "the export was D6's answer to search". Every unqualified `D6` in what follows is a statement
about the old position and is marked as such; **§6 is the section that had to be re-derived rather than
edited**, because D6's justification was cryptographic and the reversal is largest here.

---

## 1. Scope and method

### 1.1 What is analysed

The system as designed in [00-architecture](00-architecture.md) §4.1: two device processes
(`capture-extension`, `capture-core` with four providers and the sandboxed `classifier-host`), five
device-facing endpoints, four cloud services, the scheduled jobs, PostgreSQL, Blob storage, Key
Vault / Managed HSM, and the analyst browser session. The v1 design is fixed, including D3 (no kernel
component), D4 (no Apple restricted entitlements) and **D6′ (content search is a three-valued
per-tenant capability, `disabled` / `attachment_names` / `full_text`, with full-text search
unrepresentable alongside customer-held keys)**. D6′ **supersedes D6** — "no server-side full-text
search over content, in any key mode" — which the product owner reversed on customer requirements;
**ADR 0014 supersedes ADR 0008**. The reversal is the largest single change to this document, because
D6's justification was cryptographic: §6 is re-derived around D6′ rather than D6 with softened wording,
§5.6 states the position on searchable encryption, and §12 lists what the product gave up.
[database/schema.sql](../database/schema.sql) implements the isolation and audit controls described here; where
this document and the schema disagree, the schema is wrong and must change.

### 1.2 Method

Three passes, in order, because each supplies input to the next. **Trust boundaries first** (§2):
enumerate every place data changes hands or changes trustworthiness, name who is on each side, and say
what crosses and in which direction. Starting from an attacker taxonomy rather than from boundaries
misses the boundaries nobody thought of as boundaries — here, the browser sandbox edge, the customer's
directory, and the customer's own key material. **STRIDE over those boundaries** (§8): one row per
plausible threat with asset, boundary, mitigation and residual — a threat table whose residual column
is all "low" was not written honestly. **Abuse cases from legitimate parties** (§10): not an outsider
breaking in, but a tenant administrator, an analyst or the vendor's own staff using features as
designed for an unintended purpose. Those do not fit STRIDE, which is why they get their own pass.

### 1.3 Excluded

- **Legal, privacy-policy and jurisdictional review**, excluded by the brief's scope note. This
  document makes no legal claims: it cites no statutes, asserts no processing is lawful, and treats no
  regulatory obligation as settled. §11 lists what the engineering system must *support* for that
  workstream and stops.
- **Physical device security** beyond loss and theft (T1) and the statement that the local spool is not
  a boundary against the device's own administrator (§2.1, §5.4); **the vendor's corporate estate**
  except where it is on the path to a production credential (§4); **supply chain and secure-SDLC
  controls** — dependency policy, SBOM, provenance, penetration-test cadence — which are delivery work
  rather than design.
- **Availability as a target.** Ingest availability of 99.9% (brief §8) is a reliability property. The
  two availability problems that are security problems — denial of service on ingest and the loopback
  broker's inverse failure mode — are included.

---

## 2. Trust model

### 2.1 The parties, and what each is trusted for

**The managed device is untrusted.** Master document Alternative B's framing, taken literally: the
device is in the possession of the person being observed, who is a local administrator on it. Nothing
on it is a security boundary **against its own user** — the encrypted spool (§5.4) defeats theft and
offline analysis, not the device administrator, and no design at this privilege level could. **Every
field a device sends is an assertion, not a fact:** the server validates structure, identity and
consistency, never truth, and §10.2, §11 and §12 all rest on that distinction. The only secret it holds
is a revocable credential usable only from that device, plus short-lived grants (§4.2).

**The browser sandbox is untrusted in both directions.** The extension is privileged — it reads request
bodies and page-context file objects (brief §5.1) — while page JavaScript, including the AI site's own
code and anything it loads, is not. The invariant is asymmetric: **the extension may read the page; the
page must never read the extension.** Observation content, keys, the spool handle and the policy bundle
are never exposed on a page-accessible object or a channel the page can post to.

**The customer's network is untrusted**, but it is not on the confidentiality path: interception is on
the device, before any network control applies, so a compromised network can disrupt collection but
cannot read prompt content at M1, because content is not on the wire at M1.

**The vendor cloud is one trust domain, segmented internally.** Azure subscription boundaries are not
security boundaries between the vendor's own services; `content-vault` exists because *which process
can decrypt* is a real boundary while *which service* is not (D7). The vendor operator is inside this
domain and is modelled as a potential adversary for content purposes, which is what makes §6's custody
modes load-bearing rather than decorative. **The operator is the most privileged human in the system
under vendor-managed keys**: someone with production access and key rights can in principle unwrap a
tenant's content key. D6′ does not pretend otherwise; it lets the customer make the unwrap visible
(§6.2). Under customer-held keys the operator is reduced to infrastructure metadata, exactly as master
doc §4.3 records. **Under D6′ the operator's position changes in one specific way for one specific
population:** a `full_text` tenant's prompt content sits in `ingest.search_text` as plaintext **outside
the key hierarchy**, so for those tenants the operator does not need an unwrap to read it. The custody
mode stops being the complete control it was, which is exactly why the schema forbids that combination
for `customer_held` and why §6.2 says the mode 2 unwrap record is no longer a complete account.

**The customer's directory is authoritative for one thing only.** Questions 2, 3 and 8 of brief §3.6
need department, population and manager, which are unobservable on the endpoint and come from the
customer's directory (master doc §5.3, Q2). It supplies that mapping and nothing else, and is not an
authentication path for the product's own access control, which is Entra ID (§4.1).

**The customer's own storage is outside the vendor's control.** The scheduled export (brief §3.6) lands
in storage the customer owns, under keys the customer holds, indexed by the customer's tooling (C31).
Once it lands the vendor has no control, no visibility into who reads it and no ability to erase it, and
the same is true of data under customer-held keys: the vendor holds no key, so the vendor cannot be the
control. This is stated here rather than discovered at contract time. **D6′ does not change this, but it
does change what it means:** the export used to be the only route to content search, so a customer who
wanted search accepted this transfer of risk as the price. Now the product offers search itself, and the
export is an additional copy rather than the answer — which makes excluding the index from it a
requirement (§6.5, §11.3) rather than a design preference.

**The analyst is a legitimate party with a partial view.** Master doc §4.3 is the authority: aggregates
freely, subject-level data with an audit trail, content only through an approved, case-referenced path,
cross-tenant reads structurally impossible. §10 treats the analyst as a misuse risk rather than a
threat actor, because the realistic failure is scope creep, not intrusion. **D6′ changes the second and
third clauses and not the fourth.** "Content only through an approved, case-referenced path" was the
complete statement of how an analyst reaches content; it is now one of two paths, and the second —
`full_text` search — has an audit trail but no case reference and no second approver (§6.3, §10.5).

**The customer's key material is the root of the confidentiality story.** Three artefacts share the
phrase "root" or "key" and must not be conflated:

| Artefact | Who holds it | What it protects |
|---|---|---|
| The enterprise root CA used for interception | The customer's fleet; private key on the device, non-exportable | Nothing. It is a liability, not an asset (§5.1, §9) |
| The per-tenant key-encryption key (KEK) | Vendor Key Vault (mode 1), the customer's vault (mode 2) or the customer's HSM (mode 3) | All stored content **objects** for that tenant |
| The per-device spool key, OS-wrapped | The device and its OS key store | The bounded local spool only |

**A fourth artefact now sits outside that table, and its absence from the key hierarchy is the whole
point.** `ingest.search_text` holds prompt text and attachment filenames as plaintext, under **no KEK and
no per-object key** (§6.3). It is therefore not protected by the sentence this section exists to make
precise, and the honest form of that sentence after D6′ is: **the KEK is the root of the confidentiality
story for content *objects*, which is a subset of the tenant's content once `full_text` is enabled.**

### 2.2 Boundaries, and what crosses them

Direction is from the vendor's perspective: **in** = toward the vendor's cloud.

| # | Boundary | What crosses | Dir. | Control |
|---|---|---|---|---|
| B1 | Device spool → Azure edge | Envelope batch, 1–500 records: identity, tool fingerprint, timestamps, mode, labels, digest, size. **No prompt text, no attachment bytes** (§2.3) | in | HTTPS 443 at Application Gateway (public device ingress); a per-device `x509` or `dpop` credential with the `public_key_thumbprint` transport binding, re-validated at the origin; schema validation; tenant-scoped idempotency (C12) |
| B2 | Browser sandbox → `capture-core` | Observed request bodies, page-context attachment bytes, tool identity (brief §5.1) | local | Native messaging on a per-install channel the page cannot reach |
| B3 | Cloud → device (policy) | Signed bundle: classifier version, mode per scope, retention class, destination allowlist, spool bounds, feature state (brief §4.2) | out | Signature verification; **failure retains the previous bundle and refuses content-reading modes** (C10) |
| B4 | Device ↔ cloud (grant) | One grant request per event; on approval a single-object upload credential and key material (C14) | both | Per-event, single-use, one object. No bulk path, which is why C5 holds structurally |
| B5 | Device → Blob (content) | Ciphertext of one object, at M3, only under a grant | out | Encrypted on the device with the object key; plaintext key never reaches Blob |
| B6 | Delivery channel → device | Signed installer, signed update manifest; content separate from code | in | Signed manifests, ring deployment with automatic halt, atomic install with rollback (C36) |
| B7 | Customer directory → cloud | Org dimension for a `user_ref`: department, population, manager (master doc §5.3, Q2) | in | Read-only sync by a workload identity; pseudonymous key only. **As built (backlog/06):** `control-api sync-directory` reads Entra ID through Graph (application client credentials) or a JSON export, and writes `ops.user_dim`; `directory_object_id_enc` is sealed with AES-256-GCM under a per-tenant key, and the clear display name is stored only while `device_identity = 'clear'` |
| B8 | Cloud → customer storage | Scheduled Parquet export (C31); metadata and labels only (§5.5) | out | Customer-owned storage, customer-held keys, no recall (§2.1) |
| B9 | Analyst browser → `query-api` | Filters, drill-downs, case-referenced retrieval, **search queries and their snippets**, exports | both | Entra ID role claim; tenant from the token only; every subject-level read audits as it is served (C30), **including every search, which commits its audit row in the same transaction or returns nothing** (§6.3) |
| B10 | Vendor operator → Azure control plane | ARM/Bicep, Key Vault operations, database configuration | both | Just-in-time elevation, approval, ticket reference, no standing access (§4.5) |
| B11 | Device network path | Intercepted flows to enumerated destinations only; everything else blind-tunnelled | local | Destination allowlist scoped by signed policy; fail-open; server-side kill switch (§9) |
| B12 | Customer HSM / vault → `content-vault` | One unwrap of a per-tenant key, over federated identity | in | Customer-controlled key; customer-visible audit of the unwrap (§6.2) |
| B13 | `query-api` → `content-vault` (search) | A search query — terms, time bounds, scope filters — and back the matching submission ids, match counts and **bounded highlighted snippets** (§6.3) | local | Internal-only ingress (D7); `query-api` has no `SELECT` on `ingest.search_text`; only the vault can read the index; the audit row commits in the same transaction as the results or neither is served |

**Boundary B13 is new, and it is the one the reversal creates.** It is the first flow in which
**content text derived from a search query crosses a service boundary**, and it is the reason §6.3
restates the "exactly one component can read content" invariant as "exactly one component can read **or
return** content" rather than leaving the old wording standing. What crosses is bounded snippets, not
full text — but it is content, it is unbounded in *volume* across queries, and it is the point at which
§10.5's abuse case lives.

**It also creates the only path in the design where the tenant-scoping of a *system* service matters.**
`query-api` composes the request and `content-vault` composes the query, so the tenant must be carried
across an internal hop that has no Entra ID token and no device credential of its own. The rule is the
one §4.1 and §7.1 already set for the external boundary, applied internally: **the tenant on B13 is the
one from the authenticated session, it is asserted over mTLS by the calling service's managed identity
and re-derived by the vault rather than trusted, and the vault applies row-level security to its own
connection as the final arbiter.** An internal caller that could name a tenant on B13 would be a
cross-tenant read primitive with no device, no browser and no user behind it — which is why §13
invariant 24 tests the grant set rather than the service's good behaviour.

**As built:** B13 is not yet carried over mTLS. `azure/main.bicep` (line 469) gives `query-api` the
vault's address as `http://…`, `azure/modules/container-apps-env.bicep` (lines 47–50) leaves
environment peer mTLS disabled, and the vault reads the caller's service, subject and tenant from
request headers that it trusts only because its ingress is internal
(`vault/content-vault/internal/auth/auth.go`). The vault's SQL path is written to set the session tenant
inside every transaction, so row-level security remains the final arbiter; the mTLS assertion of the caller is the
part that is not in place (§5.2). B13 is now exercised: `query-api` forwards content search and approved
retrieval to the vault with those headers, naming the tenant and subject it took from its own session —
which today is a development principal header, not an authenticated one. The vault does not restrict
routes by calling service, so any of its three allowed callers may call any route. In the local auth lab
the vault connects to the database as the owner, so row-level security is not exercised there.

Three boundaries the design deliberately does **not** create: no central plaintext inspection point
(Alternative A must decrypt before it can decide, which brief §1.2 forbids); no device-to-`content-vault`
path (D7 — internal ingress only); and no content index outside `content-vault`'s read path. The third
was previously "no vendor-side content index in any key mode" (D6) and cannot be stated that way after
D6′. The honest replacement is narrower than the original and is asserted in §13: **the index exists,
and it is reachable by exactly one component.** Its confidentiality no longer rests on the key model at
all, which is why the ranking in §3 puts it directly below the content itself.

### 2.3 The crucial claim, and its exact limits

> Content is interpreted where it is observed, and by default the content itself does not leave the
> machine. What crosses the network is a classification, a digest and dimensions. Content crosses only
> on an explicit, per-event grant from the backend. (Brief §1)

Stated precisely, so it can be defended in front of a customer's security reviewer:

1. **Interpretation is local.** `classifier-host` runs on the device (master doc §4.2 step 4); the
   prompt is not sent anywhere to be classified.
2. **At M0 the collector is not permitted to read content at all**, and the contract enforces it rather
   than trusting the collector: a digest, label set, classifier version, confidence or excerpt on an M0
   prompt record is *rejected*, because its presence is evidence that the device read content it was
   not permitted to read (schema M0 branch; `ingest.observation.observation_m0_carries_no_content`).
3. **At M1 what crosses is a label set with confidences and a classifier version, a digest, a size and
   dimensions** (brief §1.1, §6).
4. **At M3 the wire does not change.** The envelope carries no prompt text and no attachment bytes at
   any mode, and a minimised excerpt at M3 is rejected, because M3's content path is the approved
   retrieval path rather than the wire (schema M3 branch; master doc §4.2 step 5).
5. **`full_text` adds a server-side store of content without adding anything to the wire.** The index
   (§6.3) is populated from content that already crossed under a grant, so clause 4 still holds
   literally. It is also the clause the reversal makes least reassuring: the claim was about what
   crosses the network, and the new risk is about what the vendor then *keeps*.

The limits, plainly. **"By default" is a default, not an absolute:** a grant moves content, and the
claim is that the grant is per-event, server-decided and logged, not that content never moves. **M2 is
the exception people miss:** at M2 a minimised excerpt *does* cross the network in every envelope — up
to 2048 characters, as a matched span with offsets or a redacted window (brief §1.1; schema
`$defs.excerpt`) — so M2 is content egress at volume and is treated as such in §3 and §5.5, and a tenant
that believes only M3 keeps content has misread the mode table. **A local observer still sees
plaintext:** the claim concerns what crosses the *network* by default, not what other processes on the
machine can see, and must never be presented as the latter. And **some content is never captured at
all**, so "does not leave the machine" is trivially true of it: WebSocket messages on an established
connection, response bodies, and mode I (brief §5.1, §5.4; master doc E4) — a coverage residual in §12,
not a security win.

**The limit D6′ adds, in one unhedged sentence.** *For a tenant with `content_search = 'full_text'`, the
vendor can read that tenant's prompt content, because an index over plaintext is itself
plaintext-derived, and no cryptographic construction gives both search and vendor-blindness.* This is
the one sentence in this document that a security reviewer should be handed first, because everything
else in §6 is machinery around it. Stated as consequences rather than as a caveat:

- **"By default the content does not leave the machine" remains true and is no longer the operative
  promise for those tenants.** The operative promise for them is the one in §6.6: search, with the
  snippet bound, the audit row and the scope ceiling as the controls — not confidentiality against the
  vendor.
- **The index is not covered by the key model, so the custody modes do not answer for it.** `full_text`
  is unrepresentable with `customer_held` (schema constraint, §13 invariant 25); a `customer_managed`
  tenant that enables it holds a plaintext copy it cannot destroy by destroying its key, and receives no
  unwrap record when a search runs (§6.2, §14.3 Q-e).
- **Searchable encryption was considered and rejected, and the reason is not squeamishness.** It leaks
  access patterns, which for a security product that logs *who searched for whom* is a leak of the
  investigation itself, and it would be indefensible in a security questionnaire that asks how content
  is encrypted at rest. §5.6 records the position rather than leaving it to be re-proposed.
- **A tenant that will not accept it keeps a real option and a real product.** `customer_held` with
  `disabled` or `attachment_names` search: structured filtering, filename search, per-event approved
  retrieval, and no vendor-readable prompt text — the position D6 offered to everyone, now offered to
  the tenants who asked for it.

---

## 3. Asset inventory, ranked

Ranked by consequence if lost or read by the wrong party, not by volume. The ranking drives §8 and §13:
where a mitigation is expensive, this list justifies it.

| Rank | Asset | Why it is worth attacking — and where it lives |
|---|---|---|
| **A1** | **Prompt text and attachment bytes at M3** | The literal content of what a person sent to a model: source code, contracts, customer records, occasionally credentials. The product's reason to exist and its largest breach surface. *Blob ciphertext plus wrapped object keys in `ops.content_object`; plaintext on the device and briefly in `content-vault`* |
| **A2** | **The full-text index over prompt content — `ingest.search_text`** | **New under D6′.** A plaintext-derived copy of every indexed prompt and attachment filename, held **outside the key hierarchy**. Only what a person authored is indexed: a `client_generated` request (a client titling, summarising or reporting telemetry for itself) carries no authored text, so `content-vault` builds no index row for it. It is ranked this high, and above every metadata asset, because it is the only store in the system that is *both* bulk content *and* readable without a key: a single successful read of this table returns searchable text for a whole tenant without an unwrap, without a case reference, without a second approver, and — under `vendor` custody — without leaving a customer-visible record. What it leaks, precisely: the **text itself**, for every unit indexed; **co-occurrence** (which terms appear together in one prompt, and therefore the shape of a document or a code change); **cross-subject correlation**, because the index is tenant-wide and `tsv` statistics span users; **timing**, because `created_at` orders index rows the same way events are ordered; and, to anyone who can run queries rather than read rows, **the existence and frequency of a term** even when no snippet is returned (§8 T15). Attachment *contents* are not in it in v1, which bounds what it leaks to prompt text plus filenames and no further (§6.3). *`ingest.search_text`, readable by exactly one component (`content-vault`) and by no other service, including `query-api`* |
| **A3** | **The content key tuple** — object data keys, per-tenant KEK, customer HSM | Reading one wrapped object key is equivalent to reading the object; reading a tenant KEK is equivalent to reading every object in that tenant. *Key Vault / Managed HSM (modes 1–2) or the customer's HSM (mode 3); wrapped keys in `ops.content_object.wrapped_dek`* |
| **A4** | **The ability to report clean while collecting nothing** | Brief §7: *"a security product that reports a clean bill of health while collecting nothing is worse than no product."* Ranked above the labels and the identity map because it is the only asset whose **compromise is invisible in the product's own output** — every dashboard stays green while the customer is blind, and the customer cannot detect it by reading the console. *The health and coverage path end to end: `ops.collector_state`, `ops.coverage_snapshot`, `mart.v_device_liveness`, the drop counter, release and kill-switch state* |
| **A5** | **The classification label set and policy decision** | A label is not "some sensitive data". `payment_card` against a named tool at a known time, aggregated by department, reveals what an organisation is *doing*: which teams handle cardholder data, which are in a legal matter, which write health-adjacent code. The labels reconstruct a map of activity with no content at all. *`ingest.submission`, `mart.agg_*`* |
| **A6** | **The mapping from `user_ref` to a real person** | It turns anonymous behavioural records into an employee-monitoring dossier, and it is the join key that makes A5 attributable. In the schema it is one encrypted column. *`ops.user_dim.directory_object_id_enc`, synchronised from the customer's directory (Q2); `user_ref` itself is pseudonymous on the wire*. **As built (backlog/06):** the sync exists and fills `ops.user_dim`; the identifier is sealed with AES-256-GCM under a per-tenant key, and the directory display name beside it is stored and shown only while `device_identity = 'clear'`, so a `hashed` tenant keeps the pseudonymous path |
| **A7** | **The audit log** | Both an evidence asset and a targeting asset: it shows who retrieved content, for which case, and when; it locates retention holes and holds; and its integrity is what makes §11's attestations worth anything. Read access to it is read access to the investigation. **Under D6′ it also records every search query and every term searched**, so it now names the subjects an analyst was curious about as well as the ones they opened. *`ops.audit`, append-only and hash-chained* |
| **A8** | **Collection-mode and search-tier configuration, and its change history** | An attacker who can lower a scope's mode *creates* the breach surface rather than exploiting it: raising a scope from M1 to M3 causes content to be requested and kept, and **raising `content_search` to `full_text` converts retained content into a queryable corpus** (§10.6). C4 requires attribution of every change, and §11.6 requires the search-tier change specifically to be attestable. *`ops.tenant.ceiling_mode`, `ops.tenant.content_search`, `ops.policy_bundle.scope_matrix`, plus an audit entry per change* |
| **A9** | **Device credentials and the enrolment path** | A credential valid for an active device is a channel into that tenant's ingest; a compromised enrolment path is how an attacker obtains one. *`ops.device`, `ops.device_credential`* |
| **A10** | **Local spool contents** | Up to the spool cap of classifications and, at M2, excerpts — on a device that may be lost. Ranked below A1 only because it is bounded, and because at M3 it holds content the server has not been granted. *An append-only segment log on the device, each record sealed with AES-256-GCM (`endpoint/capture-spool`, §5.4)* |
| **A11** | **The enterprise root CA and the interception path** | Not a data asset but a *capability* asset, and the largest blast radius in the product (master doc §5.5). Whoever controls the interceptor reads everything it intercepts; §9 is about nothing else. *Device trust store; non-exportable private key in the platform key store* |
| **A12** | **Classifier releases and rule definitions** | The ability to push a classifier change is the ability to change what the product reports, or to make it block. C20 requires non-enforcing evaluation first and instant reversal. *`ref.classifier_release` state machine: `shadow` → `enforcing` → `rolled_back`, with `retired` as the fourth state the table admits* |
| **A13** | **The reconciliation drift record and coverage gaps** | Where the system records "coverage was not achieved". Attacking it converts a visible gap into a silent one — A4 by a slower route. Under D6′ it is also where index-expiry drift surfaces, so degrading it hides both coverage and retention failures. *`ops.reconciliation_run`, `ops.coverage_snapshot.gap_reason`* |

Two consequences that are easy to get backwards. **A4 is not an availability problem:** a path that
fails silently is a *security* failure, because every downstream decision — an investigation, a risk
report, an attestation — assumes the visible data is all the data, which is why §8 T12 gets its own
row. And **A5 and A6 together are more dangerous than A1 alone:** A1 requires a grant, a case reference
and a second approver (C16), whereas A5 and A6 are ordinary aggregate and subject-level reads that the
audit trail records but does not prevent. The control there is aggregation and attribution discipline,
and §10 is where that gets tested.

**A2 changes the first of those two claims, and the change runs the wrong way.** Under D6, A1 was the
asset that required a grant, a case reference and a second approver, and A5/A6 were the ones that did
not. **A2 is content that requires none of the three**: for a `full_text` tenant, prompt text is
reachable by a query that carries an audit trail but no approval, so the cheapest path to content in the
product is no longer the approved one. A2 is ranked below A1 because the index holds no attachment
bytes and no content older than its TTL, and above A3 because A3 is only as good as the key store's
controls while A2 is plaintext at rest. §8 T13–T16 and §10.5 are the consequences.

---

## 4. Identity and access

### 4.1 Humans — Entra ID

Analysts, approvers, tenant administrators and auditors authenticate to Entra ID. There is no local
account, no shared analyst login and no separate password store.

- **Role claims, never query parameters.** Roles are `viewer`, `analyst`, `approver`, `auditor`,
  `tenant-admin`. `approver` is deliberately separate from `analyst`: C16's second approver is
  worthless if the same principal can hold both and click twice in one session.
  **ASSUMPTION:** §14.1 A7 — whether the approver is a customer-side or vendor-side role is left open
  by master doc Q9, and this document does not decide it.
- **The tenant comes from the token.** `tenant_id` is a claim, never read from a body, query string or
  header. §7 makes this mechanical at the database.
- **Phishing-resistant authentication for privileged roles**, multi-factor for all humans. The
  dashboard is a static SPA behind Entra ID; the browser holds tokens, never database credentials and
  never content keys.
- **Human revocation is near-real-time**, because content retrieval and export are high-consequence
  and a long-lived access token would make "revoked" a fiction for its lifetime.

### 4.2 Devices — per-device revocable credentials with transport binding

| Property | Mechanism | Source |
|---|---|---|
| Enrolment | One-shot; the device generates a key pair whose private key is hardware-backed where the platform supplies a TPM or Secure Enclave. It submits a PKCS#10 CSR (`x509`) or a public JWK and a proof of possession (`dpop`); the bootstrap credential is a short-lived, single-use enrolment token | Brief §4.2; ADR 0020 |
| Re-enrolment | Idempotent after re-imaging: returns the existing identity rather than creating a duplicate | C11 |
| Identity | **Revocable per device.** A revoked device is rejected and marked accordingly | Brief §4.2 |
| Transport binding | One seam with three modes (ADR 0020 decision 2). `x509`: the certificate presented on the connection or forwarded by the edge in `X-Client-Cert` is re-validated against the trust bundle, and its **SHA-256 SPKI thumbprint** must equal `ops.device_credential.public_key_thumbprint`. `dpop`: the token's `cnf.jkt` and the per-request proof must equal the same `public_key_thumbprint` (RFC 7638), with the proof's `jti` refused on replay. `dev`: no cryptography, refused unless explicitly acknowledged at startup | **derived from** brief §4.2's "mutually authenticated" plus "revocable per device" — a bearer token alone would be replayable from any host and would make revocation a race |
| Revocation check | Checked on every ingest and control request against server state, with a short cache. The cache window is a bounded, stated exposure | Derived from the same |
| Lifecycle | Short-lived by construction; renewal is automatic and auditable; a device that cannot renew stops sending rather than sending unauthenticated | Derived from C10's fail-closed posture |

The property that matters: **a stolen device credential is not sufficient.** It must be presented from
a connection proving possession of the device's private key, and where that key is non-exportable and
hardware-backed, exfiltrating the credential file yields an unusable secret. Where it is not
hardware-backed, §14.1 A2 records the honest position.

**As built:** the pluggable seam and `control-api` exist. `ingest-api` selects among a direct-TLS
certificate, an edge-forwarded certificate and DPoP by what the request presents
(`ingestion/ingest-api/internal/auth/pluggable.go`); the production paths share one status check and
one transport-binding comparison against `ops.device_credential.public_key_thumbprint`
(`internal/auth/binding.go`), where `x509` uses SHA-256 over the certificate SPKI and `dpop` the RFC
7638 JWK thumbprint. `control-api` serves `POST /v1/enrol` for both modes and `POST /v1/token`, and the
schema carries `credential_type`, `public_key_jwk`, `ops.enrolment_token` and `ops.dpop_replay`. What is
**not** deployed is the Azure wiring: Front Door is still the declared edge, there is no Application
Gateway module, and every app in `azure/main.bicep` passes `keyVaultEnv: []` — so a deployed container
cannot yet authenticate a device (ADR 0019, ADR 0020). The device-auth lab proves the seam through a
simulated Application Gateway; Azure is the part that is not built.

### 4.3 What revocation does to in-flight work

Ambiguity here is where data leaks, so the behaviour is defined against work already in progress.
Master doc §4.4 fixes it; the security reading is:

| In-flight state | What happens | Why this is the safe answer |
|---|---|---|
| Batch in transit | Rejected `401`. The device stops sending and **retains** the spool rather than discarding it | Discarding would destroy evidence of a compromise; retaining preserves what the device observed for an investigator |
| Grant issued, upload not started | The single-object, short-lived upload credential is void rather than retried; the event's content state returns to `local_only` | A revoked device must not be able to complete a content upload |
| Upload in progress | Refused at the object endpoint; ciphertext already written for a void grant is not attached to the event and is treated as orphaned | A partial upload must not become a readable object |
| Analyst session | Access and refresh tokens revoked; in-flight queries abandoned at the next authorization check | A half-served result set is not a served result set |
| Device state | Marked `revoked`, with **the actor who revoked it** and the time (`ops.device.revoked_by`) | C4's attribution rule applies to revocation as much as to mode changes |

Revocation is a security operation, so it is an audited one.

### 4.4 Services — no shared secrets, no borrowed privilege

Every service carries a **user-assigned managed identity**. There is no database password anywhere,
so there is no password to rotate, leak, commit or find in configuration. Each service has its own
identity and its own database role. The schema names them: `sac_owner` (owns every object, never used
at runtime), `sac_migrator` (DDL only, the sole role holding `BYPASSRLS`, and no runtime service is a
member of it), and one runtime role per component.

| Service | Role | May | Structurally cannot |
|---|---|---|---|
| `ingest-api` | `sac_ingest` | Insert observations and rejections; insert and update submissions and the usage ledger; read-only `SELECT` on tenant, device, credential and retention state; insert audit. Collector health is written by `sac_control`, not here | Select content or wrapped keys; **any** Key Vault unwrap right |
| `control-api` | `sac_control` | Tenant and policy configuration, device state, grant decisions, policy signing; column-scoped read of submission metadata | Prompt content; unwrapped keys |
| `content-vault` | `sac_vault` | Wrapped keys, ciphertext objects, grant state, **and the full-text index over prompt text and attachment names — it is the only role granted `SELECT` on `ingest.search_text`.** **The only unwrap right in the system** | Any user-facing endpoint — internal ingress only (D7) |
| `query-api` | `sac_query` | Events, labels, aggregates, findings, audit; calls `content-vault` for content **and for search** | Unwrapped keys; it is not granted `SELECT` on `ops.content_object` **or on `ingest.search_text`** at all |
| `aggregator`, `reconciler` | `sac_ops` (one database role shared by both jobs) | Rollups, expiry, erasure mechanics, drift detection — including `DELETE` on `ingest.observation` and `ingest.submission` and row access to `ops.content_object` and `ops.grant` for expiry | Content decryption; unwrap |
| Dashboard | Static SPA + Entra ID | `query-api` only | Database, Blob, Key Vault |
| CI/CD | Federated workload identity, no stored secret | Deploy artefacts and migrations | Runtime data; key material |

**As built:** the content path the table describes now runs — `control-api` decides grants and finalises
uploads, `content-vault` mints and wraps object keys, records objects, writes the index row and serves
approved retrievals, and `query-api` forwards search and retrieval to it — but the role separation above
has been exercised only as code, not as database identities. The local auth lab connects every service
as the database owner, so neither row-level security nor these grants is tested there. Three statements
`control-api` now issues need grants `database/schema.sql` does not give `sac_control`: a read of
`ingest.observation` (to check the event is that device's), the `content_state` update on
`ingest.submission`, and the write to `ops.usage_daily`. `sac_vault` likewise has only `SELECT` on
`ops.erasure_receipt`, which the vault's shred path inserts into. The key store in the lab is the vault's
file-backed development KEK, not Key Vault.

Two separations are load-bearing and are asserted in §13: **`ingest-api` has no unwrap right** (it
accepts attacker-influenced input at the highest volume in the system, so a memory-safety bug there
must not become a content breach), and **`query-api` has no unwrap right** (it serves the widest
audience, so it must ask `content-vault`, keeping the authorization decision and its audit obligation
in one place). The search feature was built without weakening either: `query-api` sends a query and
receives bounded snippets, and the index is not a table `query-api` can address at all.

**As built:** the first separation is not met by the declared Key Vault role assignments.
`azure/main.bicep` (lines 242–246) assigns `ingest-api`'s identity the `cryptoServiceEncryption`
operational role at vault scope, which `azure/modules/keyvault.bicep` (lines 47 and 96) resolves to
the built-in *Key Vault Crypto Service Encryption User* role. That role's data actions are
understood to include key wrap and unwrap; the role definition itself is not in the repository and
has not been confirmed against a subscription. The CI assertion in
`azure/pipelines/policy-scan.yml` inspects only `unwrapPrincipalIds`, so it does not see this
assignment. The requirement stands as written; the assignment is what has to change.

### 4.5 Database roles and privileged access

Application roles are **non-owners without `BYPASSRLS`**, so row-level security applies to them whether
or not they wrote the policy (§7); they carry no `SUPERUSER`, `CREATEDB` or `CREATEROLE`, and default
privileges revoke `ALL` from `PUBLIC`, so a new table is invisible until granted on purpose.

- **No standing production access.** Key-management roles and the database administrator role are
  eligible-only, activated just in time, with approval for the key roles, a ticket reference and a time
  limit (master doc §4.3).
- **Break-glass accounts exist, sit outside the ordinary conditional-access path, and alert on any
  use.** A break-glass path nobody notices is a standing backdoor with better branding, and every
  privileged activation is itself audited, because an elevation that is not recorded is
  indistinguishable from a compromise after the fact.

**The rule: no component holds a credential it does not need.** Not an aspiration — asserted per
service above and tested in §13. The corollary used to be that the number of components holding any
decrypt capability is exactly one. **D6′ restates it rather than preserving it**, because "decrypt
capability" no longer describes the thing being confined: the index is readable *without* decryption, so
the property to assert is that **exactly one component can read or return tenant content — by unwrapping
a content object, or by executing a search over the index — and it is `content-vault` in both cases**
(§6.3, §13 invariant 24). The cost is that this one service is now the sole control in two directions at
once, and the strength of the whole confinement rests on its authorization and audit being correct for a
**query** interface and not only a retrieval interface.

---

## 5. Cryptography

### 5.1 The enterprise root CA — the most sensitive artefact the product installs

The interceptor needs a certificate the device will trust, which means installing a root CA into the
customer's trust store. This is the most sensitive thing the product puts on a machine, more sensitive
than the content it collects: content is bounded by grants, and a root is not.

| Constraint | Why |
|---|---|
| Installed **only** into a store the browsers actually honour: Windows Local Machine → Trusted Root or the Enterprise/Group Policy store; macOS Default or System keychain with "Always Trust"; Linux the system trust store (`update-ca-certificates` / `p11-kit`) | Brief §5.2: neither browser inherits trust from the platform default store, and **the wrong store fails silently** — producing either no coverage or an unverified assumption about what is trusted |
| A **hierarchy, not one certificate**: an offline root signs one constrained intermediate, which issues short-lived leaves for the proxy | The key that can mint trusted certificates must not live on 5,000 endpoints. Short leaf lifetimes mean a compromised leaf expires without a fleet-wide trust change |
| Leaves are issued **only for enumerated interception destinations** (§9) | A leaf for `chatgpt.com` is not a universal TLS forgery primitive. That is the difference between an interceptor and a device-wide MITM kit |
| The root's private key is **non-exportable** and hardware-protected, and never leaves the issuing infrastructure | If the root key is extractable, the compromise is not "our product is abused" but "every TLS connection from these machines is forgeable" |
| Trust is **removable by the mechanism that installed it** — MDM profile or Group Policy | A trust anchor the vendor cannot cleanly remove is a permanent liability on a machine the vendor does not own |

Every constraint above reduces the blast radius; none eliminates it. §9 is the full treatment, and §12
records the residual as accepted because the alternative (no interception, master doc Alternative C)
fails the product's core requirement.

### 5.2 Transport — TLS 1.3 on the wire

TLS 1.3 with forward secrecy on every hop: device to edge, service to service, service to PostgreSQL,
analyst browser to `query-api`. The device hop authenticates with a per-device credential, and mTLS is
no longer the only mode: `x509` client-certificate authentication and DPoP (RFC 9449) are both
first-class production modes, and the transport binding for both is the same
`ops.device_credential.public_key_thumbprint` (§4.2, ADR 0020).

**As built:** the requirement is not met on the one service-to-service hop the deployment declares.
`query-api` is given `content-vault`'s address as `http://…` (`azure/main.bicep`, line 469);
peer mTLS is disabled on the Container Apps environment (`azure/modules/container-apps-env.bicep`,
lines 47–50); and the vault binary serves plain HTTP (`vault/content-vault/cmd/content-vault/main.go`,
`ListenAndServe`) and trusts identity headers set by its ingress
(`vault/content-vault/internal/auth/auth.go`). On the device hop, `ingest-api` enforces TLS 1.3 with a
verified client certificate when it terminates TLS itself, and otherwise authenticates the
edge-forwarded certificate or DPoP; the deployment supplies the material for none of these yet (§4.2,
ADR 0019). The content path adds two more internal hops with the same shape, neither declared in
`azure/` yet: `control-api` → `content-vault` (the object key for a granted upload, and the finalise),
over plain HTTP with the same trusted headers; and the storage layer's finalise call to `control-api`,
authenticated by an HMAC over the body under a key the two share. In the local lab the vault also reads
stored ciphertext back with an unauthenticated GET, which is a stand-in for a storage credential.

| Protected | Against |
|---|---|
| The envelope batch in transit (identity, labels, digest, dimensions) | Passive observation on the customer's network or any intermediate network |
| The device credential and its proof of possession (the mTLS handshake, or a DPoP signature over the request) | Replay by an observer who captures the handshake or the request |
| Content ciphertext on upload | Anyone on the path — the object is encrypted before it is sent |

**What transport security does not protect: anything from the endpoint.** The device is untrusted
(§2.1), so TLS between device and vendor protects the *network*, not the vendor.

**The DPoP token endpoint widens the device-facing surface, and how that is bounded.** `POST /v1/token`
is a second entry point on the device edge that a device calls before it holds an access token, so it is
exposed on the same gateway as the data path and is deliberately narrow. It accepts only a client
assertion signed by the device's **registered** key plus a per-request DPoP proof; the issued token is
sender-constrained (`cnf.jkt`) and short-lived; the proof's `jti` is refused on replay within its
window; and a wrong key or a replayed proof is a `401`, not a token. It cannot name a tenant other than
the one the credential lives in, and it mints no content capability. The endpoint and its bounds are
[02-ingest-and-transport](02-ingest-and-transport.md) §5.6.

### 5.3 The content key hierarchy

```
Object plaintext (one submission's prompt or attachment)
        │  AES-256-GCM, fresh data key per object, authenticated
        ▼
Per-object data key ──wrapped by──► Per-tenant key-encryption key (KEK)
        │                                   │
        │                                   ├─ mode 1: vendor Key Vault / Managed HSM
        │                                   ├─ mode 2: the customer's vault, via federated
        │                                   │          workload identity
        │                                   └─ mode 3: the customer's HSM; never leaves it
        ▼
Wrapped data key in ops.content_object (with kek_id and kek_version), beside the blob reference
```

Invariants: **the per-object key is generated on the device**, used to encrypt the object before
upload, and reaches the backend only in a form the grant authorized — one object, one key, fresh every
time, so compromising one object key compromises one object. **The KEK never leaves the key store** in
any mode; in modes 2 and 3 it never enters the vendor's boundary at all, and only the unwrap *result*
crosses (B12). **Unwrap happens in exactly one service** (`content-vault`, D7), and the unwrapped key
exists only in that process's memory for the duration of one operation. **Wrapped keys and ciphertext
live in different systems** — a database row and a Blob object — and neither alone is sufficient.
**Per-object keys are independently shreddable**, the narrow case D1 relies on: D1 is explicit that row
deletion, not key destruction, is the primary mechanism for *events* because it produces a literal
receipt, while key destruction remains correct for tenant offboarding and customer-held-key content.

**The hierarchy above has one gap, and D6′ opens it deliberately.** Every stored artefact it covers —
object plaintext, the wrapped data key, the KEK — is either ciphertext or a key. `ingest.search_text` is
neither: it is plaintext prompt text and plaintext filenames, with **no KEK, no per-object key, and no
unwrap in any read path** (§5.6, §6.3). Nothing in §5.3's invariant list applies to it, and no amount of
key hygiene in this section protects it. That is the cryptographic cost of the decision, stated where the
cryptography is described rather than only where the decision is justified.

### 5.4 Local spool encryption

The spool is bounded and encrypted at rest (C22). At M2 it holds excerpts and at M3 content the server
has not been granted, so it is the one place on the device where the product's data is recoverable.

| Layer | Mechanism | Protects against |
|---|---|---|
| Record encryption | The spool is an append-only segment log (`endpoint/capture-spool`), not a database: each record is one frame sealed with AES-256-GCM under the spool key, with the frame header as additional authenticated data; the file is never a plaintext store | Offline disk analysis, file carving, backup capture, casual inspection |
| Key wrapping | The spool key is wrapped by the platform key store — DPAPI scoped to the service account on Windows, Keychain on macOS, the kernel keyring or a TPM-backed secret (`systemd-creds`) on Linux (01-collectors §12) — so the key is not stored beside the data. **As built:** `endpoint/capture-spool/keys.go` ships only an in-memory and a file key provider, both reporting `Sealed() == false`, and makes no DPAPI, Keychain or keyring call; what the code enforces today is that the key file may not live inside the spool directory | Copying the spool file to another machine and opening it there |
| Bounds | A configured cap; on overflow the **oldest** entries are dropped and a counter is incremented (`ops.collector_state.spool_dropped_total`) | Unbounded local retention, which would be the largest uncontrolled copy of the data in the system |
| Lifecycle | Entries purged once acknowledged; a revoked or stopped device retains rather than discards, so an investigation can recover what it saw (§4.3) | Loss of evidence after an incident |

**Stated honestly: the spool is protected against theft and offline access, not against the device's
own administrator.** A local administrator can ask the OS to unwrap the key on that machine, because
the OS cannot distinguish this product from any other process running as that user. This follows from
D3 and D4 and is recorded in §12.

### 5.5 Hashing — digests, dedup, and what a digest can leak

| Use | Construction | Notes |
|---|---|---|
| `content_digest` | SHA-256 over normalised content, `sha256:<64 lowercase hex>` | Required at M1 and above, absent at M0; format fixed by the contract (`$defs.sha256`) |
| `dedup_key` | SHA-256 over tenant, device, tool, the normalised content digest and a time bucket | Brief §4.1's derivation. At M0 the device cannot read content, so it is derived from a time bucket and size instead and **dedup is correspondingly weaker** — the contract says so explicitly |
| Audit chaining | A per-tenant hash chain over audit rows (`prev_hash`, `row_hash`), with the chain head anchored periodically to write-once storage | §8 T5. The anchor is what turns a self-consistency check into tamper evidence |
| Spool integrity | Per-frame authentication: every spool frame is AES-256-GCM-sealed with its header as additional authenticated data, and a complete frame that fails authentication makes the spool refuse to open (`endpoint/capture-spool`) | Detects local tampering between capture and transmission |

**The honest caveat about an unkeyed digest.** A plain SHA-256 of content is not confidential. For
guessable content — "the Q3 layoff list", a short prompt, a document already public — anyone holding
the digest can confirm a guess by recomputing it. That is a confirmation oracle and a real weakening
relative to the content it stands for. The design accepts it because **the brief requires the digest to
be present even when content is not stored, so that dedup and later retrieval both work (§3.2)**:
keying it per tenant would break the cross-route dedup R9 depends on unless every route held the same
key, and keying it per device would defeat dedup entirely, since two routes observing one submission
must compute the same value and the canonicalisation is normative (master doc §5.2). The compensating
controls are that digests are **internal-only** — never returned by the analyst API, never rendered in
the dashboard, and **excluded from the Parquet export**, which would otherwise be a cross-database
correlation surface in storage the vendor cannot control — and that they sit under the same row-level
security and key custody as the content they describe, with no lower-privilege path to them. The
residual is in §12, not described as "not a risk".

**One continuity point the reversal forces into the open.** The digest is an unkeyed function of content
that the server holds at M1, and the index is plaintext content that the server holds at `full_text`.
They are different artefacts with the same root cause, and §12 R10's searchable-encryption reasoning
applies to both: **any server-side structure that lets the vendor answer questions about content is
itself derived from content the vendor could not otherwise read.** The digest was accepted because brief
§3.2 requires it and because it is one-way and guessable only for guessable content; the index is
accepted because the product owner ruled search a required capability (§6.6). Neither is a cryptographic
oversight, and neither can be fixed without giving up the feature it serves.

**A second, wider reversal, recorded here rather than left implied (ADR 0021).** The device read and
the dashboard now show a clear hostname and the submitting account name by default, where this
document's A6 assumption kept names off the wire and resolved a person only through the encrypted
`ops.user_dim.directory_object_id_enc`. The owner directed it; the reasoning and the
`ops.tenant.device_identity` opt-out are in ADR 0021. The security reading is not softened: a store
compromise now yields a behavioural record attributed to real people and real machines, so A6's
"anonymous behavioural record" is no longer the default product. What remains true is that the
setting is a real control — a tenant that sets `hashed` transmits and stores neither the clear
hostname nor the name — and that the read which returns them is audited as served.

### 5.6 The search index is plaintext-derived, and searchable encryption does not fix it

**What the index is.** `ingest.search_text` stores `prompt_body` and `attachment_name` units as
**plaintext `text`** with a generated `tsv`, a GIN index over `tsv` and a partial trigram GIN index
over `body` for `attachment_name` rows only, in the same PostgreSQL instance as
the metadata (§6.3). It is encrypted at rest by the platform's storage encryption and by nothing else:
there is no application-level encryption, no per-tenant key, and no unwrap in the read path. Anyone who
can execute a `SELECT` against that table reads prompt text for the tenant. Exactly one role can
(`sac_vault`, §4.4), which is a real control and is asserted in §13 — but it is an *authorization*
control, not a cryptographic one, and the difference matters the moment an authorization control is
misconfigured, a backup is restored somewhere else, or an operator holds both a database role and
elevation (§8 T14).

**Why searchable encryption is not the answer, in the form the question will be asked.** The question a
security reviewer asks is "can't you encrypt the index and search the ciphertext?" The answer is no, and
the reasons are not squeamishness:

- **Order-preserving and deterministic encryption leak the thing they were meant to hide.** Encrypting
  each term deterministically means the index reveals which prompts share terms; order-preserving
  encoding reveals frequency and range. **ASSUMPTION:** §14.1 A17 — that these are settled breaks rather
  than open questions. **This document cites no specific attack**, states the position as an assumption
  rather than as a demonstrated result, and draws no legal conclusion from it; what it can say without
  qualification is that a security questionnaire asking how content is protected at rest will not accept
  either construction as encryption.
- **Searchable symmetric encryption leaks access patterns by construction.** It is the property the
  construction is defined around, not an implementation defect. For this product the leaked pattern is
  *which analyst searched for which term, when, repeatedly* — the investigation itself, which is A7 —
  and reconstructing queries from access patterns is the standard published attack.
- **Fully homomorphic or trusted-execution approaches do not remove the conclusion either.** They change
  where plaintext exists during the query, not whether an operator with control of the running system
  can reach it, and they put a research-grade dependency on the critical path of a security product at a
  500–5,000-employee price point.
- **Partial mitigations are worth having and do not change the sentence.** Index encryption at rest
  under a vendor key, and a confidentiality boundary that keeps the index out of backups and replicas
  (§8 T14), reduce the exposure of the *copy*; they do not make an index over plaintext searchable
  without the vendor's code path reading plaintext.

**The position, stated once so it does not have to be re-argued in a sales cycle:** *there is no
construction that gives the product server-side full-text search over prompt content and simultaneously
guarantees the vendor cannot read that content; the design therefore gives the second guarantee up for
the tenants that enable the first, and keeps it for the tenants that do not.* That is the whole of the
cryptographic argument on this point, and §6.6 states the product consequence.

---

## 6. The key model and the search decision, decided together (D6′, R10)

Brief §3.5 states the constraint as fact: **customer-held keys and server-side full-text search over
content cannot both exist.** If the vendor cannot read content, the vendor cannot index it for search.
R10 adds the urgency: this is "not a feature that can be added later without changing the key model",
and the key model and the search promise must be decided **together, before either is built**.

**The decision as originally taken, D6, chose one side of that line: customer-held keys permanently,
and therefore no content search in any key mode.** **D6 is reversed.** The product owner has ruled that
content search is a required product capability, customer requirements supersede internal ADRs, and
**ADR 0014 supersedes ADR 0008**. What follows is the security position re-derived from that ruling
rather than the old position restated with softer wording: the incompatibility §3.5 identified is real,
and the way this design keeps it is to make it **unrepresentable in the schema** and to **charge the
customer the capability they are buying** rather than pretending it is free.

**The decision, D6′: `content_search` is a three-valued capability, chosen per tenant alongside the
custody mode, with the incompatibility between full-text search and customer-held keys enforced by a
database constraint rather than by an assertion in a document.**

### 6.1 The three search tiers, and which custody modes they admit

The capability is `ops.tenant.content_search ∈ {'disabled','attachment_names','full_text'}`, a
per-tenant ceiling (D6′, ADR 0014).

| Tier | What it adds | Custody modes permitted | Collection mode required |
|---|---|---|---|
| **`disabled`** | Nothing. Structured filtering over metadata, labels, tool, user, period, rule, severity and review state — brief §3.5's "metadata and label search is unconstrained" | Any | Any |
| **`attachment_names`** | Substring and fuzzy search over **attachment filenames** | Any, **including `customer_held`** | M1 or above |
| **`full_text`** | Full-text search over **prompt text**, with highlighted snippets | `vendor` or `customer_managed` **only — forbidden with `customer_held`** | M3 for the scope |

Three things about that table are load-bearing rather than descriptive.

**The incompatibility is enforced, not asserted.** A check constraint on `ops.tenant` makes
`content_search = 'full_text'` together with `key_custody = 'customer_held'` **unrepresentable**: the row
cannot be written, by any role, through any code path, including a migration that intended to. Brief
§3.5's "cannot both exist" has stopped being a sentence in a requirements document that a later release
can negotiate with and become a **schema invariant** (§13 invariant 25). This is the mechanism that
makes the reversal safe to state: the promise to a customer-held tenant is no longer "we have decided
not to index your content" but "your tenant's row cannot carry the setting that would".

**`attachment_names` is deliberately on the permitted side of the line.** A filename is metadata that
the M1 envelope already carries — a file upload reports a filename and never bytes (brief §5.1) — so
searching filenames requires **no access to attachment content and no new key-model capability**. A
tenant that will not accept vendor readability keeps a real, useful search tier. Making filename search
conditional on `full_text` would have forced those tenants to choose between no search and giving up
their key model for a capability they did not need.

**`full_text` is a request for a stronger collection mode than the tenant may have.** An index row can
exist only for content that reached the server, and content crosses only on a grant (C14) — so
full-text search over prompts is **empty and useless for a tenant whose ceiling is M1 or M2**. Saying so
plainly is part of not overselling the feature: `full_text` requires M3 for the scope being searched,
which is the same M3 that brief §1.1 places on the far side of the storage-and-lifecycle boundary, and
therefore the same M3 that carries the content store, the retention machinery and the approval gate.
**A tenant that wants searchable prompt text has bought M3.** **derived from** brief §1.1 (M2→M3 is the
storage boundary), §4.4 (content arrives only under a grant) and §3.5 (search requires the server to be
able to read).

**ASSUMPTION:** §14.1 A11 — that a `full_text` tenant must also carry an M3 ceiling on the searched
scope. The ADR states the capability requirement as "M3 for the scope", and the schema resolves the
tenant-level half as a table constraint alongside the custody rule:
`tenant_search_tier_requires_collection_mode` on `ops.tenant` admits `full_text` only with
`ceiling_mode = 'm3'` and `attachment_names` only with a ceiling above M0. The per-scope half stays
with the signed bundle's narrowing (§6.3).

### 6.2 The three custody modes

All three share the hierarchy of §5.3 and differ in **who holds the KEK**, and therefore in what the
vendor can do. The schema makes the choice explicit per tenant (`ops.tenant.key_custody`:
`vendor`, `customer_managed`, `customer_held`).

| | Mode 1 — vendor-managed | Mode 2 — customer-managed | Mode 3 — customer-held |
|---|---|---|---|
| **KEK lives** | Vendor Key Vault / Managed HSM, per tenant | The customer's own Azure Key Vault, reached through federated workload identity | The customer's HSM; the key never leaves it under any operation |
| **Who can unwrap** | `content-vault`, using the vendor's identity | `content-vault`, using an identity the *customer* federated and can revoke | The customer's HSM, on a request carrying a wrapped key. The KEK does not move |
| **What the vendor can read** | Content, after an internal authorization decision | Content, after an unwrap the customer's own key store records and can see | Content only for the duration of an operation the customer's HSM authorized |
| **Who sees the unwrap record** | The vendor | The vendor **and the customer**, in the customer's control plane | The customer only |
| **Vendor can be compelled to produce content** | Yes, if it can be made to unwrap | Only by compelling an unwrap, which the customer sees | Only by compelling the customer, not the vendor |
| **Destruction** | Delete the key, after the recovery window (§6.4) | The customer disables or deletes the key | The customer destroys it; the vendor cannot prevent this |
| **Failure mode** | Vendor loses the key: content is unrecoverable and the vendor owns that loss | Customer disables the key or revokes federation: **content becomes temporarily unretrievable while metadata and dashboards keep working** (master doc §4.4) | As mode 2, plus retrieval stops if the customer's HSM or the key-management path is unreachable |
| **Operational cost** | Lowest — one deployment model | Per-tenant federation plus a customer-side operational dependency | Highest — a customer-side service, a live integration, and a customer who must run it |
| **Search tiers it admits** | `disabled`, `attachment_names`, `full_text` | `disabled`, `attachment_names`, `full_text` | `disabled`, `attachment_names` — **`full_text` is unrepresentable** |

**What mode 2 actually guarantees, and what it does not.** This is the claim most likely to be
overstated in a sales conversation:

> Mode 2 guarantees **the vendor cannot read content without an unwrap that the customer's own key
> store records.** It does **not** guarantee that the vendor can never read content.

The difference is not pedantic. Under mode 2 the vendor still holds the capability; what changes is
that exercising it leaves a record the customer owns and can audit, alert on and revoke. That is a
*detectable* capability, and detectability is a strong control — but a customer told "the vendor can
never read your content" under mode 2 has been told something false and will eventually discover it.
Only **mode 3** supports the stronger sentence, and even then with two qualifications: the plaintext
still exists in `content-vault`'s memory for the duration of the authorized operation, and the vendor's
service is still the code that requested it. Mode 3 removes the vendor's *standing* capability, not the
vendor's *code path*.

**And the reversal puts a new qualification on mode 2 specifically.** An unwrap record in the
customer's key store used to be a complete account of the moments the vendor could read a tenant's
content, because every such moment required an unwrap. Under `full_text` it is not, because the index is
a plaintext-derived copy held outside the key hierarchy: a search reads it **without an unwrap**, and
`content-vault` can serve a snippet whose plaintext never came from a key store. **A mode 2 tenant that
enables `full_text` therefore has a vendor-reading path that its own key store does not record.** That
is exactly why the constraint pairs `full_text` with `vendor` and `customer_managed` only — modes whose
key store was never the customer's evidence of non-reading — and why a mode 2 tenant whose reason for
choosing mode 2 was *visibility* should read §6.5 before enabling it. See §14.3 Q-e.

**ASSUMPTION:** §14.1 A3 — mode 3 is reached through a standard key-management interface over mutual
TLS with the customer's HSM as custodian. The brief requires customer-held keys (§3.3) but names no
protocol, and the master document assumes Azure-native services (S1); a customer-side HSM is where S1
does not hold.

### 6.3 The index, and what it costs

`ingest.search_text` is one row per searchable unit:

```
(tenant_id, submission_id, unit_kind, unit_index, body, tsv, created_at, expires_at)
    unit_kind IN ('prompt_body','attachment_name')
    tsv        generated column over body
    GIN on (tenant_id, tsv)
    GIN on (tenant_id, body gin_trgm_ops) WHERE unit_kind = 'attachment_name'
```

| Property | Value, and why |
|---|---|
| **What is written** | Prompt text (`prompt_body`) and attachment **filenames** (`attachment_name`). **Attachment *contents* are not indexed in v1** — that is the 50–500 GB/year tier (brief §3.1), and indexing it would multiply the plaintext-derived store by the same factor for a search most tenants have not asked for |
| **When it is written** | From content already on the server under a grant (C14, §6.1). The index creates no new egress from the device and no new collection mode |
| **Where it is read** | Only `content-vault`: the one component that already holds the only key-store unwrap right and has internal-only ingress (D7). `query-api` calls it and is **not granted `SELECT` on `ingest.search_text` at all**, the same structural exclusion that already applies to `ops.content_object` (§4.4) |
| **Lifecycle** | Every row carries `expires_at` and is purged on it, so the index is a **third** copy of the content that C34's two independent, periodically reconciled expiry mechanisms must cover (§11.4) |
| **Isolation** | `tenant_id` leading, RLS enabled and forced, no `BYPASSRLS` for any runtime role — identical to every other table (§7.1) |
| **The second isolation layer** | **Does not apply.** Text is stored as text, not as ciphertext under the tenant KEK. A cross-tenant read here returns readable rows, so §7.2's arithmetic layer protects `ops.content_object` and not this table (§7.3) |

**The invariant "exactly one component can read content" survives the change, and it is now stated more
precisely than it was.** No other component gained a read: `query-api` still cannot reach content, and
now cannot reach the index either, so the search feature did not widen the set of services that hold a
content-read path. What changed is the definition of the thing being confined. The old invariant was
"exactly one component can *decrypt* content". The correct statement now is:

> **Exactly one component can read or return content — `content-vault` — and it can do so both by
> unwrapping ciphertext for an approved retrieval and by executing a search query and returning bounded
> snippets over the index it alone can read.** No second component acquires a content-read path, and no
> content text reaches a caller except through `content-vault`'s authorization and audit.

That is a weaker property than the old one in one respect that must not be glossed: the vault used to be
able to answer only "what is this one event's content, for a case with an approver", and it can now
answer "which events mention this term", which is a **query interface over content rather than a
retrieval interface**. Confining a capability to one component is worth something only if that
component's authorization and audit are strong, and §10.5 is where that is tested rather than assumed.

**Scope narrowing: the tenant tier is a ceiling, never a global switch.** Brief §1.1 requires every
collection setting to be per-tool, per-class and per-population rather than a global flag, and brief
§1.1's "it must not be possible to configure the system into an upload-everything state" is the same
requirement stated as a prohibition. `content_search` is therefore narrowed exactly as `ceiling_mode`
is: **the signed policy bundle (C3) carries the effective search tier per scope, and the bundle is
narrowed before it is signed.** A tenant cannot enable `full_text` "for everything"; it enables it for
named scopes, and a scope that is not named carries `disabled`. **derived from** C1 and C3 applied to
the search capability — the same pre-signature narrowing that already makes a device unable to exceed
its tenant's collection ceiling.

**Audit-on-read applies to every search, not to every snippet read.** Brief §3.6 requires that "every
read of subject-level data writes an audit entry as it is served, not afterwards", and C30 makes it a
design rule. A search is a read of subject-level content data, so:

- **Every search commits its audit row in the same transaction that serves it**, and **fails closed**:
  no audit row, no results. The audit entry carries the query terms, the scopes searched, the acting
  principal, the tenant (from the token), the result count and the snippet count as `ops.audit.detail`.
- **The search terms are themselves subject-level data.** An audit log recording that an analyst
  searched for an employee's name is a record of an investigation into that employee. It inherits A6's
  sensitivity and the append-only, hash-chained, `auditor`-readable controls of §11.5.
- **A search that returns zero rows is still audited.** The failure mode this prevents is the one that
  matters most: a query used as a confirmation oracle (§8 T15) produces its signal precisely when it
  returns nothing.

**The approval gate protects retrieval, not search, and that is a real reduction in control.** C16
requires a case reference and a second approver for **full-content retrieval**, and that gate is
unchanged: opening one event's prompt text still needs both. A search returns **bounded highlighted
snippets** — `ts_headline`-style spans around the match, not the full text, with the bounding rule
recorded as configuration (§14.3 Q-f) — so the gate's purpose, which is to make full-content exposure a
two-person decision, is not met by search. **Stated without hedging: a tenant that requires approval for
any exposure of content should not enable `full_text`.** The tier is for tenants who have decided that
bounded search over prompt text is an ordinary analyst capability; it is not a version of approved
retrieval, and no configuration makes it one. §10.5 and §12 R14 carry the honest account of what that
concession costs.

### 6.4 Why destroying a key must destroy the content

Brief §4.4: "where the customer supplies the key, destroying it must destroy the content." That holds
only if four things are true.

1. **There is exactly one copy of the plaintext.** A second copy — a cache, a backup, a temp file, a
   search index, a log — means destroying the key destroys nothing. This is why D6's "no content
   search" was a cryptographic requirement and not only a product limitation: a full-text index is a
   plaintext-derived copy that survives key destruction. **D6′ accepts that consequence and confines it
   to the tiers and custody modes where nothing was promised about key destruction**: the constraint in
   §6.1 means a `customer_held` tenant never has one, and `attachment_names` over filenames is not a
   copy of key-protected content at all. What the design gives up is the *universal* form of the
   sentence, not the sentence itself.
2. **There is no path to the plaintext that bypasses the key.** No extracted text, no thumbnails, no
   embeddings, no excerpts beyond the bounded M2 window.
3. **The wrapping is not brute-forceable.** AES-256 for the key wrap, so a party holding ciphertext and
   wrapped key without the KEK has no attack better than exhausting the key space — which is also why
   keys are not derived from anything memorable.
4. **Destruction reaches every copy of the key, including the recovery window.** **ASSUMPTION:** §14.1
   A4 — the per-tenant KEK uses the shortest key-store recovery window the platform permits consistent
   with the customer's backup policy, and the *actual* window is disclosed in the erasure receipt. A
   receipt claiming immediate destruction that is wrong is worse than one stating the window correctly.

| Mode | "Destroyed" means | Who acts | What the receipt can honestly say |
|---|---|---|---|
| 1 | The vendor deleted the tenant KEK, after the configured recovery window | The vendor, on offboarding or an approved erasure | "The key was deleted at T; the recovery window expired at T+N; the ciphertext is undecryptable by us" |
| 2 | The customer disabled or deleted the key in their own vault | **The customer** | "The customer destroyed the key at T. We cannot decrypt it and cannot prevent this" |
| 3 | The customer destroyed the key in their HSM | The customer | The same — and the vendor has nothing to show, which is the point |

**One row in that table now has a footnote, and it is the row that matters commercially.** In modes 1
and 2 the receipt is a true statement about **ciphertext** and a **false** statement about the tenant's
content once `full_text` is enabled, because `ingest.search_text` holds prompt text that was never
encrypted under the KEK and is therefore untouched by destroying it. A tenant that turns on `full_text`
has a third copy of its prompt content — the index — that erasure must reach by **row deletion and
expiry** rather than by key destruction, which is exactly the mechanism **D1** already prefers for events
because it produces a literal receipt. §11.4 states what that receipt must then say, and §12 R15 records
the residual: an index row that neither mechanism reaches by the time the receipt is written is content
the receipt claims was removed.

Modes 2 and 3 carry a consequence the vendor must be candid about: **the vendor cannot guarantee
availability of content it cannot decrypt.** A customer who destroys a key during a dispute has
destroyed the evidence and the vendor cannot stop them. That is the correct trade — it is what "the
vendor cannot read it" means from the other side — but it belongs in the contract, not in a surprise.

### 6.5 The export path is no longer the answer to content search

Brief §3.6 requires bulk export to the customer's own storage in a columnar format on a schedule,
because "one customer in ten will want to run their own SQL". **D6 made that path carry the search
requirement.** D6′ removes that job from it: the product now answers "find every prompt mentioning this
project" itself, at `full_text`, so the export goes back to being what brief §3.6 asks for and no more.

What does not change is the shape of the export or the reason for its limits: it lands in storage **the
customer owns**, under keys **the customer holds**, indexed by **the customer's tooling** (B8); it
carries metadata and labels; and it **still excludes `content_digest` and `dedup_key`** (§5.5, §14.1
A6), because the reason for that exclusion was never the search decision — a digest in customer storage
is a correlation surface and a confirmation oracle wherever it sits.

What does change is that the export **must not become the leak that the search answer avoids.** Two
consequences follow and are asserted in §11.3: `body` and `tsv` from `ingest.search_text` are **never
in the export** — the export is not a second, unbounded copy of indexed content in storage the vendor
cannot delete from — and a per-subject export or erasure now has to resolve the index as well as the
content store. A customer who wants searchable content *in their own warehouse* still needs a
component **inside their environment** that the vendor does not operate and cannot read (master doc
Q11, **not assumed** here — §14.1 A8); what is no longer true is that this is the only content search
the product offers.

### 6.6 What D6′ is, stated without softening

**This change makes the product weaker as a confidentiality system, for the tenants that use it, and it
buys them a capability they asked for.**

**For a `full_text` tenant the vendor can read prompt content, because an index over plaintext is itself
plaintext-derived.** No cryptographic trick gives both search and vendor-blindness — searchable
encryption leaks access patterns and would be indefensible in a security questionnaire — so the honest
statement is the plain one, and it is the sentence a customer's security reviewer will find in this
document before a sales engineer finds a way around it.

The rest follows from that sentence rather than softening it:

- **The old universal promise is gone.** "The vendor cannot read your content" was true of every tenant
  and is now true only of `customer_held` tenants, and only while they stay on `disabled` or
  `attachment_names`. A customer-held tenant's promise is *stronger* than it was, because it is now
  enforced by a database constraint rather than by a decision (§6.1); the promise that disappeared is
  the one that covered tenants who never needed it.
- **`customer_held` remains fully supported**, and it is not a deprecated path: those tenants get
  structured filtering plus filename search, which is a real tier and not a consolation prize. What
  they do not get is prompt-text search, and no configuration on their tenant can give it to them.
- **The index is a new asset, ranked as content, and treated as content** (§3 A2, §5.6). It is a
  plaintext-derived copy of everything indexed, it is the single richest target the product now holds
  per byte because it holds text without holding a key, and the controls around it are the ones §13
  asserts invariants for rather than the ones §6.2's key model supplies.
- **Key destruction no longer erases everything** (§6.4, §11.4, §12 R15). For a `full_text` tenant,
  erasure is a deletion problem in two stores instead of one, and a key-destruction receipt that does not
  say so is a false receipt.
- **The approval gate no longer covers all content exposure** (§6.3, §10.5, §12 R14). Content reading
  moved from "one event, one case, two people" to "one query, bounded snippets, one person, audited".
- **`content-vault` became an interface as well as a component** (§6.3, §13 invariant 24). The
  confinement survived; the thing confined got bigger.

A security document that quietly re-justified this decision would be worse than useless, so the position
is recorded here as a **deliberate, customer-driven reduction in the confidentiality guarantee, taken
with the incompatibility enforced in schema and the residual costs named in §8, §10 and §12** — not as a
change that turned out to be free.

---

## 7. Tenant isolation (C32, D2)

Brief §3.3 requires isolation enforced **at the storage layer**, so a cross-tenant read is
*structurally impossible, not merely unauthorised*. That has a testable meaning: no SQL statement a
correctly-configured application role can execute may return another tenant's rows.

### 7.1 The mechanism

```sql
-- tenant_id is the leading column of every primary key and every index (C32).
CREATE TABLE ingest.observation (
  tenant_id uuid NOT NULL, event_id uuid NOT NULL, -- ...
  PRIMARY KEY (tenant_id, event_id)
);

-- Isolation is a database policy, not an application convention.
CREATE FUNCTION ops.current_tenant() RETURNS uuid LANGUAGE sql STABLE AS $$
  SELECT nullif(current_setting('app.tenant_id', true), '')::uuid $$;

ALTER TABLE ingest.observation ENABLE ROW LEVEL SECURITY;
ALTER TABLE ingest.observation FORCE  ROW LEVEL SECURITY;   -- applies to the owner too
CREATE POLICY tenant_isolation ON ingest.observation
  USING      (tenant_id = ops.current_tenant())
  WITH CHECK (tenant_id = ops.current_tenant());

CREATE ROLE sac_ingest NOBYPASSRLS;                         -- non-owner, cannot bypass
```

Four properties make this more than a filter, each asserted separately in §13. **`FORCE`, not merely
`ENABLE`:** row-level security does not apply to a table's owner by default, and without `FORCE` any
migration or maintenance path running as the owner reads everything, which C32's "structurally" does
not permit. **Application roles are non-owners without `BYPASSRLS`:** only `sac_migrator` holds it, it
does DDL only, and no runtime service is a member of it — `BYPASSRLS` is the one attribute that
converts a structural control back into a conventional one. **The tenant comes from the authenticated
session, never from the request body:** it is derived from the device credential (B1) or the Entra
token claim (B9) and set on the session inside the same transaction that runs the query, never
supplied by the caller. And **a session with no tenant set reads zero rows, not all rows:**
`ops.current_tenant()` returns `NULL` when unset, `tenant_id = NULL` is `NULL`, and a policy that is
not true excludes the row — the correct fail-closed default, and one that must be tested explicitly,
because an unset tenant reading everything is the classic RLS implementation error.

**`ingest.search_text` receives exactly the same treatment and none of it is optional**: `tenant_id`
leading in the primary key and in both GIN indexes' backing structures, `ENABLE` **and** `FORCE` row-level
security, and a runtime role that does not own it and does not hold `BYPASSRLS`. The reason it needs
saying at all is that this is the table where a missing policy is worst: a cross-tenant read that
escapes RLS here does not return unreadable bytes or an unattributable label, it returns **another
tenant's prompt text in the clear** (§7.2). The catalogue test behind §13 invariant 7 therefore matters
more for this table than for any other, and the cross-tenant canary (invariant 9) must be planted in it.

### 7.2 Per-tenant keys as the second, independent layer

Row-level security is a *policy* layer enforced by database code, and a migration can weaken it. The
second layer is arithmetic and depends on no query being correct: every tenant has its own KEK (§5.3),
so a cross-tenant read **returns ciphertext the reader cannot decrypt**, and the failure stops being
"unauthorised disclosure" and becomes "unreadable bytes". Row-level security fails when a migration
drops a policy, a table is created without one, or a role acquires `BYPASSRLS`, and when it fails, rows
become *visible*; the KEK layer fails never, because it is arithmetic, and when it holds, rows become
*unreadable*. Neither layer is claimed sufficient. **ASSUMPTION:** §14.1 A1 — the per-tenant KEK is
mandatory for every tenant in every custody mode, including mode 1; without it, layer two does not
exist for vendor-managed tenants. The schema enforces the related rule that a KEK must exist before
the ceiling reaches M3, the mode at which content is stored:
`CHECK (ceiling_mode <> 'm3' OR kek_id IS NOT NULL)`. An M2 tenant may therefore have no KEK.

**The second layer has a hole, and D6′ is what puts it there.** The KEK layer holds for
`ops.content_object` because those rows are ciphertext. **`ingest.search_text` stores plaintext**, so a
cross-tenant read of the index is not converted into unreadable bytes by any key: it is converted into
nothing at all. For `full_text` tenants the index is therefore protected by **one** layer where the
content store is protected by two, and that layer is the policy layer — the one this section explicitly
says a migration can weaken. This is the honest form of §12 R8 after the reversal: RLS drift used to
threaten labels, aggregates and the identity map (A5, A6); it now also threatens prompt text (A2).
**ASSUMPTION:** §14.1 A12 — that the index is not additionally encrypted at rest under a vendor-held
key. Doing so would raise the cost of a stolen backup or a restored replica (§8 T14) without changing
the vendor-readability conclusion, since the vendor holds that key; it is recorded as an assumption
rather than a decision because the ADR does not state one.

### 7.3 The deliberate deviation from a literal reading of C32

C32 reads: tenant is leading in every index **and every partition**; isolation enforced at the storage
layer. Master doc **D2** does not partition in v1, because four years of a full tenant is under 18M
rows and partitioning would add migration and query complexity for no benefit. What is delivered
instead: tenant leading in every primary key and index, row-level security forced on every table, and
an event table built so monthly range partitioning on receive time can be added later without an
application change, triggered at 50M event rows for a tenant. This is a recorded deviation from the
literal text, tracked as **ADR 0009**. The security consequence is **nil**: partitioning is a layout
decision, and the isolation properties above hold identically with or without it.

**A second deviation is now in view and it is not nil.** The index is tenant-scoped and tenant-leading,
which satisfies C32 in form. But it is the first tenant-scoped table whose confidentiality does not rest
on a key, and D6′ does not partition or otherwise segregate it per tenant either — it is one table with
two GIN indexes, shared across tenants under RLS. **ASSUMPTION:** §14.1 A13 — that per-tenant or
per-tier partitioning of `ingest.search_text` is a later optimisation rather than a v1 requirement. The
reason to state it is that partitioning would give back a *layout* layer of separation, and the reason not
to require it now is that the honest exposure is one shared table plus one policy layer, which is easier
to reason about than a half-partitioned design. §8 T16 is the threat that argues for revisiting it.

---

## 8. STRIDE analysis

Each row names the asset from §3, the boundary from §2.2, the mitigation in the design and the
residual that remains. Likelihood and impact are for the residual, judged for a 5,000-device tenant
with a security team of two to five people.

| # | STRIDE | Threat | Asset | Boundary | Mitigation | Residual |
|---|---|---|---|---|---|---|
| **T1** | I, S | **Compromised managed device.** Malware or the user's own administrator reads the spool, extracts the device key, or requests grants to pull content the device already observed | A1, A9, A10 | Device, B1, B5 | Device private key non-exportable and hardware-backed where the platform allows; spool encrypted under an OS-wrapped key; grants per-event, single-use and server-decided, so a device can request but not obtain content; revocation on report; the server never pushes content | **Medium.** A live compromised device holding content it was legitimately granted is indistinguishable from a healthy one. Content the server never granted stays unreadable to the attacker — the design working, and the reason the M3 grant budget must stay small |
| **T2** | S, E, I | **Malicious insider with a valid device credential.** A real employee on a real managed device sends forged or inflated events to hide their own activity, or exfiltrates the credential | A4, A5, A7, A9 | B1 | Transport binding makes the credential unusable off that device; enrolment is authenticated and device-bound; labels are checked against a validated classifier release; per-device volume anomalies are detected from the event stream itself | **Medium.** A device can always lie about what it observed. The server detects inconsistency and volume, never truth. This is the defining residual of endpoint-side interpretation |
| **T3** | I | **Hostile or over-scoped analyst.** Legitimate credentials used to build a picture of an employee rather than to investigate a security event. **Under D6′ the same actor has a second and cheaper route to content — see T13** | A2, A5, A6, A7 | B9, B13 | Content *retrieval* needs a case reference and a second approver (C16); every subject-level read audits as it is served (C30), **including every search and every zero-result search** (§6.3); aggregates are the default and subject-level requires a disclosed reason; the audit log is readable by a separate `auditor` role | **Medium-High, raised from before.** The product is a subject-level monitoring tool; an analyst with a plausible reason can produce a plausible-looking dossier. §10.1 states the honest position: caps and audit raise the cost, they do not prevent it. **The reversal widens this residual rather than T13 being a separate problem** — the same analyst now reaches content text without the second approver, so the control that used to bound this row no longer applies to one of the two content paths |
| **T4** | I, T, E | **Compromised vendor operator.** Infrastructure access used to unwrap a tenant key, **read the plaintext index without an unwrap**, alter retention, or suppress a health signal | A1, A2, A3, A4, A7, A8 | B10, B12 | No standing production access; just-in-time elevation with approval and a ticket; unwrap confined to one service; **under modes 2 and 3 the unwrap is recorded in the customer's own key store** (§6.2); audit is append-only and hash-chained; reconciliation detects retention and policy drift; **the index is reachable only by `sac_vault` and only through a service with internal-only ingress, and every read of it through the search path is audited** | **Medium under mode 1; Low-Medium under modes 2–3 — and the second figure is now conditional.** Under vendor-managed keys a determined operator inside the platform trust domain can reach content, with detection and audit as the only controls. **For a `full_text` tenant the mode 2 and mode 3 guarantees do not cover the index**: reading prompt text from `ingest.search_text` needs no unwrap, so nothing appears in the customer's key store when it happens. The custody-mode control the customer chose still holds for content *objects* and no longer holds for indexed content (§6.2, §14.3 Q-e) |
| **T5** | T, R | **Tampering with the audit log.** Deleting or reordering entries to erase the record of a retrieval, **a search**, a mode change or an export — including by the party whose act it records | A7, A8 | B9, B10 | Append-only enforced three ways: no runtime role holds `UPDATE` or `DELETE`; a statement-level trigger raises for every role including the owner; a per-tenant hash chain with the chain head anchored to write-once storage. The audit write is in the same transaction that serves the read (C30), **and for a search that transaction fails closed: no audit row, no results** (§6.3) | **Low** for undetected tampering; **Medium** for coverage gaps. The chain proves the log was not altered; it cannot prove that every act requiring an entry produced one, which is why §13 asserts the write path per operation. **A search raises the value of this row rather than changing its likelihood**: the log now records what analysts were curious about as well as what they opened (A7) |
| **T6** | S, T | **Forging or replaying events.** Injecting fabricated submissions to inflate a rival's numbers, or replaying a captured batch to corrupt counts | A4, A5, A9 | B1 | Per-device credential with transport binding; idempotency enforced by a store constraint, not code (C12); server-assigned `received_at`, which a device is contractually forbidden to send; device time retained separately so skew is visible rather than normalised; duplicates counted and reported | **Low-Medium.** Replay is neutralised by the store. Forgery from a genuine device is T2, not T6. The residual is count corruption among a tenant's own devices, which reconciliation surfaces but does not prevent |
| **T7** | I, S | **Bypassing interception.** A client that ignores the system proxy, an HTTP/3 client using QUIC, a pinned-certificate client, or a user-disabled extension — any of which moves content off the device unobserved | A4, A5 | B11 | Interception scoped to enumerated destinations; QUIC disabled by policy **and** UDP/443 blocked at egress, because browser policy alone is insufficient; CLI trust and proxy configured centrally through a managed shell profile; a pinned or misconfigured client is **excluded and recorded as a named coverage gap** (`ops.coverage_snapshot.gap_reason`), never left broken; coverage is an output of the collection path (R11) | **High — a coverage residual, not a confidentiality one.** The product does not prevent a user reaching an AI tool; it reports that it could not see. Some fraction of traffic is unobserved, which is why A4 outranks the labels. **D6′ sharpens the honesty here**: content the product never observed is content the index does not contain, so a `full_text` tenant's search results are complete over *retained indexed content* and silent about coverage — the snippet panel must not be read as "this is everything" (§11.3) |
| **T8** | S, T, I | **Root certificate abuse — the interceptor turned against the user.** The component that intermediates TLS is compromised and its position used to read traffic beyond the enumerated destinations | A11, A1 | B11 | Root private key offline and non-exportable; short-lived leaves minted **only for enumerated destinations**; interception destination-scoped rather than fleet-wide packet inspection (brief §1.2); fail-open with port release; signed updates, ring deployment with automatic halt, and a server-side kill switch (C36); direct egress still works when the provider is off | **Medium-High, accepted.** A compromised interceptor has the largest blast radius in the product and no control removes that — they bound it. §9 and §12 say so rather than describing the scoping as a solution. **Unchanged by D6′, and D6′ adds nothing to it**: the interceptor still reads only what it intercepts, and the new exposure is server-side |
| **T9** | E, T | **Document parser exploit.** A crafted PDF or office document attached to an AI tool exploits the parser and gains execution inside the process holding model weights and spool access | A1, A10 | B2, device | Highest-risk code in the product (brief §5.1, R8): parsing runs in a **child process** spawned by the classifier host, with a memory cap and hard timeout, so an exploit cannot reach model weights or spool keys. The parser never runs in a browser context | **Medium.** Isolation converts a parser exploit from full device compromise into a sandbox escape plus a privilege boundary — real, but not free. The residual is an escape from the child into the classifier host. **D6′ leaves it unchanged but worth restating**: the index holds filenames and prompt text, never parsed attachment text, so a parser exploit does not become an index-poisoning primitive (§6.3) |
| **T10** | D | **Denial of service on ingest.** A flood of batches, or a device replaying at maximum rate, delays ingest past usefulness or exhausts the query path | A4, A13 | B1 | Per-device credential required; per-device rate limits at the edge; ingest decoupled from aggregation so a slow aggregator cannot stall it (C13); devices buffer through outages (brief §8's 99.9% target); the drop counter makes resulting loss **visible** | **Low-Medium.** A small authenticated surface. The residual is that a *legitimate* device flooding at maximum rate is indistinguishable from a fault, and the response is a rate limit that looks like the T7 coverage gap. **D6′ adds a query-side variant**: a search is far more expensive than a filtered list read, so the search path needs its own bound (result cap, timeout, index-only ordering) or an analyst with a broad scope can degrade the query layer for everyone |
| **T11** | E | **Privilege escalation through the collector.** A local unprivileged process attacks `capture-core` for its keys, its policy bundle, or the ability to make it act for the attacker | A9, A10, A11 | Device | The privileged surface is kept small: proxy provider, the loopback broker on a bound port, process enumeration, the spool. IPC is reachable only by installed components; the broker must never hold the port if it cannot serve and must release it on crash (E14); the interceptor fails open; a tampered policy bundle causes a signature failure and a fallback to M0, never to permissive behaviour (C10) | **Medium.** A privileged service on an untrusted device is a target. The design's answer is a small privileged surface plus fail-closed policy handling, not the absence of a target. Inherent to D3 |
| **T12** | R, T, I | **"Reports clean while collecting nothing."** A path disabled, tampered with or silently broken while the console shows a healthy tenant, so every downstream decision assumes the visible data is all the data (brief §7) | A4, A13 | Device, B1, B3, B6 | No collection path may fail into a reported success (C25); per-provider state, version, last successful capture and permission state (C23); tamper reported rather than inferred from absent events (C24); a server-side liveness view (`mart.v_device_liveness`) marks a silent device `stale` or `never_reported`, so silence becomes a record; spool overflow increments a **reported** counter (C22); coverage state is an output of the collection path (R11) | **Medium, and the highest-consequence residual here.** The attack needs no exploit: disabling an extension, breaking a trust store or letting a certificate expire all yield a green console. The controls convert silence into a record, and a record is only as good as the person reading it |
| **T13** | I, E | **Search used as a bulk content-reading tool, bypassing the per-event approval gate.** An analyst with legitimate `full_text` access runs broad or iterated queries — a common term, a department name, a person's name, a wildcard-ish substring — and reads prompt content at volume. The approval gate that protects content *retrieval* (C16) is not in this path at all, so the evasion is achieved by using the feature as designed | A2, A1, A5 | B9, B13 | Every search audits in the same transaction and fails closed (§6.3, C30); the tenant tier is a **ceiling** narrowed per scope by the signed bundle, so search is never global (§6.1, C3); snippets are **bounded** rather than full text, with the bound recorded as configuration (§14.3 Q-f); result sets are capped and cursor-paginated (brief §3.6) so a corpus cannot be drained in one query; the audit log records the terms searched, is readable by the separate `auditor` role, and a per-analyst query rate and breadth baseline is a reviewable signal | **High, and this is the most serious single consequence of the reversal.** Every mitigation above is *detection or friction*, not prevention. An analyst with a plausible reason and a broad scope can read a great deal of prompt text with one approval-free workflow, and the snippet bound only sets the price per query — it does not cap the number of queries. **The honest reading is that approval for content exposure is no longer a property of this product for `full_text` tenants**, which is why §6.3 says plainly that a tenant requiring approval for any content exposure should not enable the tier, and why §10.5 exists |
| **T14** | I | **Index contents leaking through backups, replicas or logs.** Prompt text in `ingest.search_text` escapes the primary through a mechanism that never had a content-read authorization decision: a database backup, a read replica or a point-in-time-restore copy in another region or subscription, a query-plan or slow-query log capturing the literal terms, a `tsvector` in an error payload, a support dump, or an analytics/CDC feed | A2, A1 | B13, B10 | The index is confined to one table readable by one role, with no export path (§6.5, §13 invariant 24/28); **backups, replicas and point-in-time restore must be enumerated as index-bearing stores**, and the third copy is where C34's two independent expiry mechanisms have to be reconciled (§6.3); statement logging must not capture the `tsv` or the matched text — the search path binds terms as parameters and the slow-query log is configured to record the query shape, not literals; support-bundle tooling is on the §1.3 vendor-estate path and must not be able to `SELECT` this table; the erasure receipt's `remaining_counts` covers index rows so a restore that resurrects them is a recorded state rather than a silent one | **Medium-High.** The primary is controlled; the copies are where this class of asset usually escapes, and the design cannot honestly claim to control a restore performed by an operator or a region-level replication the platform performs on the vendor's behalf. The residual includes the case where a restored copy is *older* than an erasure, which is a deletion failure the receipt cannot see (§12 R15). **ASSUMPTION:** §14.1 A14 — that exclusion of `ingest.search_text` from backups, replicas and statement logs is achievable by configuration rather than requiring a separate store |
| **T15** | I | **A search query as a confirmation oracle.** A query returns counts, snippets or timing that let a caller confirm a fact about content they cannot read: whether a term exists at all (a zero-result versus non-zero-result signal), how many prompts mention a name, which tool or period it appears in, or whether a guessed phrase is present. The attacker supplies a hypothesis and reads the answer | A2, A5, A4 | B9, B13 | Result counts and snippets are returned only to an authenticated, tenant-scoped, audited principal, so a probe is attributable; **every search is audited including zero-result searches**, which is what turns a quiet oracle into a recorded series; match counts are bucketed or withheld below a floor so "exactly 1" is not disclosed where a tenant's population is small; the scope-narrowed tier means existence can only be probed within scopes the tenant enabled; the `content_digest` oracle in §5.5 is *not* widened by this — digests remain internal-only and absent from exports | **Medium, accepted.** Attribution does not stop a patient insider, and a legitimate analyst probing content they are entitled to reach is hard to distinguish from one confirming a guess. **Cross-tenant probing is structurally excluded** by tenant-leading keys and forced RLS (§7.1) — this oracle operates inside one tenant. **The residual is bounded by the fact that the index holds only what the tenant decided to index in the first place**; a tenant that did not enable `full_text` has no such oracle |
| **T16** | I | **Cross-tenant inference through term statistics.** A query's result count, ranking, highlighted fragments or timing reveal information about *other* tenants' content: shared index structures and a shared query planner mean per-tenant term frequencies, corpus sizes and document-length statistics can leave a signal in a response, or be recovered by an operator who can run the same query across the table | A2, A5 | B13, B7 | Tenant from the token only (§4.1); tenant-leading keys and forced RLS on `ingest.search_text` (§7.1); `query-api` cannot address the table at all, so only `content-vault` composes a query and it composes it **always tenant-scoped, never global**; scoring is computed over the tenant's own rows rather than a global statistics table — no cross-tenant document-frequency structure is maintained; the entry that is never created is the one that leaks, so a shared statistics or dictionary table is prohibited and asserted in §13 | **Low-Medium.** No mechanism in the design computes statistics across tenants, and RLS makes the direct read structurally impossible — this row is in the table because the *absence* of a cross-tenant structure is a property that must be maintained as the feature grows, not because a leak exists today. The residual is a query-planner or index-level side channel that only an operator with database access could reach, which is T4 with a different name. **ASSUMPTION:** §14.1 A13 — per-tenant partitioning of the index is deferred; if it is revisited, this row is the reason |

Not covered by this table: insider abuse by legitimate parties (§10, which has no attacker and no
boundary crossing), supply chain (§1.3), and physical theft as a distinct case (T1, with the spool's
OS-level wrapping as the only additional control — §5.4 says what that is worth).

**The four rows D6′ adds — T13 to T16 — are one decision seen from four directions**, and none of them
is a bug that can be fixed by tightening code. T13 is the approval gate no longer covering all content
exposure; T14 is the index existing in more places than the authorization path knows about; T15 is the
index answering questions rather than returning documents; T16 is the index not being per-tenant at the
storage level. Their residuals are the honest price of §6.3's feature, and §12 R14–R16 record them as
accepted rather than mitigated.

---

## 9. The interceptor as a security liability

The egress proxy provider installs a root CA and intermediates TLS for enumerated destinations
(brief §5.2, §5.5; R4). Mechanically it is a man-in-the-middle: it presents a certificate the client
trusts, terminates the connection, reads the plaintext and re-originates the request. Every technique
it uses is one an attacker uses.

### 9.1 The blast radius, stated exactly

| If this is compromised | The consequence |
|---|---|
| A leaf certificate's private key | Forgery for the enumerated destinations until the leaf expires. Bounded by short lifetimes and the destination scope |
| The signing intermediate | Forgery for any destination, from any machine trusting the root, until the intermediate is revoked — a fleet-wide operation |
| The root's private key | Every TLS connection from every enrolled machine is forgeable, indefinitely, by whoever holds it |
| The proxy **process** (no key stolen) | Read and modify access to whatever it intercepts on that device while compromised — the ordinary MITM position, reached without stealing a key |
| The **update channel** for the component | Code execution with that position across the fleet. The worst case, and the reason C36 exists |

The distinction that matters is between the last two and the first three. The design keeps private keys
off endpoints wherever it can — root offline, intermediate issuing leaves, leaf non-exportable in the OS
key store — so the *common* compromise is a process compromise on one device, not a fleet-wide key
compromise. That is a meaningful reduction and not a guarantee: a compromised interceptor process on a
device is still a MITM for that device, and no architecture that reads TLS can say otherwise.

### 9.1a The index does not change this component, but it does change the ranking of liabilities

**D6′ changes nothing about the interceptor, and that is worth stating explicitly rather than leaving it
to be inferred.** Collection is unchanged: content still crosses only on a grant, the interceptor still
reads only enumerated destinations, and `full_text` creates no new device-side egress. A tenant that
declines the interceptor still gets the same product minus modes D, E, G and H — and that tenant's
`full_text` index is correspondingly thinner, because there is less content for it to hold. **The
interceptor's blast radius is untouched and it is no longer the only concentrated liability in the
design.** §3 now ranks `ingest.search_text` (A2) directly below the content itself and above the key
tuple: it is one table, in one database, holding searchable plaintext for every `full_text` tenant, and
its compromise is a *server-side* event with no certificate, no interception and no device involved.
The two liabilities are not comparable in kind — the interceptor is a capability that reads traffic in
flight, the index is a store that holds text at rest — but a reader who takes §9.4's "concentrates risk
as nothing else in the product does" as still literally true has missed the reversal. The accurate
version after D6′: **the interceptor concentrates risk on the endpoint; the index concentrates risk in
the vendor's own database; both are accepted, and only the first has a deployment choice attached to
it** (§6.6, §12 R14–R16).

### 9.2 Why the component concentrates risk, and why it exists anyway

Brief §2 requires full content for modes D, E, G and H — desktop AI applications, IDE and CLI coding
agents, scripts calling provider APIs, and agentic flows. Those clients honour the system TLS stack and
proxy, and there is nowhere else to observe them. Master doc Alternative C (per-runtime shims, no
interception) was rejected as the primary mechanism because it cannot reach closed desktop applications
at all, cannot see a script that ignores the shim, and fails the requirement that discovery classify
*behaviour* rather than match an enumerated list (brief §2). So the choice is an interceptor, with a
root certificate and everything in §9.1, or no visibility into four of nine usage modes. The design
takes the interceptor, **builds it last** (build step 6, after the product works without it), and bounds
it below. Naming this as a concentrated risk is the point; a document describing interception as
"scoped, therefore safe" would be misleading.

### 9.3 The bounds, and what each is worth

**Fail open, always.** If the proxy cannot serve it releases the port, restores the system proxy and
lets traffic egress directly, so egress continues unobserved rather than breaking. A security product
that takes the customer's network down gets uninstalled, and the coverage gap is recoverable while the
outage is not. Paired with master doc §4.4's reporting requirement, the provider goes `absent` and the
coverage report names the affected modes — failing open is loud rather than quiet.

**Interception is scoped to enumerated destinations, not all traffic.** Everything else is
blind-tunnelled, which is stronger than "we only look at AI sites": the component has no capability to
read a connection it was not configured to read, and the destination scope is part of the signed policy
bundle (B3). It is also what keeps the product inside brief §1.2's prohibition on fleet-wide packet
inspection — a central gateway must decrypt before it can decide and therefore inspects everything,
while a destination-scoped interceptor does not.

**The exclusion artefact and the bake period.** A component that installs a root and intermediates TLS
resembles malware to other endpoint security products, and brief §5.5 says to expect false positives
(R4). Two deliverables follow: **an exclusion artefact ships with the component** — the paths, process
names, certificate thumbprints and behaviours other endpoint products need excluded, packaged so a
customer's IT team can deploy it rather than reverse-engineer it — and **a bake period precedes wide
rollout**, on a small ring with an explicit promotion criterion, so those reactions are discovered
before they reach 5,000 machines.

**The server-side kill switch.** The provider can be disabled from the server without shipping code
(C36), which is what makes a bad update survivable: signed manifests, ring deployment with automatic
halt, atomic install with rollback and strict separation of content from code reduce the probability,
and the kill switch bounds the consequence when they fail.

**Interception is not the product.** It arrives last, so a customer can run modes A, B, C, F and I with
no certificate on any machine. A tenant that declines the interceptor has a smaller blast radius and a
smaller coverage number, and knows which one it has (R11, C23).

### 9.4 The candid summary

This component concentrates risk on the endpoint as nothing else in the product does — **the qualifier is
new, and D6′ is why**: the server-side full-text index (§3 A2) is now the other concentrated risk, and it
holds searchable plaintext with no certificate and no deployment choice attached. The interceptor reads
plaintext that was not addressed to it, on machines the vendor does not own, using the same mechanism an
attacker would. It is justified because four of the nine required usage modes are otherwise
unobservable, and bounded by destination scoping, an offline root, short-lived leaves, fail-open
behaviour, a bake period and a kill switch. A customer who does not accept that trade should not deploy
the interceptor provider, and the product must make that choice honest rather than presenting coverage as
all-or-nothing. **The same sentence now has to be available for the index**: a customer who does not
accept the server reading indexed prompt text should stay on `attachment_names`, and §6.3 requires the
product to say so at the point of the decision rather than in a footnote.

---

## 10. Abuse cases — misuse by legitimate parties

Each is a party using the product as designed, for a purpose the design did not intend. Where the
honest answer is "the product cannot prevent this, only record it", that is the answer given.

### 10.1 An analyst using the product for surveillance rather than security

The realistic pattern: an analyst filters subject-level data by person, repeatedly, over months,
building a picture of an employee's activity or associations without ever opening a case.

**What limits it:** content retrieval requires a case reference and a second approver (C16), so
surveillance cannot reach prompt text **through the retrieval path** without a second person; every
subject-level read writes an audit entry **as it is served** (C30), so the pattern exists in the record;
the audit log is readable by an `auditor` role deliberately separate from the analyst role; and
per-subject time series are a first-class query shape (brief §3.6 question 6), so the product makes the
abuse pattern easy to query — and therefore easy to review.

**What it does not do: the product cannot prevent this.** A subject-level read with a plausible reason
is indistinguishable from a legitimate one, and this document will not invent a control that does not
exist. Whether the audit trail is ever read is an organisational property. The mitigation is detection
after the fact, which is why §11's framing matters: the system can attest that a read happened, and
cannot attest that it was justified. **D6′ removes one of the three limits named above for `full_text`
tenants**, and §10.5 is that change stated as its own abuse case rather than buried here.

### 10.2 A tenant configuring collection beyond what its population has acknowledged

An administrator raises a scope from M1 to M3, or widens the population, beyond what the deployed
notice describes — or enables a content-reading mode on a population that never acknowledged anything.

**What limits it:** the acknowledgement record exists (`ops.notice_acknowledgement`) and enrolment can
require a notice version before a content-reading mode is enabled (§11.1); mode is per-tenant **and
per-scope** (C1) and is applied before a bundle is signed, so a device can never exceed its tenant's
ceiling (C3); every change records who made it and when (C4); and the system cannot be configured into
an "upload everything" state (C5), because content crosses only on a per-event grant with a budget.

**What it does not do:** the system enforces *its own* configured ceiling, not the notice's terms.
**The product can only record an assertion that a notice was acknowledged, and enforce the limit it was
configured with.** Bridging that gap is the legal workstream's job; §11 does not pretend otherwise.

### 10.3 An administrator exporting data without a case reference

Bulk export to the customer's own storage (C31) and per-subject export (C18) are administration paths:
an administrator can export metadata and labels for an entire tenant on a schedule, into storage the
vendor does not control.

**What limits it:** exports are audited as acts and as configuration changes, with actor, scope and
parameters; the per-subject export is a designed support path rather than an ad-hoc query, so it
produces a manifest of what was exported and where (brief §4.5); and the destination is the customer's
own storage under the customer's own identity, so the vendor's configuration cannot redirect it
arbitrarily.

**What it does not do: once an export lands in customer storage, the vendor has no control over it, no
visibility into who reads it, and no ability to recall it** (§2.1). An export without a case reference
is not prevented, only recorded — and the receipt naming subject and scope is what makes it reviewable
even when it is not preventable.

**What D6′ adds to this case, in both directions.** The export is no longer the *only* route to content
search, so an administrator who wants to read prompt text no longer has to justify an export to do it —
which lowers the pressure on this abuse case and moves that pressure to §10.5. In the other direction,
the export must not become the leak that the indexed copy was confined to avoid: **`body` and `tsv` from
`ingest.search_text` are never in the export**, asserted in §13 invariant 28, because an export carrying
indexed prompt text would be a second unbounded content copy in storage the vendor can neither audit nor
erase (§6.5).

### 10.4 The vendor's own staff

| Vendor role | Technical limit | Honest assessment |
|---|---|---|
| Support engineer with production access | No standing access; just-in-time elevation with approval; cannot read content **objects** under modes 2 and 3; audit records the access | Under mode 1 a determined operator inside the platform trust domain can reach content (T4). Custody mode is the control the customer chooses. **For a `full_text` tenant this row is weaker than it reads**: the index is plaintext, so an elevation that reaches `sac_vault`'s data plane reaches prompt text with no unwrap for the customer's key store to record — see T4 and §14.3 Q-e |
| Platform operator | Managed-identity permissions scoped per service; **only `content-vault` can unwrap**, with internal ingress only (D7); key roles eligible-only with approval | No single operator identity holds both data-plane access and unwrap rights. That is not the same as no combination of elevations reaching both. **The index changes the second sentence**: a data-plane elevation reaching `sac_vault` now reads indexed plaintext as well as wrapped keys, so the combination to be feared is no longer "data plane **and** unwrap" but "data plane **alone**", for `full_text` tenants |
| Engineer with deployment rights | Signed artefacts only; deployments audited as configuration changes; kill switch and rollback are mechanisms an operator already has | Deployment rights are effectively the ability to change fleet-wide behaviour, which is why content is separated from code (C36) and updates are ring-deployed. **A deployment is also how a search default, a snippet bound or a scope-narrowing rule changes**, so §11.6's attestation of who enabled `full_text` and when covers configuration rather than only the enabling act |

**The candid position:** the vendor's own staff are the hardest population to limit, because they
legitimately need the access that would constitute the abuse. The design minimises the population
holding unwrap rights to one service, makes every elevation and unwrap auditable, and lets the customer
choose a custody mode in which the vendor's capability is recorded in the customer's own key store
(§6.2) or does not exist at all (mode 3). Beyond that, **the control is contractual and organisational,
and engineering cannot supply it. D6′ narrows what that sentence covers**: for a `full_text` tenant the
mode 2 and mode 3 records no longer capture every vendor read of content (§6.2, T4), so the contractual
and organisational control has more work to do than it did, not less.

### 10.5 An analyst reading content in bulk through search instead of through approved retrieval

This is T13 as an abuse case, and it is the most consequential thing D6′ does. The pattern has no
attacker and no boundary crossing: an analyst with legitimate `full_text` access, a tenant scope the
policy bundle permits, and a plausible reason — "looking for exposure of the Project X codename" — runs
search after search. Each returns bounded snippets. Enough of them reconstruct a corpus: a broad term
returns fragments of many prompts, a narrower term returns more of each, and iterating terms against a
stable corpus is a reading technique, not an attack. **Nothing in the workflow requires a case
reference, and nothing requires a second person.**

**What limits it:** every search writes an audit entry in the same transaction that serves it and fails
closed if it cannot (§6.3, C30), so the pattern is in the record including the zero-result queries; the
audit entry carries the terms, the scopes and the counts, which is what makes breadth reviewable rather
than merely present; the audit log is readable by a separate `auditor` role; the tenant tier is a
**ceiling** narrowed per scope by the signed bundle (C3), so no analyst can search everything; snippets
are bounded and result sets are capped and cursor-paginated, so the cost of reading a corpus is
proportional to its size rather than constant; and per-analyst query rate and scope breadth are
queryable signals in their own right, because the audit table is a first-class read path (brief §3.6
question 9).

**What it does not do: the product cannot distinguish this from diligent investigation.** Every control
above is detection, friction or a record — none is a gate. The second approver that bounds §10.1 exists
precisely because content exposure is supposed to be a two-person decision, and **search is the path that
does not have it**. An analyst who abuses search produces exactly the audit trail a thorough analyst
produces. **The honest answer to "what stops it" is: nothing in this design, and the only real control is
that the tenant's search scope was narrowed when the bundle was signed and that somebody reads the audit
log** — which is §12 R12's unread-report problem applied to a second, more sensitive report. A tenant
that is not prepared to review search activity should not enable `full_text`, and §6.3 requires the
product to say so at the point of the decision.

### 10.6 A tenant administrator enabling full-text search beyond what the deployed notice describes

The same shape as §10.2 one tier up. An administrator sets `content_search` to `full_text`, or raises a
scope from `attachment_names` to `full_text`, on a population whose notice and acknowledgements describe
a product that does not index prompt content for search — **which is what every notice written against
D6 describes.** The capability change is small on screen and large in substance: it converts retained
content from "retrievable by an approved path" into "queryable by any analyst with the scope".

**What limits it:** `full_text` is unrepresentable with `customer_held` (schema constraint, §13
invariant 25), so the combination cannot be configured by accident or by an administrator who has not
read §3.5; the tier is per-scope and applied before a bundle is signed (C1, C3), so a change is visible
in the signed policy history; **every change to the search tier records who made it and when (C4), and
§11.6 makes it attestable from the audit log** — which tenant, which tier, effective when, by which
actor; and the `full_text` tier requires the scope to carry M3, so enabling it on an M1 population does
nothing rather than silently extending collection (§6.1).

**What it does not do:** the system enforces its own configured tier, not the notice's terms, exactly as
§10.2 says for collection mode. **The product can record that a tenant enabled a content-search
capability, when and by whom; it cannot attest that the notice the population acknowledged described
that capability.** Bridging that gap is the legal workstream's job and §11.6 lists what that workstream
can obtain from the system rather than what it should conclude.

---

## 11. What the legal workstream needs from engineering

Capabilities only. No legal analysis appears here: the brief's scope note excludes that workstream, so
this section does not assert that any processing is lawful, cites no statute, and treats no obligation
as settled. Each item is a functional requirement per the brief's scope note, framed as **"the system
can attest that X happened."** Where the system can only record an assertion rather than prove a fact,
that is stated.

### 11.1 Notice acknowledgement, and gating a content-reading mode on it

Brief §3.2 requires a notice-acknowledgement record keyed by tenant, user and version, recording which
notice version a user acknowledged and when: *"this records the fact so enrolment can require a version
before content-reading modes are enabled."*

- **Can attest:** that user U in tenant T acknowledged version V at time X, and that no policy bundle
  enabling M1 or above was signed for a scope covering U before that record existed.
- **Cannot do:** verify the *content* of the notice, attest that the notice was adequate, or attest that
  the acknowledgement was informed. **The system records a user's assertion that they acknowledged a
  document the deployment workstream supplied**; it does not prove what that document said, storing it
  as a version reference rather than text the system interprets.
- **Enforcement point:** the gate belongs at policy signing, because C3 requires the mode in force to be
  applied before a bundle is signed. A device therefore cannot be handed a content-reading mode for a
  population with no acknowledgement record — the refusal happens where the bundle is made, not where it
  is consumed.
- **The unsolved case, stated rather than hidden:** a user who has not acknowledged and whose scope is
  already at M1. Whether that user is forced to M0, excluded from collection, or flagged is a
  deployment-workstream decision; the system supports all three and does not choose.
- **The case D6′ adds, and it is not the same gate.** Enabling `content_search = 'full_text'` exposes
  content to a *new class of reader* — any analyst with the scope — without changing what crosses the
  wire, so it is invisible to the collection-mode machinery §11.2 describes. The system can attest the
  same shape of fact for the search tier as for the mode (**§11.6**), but it cannot attest that the
  notice the population acknowledged mentioned search at all. **The system can attest that a tier was
  enabled; it cannot attest that anyone was told.** That distinction is the whole of what engineering
  owes here.

### 11.2 The per-tenant collection ceiling and its enforcement

- **Can attest:** the configured mode per scope, the effective mode applied to any given event, and the
  complete change history of both — who changed it, when, and from what to what (C1, C3, C4).
- **Enforcement:** the ceiling is applied before a bundle is signed (C3), so a device cannot exceed its
  tenant's configuration even if compromised; mode is the gate an observation passes through first,
  rather than a flag consulted by the upload path (master doc §4.2 step 3); and the per-event grant with
  a budget (`ops.tenant.content_budget_bytes_per_day`, C14) is what makes "upload everything"
  structurally unreachable (C5).
- **Cannot do:** attest that the configured ceiling matches anything external. It enforces the number it
  was given; §10.2 is the same point from the abuse direction.

### 11.3 The per-subject export

- **Can attest:** that a per-subject export was produced covering that subject's events and any stored
  content, within a bounded time, in a documented format, with a manifest naming what was included, what
  was withheld, and by which mechanism (brief §4.5, C18).
- **Cannot do:** produce content a tenant retains under customer-held keys without a customer-side
  component (§6.5, master doc Q11). Where that component does not exist, the export covers metadata and
  labels, and the manifest says so.
- **The gap it must report rather than hide:** events still in a device's offline spool are not in the
  export. An export is a statement about the store and must carry its own coverage statement rather than
  implying completeness (C25 applied to an export path).
- **What D6′ changes about the export, and it cuts both ways.** Brief §4.5 requires the export to cover
  "that subject's events **and any stored content**", and the index is now stored content — so **a
  per-subject export must resolve `ingest.search_text` as well as the content store**, and the manifest
  must say which of the two it drew from. For a tenant without `full_text`, the index contributes nothing
  and the manifest says that too, because "no indexed content for this subject" and "indexing is off for
  this tenant" are different facts and collapsing them produces a misleading export. **The subject whose
  text is in the index is not only the subject who sent it:** `tsv` statistics and snippets span the
  tenant, so a term from subject A's prompt can appear in a snippet served against subject B's search,
  and an export scoped to one subject cannot be a complete account of what a search would reveal about
  them. **ASSUMPTION:** §14.1 A15 — that this residual is reported in the export manifest rather than
  resolved, because resolving it would require per-subject index partitioning that §14.1 A13 defers.
- **The export must not carry the index.** `body` and `tsv` are excluded from both the scheduled Parquet
  export and the per-subject export (§6.5, §13 invariant 28). The export is the customer's copy under the
  customer's keys and the vendor cannot erase it; making it a second full copy of indexed prompt text
  would undo the confinement that §6.3's single-reader rule establishes, in storage the vendor can
  neither audit nor reach.

### 11.4 The erasure receipt

- **Can attest:** what was removed, when, and by which mechanism — a *literal* receipt naming counts and
  mechanisms (`ops.erasure_receipt.removed_counts`, `mechanisms`), which is the point of **D1**: deletion,
  not crypto-shredding, erases events. "These 1,247 rows and these 38 objects were removed" is
  attestable; "we destroyed a key, trust us" is not.
- **Two independent mechanisms, periodically reconciled** (C34), so no single deletion path is trusted;
  drift is recorded and alerted rather than silently corrected. The receipt also carries
  `remaining_counts`, because claiming total removal while a hold preserved something (C35) is worse than
  no receipt. Holds suspend expiry for a named scope with a visible scope and their own expiry; where a
  hold and an erasure request conflict the master document leaves precedence open (Q10), and until it is
  resolved the system records both facts rather than picking one.
- **Cannot do, and must say:** a receipt is a statement about systems the vendor operates. It cannot
  attest to copies already exported to customer storage (§10.3), nor to content on a device that has not
  reported, and it carries those limits rather than implying universality.
- **What D6′ adds to the receipt, and the receipt must say it.** For a `full_text` tenant the content is
  in **three** places, not two: the content store, the device spool, and **`ingest.search_text`**. The
  C34 mechanisms are stated as two and are now three, because index rows expire on `expires_at` as their
  own path rather than riding on either the row-level or the blob-lifecycle mechanism (§6.3). Two
  consequences follow, and neither is optional:
  - **A key-destruction receipt is no longer a content-erasure receipt for those tenants.** Destroying a
    tenant KEK or deleting a customer-held key leaves the index intact, so a receipt that says "the key
    was destroyed at T" and stops is a **false statement about the tenant's content**, not merely an
    incomplete one (§6.4, §12 R15). The receipt must state that indexed text was removed by deletion and
    expiry, and must carry `remaining_counts` for the index as well.
  - **The reconciliation has a third surface, and it can fail silently.** If the index purge path drifts
    from the other two, the result is indexed content that outlives the retention the customer was told
    about — with no key destruction to catch it. Drift is recorded and alerted (C34) or it is a silent
    retention failure, which is the §10.5 problem with an unbounded window.
  - **The independent mechanisms must be independent for the index too.** A single scheduled purge that
    is also the only path is the single deletion path C34 forbids; §14.3 Q-g records that the second
    index-expiry mechanism is not yet chosen.

### 11.5 The audit trail and the collection-mode audit

- **Can attest:** every mode change, content grant, content reveal, **every search including its terms,
  scopes and result counts**, export and configuration change, with actor and time (brief §3.2, C4,
  C30), append-only, hash-chained, and independently readable by an auditor role — including the full
  attributed history of a collection mode per scope and its correspondence with the bundles signed
  after it. Brief §1.1 makes that last part a defect threshold: *"a mode that changes silently is a
  defect."*
- **Cannot do:** attest to reasons, or to consistency with anything external. An entry shows content was
  retrieved under a case reference; it does not show the case reference was genuine, **and a search entry
  does not carry a case reference at all** — that is the concession §6.3 makes. It records what was
  configured and by whom, and what was searched and by whom: the input that workstream needs, not the
  conclusion it will draw.
- **The property that matters most for search.** Brief §3.6 requires the audit entry to be written *as
  the read is served*, and §6.3 makes it fail closed. So the record of a search is not a log line emitted
  afterwards by a best-effort path — **if the audit write fails, the search returns nothing.** An
  unlogged search is a defect, not an incident to be reviewed, and §13 invariant 26 asserts it.

### 11.6 Which tenants enabled `full_text`, when, and by whom

**Enabling `full_text` is not a configuration change with disclosure consequences; it is a change in what
the product does with content, and the system must be able to attest to it as an act.** This section
lists the capability and stops; whether enabling it triggers a disclosure obligation, a notice change or
a reassessment is the legal workstream's question and this document does not answer it.

- **Can attest:** for each tenant, the value of `content_search` and which actor set it, with the
  effective time and the previous value — including the **scope-level** narrowing in the signed policy
  bundles, so the record shows not only that a tenant enabled `full_text` but which tools, classes and
  populations it was enabled for (C1, C3, C4). Narrowing is a change too, and a record of enablement that
  omits subsequent narrowing overstates the exposure in one direction and understates it in the other.
- **Can attest:** that a per-tenant capability state is **current rather than inferred from the audit
  trail alone** — the tenant configuration row and the audit history must agree, which is the same
  reconciliation posture §11.2 takes for the collection ceiling.
- **Can attest:** that the change was possible at all, in the schema sense: that no `full_text` tenant
  holds `customer_held` keys, because that state is unrepresentable (§13 invariant 25). This is a
  stronger form of attestation than a log entry — it is a statement about what the system can express.
- **Can attest, for the tenants that did *not* enable it:** that `full_text` was never enabled, which is
  the statement a customer-held tenant's own security review needs from the vendor. For those tenants
  the answer is structural: the constraint is what makes it true, not the absence of a log entry.
- **Cannot do:** attest that the tenant's notice, privacy documentation or customer communications
  described search; attest that a population was informed; or attest that enabling it was appropriate.
  **The system can say which tenants can read indexed prompt text, and since when, and who turned it on.
  It cannot say whether anyone agreed to it** — the same gap §11.1 describes for notice content, at the
  tenant-configuration level rather than the per-user level.
- **The operational shape, because a per-tenant attestation is only useful if it is obtainable:** the
  answer must be a first-class report an auditor can produce without engineering help — a list of
  `full_text` tenants with their enabling actor and effective date, produced through the same
  `query-api` and `auditor` role path as every other attestation in this section, and it must be
  derivable from the audit log rather than from a separate configuration history that could drift from
  it. **ASSUMPTION:** §14.1 A16 — that this report is a `query-api` capability over existing audit and
  tenant-configuration data rather than a new store.

### 11.7 What the index changes about onboarding and offboarding

Stated here because both are support and administration paths rather than security mechanisms, and both
are visible to a customer's reviewer:

- **Onboarding:** whether a tenant starts at `disabled`, `attachment_names` or `full_text` is a product
  decision this document does not take. What it records is the security requirement: **the tier must be
  an explicit recorded decision, not a default that follows from the custody mode.** A tenant that
  chose `vendor` custody "for simplicity" must not thereby acquire a prompt-content index nobody
  decided on, and §11.6 requires the enabling act to be attributable.
- **Offboarding:** brief §3.3's offboarding path must resolve the index as well as the content store.
  Destroying the tenant KEK is no longer sufficient to make a `full_text` tenant's content
  undecryptable, for the reason §6.4 gives: the index was never encrypted under it. The offboarding
  receipt inherits §11.4's rule, and the honest tenant-facing statement at offboarding is that
  undecryptability applies to content objects and that indexed text is removed by deletion.

---

## 12. Residual risks

The honest list. Each is **accepted** (the design lives with it), **mitigated** (a control reduces it
and a residual remains) or **transferred** (it belongs to a party other than the vendor).

| # | Residual risk | Status | Honest assessment |
|---|---|---|---|
| **R1** | **Coverage cannot be fully measured.** Brief §5.5 requires planning for 70–85% management coverage, never 100% | **Accepted** | An unmanaged device, a proxy-ignoring client, a pinned certificate, a disabled extension and a QUIC flow all produce *no data*, and no data is indistinguishable from no usage without a positive signal at the device. Coverage is made an output of the collection path (R11) and gaps are reported by name (`ops.coverage_snapshot.gap_reason`, C23) — which measures the gaps the product *knows about*. The gaps created by management coverage itself are outside the product's reach. **ASSUMPTION:** §14.1 A5 — the estate-level coverage figure is the customer's property, so the product reports its own coverage and records the estate number as unknown |
| **R2** | **Content that is structurally uncapturable.** WebSocket messages on an established connection, mode I (embedded on-device inference), response bodies | **Accepted** | Brief §5.1: only the WebSocket handshake is observable, not the messages. Brief §2 mode I is detection-only with no access to the prompt. Brief §1.2 excludes response-side capture from v1. These are environment facts, not defects. What the design owes is *representation*: mode C carries best effort with mandatory tool identity and volume; mode I reports `model_detection` with its `detection_basis` and no prompt; the coverage report shows the affected modes rather than rounding them away (E4, D8) |
| **R3** | **A customer-held key is lost.** An HSM fails, a key is destroyed by accident, a vault is offboarded without notice | **Transferred** | Modes 2 and 3 mean the customer holds the only key. The vendor cannot recover the content, cannot prevent the loss, and must not imply otherwise. What is owed: an explicit key-unavailable result rather than an empty one (C17), an alert distinct from data loss, and a contract saying the loss is the customer's. **The vendor's own equivalent — losing a vendor-managed KEK — is not transferable and is genuine unrecoverable loss**, mitigated only by key-store durability and by having no escrow the vendor itself could abuse |
| **R4** | **The vendor can be compelled to produce data it can decrypt under vendor-managed key mode** | **Accepted — and it is a mode choice** | Under mode 1 the vendor holds the KEK and can unwrap; if compelled, the design offers no technical obstacle. This is why modes 2 and 3 exist and why §6.2 states the difference rather than smoothing it over: mode 2 makes the unwrap visible in the customer's own key store, mode 3 removes the standing capability. **The vendor must not describe mode 1 as equivalent to the others.** The choice is the customer's and it is a real one. **D6′ adds a row to this residual rather than changing it**: for a `full_text` tenant the vendor can produce **indexed prompt text** without unwrapping anything, so the mode 2 and mode 3 obstacles do not apply and the customer's key store records nothing. That is not a mode choice — it is a consequence of the search tier (§12 R14, T4) |
| **R5** | **Concentration of risk in the interceptor** | **Accepted** | A root-certificate-installing TLS interceptor is mechanically a MITM, resembles malware to other endpoint security products (R4), and is the one component whose failure mode breaks the customer's network rather than losing a data point (§9). Offline root, short-lived leaves, destination scoping, fail-open, bake period, exclusion artefact, kill switch and last-in-build-order all bound it. **None removes the concentration.** A customer who cannot accept it should decline the provider and accept the coverage gap, which must remain a supported deployment. **It is no longer the only concentration** (§9.1a): `ingest.search_text` is the other, and unlike this one it has no deployment choice that removes it other than staying off `full_text` |
| **R6** | **The device is untrusted, so every device-reported fact is an assertion** | **Accepted** | A device can misreport what it observed (T2), be compromised (T1), and be administered by the person being observed (§2.1). Server-side validation checks structure, identity and consistency, never truth. This is the defining residual of endpoint-side interpretation and cannot be designed away without centralising inspection, which Alternative A was rejected for |
| **R7** | **The local spool is not a boundary against the device's own administrator** | **Accepted** | §5.4. Encryption and OS key wrapping defeat theft and offline analysis; a local administrator can ask the OS to unwrap the key on that machine. Follows from D3 and D4 and is inherent to shipping user-space code on a machine the user controls |
| **R8** | **Row-level security is a policy layer, and a migration can weaken it** | **Mitigated, now with a named exception** | Two independent layers (§7.2): forced RLS with non-owner roles, and per-tenant KEKs under which a cross-tenant read returns unreadable ciphertext. The residual: the cryptographic layer protects only *content* **objects**. A cross-tenant read of labels, aggregates or the identity map would still disclose A5 and A6. **D6′ adds the index to the unprotected side, and it is the worst entry on that list**: `ingest.search_text` holds plaintext, so for a `full_text` tenant a cross-tenant read that escapes RLS returns **another tenant's prompt text in the clear** with no second layer behind it (§7.1, T16). Canary tests (§13) exist to detect policy drift, not to assume it cannot happen |
| **R9** | **Analyst misuse cannot be prevented, only recorded** | **Accepted, and widened by D6′** | §10.1. Case references, a second approver and audit-as-served raise the cost and guarantee a record. They do not make an unjustified subject-level read impossible, and no control here pretends to. **For a `full_text` tenant the case reference and the second approver are no longer on the content path at all**, so the controls that bound this residual bound only the retrieval path (§10.5, T13). The audit record is unchanged in strength and the exposure it records is larger |
| **R10** | **Unkeyed content digests are a confirmation oracle** | **Accepted — and D6′ adds a second, larger oracle beside it** | §5.5. A guessable prompt can be confirmed against its digest by anyone holding it. Keying the digest is incompatible with the cross-route dedup R9 requires, so the design accepts the weakness, confines digests to internal use, and excludes them from exports. **A search result is the same class of oracle with a wider aperture** (§8 T15): a digest confirms one guess about one prompt, while a search confirms hypotheses about the whole indexed corpus, and it is reachable by a legitimate analyst rather than only by someone holding a digest. The digest controls — internal-only, absent from exports — do not transfer to the index, because the index's whole purpose is to answer questions |
| **R11** | **Coverage depends on other endpoint security products not interfering** | **Transferred, partly** | Brief §5.5 expects false positives from other endpoint products (brief R4). The exclusion artefact and bake period reduce incidence; the customer's endpoint product decides whether the interceptor runs. Where it does not, the coverage report says so — the only honest position available |
| **R12** | **The "reports clean while collecting nothing" outcome** | **Mitigated, with a named dependency** | Every path reports its own health, tamper is reported rather than inferred, the liveness view turns silence into a record, and drop counters are visible (C22–C25, R11). **The mitigation requires a human to read the coverage report.** Nothing in the design compels action on a tenant that looks green with one red coverage row, and a product whose central failure is an unread report should say so |
| **R13** | **Content already exported to customer storage is beyond the vendor's control** | **Transferred** | §2.1, §10.3. No recall, no erasure, no visibility. This used to be the *intended* architecture — the export was D6's answer to the search question, so a customer who wanted search accepted this transfer as the price of it. **After D6′ it is no longer the answer, only an additional copy**, which changes its status from a designed trade to an exposure the design must not enlarge: `body` and `tsv` stay out of every export (§6.5, §13 invariant 28) |
| **R14** | **Search is a bulk content-reading path that bypasses the per-event approval gate** | **Accepted** | §8 T13, §10.5. This is the central security cost of the reversal and it is not mitigable by design, only detectable: bounded snippets, a scope-narrowed ceiling, capped and paginated results, and an audit entry per search that fails closed. **None of those is a gate.** For a `full_text` tenant, content exposure is no longer a two-person decision, and the design says so rather than describing the snippet bound as a control that replaces approval. The only real constraint is that the tenant's search scope was narrowed when the bundle was signed, and the only real control is that somebody reads the search audit — which is R12's unread-report problem applied to a more sensitive report |
| **R15** | **Erasure and key destruction no longer reach all copies of content for a `full_text` tenant** | **Accepted, and it must be disclosed** | §6.4, §11.4, §8 T14. Indexed prompt text was never encrypted under the tenant KEK, so crypto-shredding does not touch it and a key-destruction receipt that claims otherwise is false. The residual has two parts: index rows are removed by a third expiry path rather than by the two C34 mechanisms, and a backup or replica restored from before an erasure can resurrect text that a receipt already reported as removed. The design's answer is disclosure — the receipt states what was removed from the index by deletion, and carries `remaining_counts` for it — not a claim of unrecoverability. **§14.3 Q-g records that the second independent index-expiry mechanism is not yet chosen**, which means the C34 requirement is currently unmet for this store |
| **R16** | **The index is a shared, unpartitioned table, and its confidentiality rests on one policy layer** | **Accepted, with a revisit trigger** | §7.2, §7.3, §8 T16. `ingest.search_text` is tenant-leading and RLS-forced like every other table, and unlike every other content-bearing table it has **no cryptographic second layer**: a policy failure returns readable text rather than unreadable bytes (R8). Partitioning per tenant or per tier would restore a layout layer of separation at the cost of migration and query complexity, and §14.1 A13 defers it. **The honest position is that the isolation property C32 calls "structurally impossible" is, for this one table, enforced by policy alone** — which is exactly the standard §7.2 says is insufficient on its own |

---

## 13. Security invariants

Properties that must hold, phrased to become executable tests. This is the security test suite; a
violation is a defect, not a finding to be risk-accepted case by case.

| # | Invariant | How it is asserted |
|---|---|---|
| 1 | **No service other than `content-vault` can call key-store unwrap** (A3, §5.3) | Key-store access-policy inspection, plus a negative test from every other service identity. **As built:** the declared assignments do not yet satisfy this — `azure/main.bicep` gives `ingest-api` the *Key Vault Crypto Service Encryption User* role (§4.4) |
| 2 | **`content-vault` has no user-facing endpoint and is unreachable from a device or a browser** (D7) | Reachability tests from each device class and from the internet |
| 3 | **A grant authorizes exactly one object** | A second upload under a grant is refused, and the refusal leaves no orphaned object attached to an event |
| 4 | **No plaintext-derived material is persisted beyond the object ciphertext and `ingest.search_text`** — no extracted text, thumbnail, embedding, or any *other* index (§6.3) | Schema inspection against the expected object set, plus a test that destroying a tenant KEK makes every content **object** path return `no_longer_available` with a reason, never an empty result (C17). **This invariant was narrowed by D6′ and the narrowing is deliberate:** the search index is a permitted exception, enumerated by name and only at `full_text`, so any *additional* plaintext-derived store remains a defect. The test that a destroyed KEK makes content unreachable must now assert what it is actually testing — the object path — and must **not** be allowed to imply that all content became unreadable, because for a `full_text` tenant the index is still there (§12 R15) |
| 5 | **An M0 prompt envelope carrying a digest, labels, a classifier version, a confidence or an excerpt is rejected, not sanitised** — its presence is evidence the device read content it was not permitted to read | The contract, the ingest validation and the table constraint all reject it (§2.3) |
| 6 | **No query without a tenant set returns rows** — an unset `app.tenant_id` yields zero rows, not all rows (C32) | Per-table test of the unset-tenant path, which is the classic RLS implementation error. **For `ingest.search_text` this test asserts on returned text, not on row count**, because a failure here returns readable content (§7.1) |
| 7 | **Every tenant-scoped table has row-level security enabled *and* forced**, and no application role owns a table or holds `BYPASSRLS` | A catalogue query that fails the build if a table is missing from the expected set, so a table cannot be added without a policy. `ingest.search_text` must be in the expected set from the commit that creates it, not added to it afterwards |
| 8 | **The tenant is never taken from the request body** | Supply a mismatched `tenant_id` in a body and confirm it is ignored in favour of the authenticated session, **including on the search path**, where a caller-supplied tenant filter is the obvious way to try to widen a query |
| 9 | **A cross-tenant canary row is never visible to the wrong tenant**, on every read path | Scheduled canary, which also detects a migration that drops a policy. **The canary is planted in `ingest.search_text` as well as in the content and metadata tables**, because this is the table where a policy failure discloses text rather than unreadable bytes |
| 10 | **The audit entry commits before content is returned** (C16, C30) | Abort the transaction after the content fetch and assert nothing was served, **on the retrieval path and on the search path** — a search whose audit write is rolled back must return no results and no snippet, and must not return a count either (§6.3) |
| 11 | **The audit table is append-only to every application role** | No `UPDATE`/`DELETE` grant, a trigger that raises for every role including the owner, and a hash chain that detects removal or reordering |
| 12 | **Every act requiring an audit entry produces one** — mode change, **search (including a zero-result search)**, grant, content reveal, export, configuration change, revocation, privileged elevation | Per-operation assertion |
| 13 | **A device cannot receive content for an event it did not report** | A grant request naming an unknown event, or another device's event, is refused |
| 14 | **A revoked device is refused on both ingest and grant paths**, and revocation records the actor | Per-path test, plus inspection of `ops.device.revoked_by` |
| 15 | **A policy bundle whose signature fails causes retention of the previous bundle and refusal of content-reading modes** — never a fallback to unsigned, empty or permissive policy (C10) | Signature-failure test per mode, asserting the effective mode does not rise |
| 16 | **No collection path can report success while collecting nothing** (C23, C25) | For every provider, state, version, last successful capture, permission state and dropped count are present or explicitly absent — and *absent* is itself a reported state |
| 17 | **An undercount is visible** (C22) | A spool overflow increments a counter that appears in the next health report, and a path that drops data is unable to report `healthy` |
| 18 | **A device that stops reporting becomes stale by server-side derivation** from `last_seen` | Liveness view test: silence becomes a record without the device's cooperation |
| 19 | **The interceptor has no persistent capability to read a destination outside the signed scope** (§9) | A connection to a non-enumerated destination is blind-tunnelled and never presented with a leaf certificate |
| 20 | **The interceptor fails open** | With the provider stopped, killed or crashed, egress continues, the system proxy is restored, and the provider reports `absent` rather than the tenant appearing healthy |
| 21 | **The kill switch disables the provider without shipping code**, and its state is visible | Kill-switch test on a ring device, with the state surfaced in the console |
| 22 | **No server-side query returns content text outside the two permitted paths** (D6′) | Enumerate every read route and confirm that content text is returned only (a) through the approved, case-referenced retrieval path, and (b) as a **bounded highlighted snippet** on the `full_text` search path. Anything else — a `tsv`, a full `text` column, an unbounded snippet, a snippet on a tenant whose tier does not permit it — is a defect. **This invariant replaces D6's absolute form**, which cannot survive the reversal and would be a lie if left standing |
| 23 | **`content_digest` and `dedup_key` are absent from the export** | Asserted against the export schema, because an export containing digests is a correlation surface in storage the vendor cannot control (§5.5) |
| 24 | **`ingest.search_text` is readable by exactly one component.** No service other than `content-vault` can read the index: `query-api` is not granted `SELECT` on it, and neither is any other runtime role (§4.4, §6.3) | A negative test from every service identity against the table, plus a catalogue assertion that the granted-role set for `ingest.search_text` is exactly `{sac_vault}` — checked as a set equality, so an added grant fails the build rather than passing quietly. The restated form of the old "exactly one component can read content" invariant: **`content-vault` can read *or return* content, by unwrap or by search, and no second component acquires either** |
| 25 | **A tenant cannot hold `customer_held` keys and `full_text` search at the same time** — the combination is unrepresentable, not merely forbidden (§6.1) | The `ops.tenant` check constraint makes the pair unwritable; asserted by attempting the insert as the migration role and as the application role and expecting a constraint violation from both, and by a catalogue query confirming the constraint exists on the deployed schema. **A migration that drops the constraint fails this test**, which is the point: brief §3.5's "cannot both exist" is enforced here rather than asserted in prose |
| 26 | **Every search commits its audit row in the same transaction that serves it, and fails closed.** No audit row, no results — not a snippet, not a count, not an empty result that reads as "no matches" | Abort or fault the audit insert and assert the search returns an error rather than a result set; assert that a zero-result search still produces an audit row; assert the audit row carries the query terms, the scopes and the acting principal (§6.3, §11.5) |
| 27 | **Search is never enabled globally.** The effective search tier comes from the signed policy bundle narrowed per scope, and a scope that is not named carries `disabled` (C1, C3) | Per-scope test: a search outside every enabled scope returns nothing, and a bundle carrying a tenant-wide `full_text` without scope narrowing is rejected at signing |
| 28 | **`body` and `tsv` from `ingest.search_text` appear in no export**, scheduled or per-subject, and in no log or support payload (§6.5, §11.3, §8 T14) | Asserted against both export schemas — the same shape as invariant 23 and for the same reason — plus statement-log and support-dump inspection for literal query terms |
| 29 | **No cross-tenant search statistics or dictionary structure exists.** Scoring and matching are computed over the tenant's own rows; no shared term-frequency, document-frequency or corpus-statistics table is created | Catalogue inspection of the schema for any structure whose grain is not `tenant_id`-leading, in the same test that covers invariant 7 (§7.1, T16) |

---

## 14. Assumptions, resolutions and open questions

### 14.1 Assumptions

Every **ASSUMPTION** in this document, so none is buried in a table.

| # | Assumption | Why, and how it is resolved |
|---|---|---|
| A1 | The per-tenant KEK is mandatory for every tenant in every custody mode, including mode 1 | §7.2 depends on it: without a per-tenant key the second isolation layer does not exist for vendor-managed tenants. **derived from** C33 ("per-tenant encryption keys"); the schema enforces the related rule with `CHECK (ceiling_mode <> 'm3' OR kek_id IS NOT NULL)`, so the KEK is mandatory from M3 and an M2 tenant may have none |
| A2 | The per-device private key is hardware-backed and non-exportable wherever the platform supplies a TPM or Secure Enclave, and §4.2's transport binding relies on that where available | The master document names no hardware-key abstraction; the stronger property is claimed only where the platform supplies it. Where it does not, the residual is ordinary: a stolen key file is a usable credential until revoked |
| A3 | Mode 3 is reached through a standard key-management interface over mutual TLS, with the customer's HSM as custodian | Brief §3.3 requires customer-held keys but names no protocol, and the master document assumes Azure-native services (S1). A customer-side HSM is where S1 does not hold, so the integration point is named (§6.2) |
| A4 | The per-tenant KEK uses the shortest key-store recovery window consistent with the customer's backup policy, and the actual window is disclosed in the erasure receipt | §6.4: soft-delete or purge protection means "destroyed" has a bounded delay before "unrecoverable". A receipt claiming immediate destruction and being wrong is worse than one stating the window and being right. **D6′ adds a boundary to what the window governs**: it applies to content objects, and index rows are removed by deletion rather than by this window (§11.4) |
| A5 | The 70–85% management-coverage figure is a property of the customer's estate, not of the product, so the product reports its own coverage and records the estate-level number as unknown | Brief §5.5 gives the figure as a planning assumption. §12 R1 depends on this reading: the product cannot measure devices it was never installed on |
| A6 | The scheduled Parquet export carries metadata and labels only, and excludes `content_digest`, `dedup_key`, **`ingest.search_text.body` and `ingest.search_text.tsv`** | §5.5, §6.5, §11.3. Without the exclusion the export is a cross-database correlation surface in storage the vendor cannot control, and after D6′ it would also be a second unbounded copy of indexed prompt text. The design mechanism survives the reversal; the reason for it is now twofold. **derived from** D6's framing of the export as the answer to cross-content search (which required that it not be a digest oracle) plus §6.5's confinement of the index to one reader |
| A7 | The approver for content retrieval may be a customer-side or a vendor-side role, and this document does not choose | Master doc Q9 leaves it open; C16 requires a second approver either way and the API carries the field. Deciding is a product decision, not a security one. **D6′ narrows what this assumption covers**: it is an assumption about the *retrieval* gate, and the `full_text` search path has no approver at all for it to apply to (§6.3) |
| A8 | Whether a customer-side content exporter is in v1 is unresolved, and §6.5 assumes it is not | Master doc Q11. Assuming it *is* in scope would assume a component inside the customer's environment that this document cannot specify |
| A9 | The device never holds the unwrapped per-tenant KEK; the server-side unwrap is the only path | **derived from** brief §4.4's "per-object encryption keys wrapped by a per-tenant key" and C14's per-event grant: the object key is delivered per grant, so the tenant KEK has no reason to be on the device, and placing it there would give every device a tenant-wide decrypt capability |
| A10 | Security-relevant retention decisions (audit retention, hold precedence, export retention) are recorded as configuration rather than compiled into code | C35 requires holds with a visible scope and their own expiry, and Q10 leaves hold-versus-erasure precedence open. Configuration keeps an unresolved question reversible. **Now includes index retention**, because `ingest.search_text.expires_at` is a fourth retention input alongside the event TTL, the content TTL and holds |
| A11 | A `full_text` tenant must also carry an M3 ceiling on the searched scope | §6.1: the index can only hold content that crossed under a grant, so `full_text` over an M1 or M2 tenant is empty rather than dangerous. The ADR states "M3 for the scope" as a capability requirement. **The schema resolves the tenant-level half**: the `tenant_search_tier_requires_collection_mode` check constraint on `ops.tenant` admits `full_text` only with `ceiling_mode = 'm3'` (and `attachment_names` only above M0), which makes the empty-index case unrepresentable rather than merely useless. The per-scope half is enforced by the bundle's narrowing at signing |
| A12 | `ingest.search_text` is not additionally encrypted at rest under a vendor-held key | §7.2. Such encryption would raise the cost of a stolen backup or a restored replica (§8 T14) and would not change the vendor-readability conclusion, since the vendor holds the key. Recorded as an assumption because the ADR states no position. **derived from** §6.6's conclusion that no key construction changes the guarantee for a `full_text` tenant |
| A13 | Per-tenant or per-tier partitioning of `ingest.search_text` is a later optimisation, not a v1 requirement | §7.3, §8 T16, §12 R16. Partitioning would restore a layout layer of separation that the index currently lacks and would narrow cross-tenant side channels. It is deferred because D2 already defers partitioning for the event table and because a half-partitioned design is harder to reason about than a clearly-stated single table. **The revisit trigger is a second data-bearing store in the same table or any cross-tenant statistics structure** |
| A14 | Exclusion of `ingest.search_text` from backups, replicas and statement logs is achievable by configuration rather than requiring a separate store | §8 T14. The ADR specifies the table, its columns and its indexes and says nothing about the platform services around it. If configuration cannot deliver the exclusion, this becomes a design change rather than an assumption, because the alternative — the index present in every restored copy — is the copy-escape path that matters most for a plaintext asset |
| A15 | The per-subject export resolves the index and reports, in its manifest, that a term in one subject's indexed text can appear in a snippet served against another subject's search | §11.3. Brief §4.5 requires the export to cover "that subject's events and any stored content", and the index is stored content; resolving the cross-subject snippet property would require per-subject index partitioning, which A13 defers. **derived from** brief §4.5 read against §6.3's tenant-wide index |
| A16 | The `full_text` enablement attestation (§11.6) is a `query-api` capability over existing audit and tenant-configuration data rather than a new store | §11.6 requires the report to be obtainable without engineering help and derivable from the audit log rather than a parallel configuration history. A separate store would be a second source of truth that can drift from `ops.audit`, which is the failure mode the attestation exists to avoid |
| A17 | That deterministic and order-preserving encryption over the index are settled breaks rather than open research questions, and that searchable symmetric encryption's access-pattern leakage is disqualifying for this product | §5.6. **This document cites no specific attack and this is the one assumption in §5.6 that is a judgement rather than a derivation.** It is labelled because a reader is entitled to check it against the literature rather than take a security document's word for it, and because the conclusion §6.6 draws — that no construction gives search and vendor-blindness — rests on it |

### 14.2 Where the brief was silent or self-contradictory, and how it was resolved

| # | The gap | Resolution taken here |
|---|---|---|
| G1 | **M2 is content egress, but the mode table reads as though only M3 involves content.** Brief §1.1 lists M2 as "a minimised excerpt" and places the storage-and-lifecycle boundary at M2→M3, inviting the reading that M2 keeps nothing | Read literally and treated as content egress: at M2 an excerpt of up to 2048 characters crosses the network *in every envelope*, which the schema confirms. This document therefore groups M2 with M3 for confidentiality purposes (§2.3 limit 2, §3 A1) even though the brief places its boundary at M2→M3. **D6′ adds a second reading to this gap**: if an M2 excerpt reaches the server, it is content the index could hold, so a `full_text` tenant's search coverage must be stated in terms of the mode that produced each indexed unit rather than assumed to be M3-wide (A11) |
| G2 | **Brief §3.5 states the key/search exclusivity as a constraint to decide on; R10 calls it a risk to resolve.** Neither says which side to choose | **Resolved twice, and the second resolution stands.** Master doc D6 first chose customer-held keys permanently with no content search, and stated the consequence as a one-sentence product limitation. **D6 is reversed: the product owner ruled content search a required capability, ADR 0014 supersedes ADR 0008, and D6′ takes both sides of §3.5's line by making the incompatibility a schema constraint rather than a choice** — three search tiers, `full_text` unrepresentable with `customer_held`, and the honest consequence stated in §6.6 as a deliberate reduction in the confidentiality guarantee. The old resolution is recorded here rather than deleted, because §3.5 and R10 are still the reason the constraint exists in the form it does |
| G3 | **"Isolation enforced at the storage layer" and "customer-held keys must be supported" sit adjacent in brief §3.3 and support different mechanisms** — RLS is policy, keys are arithmetic | Implemented as two independent layers rather than choosing (§7.2). The brief does not say which it means by "structurally impossible"; the honest reading is that only the cryptographic layer is arithmetic, and only the policy layer covers non-content data. **D6′ empties the second layer for one table**: `ingest.search_text` is covered by the policy layer alone (§7.2, §12 R16) |
| G4 | **Brief §3.1 requires the digest to be present "even when content is not stored, so dedup and later retrieval both work", which forecloses keying it; §4.1 makes the dedup key depend on it.** The privacy consequence of an unkeyed digest is not addressed | Accepted and labelled (§5.5, §12 R10): digests stay unkeyed for dedup, are confined to internal use, and are excluded from exports. Keying them would break the cross-route dedup R9 requires. **D6′ does not change this resolution and does repeat its shape**: the index is a second structure that lets the vendor answer questions about content, accepted for the same class of reason — a product requirement — and labelled rather than presented as free |
| G5 | **Brief §5.2 requires a failing client to be excluded rather than left broken; §7 requires that no path fail into a reported success.** An exclusion looks like a silent coverage gap | The exclusion must be a **named coverage-gap entry** (§9.3, T7), which is also the master document's resolution. "Excluded" and "silent" are different states and the health model must keep them different |
| G6 | **Brief §5.5 expects a bake period and an exclusion artefact for endpoint-security false positives; §8 sets an ingest availability target of 99.9%.** A fail-open component protects availability and costs coverage | Prioritised in that order and stated as such: §9.3 fails open and reports `absent`. A security product that breaks the customer's network gets uninstalled, and the coverage gap is the recoverable failure |
| G7 | **The brief's scope note excludes legal review while making several capabilities binding functional requirements** — notice acknowledgement, a collection ceiling, a per-subject export, an erasure receipt | Taken literally: §11 specifies each as a capability framed "the system can attest that X happened", asserts no legal conclusion, and marks where the system records an assertion rather than proving a fact. **§11.6 and §11.7 extend the same treatment to the search tier**, which is a new disclosure-relevant fact (§11.6 lists it and draws no conclusion) and a new input to the erasure receipt (§11.4). **D6′ is not a legal analysis and this document does not treat the product owner's ruling as one** |
| G8 | **Brief §3.2 requires an audit entry for every content grant, reveal, export and configuration change, but does not say what makes the log trustworthy.** An append-only log the application can rewrite is not evidence | Added as a design mechanism rather than a brief requirement: append-only grants plus a trigger, a per-tenant hash chain and a periodic anchor (`ops.audit`) — which is what makes §11's attestations checkable rather than merely asserted. **Brief §3.6's "every read of subject-level data writes an audit entry as it is served" now covers searches**, which is the highest-volume subject-level read the product will have; the same mechanism absorbs it, and §13 invariant 26 makes the fail-closed behaviour a test rather than an intention |
| G9 | **Brief §3.5 says the key/search decision must be made "before anyone builds a content search feature", and D6 made it in the negative.** The brief never states what the search feature must do — no snippet bound, no result cap, no scoping rule, no approval position | Read as a gap this document has to close rather than one the ADR closes: the tier model, the scope-narrowing rule, the bounded-snippet position, the audit-on-read requirement and the explicit statement that approval protects retrieval and not search are **design decisions recorded here** (§6.1, §6.3), every one of them traceable to a brief requirement (C1, C3, C30, C5, §1.1) and none of them derivable from the brief alone. The two that are genuinely open — the snippet bound and the second index-expiry mechanism — are Q-f and Q-g |

### 14.3 Open questions owned by this document

| # | Question | Impact if unresolved |
|---|---|---|
| Q-a | Whether a device must prove the policy bundle it applied is the one it was issued — a signed, device-bound policy receipt — rather than merely reporting a version | Without it a tampered device can claim compliance with a mode it never applied, and T2's residual widens from observation truth to mode compliance |
| Q-b | The exact contents of the coverage report a human is expected to read, and what happens when nobody does | R12 depends on this entirely. A coverage report that exists and is not read is equivalent to no coverage report |
| Q-c | Whether a second approver is required per retrieval or may hold a standing approval for a case, and what the maximum standing window is | C16 is satisfied either way, but a standing approval degrades the control toward a single-actor path and changes §10.1's honest position. **D6′ makes this question less important than it was**: for a `full_text` tenant the second approver is already off one of the two content paths (§10.5), so tightening the retrieval gate no longer bounds the overall exposure |
| Q-d | Whether the erasure receipt must enumerate objects individually or may state counts by mechanism | D1's value is a literal receipt; the difference between "1,247 rows and 38 objects" and "content for this subject was removed" is the difference between evidence and assurance. **D6′ adds the index to this question**: a receipt that names counts for objects and is silent about index rows is the §11.4 false-receipt case |
| Q-e | **Whether the mode 2 unwrap record must be supplemented for `full_text` tenants** — a separate, customer-visible record of search activity or of index reads, since the customer's key store no longer captures every vendor read | §6.2 and T4 depend on it. Without something of this kind, a `customer_managed` tenant that enables `full_text` has bought a capability whose exercise is invisible in *their* control plane, which is the property they chose mode 2 for. This document does not resolve it; it records that the guarantee mode 2 is sold on has a hole for this tier and that the fix, if any, is a new customer-visible signal rather than a key-model change |
| Q-f | **The bounded snippet: how many characters, how many snippets per result, and whether counts below a floor are withheld** | §6.3 asserts snippets are bounded and §8 T13/T15 rest on that bound being real, but the ADR states no figure. Too generous a bound makes search a *de facto* content read and reduces T13's friction to nothing; too tight a bound makes the tier useless and pushes tenants to the export path, which is worse (§12 R13). The T15 oracle also depends on whether small match counts are disclosed, which interacts with tenant population size |
| Q-g | **The second independent expiry mechanism for `ingest.search_text`** | C34 requires two independent, periodically reconciled mechanisms, and §11.4 notes the index now needs its own pair. A single purge path is precisely the single deletion path C34 forbids, and the consequence of drift here is indexed content outliving its retention with no key destruction to catch it (§12 R15). Until this is answered, C34 is met for events and content objects and **not** for the index |
| Q-h | **Whether a search must carry a case reference even though retrieval does** | §6.3 states the current position plainly — search has audit but no case reference or approver — and C16 requires both only for retrieval. Requiring a case reference on search would restore some of the two-person discipline at the cost of the analyst workflow the feature exists for, and it is a product decision this document flags rather than takes. It is the single highest-leverage mitigation available for T13 if the product owner will accept the cost |
