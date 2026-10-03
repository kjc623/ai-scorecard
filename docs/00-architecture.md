# Shadow AI Capture — Architecture

**Status:** proposed · **Cloud:** Microsoft Azure · **Data class:** regulated personal data (GDPR/CCPA)

This is the master document. It states the problem, the constraints that bind the design, three
structural alternatives, the recommendation, the interfaces and data model, the order to build in,
and what is still unknown.

Everything here derives from *Shadow AI Capture — Product Requirements & Engineering Context*
(hereafter **the brief**). Where a decision rests on something the brief does not say, it is marked
**ASSUMPTION** and listed in §7. Requirement references use the brief's own numbering (`§4.3`, `R9`).

Companion documents: [01-collectors](01-collectors.md) · [02-ingest-and-transport](02-ingest-and-transport.md) ·
[03-data-platform](03-data-platform.md) · [04-dashboard-and-query](04-dashboard-and-query.md) ·
[05-platform-delivery](05-platform-delivery.md) · [06-security-and-threat-model](06-security-and-threat-model.md) ·
[database/schema.sql](../database/schema.sql) · [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json)

---

## 1. Problem

### 1.1 What the product is

Shadow AI Capture records what employees send to generative AI tools from company-managed devices,
and makes it queryable. The customers are 500–5,000-employee companies that have *not* bought
enterprise AI. Because the employer is not the account holder on the tools their staff use, there is
no vendor-side API to pull from (brief §1): **if the system does not observe the usage on the device,
the data does not exist anywhere.**

The defining property (brief §1) is an inversion of the usual collection design:

> Content is interpreted where it is observed, and by default the content itself does not leave the
> machine. Classification, a digest and dimensions cross the network. Content crosses only on an
> explicit, per-event grant from the backend.

That property is not a privacy nicety bolted onto a monitoring system. It is the thing that makes the
breach surface small enough to sell, and it constrains almost every decision below: it forces
classification onto the endpoint, it forces the content-retrieval path to be grant-driven rather than
query-driven, and it forbids the obvious centralised-inspection architecture (§3, Alternative A).

### 1.2 Goal — what must be true when this ships

A security analyst at a customer can open a dashboard and answer the ten questions in brief §3.6 —
which AI tools are in use, which are unsanctioned and by whom, what classes of sensitive data are
going out, what spiked, what a specific person sent — with every number reconciled to a stated
collection state, and with the system able to say **where it could not see** as clearly as it says
what it saw.

Stated as properties that must hold:

1. **Coverage is honest.** Every collection path reports its own health, and a path that is not
   working appears as `degraded` or `absent` rather than as an absence of data (§7, R11).
2. **Undercounts are visible.** A device that dropped events because its buffer filled reports the
   count; a device that stopped reporting becomes visibly stale rather than quietly silent (§7).
3. **Content stays put by default.** With the tenant's `content_search` set to `disabled` or
   `attachment_names`, no content leaves a device unless a specific event has been granted. A tenant
   that enables `full_text` trades that property for prompt-content search, deliberately and per
   tenant; "upload everything" remains unconfigurable either way (§4.4, ADR 0014).
4. **Every number is explainable.** Deduplication, dual clocks and route attribution mean a customer
   can challenge a count and be shown why it is what it is (§4.1, R9).
5. **Erasure produces a receipt.** Deletion is verifiable by the customer, not asserted (§3.4).

### 1.3 Why this is hard

**The acquisition mechanisms are mutually incompatible.** The nine usage modes in brief §2 are not
nine settings on one collector; they are nine different kinds of observation. Mode A is DOM and
request bodies inside a browser sandbox. Modes D and G are TLS inside other processes. Mode F is
*plaintext* HTTP on the loopback interface. Mode I has no prompt at all and is a process-detection
problem. No single mechanism reaches more than three of them. A design that pretends otherwise will
ship a coverage number it cannot defend.

**Interception is the high-risk component and it is also the one the brief keeps warning about.**
A root certificate plus TLS termination is indistinguishable, to other endpoint security products,
from a man-in-the-middle (R4). It has the largest blast radius of anything in the product (§5.5) and
its failure mode is *breaking the customer's network*, not losing a data point.

**One collection path fails inverted.** The local-inference path (mode F) holds a TCP port the user's
own tool needs. Unhealthy, it does not lose data — it stops the user's local AI from starting (§5.3).
Everywhere else "fail closed" means "collect nothing"; here it means "break the user".

**Coverage is partially unmeasurable.** 70–85% management coverage is the planning assumption (§5.5).
The unobserved remainder is a normal operating condition, and the product has to represent it rather
than round it away.

**The data is tiny; the endpoint is everything.** 12,000 events/day is 0.14 events/second (brief
§3.1). The whole cloud side is a small relational database and a few services. Effort allocated to
the server tier instead of the collectors is effort spent on the easy half.

### 1.4 Load and scale

From brief §3.1, with the arithmetic made explicit:

| Quantity | Value | Note |
|---|---|---|
| Managed devices | ≤ 5,000 | per tenant |
| AI-active users | ~4,000 | 80% of devices |
| Submissions/day | ~12,000 | 1–4 per active user |
| Submissions/year | ~4.4 M | |
| Mean ingest rate | **~0.14 events/s** | 12,000 / 86,400 |
| Worst-case burst | **~50–500 events/s** | 5,000 devices flushing a 1–500 batch after an outage (§4.3) |
| Event row size | 1–2 KB | metadata + labels + digest |
| Event growth | **5–9 GB/year** | |
| Attachment content at M3 | **50–500 GB/year** | the only unbounded cost |
| Raw process telemetry, unfiltered | orders of magnitude larger | must never be shipped (R7) |

**Conclusion the numbers force.** Four years of events for a full tenant is under 40 GB and under
18M rows. This does not need partitioning, a columnar warehouse, a separate search cluster or a message
broker — including the content search index of §4.5 D6, which lives in this same database: prompt text
at ~4.4M submissions/year and ~1 KB each is roughly 4.4 GB per tenant per year before its `tsvector`
and GIN index. It needs one well-indexed relational database, and the discipline to keep the attachment
tier bounded because that is the only line item with a tail.

**Cost, corrected.** An earlier draft of this section estimated $300–700 per tenant per month.
[05-platform-delivery](05-platform-delivery.md) §11 shows that its own arithmetic does not support
that band, and the correction is recorded here rather than left as a discrepancy between two
documents:

| Configuration | Per tenant per month | Note |
|---|---|---|
| Recommended production (Postgres `D2ds_v5` + zone-redundant HA, Front Door Premium) | **≈ $830–840** | the figure to plan against |
| `B2s` + HA | ≈ $490 | burstable database; the $300–700 band holds only here or below |
| `D2ds_v5`, no HA | ≈ $640 | not recommended: the database is the system of record |
| Marginal cost of one more device | ≈ $0.00 | 1,000 more devices ≈ $0.01/month |

Two things follow that the earlier band obscured. First, the **fixed** cost dominates: Front Door's
base fee and the database instance are the drivers, not data volume, so the cost model is close to
flat per tenant and the product's economics improve with tenant count rather than with usage.
Second, the **attachment tier is not the cost problem** — at 500 GB/year and a 12-month hot
retention it is single-digit dollars per month. It is a *breach-surface* and *lifecycle* problem,
which is why the per-tenant content budget (`ops.tenant.content_budget_bytes_per_day`) is a governance
control rather than a cost control. Managed HSM is the one genuine cost
outlier at roughly $3,360/month, about four times everything else combined, so it is offered
per-contract and not as a regional default.

**All cost figures are estimates**, at Azure East US list price, pay-as-you-go, no committed-use
discount, as of 2 October 2026, and must be re-baselined against the pricing calculator before any
commercial commitment.

---

## 2. Constraints

Every row cites where it comes from. A constraint with no source would be an assumption, and is
marked as one.

### 2.1 Product requirements

| # | Constraint | Source |
|---|---|---|
| C1 | Four collection modes M0–M3, selectable per tenant **and per scope** (tool, data class, user population), changeable without re-purchase | §1.1 |
| C2 | M0→M1 is a permission boundary; M2→M3 is a storage and lifecycle boundary | §1.1 |
| C3 | The mode in force is applied **before** a policy bundle is signed; a device can never exceed its tenant's ceiling | §1.1 |
| C4 | Every mode change is attributed to an actor and a time | §1.1 |
| C5 | It must not be possible to configure the system into an "upload everything" state | §1.1 |
| C6 | Nine usage modes A–I must each be captured to the fidelity stated in the matrix | §2 |
| C7 | Discovery must classify *behaviour* (a user-authored payload sent to a generative endpoint), not match a curated brand list | §2 |
| C8 | Sanctioned state is per-tenant; `unknown` must not be conflated with `prohibited` | §2 |
| C9 | No keystroke logging; no filesystem indexing or file-at-rest scanning; no response-side capture in v1; no productivity analytics; no fleet-wide packet inspection | §1.2 |
| C10 | Policy distribution is signed and versioned, supports `304`, and **must retain the previous bundle on signature failure** — never fall back to unsigned or empty | §4.2 |
| C11 | Re-enrolment after re-imaging is idempotent and returns the existing identity | §4.2 |
| C12 | Event ingest is batched 1–500, idempotent per event key **enforced by the store**, with per-reason rejection detail | §4.3 |
| C13 | Ingest latency must not depend on aggregation latency | §4.3 |
| C14 | Content never arrives unsolicited: device requests a grant, backend decides, denial carries a reason | §4.4 |
| C15 | Per-object keys wrapped by a per-tenant key; customer-supplied key destruction destroys content | §4.4 |
| C16 | Content retrieval requires a case reference and a second approver; the audit entry is written **before** content is returned | §4.4 |
| C17 | An expired or erased record returns an explicit "no longer available", never an empty result | §4.4 |
| C18 | Per-subject export exists as a designed support path | §4.5 |
| C19 | Classification: deterministic rules first, statistical model for fuzzy classes; output is a label set with confidences and a version, never a boolean | §6 |
| C20 | Rules and models are promotable without a software release, evaluated non-enforcing first, reversible instantly | §6 |
| C21 | Classification degrades gracefully and must never silently emit "no sensitive data found" | §6 |
| C22 | Bounded local buffering, encrypted at rest; on overflow drop oldest, increment a counter, **report the counter** | §7 |
| C23 | Per-collector health: state, version, last successful capture, permission state | §7 |
| C24 | Tamper detection is reported, not inferred from absent events | §7 |
| C25 | No collection path may fail into a state that reports success | §7 |
| C26 | Two clocks; server time authoritative for display; skew reported per device | §3.6, §7 |
| C27 | Time-bucketed aggregates must not scan raw events; dashboards read only precomputed aggregates | §3.6 |
| C28 | Backfill is idempotent: aggregates are upserts, never increments | §3.6 |
| C29 | Cursor pagination only; no unbounded result sets over the event table | §3.6 |
| C30 | Every read of subject-level data writes an audit entry **as it is served** | §3.6 |
| C31 | Bulk export to the customer's own storage in a columnar format, on a schedule | §3.6 |
| C32 | Tenant is the leading dimension of every index and every partition; isolation enforced at the storage layer | §3.3 |
| C33 | Per-tenant encryption keys; customer-held keys must be supported | §3.3 |
| C34 | Time-based expiry enforced by **two independent mechanisms**, periodically reconciled | §3.4 |
| C35 | Holds suspend expiry for a named scope, with a visible scope and their own expiry | §3.4 |
| C36 | Update safety: deployment rings with automatic halt, signed manifests, content/code separation, atomic install with rollback, server-side kill switch | §5.5 |

### 2.2 Environment facts

These are observed facts to design against, not preferences. Several invalidate the intuitive design.

| # | Fact | Consequence | Source |
|---|---|---|---|
| E1 | MV3 permits observational request capture, and **policy-installed extensions retain `webRequestBlocking`** | The extension can both observe and inline warn/block | §5.1 |
| E2 | Request bodies are readable via `onBeforeRequest` with `requestBody` | Prompt text is obtainable in the browser | §5.1 |
| E3 | File uploads report a **filename, never bytes** | Attachment contents must be read in page context before the request is built | §5.1 |
| E4 | Messages on an established WebSocket are invisible; response bodies are never exposed | Some usage is structurally uncapturable — must be represented, not hidden | §5.1 |
| E5 | Broad host permission is required for discovery | Observation is broad; emission stays narrow — separate decisions | §5.1 |
| E6 | Enterprise roots are honoured for TCP but **not QUIC** | QUIC must be disabled by policy **and** UDP/443 blocked at egress | §5.2 |
| E7 | Roots are honoured only from specific stores (Windows LM→Trusted Root / Enterprise; macOS Default or System with Always Trust) | Wrong store fails silently | §5.2 |
| E8 | Without a kernel component, interception only reaches apps honouring system proxy | A coverage boundary to measure, not assume | §5.2 |
| E9 | Go reads the OS trust store; Node.js needs `NODE_EXTRA_CA_CERTS` and ignores proxy variables by default; Python needs a CA bundle path | CLI coverage requires a configured shell environment | §5.2 |
| E10 | CLI tools are launched from a shell | A managed shell profile is the single lever for proxy and trust | §5.2 |
| E11 | Local inference serves **plaintext HTTP on loopback** (11434, 1234, 8080) | Nothing to decrypt; being *in the path* is achievable in user space | §5.3 |
| E12 | Loopback is invisible to network appliances and OS content filters | Only a device-resident component can see mode F | §5.3 |
| E13 | Servers can generally be moved off the default port via their own config — **unvalidated** | The port-occupation strategy is a hypothesis until R1 is closed | §5.3 |
| E14 | Mode F's failure mode is *breaking the user's local AI* | Requires clean release, crash recovery, and never holding a port it cannot serve | §5.3 |
| E15 | macOS Endpoint Security is **telemetry only** — no payload read | On macOS any endpoint component contributes metadata, never content | §5.4 |
| E16 | Screen Recording **can never be pre-granted** by MDM | Any approach depending on it breaks zero-touch permanently — do not depend on it | §5.4 |
| E17 | Accessibility / Full Disk Access / Files-and-Folders **can** be pre-granted | These are usable; Screen Recording is not | §5.4 |
| E18 | Restricted entitlements are an approval gate with no SLA | Treat as a schedule dependency, not a coding task | §5.4 |
| E19 | Code-signing validity is 460 days | Rotation must be automated; expiry is a known-date fleet outage | §5.5 |
| E20 | EV certificates no longer bypass SmartScreen | Expect "unrecognised application" prompts during pilots | §5.5 |
| E21 | The product will not qualify for Microsoft's security-vendor allowlist | Assume unwanted-software treatment while reputation builds | §5.5 |
| E22 | A root-cert-installing TLS interceptor resembles malware | Ship exclusions as an artefact; budget a bake period | §5.5 |
| E23 | Browser extension deployment is one policy per browser; Firefox and Safari are effectively unreachable | Chromium (Chrome + Edge) only for v1 | §5.5 |
| E24 | Plan for 70–85% management coverage | The unobserved remainder is normal and must be represented | §5.5 |
| E25 | EDR/MDM APIs expose no content and permit no in-sensor injection | The product must ship its own collection | §5.6 |

### 2.3 Performance targets

From brief §8. These are the numbers the design is sized against.

| Target | Value |
|---|---|
| Classification latency, interactive path | ≤ 150 ms p95 |
| Warn/block decision | ≤ 300 ms p95 |
| Steady-state endpoint resource use | not perceptible; perceptible impact is a defect |
| Ingest availability | 99.9%, devices buffering through outages |
| Event visible in query layer | < 60 s from receipt |
| Dashboard aggregate query | < 2 s p95, from precomputed aggregates |
| Content retrieval once granted | < 30 s |

### 2.4 Stack constraints

| # | Constraint | Source |
|---|---|---|
| S1 | Cloud is Microsoft Azure; managed services are acceptable | user answer, this session |
| S2 | Data class is regulated personal data under GDPR/CCPA | user answer, this session |
| S3 | Single-tenant-per-customer is wrong: `per-tenant` appears throughout, buyers demand customer-held keys, and "one customer in ten will want to run their own SQL" | §3.3, §3.6 |
| S4 | Legal, privacy-policy and jurisdictional review is **excluded** from this workstream — but functional requirements that exist to support it are binding | brief scope note |
| S5 | Data residency is not stated in the brief | **ASSUMPTION** — see §7 Q1 |

### 2.5 What the brief deliberately leaves open

Form factor, process model, interception strategy, storage engine, service boundaries and UI
(brief preamble). Those are the substance of §3 and §4.

---

## 3. Alternatives

Three designs that differ in *structure*, not detail. Each is described by how and where content is
acquired, because that is the axis on which they actually differ.

### Alternative A — Central inspection at a managed network egress

```
 Devices ──► corporate/DNS-routed egress ──► Cloud secure web gateway (TLS terminated)
                                                    │
   thin agent: process + user attribution ──────────┤
                                                    ▼
                                    classifier farm ──► Postgres ──► dashboard
 No content-reading component on the endpoint at all.
```

No endpoint component reads content. A cloud secure-web-gateway (or an on-premises forward proxy)
terminates TLS for egress traffic; a very thin device agent reports which process and user owned
each flow, so the gateway can attribute what it sees. Classification runs centrally.

**Makes easy.** One place to update classifiers, so §6's promote-without-a-release requirement is
trivially satisfied. No root certificate on endpoints, therefore no E6/E7/E22 problems and no bake
period. No per-application coverage question (E8) because there is nothing to configure per app. No
loopback problem, no port occupation, no document parsing on the endpoint, no Apple entitlements, no
endpoint resource budget. Deployment is a PAC file and a small agent. It is by a wide margin the
cheapest and fastest thing to build.

**Makes hard.** Everything the product is for.

- Remote and hybrid workers off the corporate egress produce **nothing**. At 500–5,000 employees,
  that is most of the population.
- Modes F (loopback), I (embedded model) and C (browser-agent) are unreachable: loopback traffic
  never leaves the machine (E12) and embedded inference makes no network call to inspect.
- The defining property of the product inverts. Content would have to transit an inspection point in
  cleartext before anything decides whether it may be kept, so "content does not leave the machine by
  default" becomes untrue as a statement about the architecture even if it stays true as a statement
  about storage.
- §1.2 forbids fleet-wide packet inspection, and a central gateway *is* fleet-wide packet inspection
  by construction — it cannot know the destination is generative until after it has decrypted it.
- The inspection point becomes a single plaintext chokepoint holding exactly the data the product
  exists to protect. It is the largest possible breach surface in the whole design space.

**Cost to operate.** Low, until legal review. Then high, and possibly disqualifying per customer.

**Fails worst.** Confidentiality, and the commercial promise. A customer who asks "where is my
employees' prompt text decrypted?" gets an answer they will not accept.

**Verdict: rejected.** It is a legitimate design for a different product — a data-loss-prevention
gateway — and worth stating explicitly because it is the first thing many teams propose.

### Alternative B — Provider suite on a shared device-side capture core (recommended)

```
 MANAGED DEVICE (treat as untrusted)
 ┌────────────────────────────────────────────────────────────────────┐
 │  Browser extension (Chromium)        Capture Core (privileged svc) │
 │  · webRequest + requestBody          · egress proxy provider       │
 │  · page-context attachment read      · loopback inference broker   │
 │  · inline warn / block               · process & model detector    │
 │  · wide observation, narrow emission · CLI trust shim              │
 │            │                         · policy engine               │
 │            │  native messaging       · encrypted spool (bounded)   │
 │            └───────────┬─────────────┘                             │
 │                        ▼                                           │
 │              classifier-host (sandboxed)                           │
 │              rules → validators → model; doc parsing in a child    │
 └────────────────────────────────────────────────────────────────────┘
                    │  one envelope · one spool · one policy
                    │  HTTPS 443 · per-device credential + mTLS
                    ▼
        Azure Front Door Premium + WAF
                    │ Private Link
        ┌───────────┴────────────┐
        ▼                        ▼
   ingest-api (Go)         control-api (Go)
   events only, no keys    enrolment · policy · grants · health
        │                        │
        │                        ▼
        │                  content-vault (internal only)
        │                  the only component that can unwrap
        │                        │
        └────────┬───────────────┘
                 ▼
   Azure Database for PostgreSQL      Azure Blob (ciphertext at M3)
                 │                    Azure Key Vault / Managed HSM
                 ▼
        aggregator (job) ──► mart aggregates
                 │
                 ▼
           query-api (Node.js) ──► dashboard (JavaScript)
```

One privileged background service per device — **Capture Core** — hosts several independent capture
*providers*: an egress proxy, a loopback inference broker, a process/model detector, and a CLI trust
shim. A separate policy-installed browser extension handles the browser surfaces, because a browser
sandbox cannot be reached from outside. Both processes run the same classifier core and write the
same envelope into the same bounded encrypted spool. The cloud side is deliberately small: an ingest
API, a control API, an internal-only content vault, scheduled rollup and reconciliation jobs, a query
API and a dashboard.

**Makes easy.** Each surface gets the mechanism that actually works for it, so the brief's §2 matrix
is met by construction rather than by hope. A failing provider degrades *one coverage row* rather
than the product — which is precisely what §7's per-collector health and §5.2's "detect the failure,
exclude the process, and record that coverage was not achieved" ask for. Classification exists once,
in one implementation, shared by both device processes (§6). Content stays on the device by default
because the only content-egress path is a per-event grant (§4.4). The cloud tier is small enough to
be operated by a team of two.

**Makes hard.** Two device processes to sign, ship and update on two platforms. A root certificate
and system proxy configuration have to be deployed and kept working across two trust-store models
(E7) and two browser policies. Coverage becomes mandatory measurement work, because with five
collection paths spread across nine usage modes the number of partial-failure states is large. The egress proxy retains the largest blast
radius in the product.

**Cost to operate.** Moderate. The endpoint tier is where the operational burden lives: update rings,
a bake period with EDR vendors (E22), signing rotation every 460 days (E19), and a coverage report
that someone has to read.

**Fails worst.** The egress proxy provider. Mitigated by scoping interception to enumerated
destinations, by blind-tunnelling everything else, by failing open, and by a server-side kill switch
that disables the provider without shipping code.

### Alternative C — Per-runtime shims, no interception

```
 Browser ── extension
 VS Code / Cursor ── IDE extension
 Claude Code / Codex ── tool hooks
 Python / Node ── sitecustomize / NODE_OPTIONS shim
 Ollama / LM Studio ── local port broker
 provider SDKs ── wrapper package
                    │
                    ▼  same envelope, same cloud side as B
```

No TLS interception anywhere. Every runtime is instrumented through its own supported extension
point: a browser extension, IDE extension APIs, CLI hooks, language-level auto-instrumentation, a
loopback broker, and SDK wrappers. Content is read at the call site, so it arrives structured rather
than parsed out of an HTTP body.

**Makes easy.** No root certificate, no MITM, no QUIC problem (E6), no trust-store problem (E7), no
EDR false positives (E22), and no kernel-adjacent anything. Per-call data is structured, so
classification is more accurate than reconstructing a prompt from a serialised request. Endpoint
resource use is minimal. The permissions story is far better: nothing needs a system-level
certificate.

**Makes hard.** It contradicts the product's central value proposition. §2 requires that discovery
classify behaviour rather than match a curated list, because "the value of the product is finding
tools nobody has enumerated yet". Shim-based capture is the opposite: every new tool is a new
integration, and an unenumerated tool is invisible. It also cannot reach closed desktop applications
(mode D) at all — ChatGPT Desktop, Claude Desktop and Copilot expose no extension point — and it
cannot see custom scripts that ignore the shim (mode G). Coverage decays continuously as the tool
landscape moves, and each shim is a bespoke codebase to maintain.

**Cost to operate.** Low per shim, unbounded in aggregate. The integration backlog is the product.

**Fails worst.** Coverage of unknown tools — the one thing the product is sold on.

**Verdict: rejected as the primary mechanism, adopted as a component.** The CLI trust shim inside
Alternative B is exactly this idea, used where it is strictly better than interception (E9/E10).

### Why B and not the others

The decision is not "which is best in general" but "which satisfies the brief's requirements without
pretending".

1. **The nine modes differ in kind, so the design must be a suite.** A is one mechanism applied
   everywhere, and it cannot reach F, I or C. C is nine mechanisms with no shared spine. B is a suite
   with a shared spine — one envelope, one spool, one policy engine, one classifier, one cloud side.
2. **The brief requires per-path honesty, which requires per-path isolation.** C25 forbids a path
   failing into a reported success; C23 requires per-collector health; R11 requires coverage state to
   be an output of the collection path. A single monolithic interceptor cannot report *which* of nine
   modes it covered. B's providers can, because each owns one coverage row.
3. **Blast radius and update safety point the same way.** §5.5 makes halt-on-regression, atomic
   install and a kill switch hard expectations. A design where one component carries network
   interception, classification, spooling and policy makes every update a fleet-wide risk. B's
   providers are independently fail-open-able.

---

## 4. Recommendation

### 4.1 The shape, and what each piece is written in

| Component | Where | Language | Why this language |
|---|---|---|---|
| `capture-extension` | Device, browser | **JavaScript** (ES modules), Manifest V3 | The only option in a Chromium sandbox; MV3 APIs are JavaScript. No dependencies |
| `capture-core` | Device, privileged service | **Go** | One static binary per platform; `net/http` + `crypto/tls` give a complete interception stack; pure-Go process enumeration on both platforms avoids cgo and therefore avoids a per-architecture build matrix; trivial cross-compilation; cheap concurrency for proxy + spool + policy polling |
| `classifier-host` | Device, sandboxed process | **Go** → native **and** `js/wasm` | One source compiled to both the native host and the extension's in-page copy, so rules and model cannot drift between them. **Amended by [ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md):** §9.1's requirement is byte-identical labels from one source, not a particular language, and the constraints that decided it — no outbound network, no Rust toolchain, Go present and building both targets offline — were not in play when this row was first written |
| Egress proxy provider, loopback broker, process detector, CLI shim | Inside `capture-core` | **Go** | Same binary; each is a package with its own start/stop/health contract |
| Document parser | Device, child process | **Go**, spawned by `classifier-host` | Highest-risk code in the product (R8); isolated in a child with a memory cap and hard timeout so a parser exploit cannot reach model weights or spool keys. Same language as its parent, per [ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md) |
| `ingest-api` | Azure Container Apps | **Go** | Consumes the same contract-generated Go types as the desktop collector; validation and idempotent write path |
| `control-api` | Azure Container Apps | **Go** | Enrolment, policy signing, health, grant decisions |
| `content-vault` | Azure Container Apps, **internal ingress only** | **Go** | The only component holding Key Vault unwrap rights; must not be reachable from devices or browsers |
| `aggregator`, `reconciler` | Azure Container Apps Jobs | **Go** + SQL | Rollups and expiry are set-based SQL; Go is the scheduler and the transaction boundary |
| `query-api` | Azure Container Apps | **JavaScript** on Node.js (`node:http`, no framework, no dependencies) | Same language as the dashboard, so the closed query vocabulary is one shape on both sides; the query layer is where the request-shape logic lives |
| `dashboard` | Azure Static Web Apps | **JavaScript** (ES modules, no framework) | Static SPA behind Entra ID |
| Infrastructure | — | **Bicep** | Azure-native, no state file to secure; Terraform is a reasonable substitute if the team is already multi-cloud |
| Server database | — | **PostgreSQL 16**, Azure Database for PostgreSQL Flexible Server | See below. 16 is the deployment target the infrastructure pins; the local lab and the recorded verification (§5.5) ran on PostgreSQL 17 |
| Local spool | Device | **SQLite** (WAL), application-level encryption | Bounded, transactional, crash-safe, no server. **As built:** `endpoint/capture-spool` is an append-only, AEAD-encrypted segment log behind the `protocol.Store` interface, not SQLite — see [endpoint/capture-spool/README.md](../endpoint/capture-spool/README.md) |

**Database answer, stated plainly** because it was an explicit question: **PostgreSQL**, not SQLite
and not Supabase, for the server. SQLite is the design's choice *on the device* as the spool (the
spool as built is a segment log that meets the same contract; see the row above) and wrong on the
server — it has no network protocol, no row-level security, no concurrent writer model and no
managed backup/PITR. Supabase is a managed Postgres with an attached product surface we neither need
nor want in enterprise procurement; the real requirements (forced row-level security, custom roles,
point-in-time recovery, private networking, a specific region) are plain PostgreSQL features. At
5–9 GB/year a small Flexible Server is over-provisioned.

### 4.2 The main path, end to end

**Step 1 — a submission happens.** An employee types a prompt into chatgpt.com, or pastes a contract
into Claude Desktop, or runs a coding agent, or starts Ollama.

**Step 2 — a provider observes it.** Whichever provider owns that surface (extension for A/B/C, egress
proxy for D/E/G/H, loopback broker for F, process detector for I) reconstructs the submission:
prompt text, attachment bytes where obtainable, tool identity, destination, size, time.

**Step 3 — the effective mode is resolved before anything else happens.** The policy engine evaluates
the scope matrix from the signed bundle — per tool, per data class, per user population — and takes
the **most restrictive** applicable mode. This value decides what may be done with the content. At
M0 nothing further touches content. This ordering is the mechanism behind C5: the mode is not a flag
consulted by the upload path, it is the gate that the observation passes through first.

**Step 4 — content is classified locally, or not read at all.** At M0 the event carries no content
fields. At M1 and above, `classifier-host` runs deterministic rules (payment cards with a checksum,
government identifiers, credential and key shapes) and then the statistical model, within a time
budget. If the budget is exhausted, the event is emitted with rules-only labels and
`confidence: degraded` — never as "no sensitive data found" (C21).

**Step 5 — what leaves the device.** An envelope containing identity, tool, timestamps, mode,
classification labels, a content digest and a size. **No prompt text and no attachment bytes**, at
any mode, until a grant exists. At M2 a minimised excerpt is included by policy; at M3 the device
notes only that it holds content locally.

**Step 6 — the envelope is spooled and sent.** Batches of 1–500 over HTTPS 443 with a per-device
credential and mTLS binding. Offline, the spool fills to its cap; beyond the cap the oldest entries
are dropped, a counter is incremented, and the counter is reported in the next health report (C22).

**Step 7 — ingest validates and writes idempotently.** `ingest-api` authenticates the device, checks
the tenant is active and the credential unrevoked, validates against the versioned schema, and
writes. Idempotency is a unique constraint in the store, not a check in code (C12). Two routes that
observed the same submission collapse to one logical fact through the dedup key, with the
higher-fidelity route winning and both routes recorded (R9).

**Step 8 — content is requested, not pushed.** If policy says the content is worth keeping, the
device asks for a grant for that specific event. `control-api` decides against mode, budget, retention
class and case reference, and answers **granted** (with a single-object upload credential and key
material) or **denied** with one of the reasons in §4.4 (C14). A grant is per event and single-use.
There is no bulk path, which is why C5 holds structurally rather than by configuration check.

**Step 9 — content is stored encrypted and retrievable only through approval.** The device encrypts
with the object key and uploads ciphertext. The wrapped key is stored in Postgres; the key wrapping
it lives in Key Vault. Retrieval requires a case reference and a second approver, and the audit entry
is written before content is returned (C16).

**Step 10 — aggregates are recomputed, not incremented.** A scheduled job recomputes each time bucket
from the immutable event table over a lookback window and **replaces** the bucket. Late-arriving
events are absorbed by the next run. This is idempotent by construction (C28), and it means the
dashboard never scans raw events (C27).

**Step 11 — an analyst asks a question.** The dashboard reads only aggregates for the ten questions in
§3.6. Drill-downs paginate by cursor (C29). Any read that resolves to a person, or to a segment small
enough to identify one, writes an audit row in the same transaction that serves it (C30).

### 4.3 Who can read what

| Actor | Can read | Structurally cannot |
|---|---|---|
| Device credential | Its own tenant's policy bundle; its own grant decisions | Any other device's data; any content key not minted for its own grant |
| `ingest-api` role | Nothing but its own write path | Content, keys — no `SELECT` on content tables |
| `control-api` role | Tenant config, device state, grant metadata | Prompt content, stored keys — it has no grant on `ops.content_object` and no unwrap right. Object keys are minted and wrapped by `content-vault` |
| `content-vault` role | Wrapped keys, ciphertext blobs | Any user-facing endpoint; it has internal ingress only |
| `query-api` role | Events, labels, aggregates, findings, audit | Unwrapped keys; it must call `content-vault` for content |
| Analyst (Entra ID) | Aggregate dashboards; subject-level data with an audit trail; content only through an approved, case-referenced path | Cross-tenant anything (row-level security, enforced by the database) |
| Vendor operator | Infrastructure metadata | Content, under customer-held key mode |

**Row-level security is forced, not merely enabled.** Every application role is a non-owner without
`BYPASSRLS`; the tenant is taken from the authenticated session and never from the request body; a
session that has not set a tenant reads zero rows rather than all rows. This is C32's "structurally
impossible, not merely unauthorised" made mechanical.

### 4.4 Failure paths

The brief's most important reliability property is C25: no path may fail into a state that reports
success. So every failure below has a *visible* output, not just a behaviour.

| Failure | What the system does | What an operator sees |
|---|---|---|
| Ingest unavailable | Device spools; batches retry with jitter and exponential backoff; spool cap enforced with drop-oldest | `collector_state = degraded`, spool depth and dropped count rising; a tenant-level ingest availability alert |
| Spool cap reached | Oldest dropped, `dropped_total` incremented, never silent | An explicit undercount counter on the device's health record and in coverage reporting |
| Duplicate events after a retry | Unique constraint rejects; the response reports accepted/duplicate counts | Duplicates counted in the batch response and in ingest metrics — never dropped silently, never double-counted (C12) |
| Two routes observed one submission | Dedup key collides; higher fidelity wins; both routes recorded on the logical fact | `observed_routes` on the record; reconciliation report reconciles route-level and logical counts (R9) |
| Digest not reconcilable across routes (e.g. canvas UI, WebSocket) | Observation stored but marked low merge confidence; never merged with an unrelated event, never discarded | Reconciliation reports "N observations with non-reconcilable digests" instead of a silently wrong count |
| Device clock skew | Both clocks retained; ordering uses device monotonic offset, display uses server time | Per-device skew reported, never normalised away (C26) |
| Policy bundle signature fails | **Previous bundle retained**, error raised and reported. If there is no previous bundle, content-reading modes are refused and the device falls to M0 — never to "no policy means no restriction" (C10) | `collector_state = tampered`, explicit error code; the device keeps collecting M0 metadata |
| Classifier cannot run or exceeds its budget | Rules-only labels emitted with `confidence = degraded`; user work is never blocked | Degraded-confidence share per device and fleet-wide; alert if it rises |
| Classifier release turns out bad | Central kill switch flips the release to shadow on the next policy poll; devices stop blocking immediately | Release state visible in the console; the rollback is itself an audit entry (C20) |
| Egress proxy provider crashes | Proxy releases the port and the system proxy is restored; egress continues direct; **fails open** | `collector_state = absent` for that provider; the coverage report shows the affected modes |
| Loopback broker crashes while holding the port | Watchdog kills and restarts; the broker releases the port before any restart attempt and only re-binds after the upstream server is proven reachable; if it cannot serve, it must not hold the port (E14) | `collector_state = degraded` with the port state; this is the one path whose failure can break a user, so it is alarmed |
| A pinned or misconfigured client will not cooperate | The process is excluded and recorded as a coverage gap; it is never left broken to preserve collection | A named coverage-gap entry, not a silent omission (E8) |
| Grant denied | Device keeps content locally within its retention, or discards it per policy; the event retains labels and digest | Denial reason surfaced: `retention_expired`, `not_policy_relevant`, `over_budget`, `mode_not_permitted` (C14) |
| Grant expired before upload completes | Upload credential is single-object and short-lived; the object is refused and the grant is void, not silently retried | Grant state `expired`; the event's content state returns to `local_only` |
| Content key destroyed, record still referenced | Retrieval returns an explicit `no_longer_available` with a reason, never an empty result (C17) | The erasure or retention receipt that destroyed it, linked from the event |
| Device credential revoked mid-flight | In-flight batch rejected 401; device stops sending; spool retained, not discarded | Device marked `revoked`; visible in device inventory with the actor who revoked it |
| Aggregation lags | Ingest is unaffected (C13); aggregates converge on the next run | Aggregate freshness watermark shown in the dashboard, so a stale number is never presented as current |
| A device stops reporting entirely | Server-side liveness job marks it stale from `last_seen`, so silence becomes a record | An explicit "devices not reporting" count — the answer to §3.6 question 7 |
| Tenant key unavailable (Key Vault outage, customer key disabled) | Metadata and dashboards keep working; content is temporarily unretrievable | Retrieval returns an explicit key-unavailable error; the outage is alerted separately from data loss |
| Reconciliation finds drift between expiry mechanisms | The finding is recorded and alerted; it is never auto-corrected silently (C34) | `ops.reconciliation_run` drift record with counts |

### 4.5 Decisions that fall out of the brief

These are the places where the brief's requirements make a common default wrong. Each is recorded as
an ADR.

**D1 — Deletion, not crypto-shredding, erases events.** At 4.4M events/year a subject's events number
in the hundreds to low thousands. Deleting them is cheap and produces a *literal* receipt — "these
1,247 rows and these 38 objects were removed" — which is far more usable to a customer than "we
destroyed a key, trust us". Crypto-shredding remains correct for one thing: the per-tenant key, whose
destruction is how tenant offboarding and customer-held-key mode destroy content. Using key
destruction as the *primary* event-erasure mechanism would be over-engineering justified by nothing
in the numbers.

**D2 — No partitioning in v1.** C32 says tenant is leading in "every index and every partition". At
this volume there is nothing to partition: four years of a full tenant is under 18M rows. Partitioning
would add migration and query complexity and buy nothing. Tenant is therefore the leading column of
every primary key and every index, and row-level security is forced on every table. The event table is
built so that monthly range partitioning on receive time can be introduced later without an
application change; the trigger is a tenant exceeding **50M event rows** (roughly eleven years at
full scale, or a 30× larger customer). This is a deliberate, recorded deviation from a literal reading
of C32 — see ADR 0009.

**D3 — No kernel-mode component in v1.** E8 states the coverage boundary that results. The
alternatives are a kernel driver, which E20/E21/E22 make commercially expensive and which adds the
largest possible blast radius, or a measured boundary. We take the measured boundary and make it a
product feature: coverage is reported per provider, per device.

**D4 — No Apple restricted entitlements in v1.** The v1 macOS design needs none of them. Root CA and
proxy are delivered by MDM configuration profile; the service binds user-space ports; process
enumeration is unprivileged. Endpoint Security (E15) would add exec telemetry for mode I and a
Network Extension would extend coverage past E8 — both are **coverage upgrades, not dependencies**.
The entitlement applications are still filed immediately, because E18 makes them a schedule gate and
the upgrade may want them. **This converts R2 from a launch blocker into an optional enhancement**,
and it is the single largest schedule de-risk in the design.

**D5 — Health is an operational channel, not an event stream.** §3.1 warns that an unbounded data
category will dominate everything else. Health emitted as events at hourly cadence across 5,000
devices would be ~44M rows/year — ten times the prompt events the product exists to collect.
So `collector_health` is upserted into `ops.collector_state` by key, and only a daily per-device
rollup enters the analytical store. This is a deliberate deviation from a literal reading of §4.1's
"every collection path must emit the same record", which we apply to *collection* records (prompts,
rollups, detections). See ADR 0011.

**D6 — Content search is a per-tenant capability, and the key model is chosen with it.** This resolves
R10, which the brief requires to be decided *before* either side is built. §3.5 is correct that
customer-held keys and server-side search over content cannot both exist. An earlier draft responded by
refusing content search in every key mode (ADR 0008); **that is reversed on customer requirement, and
ADR 0014 supersedes it.** The resolution now follows §3.5 more literally than the refusal did: the key
model and the search promise are chosen *together*, one tenant at a time.

| `content_search` | Adds | Key custody permitted | Mode required |
|---|---|---|---|
| `disabled` | structured filtering only — tool, user, date, class, rule, severity, review state | any | any |
| `attachment_names` | substring and fuzzy search over **attachment filenames** | any, **including `customer_held`** | M1 or above |
| `full_text` | full-text search over **prompt text**, with highlighted snippets | `vendor` or `customer_managed` | M3 for the scope |

- §3.5's mutual exclusivity is enforced as a **check constraint**, not a policy note: `full_text`
  together with `customer_held` is unrepresentable in the database. The choice is made once, at
  onboarding, which is what "cannot be retrofitted" demands.
- The tenant tier is a **ceiling**; the signed policy bundle narrows it per scope, so search is never
  enabled globally and C5 keeps its meaning.
- Search executes in `content-vault`, not `query-api`, so the invariant that exactly one component can
  read content survives rather than being quietly retired.
- The customer's own export (C31) remains, now complementary rather than the only answer.

Three consequences are stated rather than buried:

1. **For a `full_text` tenant, the vendor can read prompt content.** A full-text index over plaintext is
   itself plaintext-derived. There is no construction that gives search without leakage — searchable
   encryption leaks access patterns and would be indefensible in a security questionnaire — and this
   design does not pretend otherwise.
2. **For a `full_text` tenant, the §1.1 property changes.** Content no longer stays on the device by
   default. The property survives as *the default* and as *a per-tenant choice*; it is no longer
   universal.
3. **The approval gate no longer covers everything.** C16's second approver still protects
   full-content retrieval, but a search returns bounded highlighted snippets. A tenant that requires
   approval for *any* content exposure should not enable `full_text`.

**`attachment_names` deserves separate emphasis.** It satisfies a large part of what customers actually
asked for — searching attachment names — with **no change to the key model and no content leaving the
device**, because a filename is metadata that crosses at M1 regardless. Attachment *contents* are not
indexed: that is the 50–500 GB/year tier, a different cost and a different breach surface, and its own
decision. See ADR 0014.

**D7 — Services are split by trust domain, not by entity.** `content-vault` — the only component
that can unwrap content keys — has internal-only ingress and no user-facing endpoint. Neither
devices nor browsers can reach it. This is why the service count is what it is: the split follows
"who can decrypt", not "which nouns exist".

**D8 — The envelope is a discriminated union on `kind`.** The brief's §4.1 record mixes fields that
only make sense for a prompt (`content_digest`, `labels`, `policy_decision`) with a requirement that
every path emit the same record. Rather than make those fields nullable and lose the ability to
validate, the contract is one record with a common core plus per-`kind` required fields. `kind` is a
closed registry — `prompt`, `usage_rollup`, `model_detection` — which is also the mechanism that makes
R7 structural: a collector defect cannot start shipping raw process telemetry, because the server
rejects any kind not in the registry. See ADR 0010.

**D9 — Suspension is a human decision expressed as two separate gates, and usage is metered forward.**
The product is operated by the vendor as a multi-tenant service and sold monthly, so the design owes two
things it did not previously carry. See ADR 0015.

- **Two gates, not one status.** `ops.tenant` gains `ingest_enabled` and `read_enabled`, separate from
  the commercial `status`. Brief §1 makes this split necessary rather than tidy: there is no vendor-side
  API, so stopping collection **destroys history that can never be re-collected**, while stopping reads
  is fully reversible. A single `suspended` value would hide that choice inside one word.
- **Nothing is automated, so the decision must be informed and recorded.** Closing a gate without a
  reason, an actor and a timestamp is unrepresentable — enforced by a check constraint, not by process.
  `mart.v_tenant_suspension_impact` shows the operator what a closure would strand: devices enrolled,
  devices reporting, events spooled, events already dropped. It is RLS-scoped to one tenant, because a
  bulk view of every tenant's exposure invites a bulk action on the one gate that cannot be undone.
- **Usage is written forward and never derived.** `ops.usage_daily` records accepted events, content
  bytes and a device snapshot per tenant per day, with **no subject reference and no device
  identifier** — so it is not personal data and an erasure neither touches it nor changes it. This
  exists because billing and erasure genuinely conflict: brief §3.4 requires stored counts to *fall*
  when a subject is erased, and the invoice must not. A usage figure derived from `ingest.submission`
  would silently understate the bill after every erasure. **As built:** the table and its grants exist,
  but nothing writes it yet — neither `ingest-api` nor `ingest.record_event()` increments the ledger.
- **`ops.subscription` holds the basis and the period and no price.** The chosen basis is flat per
  tenant plus per enrolled device; keeping it as data means changing what is metered is a row change
  rather than a release. The system produces billable usage; invoicing lives elsewhere.

### 4.6 What would change the recommendation

- **If R1 (local-inference lab validation) fails** — if the common tools cannot be moved off their
  default ports and clients do not resolve a substitute — mode F loses its broker, and it becomes a
  detection-only path like mode I. That changes the §2 matrix, not the architecture.
- **If Apple grants the entitlements quickly**, the macOS coverage upgrade lands earlier and moves
  the provider split slightly; D4 is a schedule decision, not a structural one.
- **If a customer requires customer-held keys *and* prompt-content search**, none of the three tiers
  serves them: `full_text` requires vendor-readable content and `customer_held` forbids it. The answer
  is **on-device distributed search** — a query fans out to devices, each searches its local spool, and
  results return matching event identifiers with minimal snippets. It preserves the key model, and it is
  a substantial subsystem whose coverage is unverifiable when devices are offline or wiped, which is why
  it is not in v1. See ADR 0014.
- **If a tenant exceeds ~50M event rows**, D2's partitioning trigger fires.
- **If a tenant's M3 retention is unbounded and large**, the attachment tier stops being a storage
  problem and becomes an archive problem — cold tier, retrieval latency, and a per-object cost model.
  C14's budget mechanism is what stops this being the default.
- **If ingestion volume grows 100×** (e.g. per-keystroke or per-request capture), the whole storage
  design is wrong and a columnar or log-structured store becomes correct. That is explicitly out of
  scope: C9 forbids the capture patterns that would cause it.

---

## 5. Interfaces and data

### 5.1 Device ↔ cloud APIs

Five endpoints. Deliberately few: every additional device-facing endpoint is another thing to
authenticate, version and keep compatible with clients on machines nobody controls.

| # | Endpoint | Direction | Purpose |
|---|---|---|---|
| 1 | `POST /v1/enrol` | device → cloud | One-shot mutually authenticated enrolment; idempotent re-enrolment returns the existing identity (C11) |
| 2 | `GET /v1/policy` | device → cloud | Signed, versioned bundle; `304` when unchanged; ETag keyed on bundle version (C10) |
| 3 | `POST /v1/events` | device → cloud | Batch of 1–500 envelopes; per-event accepted/duplicate/rejected result with reason codes (C12) |
| 4 | `POST /v1/health` | device → cloud | Upserted collector state: state, version, permissions, last success, spool depth, dropped count (C23, D5) |
| 5 | `POST /v1/content/grant` → `PUT <blob>` | device → cloud | Request a grant for one event; on approval returns a single-object upload credential and key material (C14) |

Retrieval and export are analyst-facing and are reached through `query-api`; they are specified in
[04-dashboard-and-query](04-dashboard-and-query.md). The retrieval endpoint itself
(`POST /v1/content/retrieval`) is served by `content-vault` on its internal ingress. **As built:**
`query-api` serves `POST /v1/query` and its probes only, and does not yet forward retrieval or export. The grant state machine, denial reasons and
upload credential scoping are in [02-ingest-and-transport](02-ingest-and-transport.md).

### 5.2 The event envelope

The full contract is [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json)
(JSON Schema 2020-12), from which TypeScript and Go types are generated so the extension, the capture
core, the ingest API and the query layer cannot disagree about the wire format. **As built:** the Go
types are consumed by `endpoint/protocol` and `ingest-api`. The TypeScript types are generated but have
no consumer, because the extension, `query-api` and the dashboard are plain JavaScript; the extension
instead carries a transcription of the `endpoint/protocol` vocabulary that its contract test checks.

Common core, required on every `kind` — the brief's §4.1 fields, with two deliberate changes:

- **`received_at` is server-assigned and must not be sent by a device.** §3.6 makes server time
  authoritative for display and retains device time for ordering and skew; a device-supplied receive
  time would be neither. The device sends `occurred_at` and `monotonic_offset_ms`; the gateway stamps
  `received_at`.
- **`schema_version` is added**, because §4.3 requires validation "against a versioned schema" and
  there is otherwise no field to validate against.

`confidence` is a closed enum `high | medium | low | degraded`, where `degraded` is the
graceful-degradation signal required by C21. It carries the meaning "the classifier did not complete
its work on this event", which is exactly what must never be reported as "no sensitive data found".

Per-`kind` required fields:

| `kind` | Additional required fields | Emitted by |
|---|---|---|
| `prompt` | `tool_fingerprint`, `size_bytes`, `policy_decision`, `direction: egress` at every mode; at M1 and above also `content_digest`, `labels`, `classifier_version` and `confidence`, all four of which are forbidden at M0; `content_excerpt` at M2 only | Every provider that observes a submission (A–H) |
| `usage_rollup` | `tool_fingerprint`, `window_start`, `window_end`, `submission_count`, `bytes_total` | Providers observing non-submission activity; daily per device per tool. This is the **only** exit for process-level observation (R7) |
| `model_detection` | `tool_fingerprint`, `detection_basis` | Mode I. Says a model ran; carries no prompt, because none is reachable |

`direction` is `egress | ingress | none`. `ingress` is reserved and unused in v1: it is the seam for
per-tenant response-side opt-in that §1.2 asks us to leave without building.

`dedup_key` is derived exactly as §4.1 specifies — tenant, device, tool, a normalised content digest
and a time bucket. The **normalisation is part of the contract**, not an implementation detail,
because two routes observing one submission must compute the same value. The canonical form, the
time-bucket width and the route-fidelity ranking are specified in
[02-ingest-and-transport](02-ingest-and-transport.md) §4 and are covered by a conformance test.

### 5.3 Data model

Four schemas in one PostgreSQL database.

**`ingest` — immutable observations.** Four tables. Two hold the events, because one is not enough to
answer R9 honestly; the third is the quarantine and the fourth is the content search index:

- `ingest.observation` — one row per *observation*, keyed `(tenant_id, event_id)`. Append-only.
  Two routes observing one submission produce two rows here.
- `ingest.submission` — one row per *logical submission*. Aggregates and findings are derived from
  this table, so a count is never inflated by overlapping routes (R9). Both are retained so a
  challenged number can be explained rather than merely defended.
- `ingest.rejected` — quarantine for validation failures, holding the validation error report and a
  content-stripped copy of the envelope, with a short TTL. This satisfies §4.3's "diagnosable from the
  server side without access to the device" without storing content the customer did not ask us to
  keep. A check constraint makes it structurally impossible to put content in it.
- `ingest.search_text` — the content search index of D6: prompt bodies for `full_text` tenants and
  attachment filenames, written and read only by `content-vault`, and cascading from its submission.

**The dedup ladder is two-tier, and this is a correction to an earlier draft.** §4.1 derives
`dedup_key` from tenant, device, tool, a normalised content digest and a time bucket — but §1.1
forbids the collector from reading content at M0, so at M0 the brief's own formula cannot be
evaluated. A single-tier design therefore double-counts every M0 submission seen by two routes, which
is precisely the R9 failure. The resolution:

| Tier | Key | Applied when | Uniqueness |
|---|---|---|---|
| Exact | the device's `dedup_key`, over a canonical content digest | `kind = prompt` and a content digest is present | unique among exact keys only |
| Weak | **server-derived** from tenant, device, tool, **kind**, a 300-second bucket and payload size | everything else: M0 by construction, canvas UIs, established WebSockets, rollups, detections | binds only rows that have no exact key |

The asymmetry is the point. Exact keys never merge with each other, so two *confident but different*
digests stay two rows — canonicalisation divergence is a defect that must be visible rather than
something the database silently papers over. The weak key binds only rows that have no exact key, so
it can never pull a confident row into a merge it does not belong in. An exact observation that
matches an existing weak-only row **adopts** it and supplies the key it lacked, which is what stops
the M0-seen-by-one-route and content-seen-by-another case from counting twice. A row that still has no
exact key keeps `merge_confidence = low`, so the residual uncertainty is visible rather than rounded
away. `kind` is part of the weak key because rollups and detections carry no size at all; without it
they would collapse into each other, fabricating a record that corresponds to nothing.

**`ops` — configuration, lifecycle, evidence and commercial state.** Tenant (including the two
enforcement gates of D9), the user directory dimension, device, device credential, collector state,
policy bundle, tool fingerprint with per-tenant sanctioned state, notice acknowledgement, retention
policy, hold, audit, grant, retrieval grant, content object, finding review, erasure receipt, reconciliation run, aggregate watermark, coverage snapshot,
subscription, and the usage ledger. Full DDL in [database/schema.sql](../database/schema.sql).

**`mart` — derived and rebuildable.** Tool/label/user/organisation/device aggregates and findings.
Nothing in `mart` holds human workflow state: review decisions live in `ops.finding_review` keyed by
finding, so that rebuilding `mart` from `ingest` cannot destroy an analyst's judgement. Every
aggregate is written with `INSERT … ON CONFLICT DO UPDATE` that **replaces** the bucket, never
increments it (C28).

**`ref` — shared reference.** Data classes, classifier releases, rule metadata, route fidelity
ranking. Not tenant-scoped.

The ten questions in §3.6 each map to a specific read path; the mapping is in
[04-dashboard-and-query](04-dashboard-and-query.md) §3.

One requirement the brief implies but does not state: question 3 asks "how much is usage growing,
**per team**", and questions 2 and 8 address people. The system therefore needs an organisational
dimension — department, population, manager — which must be **synchronised from the customer's
directory** (Entra ID, or their IdP), because it is not observable on the endpoint. That is an inbound
data flow the brief does not describe, and it is listed as **Q2** in §7. Until it exists, aggregate
grouping is by tool, class and user only.

### 5.4 States that must never be merged

Brief §3.2 lists five pairs. Each becomes a distinct enumerated column with a check constraint, and
each is asserted by a test that no view, aggregate or API response collapses two values into one.

| Column | Values that stay distinct |
|---|---|
| `ingest.submission.content_state` | `not_captured` · `local_only` · `uploaded` · `shredded` |
| `ops.tool.sanctioned_state` | `sanctioned` · `unsanctioned` · `unknown` |
| `ingest.submission.policy_action` | `blocked` · `warned` · `logged` |
| `ops.collector_state.state` | `healthy` · `degraded` · `absent` · `tampered` |
| `ops.finding_review.review_state` | `disputed` · `confirmed` · `open` |

The content state lives on the submission, not on `ops.content_object`, because two of the four states
describe content that has no stored object at all: `not_captured` at M0 and M1, and `local_only` at M3
before any grant. A row in `ops.content_object` exists only once something has actually been uploaded,
so its own state is the narrower `uploaded | shredded`.

**As built:** nothing sets `content_state` to `local_only`. `ingest.record_event()` inserts every
submission with the default `not_captured`, the envelope carries no content-state marker (ADR 0017), and
no service writes the column, so the `local_only` state is defined in the schema and not yet reached.

`shredded` carries a reason (`retention_expired`, `erasure`, `hold_released`, `tenant_offboarded`) so
that C17's explicit "no longer available" can say *why* rather than just *no*. `not_captured` and
`local_only` are kept apart from `shredded` because "we never had it" and "we had it and destroyed it"
are different answers to give a customer, and a system that cannot tell them apart cannot be audited.

### 5.5 How the data model was verified

A schema that has only been read is a claim, not a fact. [database/schema.sql](../database/schema.sql) was executed
against a real PostgreSQL 17 server — the local lab's version; the deployment target is 16 — and [database/invariants.test.sql](../database/invariants.test.sql) asserts its
properties as the runtime roles rather than as a superuser, because a superuser bypasses row-level
security and would therefore prove nothing about it:

```
psql -v ON_ERROR_STOP=1 -f database/schema.sql
psql -v ON_ERROR_STOP=1 -f database/invariants.test.sql
```

47 assertions covering the mode boundary, the tenant-isolation guarantee, the dedup ladder,
the policy ceiling, the audit hash chain, append-only enforcement, retention materialisation and the
states that must not be merged. The tests are part of the deliverable rather than a one-off check
precisely because the properties they assert are the ones the product's credibility rests on.

---

## 6. Build order

Each step leaves something that runs and can be tested, and each is ordered so that the highest-risk
and highest-privilege work happens **last**. The deliberate principle: *ship the lowest-privilege
mechanism that produces value first, and do not build the interceptor until the product works without
it.*

| # | Milestone | What runs at the end | Why here |
|---|---|---|---|
| 0 | **Contracts and schema** — envelope JSON Schema, dedup normalisation spec, mode semantics, `database/schema.sql`, generated TS and Go types, conformance tests | A schema and a contract test suite; nothing collects yet | Every component depends on the wire format. Getting this wrong is the most expensive error available, and it is the one the product cannot retrofit |
| 1 | **Cloud spine with a synthetic emitter** — ingest, control, storage, aggregator, query API, dashboard, one synthetic device | An end-to-end pipeline answering all ten §3.6 questions from generated data, deployed by CI | Proves the whole server tier and the query shapes with zero endpoint risk. If the ten questions cannot be answered here, no collector will fix that |
| 2 | **Browser extension, M0 then M1** — Chromium only; `webRequest` observation; rules classifier; spool; enrolment; policy | Real classification of real prompts on real machines, with no elevated privilege and no certificate | The highest-value, lowest-privilege mechanism, and the one where content is most reliably obtainable (E1–E3) |
| 3 | **Honesty layer** — health, coverage snapshots, tamper signals, spool-drop counters, drift and skew reporting, device liveness | A dashboard that reports what is *not* being collected | C22–C25 and R11 are properties, not features. Building them after the collectors means retrofitting truth into a system designed to look successful |
| 4 | **Content lifecycle** — grants, content vault, per-object keys, approval workflow, retention, erasure receipts, holds, two-mechanism reconciliation | M2 and M3 real end to end, with receipts | The M2→M3 boundary is a storage and lifecycle boundary (§1.1). It needs the audit and retention machinery from step 3 to be meaningful |
| 5 | **Desktop Capture Core, detection only** — process/model detector, daily rollups, collector health | Mode I and M0-grade volume for desktop tools, with no interception at all | Exercises the Core's lifecycle, spool, policy and update machinery with the smallest possible blast radius |
| 6 | **Egress proxy provider, Windows** — scoped destinations, enterprise root, QUIC disabled, fail-open, kill switch, EDR exclusions shipped | Modes D, E, G, H on Windows | The highest-risk component, built last on the platform where trust-store handling is best understood, behind a kill switch and a ring deployment |
| 7 | **CLI trust shim** — managed shell profile, proxy and CA environment for Go/Node/Python, coverage-gap reporting | Coding agents and scripts | Cheap, uses E10's lever, and its failures are coverage gaps rather than outages |
| 8 | **Loopback inference broker** — port occupation with clean release, watchdog, upstream preflight | Mode F | Gated on R1. Its failure mode breaks the user's tool (E14), so it ships only after lab validation and with the port-release contract proven under crash, kill and repeated-failure tests |
| 9 | **macOS parity** — config profile, root trust, LaunchDaemon, both platforms in the update rings | macOS at Windows parity for modes A–H | Second platform, same code, different trust plumbing (E7) |
| 10 | **Export and administration** — scheduled Parquet export, subject export, tenant offboarding, full tenant lifecycle | C18, C31 and the operational escape hatches | Export stops being the *answer* to search (ADR 0014 provides search directly) and becomes what brief §3.6 actually asks for: a supported path for the customer who wants to run their own SQL |

**Schedule note.** R2 (Apple entitlements) is no longer on the critical path per D4, but the
applications should still be filed in week 1 — they are free to file and their absence becomes
binding the moment a coverage upgrade is wanted. R1 (local-inference validation) gates step 8 only,
so it can run in parallel with steps 0–7 without blocking them.

---

## 7. Open questions

Each with an owner and a way to close it. Items Q1–Q5 change the design; Q6–Q17 change parameters or
scope within it.

| # | Question | Owner | How to close it | Impact if unresolved |
|---|---|---|---|---|
| Q1 | **Data residency.** The brief does not state whether tenants require in-region storage or EU-only processing. **ASSUMPTION:** enterprise buyers in this segment will require it, so region is pinned per tenant and enforced at ingest | Product + legal | Ask the first three design-partner customers | Without an answer, either we over-build per-region deployments or we discover the requirement after the data model is fixed. A region column and a fail-closed check are cheap now and expensive later |
| Q2 | **The organisational dimension.** Questions 2, 3 and 8 need department, population and manager, which are not observable on the endpoint and must come from the customer's directory | Product + integration | Define which directory is authoritative per tenant and how it is synced; Entra ID is the default | Three of the ten headline questions cannot be answered. Scope them out explicitly if no directory sync is available |
| Q3 | **Dedup normalisation across routes.** Two routes must compute the same `dedup_key` from differently-shaped observations. Where a route cannot extract canonical text, the event must be marked rather than silently merged or dropped | Collection lead | Write the canonicalisation spec, then a conformance test with recorded traffic from the extension and the proxy for the same submission | R9 materialises as inflated or deflated counts that cannot be reconciled — the exact failure the brief says customers will challenge |
| Q4 | **Classifier latency in the extension.** §8 allows 150 ms p95 on the interactive path. Whether the statistical model meets that inside a browser WASM sandbox is unmeasured | Classification lead | Benchmark rules-only, rules+small model, and rules+model-on-worker against recorded prompt traffic | If it misses, the model moves to the native host via native messaging, or runs asynchronously with the label arriving on a follow-up record. Either changes the interactive path |
| Q5 | **Mode F port strategy (R1).** Claiming well-known ports assumes every tool can be moved off its default port and every client resolves the substitute | Endpoint lead | Lab validation against the tools customers actually run, both platforms | Mode F drops to detection-only. This is the brief's own top risk and it is a validation task, not a design task |
| Q6 | Default retention per data class and per mode; default per-tenant content budget | Product + legal | Start from the deployment workstream's retention classes; express as policy defaults, not code | Content tiers grow unbounded — the only unbounded cost in the system (§3.1) |
| Q7 | Which classes the deterministic rules cover at launch, and their precision on real prompt data (R6) | Classification lead | Build an evaluation set from consented pilot data before tuning thresholds | A noisy classifier makes the product worse than no product. Ship a narrow high-precision rule set rather than a broad unreliable one |
| Q8 | ~~Content search versus customer-held keys (R10)~~ | — | **Closed by ADR 0014.** Content search is a per-tenant capability; `full_text` requires vendor-readable content and the incompatible pair is unrepresentable in the database. The open part is whether on-device distributed search is needed for tenants who insist on both | — |
| Q9 | Whether the second approver for retrieval is a customer-side role or a vendor-side role, and for which tenants | Product + security | Decide per deployment model; the API carries the field either way | C16 is not satisfiable in an emergency path if the approver is unavailable |
| Q10 | Hold semantics under a conflicting erasure request | Product + legal | Define precedence explicitly and record it in the audit trail | Two requirements pull opposite ways; silence produces inconsistent operator behaviour |
| Q11 | Whether the customer-side content exporter is in scope for v1 | Product | Decide whether v1 export is metadata-and-labels only (recommended) or includes content | Content export under customer-held keys needs a component inside the customer's environment |
| Q12 | Azure platform facts: **`pg_trgm` and `btree_gin` must both be on the Flexible Server allow-list in every target region**, or content search cannot be provisioned; also the instance connection ceiling. The server's own allow-list parameter (`azure.extensions`) is set to both in `azure/modules/postgres.bicep`; what remains open is their availability in each target region. `pgcrypto` and `pg_partman` are no longer needed — `sha256()` and `gen_random_uuid()` are built in | Platform | Check the extension allow-list and tier limits against the target region before provisioning | Content search cannot be provisioned where the extensions are unavailable, which would make ADR 0014 undeliverable in that region |
| Q13 | Whether the browser-agent surfaces (mode C) can be captured above best-effort | Collection lead | Instrument the failure rate on the leading browser-agent UIs (R5) | Mode C stays best-effort, which the brief already permits |
| Q14 | **Make `ingest.observation` reference its `ingest.submission`,** so that erasure and retention cascade instead of relying on both tables being deleted by the same predicate. Today the observation is written before the submission exists and is immutable, so no foreign key can be set — the reconciler carries a check for orphans instead | Data platform | Reorder `ingest.record_event` to resolve the submission first, add the foreign key with `ON DELETE CASCADE`, and accept that `observation_count` becomes a derived value the reconciler recomputes (it already is) | An erasure that removes a submission but misses its observations would leave labels and digests behind after the customer has a deletion receipt — the exact failure the receipt exists to rule out. The check detects it; only the constraint prevents it |
| Q15 | **Whether on-device distributed search is needed** for tenants who require customer-held keys *and* prompt-content search, since no current tier serves both | Product | Ask the customers who require customer-held keys whether filename search is sufficient | Those tenants get `attachment_names` at best, and `full_text` is unavailable to them by construction rather than by policy |
| Q16 | **The index's second independent expiry mechanism.** Brief C34 requires time-based expiry for events *and content* to be enforced by two mechanisms reconciled against each other. The index has one: the sweep of `ingest.search_text.expires_at`, plus a cascade from the submission that is not an independent path. C34 is therefore **currently unmet for this table** — see docs/06 §11.4 and its Q-g | Data platform | Choose a second path: a separately scheduled job deriving its work from `ingest.submission.expires_at` rather than sharing the sweep's predicate, with its own failure reporting and a reconciliation check | An undetected single-path failure leaves terms from erased content in a searchable structure after the customer holds a deletion receipt. This is the one place where the content-search change leaves a stated requirement unmet rather than traded away |
| Q17 | **Whether a search must carry a case reference.** docs/06 ranks this the highest-leverage mitigation for its T13 — search used as a bulk content-reading tool that bypasses the per-event approval gate (residual: high) — and it is a product decision, not an engineering one | Product + security | Decide whether requiring a case reference on `full_text` search is acceptable friction, per tenant or globally | Without it, `full_text` means an analyst can read fragments of content across many subjects with a single query and no approval. Every other mitigation is detection or friction; this is the only one that is a gate |

### 7.1 Risk register

The brief's R1–R11 with the design response, so that no listed risk is left without an owner.

| Risk | Design response |
|---|---|
| R1 local-inference validation | Gates build step 8 only (Q5). Not on the critical path |
| R2 Apple entitlements | **De-risked by D4** — v1 needs none of them. Filed in week 1 anyway |
| R3 Accessibility pre-granting may change | No v1 mechanism depends on Accessibility; it would only affect a future coverage upgrade |
| R4 EDR false positives | Egress proxy is last (step 6), ships with an exclusion artefact and a bake period; kill switch available |
| R5 Browser-agent fidelity | Best-effort by design in §2 mode C; failure rate instrumented from step 2 |
| R6 Classifier false positives | Shadow mode before enforcing (C20); evaluation set required before thresholds are tuned (Q7) |
| R7 Unfiltered process telemetry | **Structural**: the only exit for process observation is a daily `usage_rollup`, and `kind` is a closed registry the server enforces (D8) |
| R8 Document parser exploitability | Isolated child process with memory cap and hard timeout, spawned by the classifier host, never in a browser context |
| R9 Double-counting | Two-table observation/submission model with the dedup key as a store-enforced unique constraint, plus a reconciliation report (Q3) |
| R10 Keys versus content search | **Closed by ADR 0014**, on customer requirement, the other way from the first draft: content search is a per-tenant capability, `full_text` requires vendor-readable content, and the mutually exclusive pair is a check constraint. The trade is disclosed rather than hidden |
| R11 Coverage unmeasurable | Coverage state is an output of the collection path from build step 3, before any interceptor exists |
