# Shadow AI Capture — Device-Side Collection

**Status:** proposed · **Scope:** the endpoint tier of [00-architecture](00-architecture.md)

How brief §2's nine usage modes are captured on a managed device, what each provider may and may not touch,
how content is interpreted before it leaves the machine, and how the device reports what it failed to capture.

*Shadow AI Capture — Product Requirements & Engineering Context* (**the brief**) is the authority.
[00-architecture](00-architecture.md) (**the master doc**) fixes component names, the envelope, the data model
and D1–D8; this document does not re-litigate any of it. Citations are `§5.2` / `R4` (brief), `C25` / `D4`
(master doc), `[master §4.4]`. Anything else is an **ASSUMPTION**, labelled at the point of use and listed in
§18. Names frozen by [contracts/event-envelope.schema.json](../contracts/event-envelope.schema.json):
`capture-extension`, `capture-core`, `classifier-host`, the parser child, and the routes `ext.web_request`,
`ext.page_context`, `ext.dom`, `proxy.tls`, `proxy.loopback`, `proc.detect`, `cli.shim`.

---

## 1. Scope

The device tier: four processes on a managed endpoint, the four capture providers they host, how each of brief
§2's nine modes is reached, how content is classified and gated on-device, how events are buffered and
authenticated, how the device reports its own coverage. Windows and macOS, Chromium-only for browser surfaces
(E23). Not covered: ingest, storage, aggregation, query ([02](02-ingest-and-transport.md),
[03](03-data-platform.md)); analyst surfaces ([04](04-dashboard-and-query.md)); deployment mechanics beyond
the device-side contracts they must satisfy ([05](05-platform-delivery.md)). Legal and privacy review is out
of scope (brief scope note); the functional requirements supporting it — per-tenant ceiling, mode-change
attribution, notice acknowledgement before a content-reading mode — are binding here (§11, §13).

---

## 2. The nine usage modes

Fidelity vocabulary, closed because a coverage report saying "partial" without saying partial *how* is the
failure R11 exists to prevent: `content_full` (prompt text **and** attachment contents, classified on-device);
`content_no_attachments` (prompt text only); `content_best_effort` (some submissions captured, fraction
measured); `identity_volume` (tool, process, count, bytes); `detection_only` (a model ran; no prompt exists).

| Mode | Surface (brief §2) | Provider / route | Observable it relies on | Fidelity | Structurally not capturable |
|---|---|---|---|---|---|
| **A** | Web chat on AI sites | `capture-extension` · `ext.web_request`, `ext.page_context`, `ext.dom` | Body at `onBeforeRequest` with `requestBody` (E2); `File` objects read in page context (E3) | `content_full` for typed prompts; `content_no_attachments` where the composer builds the upload in a worker or canvas | WebSocket messages (E4); response bodies (E4); non-Chromium browsers (E23) |
| **B** | AI features inside non-AI SaaS | `capture-extension` · `ext.web_request`, `ext.page_context` | Request **shape** on the SaaS's own origin — chat-shaped array, prompt-shaped member, generative response contract — not the hostname (C7) | `content_full` for JSON bodies; `content_best_effort` where the feature streams over a WebSocket | The WebSocket gap; in-page model output; opaque or SaaS-proxied body shapes |
| **C** | Browser-agent mode | `capture-extension` · `ext.web_request` + agent marker (§7.5) | The agent's own egress requests and bodies; automation marker in headers or client hints; burst shape per tab | `content_best_effort` for content (R5 anticipates this; §2 permits it). **`identity_volume` is mandatory and is met** | Prompts synthesised inside the agent runtime with no observable body; canvas / closed-shadow-DOM rendering (R5), affecting `ext.dom` only |
| **D** | Desktop AI apps | `capture-core` · `proxy.tls`; `proc.detect` for identity | TLS terminated only for enumerated destinations (§5.1); process and signature identity | `content_full` where the app honours the system proxy and accepts the local root — most Chromium/Electron clients do, many native apps do not (E8); fraction measured per app | Apps ignoring system proxy settings (E8); apps pinning certificates (§5.6) |
| **E** | IDE and CLI coding agents | `capture-core` · `proxy.tls` (GUI IDEs); `cli.shim` + `proxy.tls` (CLI) | System proxy for Electron IDEs; the managed shell environment for CLI agents (E10) | `content_full` where the client cooperates | Clients bundling their own TLS stack, ignoring environment variables, or pinning — detected, excluded, recorded (§5.6, §15.3) |
| **F** | Local inference over a loopback HTTP API | `capture-core` · `proxy.loopback` | Plaintext HTTP body on a loopback port while the broker is in the path (E11); loopback is invisible to appliances (E12) | `content_full` on the intercepted port | Ports the broker does not hold; **the mode is gated on R1** — if tools cannot move off their defaults, F degrades to `detection_only` ([master §4.6]) |
| **G** | Custom scripts and SDKs calling provider APIs | `capture-core` · `proxy.tls` + `cli.shim` | Proxy environment plus an added CA bundle path per runtime (E9) | `content_full` inside the environment the shim manages | Scripts started outside the managed environment — services, schedulers, IDE-spawned processes that clear it |
| **H** | MCP and agentic flows, prompt generated not typed | `capture-core` · `proxy.tls`; `proc.detect` for the MCP host | The egress body, an ordinary chat-shaped request regardless of author | `content_full` | Nothing beyond D/E's client-cooperation boundary; the design does **not** distinguish user- from machine-authored prompts |
| **I** | Embedded on-device models, no local socket | `capture-core` · `proc.detect` | Process image, code signature, loaded modules, listening sockets, compute signature (§4.4) | `detection_only` — brief §2 asks for no more | The prompt, by definition: no socket, no request (§16) |

**Mode E is two mechanisms under one label**: a GUI IDE is intercepted like D, a CLI agent reached via the
shell like G, so §15 keeps two expected/observed pairs under the one mode rather than hiding the case where
one works and the other does not. **Mode C's two halves are measured separately**: "tool identity and volume
are mandatory" constrains the *identity* signal, so a report showing 100% identity/volume and 40% content for
C is the correct, honest output.

### 2.1 Two cross-cutting requirements

**Discovery is behavioural (C7).** No provider decides "generative AI submission" by matching a hostname
against a brand list; the decision is a predicate over observable request structure (§8.2). Hostnames and
destination allowlists exist for two other purposes: scoping interception (brief §1.2 forbids fleet-wide
inspection, so the proxy needs an enumerated set it will decrypt — policy data, §5.1, not the discovery
mechanism) and attribution hints (a destination the tenant already sanctioned or denied per C8 may shift a
prior, but a tool the tenant has never heard of stays discoverable).

**Sanctioned state is per-tenant (C8).** The device stores no global allow/deny; it resolves
`sanctioned | unsanctioned | unknown` from the signed bundle. `unknown` is distinct and behaves distinctly —
no rule matches, nothing is blocked, everything is reported — and is never rendered as `unsanctioned` (brief
§3.2). With no bundle at all the device is at M0 and holds no opinion about sanction (§13.3).

### 2.2 Per-mode notes that change the design

- **Mode B.** "Intercept this host and take the body" would be wrong twice: it would read far more
  non-generative traffic than the brief's intent permits, and it would break the first time a SaaS moved a
  path. The §8.2 predicate runs against every observed request on every host and only matches are emitted, so
  SaaS traffic is observed broadly and evaluated per request (E5's wide observation, narrow emission). Endpoint
  paths are evidence, never identity.
- **Mode C.** The prompt may be written by the agent rather than typed. The design does not distinguish the
  two: the user initiated the run, the payload still leaves a managed device, and brief §2 asks for
  best-effort content with mandatory identity and volume — met with no DOM access at all. **ASSUMPTION (A4):**
  attribution is to the device's signed-in `user_ref`, because the device cannot observe who authored text
  inside an agent runtime.
- **Mode H.** Captured, not excluded: an agent reading a repository and sending it to a model is plausibly the
  highest-volume egress case, and §1.2's non-goals do not exclude it. What is not built is any attempt to
  label an event machine-authored — a judgement the device cannot make defensibly, and getting it wrong would
  silently discount real exposure.
- **Modes E and G** ride the same managed shell environment (§4.5), so their coverage fails together when the
  shim is removed and independently for a client that ignores the environment.

---

## 3. Device process model

### 3.1 The four processes

| Process | Language / form ([master §4.1]) | Runs as | Hosts | May touch | Must never touch |
|---|---|---|---|---|---|
| `capture-extension` | TypeScript, MV3, policy-installed in Chrome and Edge (E23) | Browser process, per profile | Modes A, B, C observation; inline warn/block (E1) | Request metadata and bodies (E2); `File` objects in page context (E3); its own storage | Filesystem outside its storage; sockets; process lists; the spool file; the CA private key |
| `capture-core` | Go, one static binary per platform, privileged service | `LocalSystem` (Windows); LaunchDaemon (macOS) | `proxy.tls`, `proxy.loopback`, `proc.detect`, `cli.shim`; policy engine; spool; ingest client | System proxy configuration; the root CA's public certificate and sealed key; loopback sockets; process enumeration; the spool | Document parsing (R8); document byte buffers; any cloud endpoint but the five in [master §5.1] |
| `classifier-host` | Go, native **and** `js/wasm` from one source ([ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)) | Child of `capture-core`, sandboxed | Rules → validators → model (brief §6); the same core compiled into the extension | Bytes handed over by a provider; signed release artefacts | Sockets; the spool; the CA key; process enumeration; spawning anything but the parser child |
| parser child | Go, separate executable | Child of `classifier-host`, one per document | One document at a time | The single document buffer it was given | Everything else: no network, no spool, no keys, no second document |

### 3.2 Why the extension cannot be merged into `capture-core`

The extension's inputs do not exist outside the browser: `webRequest` with `requestBody` (E2) and the
page-context `File` read (E3) are browser-internal. The alternatives for reaching into a live Chromium process
— a DevTools debug port, or injected code — require the browser to run with debugging enabled, which modifies
the user's browser, is detectable, is a well-known malware pattern (E22), and would make the product
conditional on controlling how the browser starts; brief §5.1 grants a *policy extension host* and nothing
more. And `webRequestBlocking` for inline warn/block (E1) is available **only** to a policy-installed
extension, so the enforcement decision must be reachable there. The cost — two processes to sign, ship and
update, plus a cross-process hop on the interactive path with a message-size ceiling (§3.4) — is worth paying,
because the highest-value, lowest-privilege mechanism then ships first, independently of the interceptor
([master §6] step 2).

### 3.3 What each process may and may not touch

| Boundary | Rule | Why |
|---|---|---|
| Content → spool | Only `capture-core` writes the spool; at M3 content goes to a separate local store. **The envelope never carries prompt text or attachment bytes at any mode** | [master §4.2] step 5; the schema forbids `content_excerpt` at M3 so the wire cannot become a content channel |
| Content → classifier | `classifier-host` gets bytes, returns labels; it never receives a tool identity, `user_ref` or destination | Identity stays out of the classifier's input, so a defect cannot leak it into a label and a model cannot be tuned on who is being classified |
| Document bytes | Only ever inside the parser child | R8: the only component parsing hostile input, and the only one assumed compromisable (§10) |
| CA private key | Sealed per device; used only by `capture-core` to mint short-lived leaves; never exported, escrowed or backed up | A CA key is an interception capability (§5.2); escrowing it turns a per-device liability into a fleet-wide one |
| Model weights and rules | Read by `classifier-host` from the signed release directory; writable only by the policy/update path | A collector that can rewrite weights can change verdicts |
| Network | `capture-core` and the extension's ingest path reach only the five endpoints in [master §5.1] | Other egress from a component that terminates TLS is indistinguishable from exfiltration |

### 3.4 IPC

**Extension ↔ `capture-core`: native messaging** — standard MV3, one host registration per browser per
platform, installed by deployment (§14). **ASSUMPTION (A2):** native messaging is the transport, because MV3
offers no other supported channel from an extension to a privileged process and the brief fixes the
extension's capabilities (E1–E5) without naming one. It is bidirectional, so the extension can be told the
policy version and a destination's effective mode and can return observations — which is what lets the inline
warn/block decision (E1) use policy it already holds instead of a round trip. Two constraints shape the
design: **a browser-enforced message size ceiling**, so attachment bytes are chunked behind a manifest
(filename, media type, byte length, digest) that lets `capture-core` refuse an oversized upload *before*
transfer, with the cap taken from the bundle's mode cap (§11.3); and **the extension cannot read the spool**,
so undeliverable observations are held in extension memory only, bounded, dropped oldest-first with a counter,
and merged into the health report when the channel returns — one writer, one encryption key, one place where
the bound is enforced. A failed connect is reported as `capture-core` `absent` **plus** extension-side
`degraded`, never as "no observations".

**`capture-core` ↔ `classifier-host`: a local socket** — Unix domain socket under the service's directory on
macOS, named pipe on Windows; request/response, length-prefixed, with a protocol version byte. The host is
spawned with the core and stays resident, keeping spawn cost off the interactive path. A **version handshake
on connect** marks `classifier-host` `degraded` on mismatch and falls back to rules-only with
`confidence: degraded`, never failing the submission (C21). A hung or crashed host is detected by request
timeout, killed, and restarted with backoff; "no answer" is `degraded`, never "no labels found". The
extension's WASM copy makes the synchronous inline decision and the native host is authoritative for the
envelope's labels, both from one Go source ([ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)), with a divergence test asserting byte-identical
labels over a fixed corpus. **`classifier-host` → parser child** is a process spawn with a parent-enforced cap
and timeout (§10).

### 3.5 Lifecycle and ordering

```
STARTUP (capture-core)                        SHUTDOWN (capture-core)
 1 load bundle, verify signature               1 provider.Stop all; proxy stops enforcing first
   (else retain previous, or M0 — §13.3)       2 RELEASE THE LOOPBACK PORT (§6.2) before anything else
 2 open and unlock the spool, enforce bound    3 drain spool to ingest, bounded by a deadline
 3 start proc.detect (no ports, no trust)      4 restore the system proxy to its pre-install value
 4 start cli.shim (files only, no ports)       5 unbind 80/443; remove the trusted root if the kill
 5 start classifier-host (idle)                   switch or an uninstall asks for it
 6 start proxy.tls, then point the system      6 stop classifier-host, then exit
   proxy at it
 7 start proxy.loopback LAST, and only after
   its upstream preflight succeeds (§6.3)
```

| Ordering rule | Why it is a contract |
|---|---|
| Broker binds **last** on startup, releases **first** on shutdown | §6.2/E14: a broker holding the port without a serving upstream breaks the user's local AI; no other provider can cause user-visible harm by starting early |
| The system proxy points at the proxy only once it is listening | A proxy configured before it binds turns every request on the machine into a connection failure |
| The spool opens before any provider starts | A provider with nowhere to write must either drop silently (forbidden, C22) or block (forbidden, brief §6) |
| `proc.detect` starts first and stops last | Cheapest provider, and the one that produces the tamper signal when something stops the others (C24) |
| Nothing uninstalls while the spool holds undelivered events without recording it | The dropped count for an uninstall is an operator-visible fact, not a silent truncation (§12.2) |

**Crash and restart.** Supervised by the platform service manager; a crash is a restart with backoff, and every
restart reports a reason code in the next health report. A crash loop — N restarts in a window — stops the
loop, leaves the proxy **off** (fail open, §5.4), releases the loopback port, and reports `absent` with the
loop count, rather than retrying into a state where the user's network is intermittently broken.

---

## 4. Provider catalogue

### 4.1 The common provider contract

One lifecycle for all four, so the core and the coverage report treat them uniformly:

```go
// One provider owns one coverage row; a provider that cannot report its own health is not a provider.
type Provider interface {
    Name() Route                       // one of the seven routes in the envelope contract
    Start(ctx context.Context) error   // returns only when ready to observe, or an error
    Stop(ctx context.Context) error    // idempotent; safe after a failed Start
    Health() Health                    // never blocks; never "healthy" after Stop
    ApplyPolicy(b Bundle) error        // diff application; never restarts the provider
}
type Health struct {
    State       CollectorState       // healthy | degraded | absent | tampered ([master §5.4])
    Detail      string               // closed vocabulary, per provider
    LastSuccess time.Time            // zero means "never" — the field C23 requires
    Counters    map[Counter]uint64   // bounded counter set, §4.3
    Since       time.Time
}
```

Four rules. **`Start` is transactional**: a provider reaches ready or releases everything it took — there is no
half-started state, because a half-started proxy is worse than a stopped one. **`Stop` never fails visibly**:
the error is logged and the provider reported `tampered`, since interference is evidence, and shutdown does
not deadlock on it. **`Health` never says `healthy` when the provider is not in the path**: for the proxy and
broker, "listening" is not "observing" (§5.6, §6.3). **No provider may fail into a state that reports success**
(C25): state derives from a positive observation — a bind, a passed preflight, a completed cycle — never from
the absence of errors. A provider failure degrades one coverage row and nothing else ([master §3],
Alternative B), so the registry starts providers concurrently and one failure does not stop the rest, except
for §3.5's two ordering dependencies.

### 4.2 Health states

Per-provider, never per-device only (C23); [master §4.4] gives the device-level state.

| State | Means here | Examples |
|---|---|---|
| `healthy` | In the path, last unit of work completed | Proxy bound **and** confirmed as the effective system proxy **and** serving |
| `degraded` | In the path or partially, a named capability not working | Broker holding the port with upstream up but classification timing out; proxy listening with a pinned client detected; enumeration blocked for one user's processes |
| `absent` | Not running, not in the path, or crashed out | Failed to start, `Stop`ped, or crash-looped |
| `tampered` | Something external changed it | System proxy rewritten away; trusted root removed; shell profile deleted; broker's port taken; the service stopped |

`tampered` is external-facing: it is C24's signal ("stopped, removed or has its configuration altered …
reported rather than inferred") and the only state raising a security finding rather than an operations one. A
device in `tampered` keeps collecting what it can — [master §4.4]'s policy-signature case is the reference.

### 4.3 Counters: R7 applied to health

Brief §3.1 warns that an unbounded category dominates everything else and R7 makes source-side filtering a hard
requirement; one level down, a per-provider per-event counter set would itself be a high-cardinality stream. So
counters are a **fixed, small, named set** with a closed vocabulary, cumulative since process start plus a
windowed delta, carried on the health channel only — `POST /v1/health`, upserted by key per D5 — never as
events, and rolled up daily at one row per device. **ASSUMPTION (A15):** the set below is closed, and a
provider reports these seven counters and nothing else, because anything richer is the same
high-cardinality-stream problem one level down.

| Counter | Meaning | Used in |
|---|---|---|
| `observed` | Units of work the provider saw (requests, sockets, cycles) | Coverage denominator, §15.2 |
| `emitted` | Envelopes produced | Coverage numerator |
| `skipped_not_generative` | Observed, but the shape predicate said no | Proves the predicate is running — not that it is right |
| `blind_tunnelled` | Destinations connected but deliberately not decrypted | §5.1 — a coverage statement, not an error |
| `not_cooperative` | Clients detected as pinned or misconfigured and excluded | E8, §5.6 |
| `dropped` | Observations lost at the provider before the spool | C22; the spool counts losses after it |
| `errors` | Provider-internal failures by cause | Diagnosis without device access |

`dropped` at the provider and the spool's `spool_dropped_total` are **separate counters**, summed for the
operator: an observation lost before the spool and one evicted from it are different failures with different
fixes, and brief §3.2's "states that must never be merged" applies to counters too.

**How this lands in the schema.** [database/schema.sql](../database/schema.sql) already keys `ops.collector_state` by
`(tenant_id, device_id, collector)`, so per-provider attribution is structural rather than something this
document has to invent; the row also carries `state`, `version`, `permissions`, `last_success_at`, `error_code`
and a `detail` jsonb. The counter map above is carried in `detail.counters`, and `error_code` carries the
closed `detail` vocabulary of §4.4–§4.6 so a coverage report can group by cause without parsing prose. Spool
depth and `spool_dropped_total` are first-class columns because C22 requires the drop counter to be reported,
not buried. That leaves one device-side obligation worth stating: the collector name must come from
`ref.collector`, so a provider cannot invent a name for a coverage path the reporting layer does not know.

### 4.4 `proc.detect` — process and model detector

**Observes** process image path, code-signature subject, version metadata, loaded modules, listening sockets
and a coarse compute signature, sampled on a cycle — no network or content path, so it can never read content.
**Starts and stops** with the core (§3.5): no ports, no trust configuration, started first, stopped last so it
can report tamper signals about the others. What it emits is where R7 is enforced structurally rather than by
discipline:

| Observation | Output | Never output |
|---|---|---|
| Evidence a local model ran — an inference-runtime process with a listening local socket, **or** a loaded inference-runtime module with sustained compute | One `kind: model_detection` with `detection_basis` from the schema's closed set (`process_scan`, `module_signature`) | Anything per-cycle |
| A candidate AI application active in a window where no provider recorded a submission | **One `kind: usage_rollup` per device, per tool, per day** | Per-process, per-exec, per-request or per-cycle records |
| Process identity for attributing another provider's event | A field on that envelope | Its own record |

`usage_rollup` is, per D8, **the only exit for process-level observation**, and the daily granularity is the
point: brief §3.1 warns that unfiltered process telemetry would dominate everything by orders of magnitude,
whereas 5,000 devices × a few tools × one row per day is tens of thousands of rows per day. A rollup may carry
`submission_count: 0`, asserting "this tool was active and we captured no submissions for it" — a coverage
fact, counted as one in §15. A candidate matches an inference-runtime signature **in the signed seed set**
(bundle data, not compiled in) or holds a listening loopback socket in the configured port map, and becomes a
`model_detection` only with evidence of use: an installed-but-idle runtime is not a model that ran, and saying
otherwise would inflate mode I with exactly the overstatement R11 warns about. It is the weakest-signal
provider — when it fails, nothing breaks and detection stops, which is why it reports loudly (`degraded` with
`detail=enumeration_partial` or `detail=signature_set_stale`, `absent` when a cycle misses its window,
`tampered` when the service is stopped). Coverage row: candidate processes seen per cycle, models detected,
rollups emitted; named gaps are processes it could not enumerate and runtimes absent from the seed set. It
**fails open by doing nothing**.

### 4.5 `cli.shim` — CLI trust shim

**Observes** nothing directly; it configures traffic, installing the per-machine shell environment that makes
modes E (CLI), G and parts of H capturable through `proxy.tls`, using E10's lever — CLI tools launch from a
shell, so one managed environment reaches all of them.

| Runtime | Trust (E9) | Proxy (E9) | What the shim sets |
|---|---|---|---|
| Go | OS trust store — works unmodified | Standard proxy variables | Proxy variables only |
| Node.js | Bundled CA list — needs an added bundle path | **Does not read proxy variables by default** | CA bundle path **and** the explicit proxy configuration Node's HTTP stack needs |
| Python (`requests`) | `certifi` bundle — needs a CA bundle path | Standard proxy variables | CA bundle path plus proxy variables |

**Starts and stops** with the core; writes files and environment entries, holds no ports, takes no locks, so
start/stop is cheap and idempotent, and uninstall removes exactly what it added — verified rather than
asserted. **Verification**: writing a file is not coverage, so on a cycle it checks that the profile exists
with the expected content and permissions, that the CA bundle exists, parses and contains the local root, and —
where an environment is readable from a process — that shells and their children actually inherited it; those
three checks are the three counters in its coverage row. **Failure mode**: a client bundling its own TLS
stack, ignoring environment variables, or pinning certificates is detected by its absence at the proxy after
being seen running (§5.6) and recorded as a named gap — a coverage failure, never a broken client. Coverage
row: environment applied to N shells, M processes inherited it, K candidates bypassed it, bypasses attributed
per client so one incompatible tool is visible instead of averaged away. It **fails open by passing every
command through untouched**.

### 4.6 Provider summary

| Provider (route) | Modes | Coverage row | Fails open by | Blast radius if it fails |
|---|---|---|---|---|
| `proxy.tls` | D, E (GUI), G, H | Intercepted destinations: N configured, M reached, K pinned clients excluded | Tunnelling without decryption | **Largest in the product** (brief §5.5): can break egress while in the path and not serving |
| `proxy.loopback` | F | Loopback inference: ports held, upstream reachable, requests brokered | Releasing the port and passing traffic direct | **Inverted** (E14): can stop the user's local model server from starting |
| `proc.detect` | I; identity and volume for D/E/H | Candidate processes seen, models detected, rollups emitted | Doing nothing; it observes passively | None user-visible; the cost is a missing detection |
| `cli.shim` | E (CLI), G | Environment applied to N shells, M inherited, K bypassed | Passing commands through unchanged | None user-visible; client-specific TLS failures contained by §5.6 |

---

## 5. Egress proxy provider — `proxy.tls`

**Observes** TLS-terminated request bodies on an enumerated set of destinations, plus connection metadata for
every other connection it handles. It is the only provider that reads content outside the browser, and
therefore the only one that can reach modes D, G and H at all.

### 5.1 Which destinations are intercepted, and which are blind-tunnelled

Brief §1.2 scopes interception to "an enumerated set of destinations". The enumeration is policy data, not
code, from three sources:

| Source | Content | Set by |
|---|---|---|
| Tenant destination allowlist | Hosts the customer has told us about | Tenant admin, via the signed bundle (brief §4.2: "destination allowlist where applicable") |
| Product seed set | Hosts for the surfaces brief §2 names — mode A's AI sites, D and E's desktop and CLI tools, G's provider APIs | Vendor, as a signed bundle field, **not** compiled in, so it changes without a software release |
| Dynamic promotion | A host `proc.detect` has seen receiving request-shaped traffic from a non-browser process, promoted for a bounded window | Device-local, time-bounded, recorded in the coverage row |

**The seed set is an interception scope, not a discovery mechanism.** It decides *what we are willing to
decrypt*; it never decides *what counts as generative*, which is §8's predicate — the distinction that keeps C7
satisfied while §1.2's enumeration requirement is honoured. A destination in none of the three sets is
**blind-tunnelled**: the CONNECT is accepted, a byte tunnel established, nothing decrypted, and the device
records only host, byte counts, duration and owning process, in a daily rollup (§4.4). **ASSUMPTION (A5):**
bounded, device-local dynamic promotion is this design's invention; without it a novel AI tool reached by a
desktop or CLI client is never decrypted and therefore invisible, contradicting the product's stated value of
finding tools nobody has enumerated, and with it promotion is time-bounded, device-local, reported and
reversible (O5).

### 5.2 Root CA handling

**ASSUMPTION (A3):** a **per-device** CA keypair is generated at install — not per-tenant, not per-fleet.
E6/E7 establish the conditions under which interception works and do not say where the key lives; per-device
generation means a stolen key covers only that device's minted leaves, and there is no vendor-held key that
could be compelled to mint a certificate for a customer's hostname. The public certificate goes into the store
the platform actually honours (E7): Windows, Local Machine → Trusted Root or the Enterprise store; macOS, the
Default or System keychain with *Always Trust* (§14). **The wrong store fails silently (E7)**, so installation
is verified by completing a real handshake against a destination whose leaf we minted, with failure reporting
`degraded` and `detail=tls_probe_failed` — the provider never reports `healthy` on the strength of a
successful file write. The private key is sealed at rest with platform key protection (A1) and readable only by
`capture-core`; leaves are minted on demand per intercepted hostname, short-lived, and never written to disk
beyond the process lifetime. **Removal is as reliable as installation**: uninstall and the kill switch in
`mode: disable` remove the trust entry and destroy the keypair, because E22 means a customer ending a pilot
must not be left with a trusted root installed by software they removed.

### 5.3 CONNECT handling

```
client ── CONNECT host:443 ──► proxy.tls
                                 │
                     host ∈ interception set?
                        │                     │
                       no                    yes
                        │                     │
              byte tunnel, no TLS      terminate TLS with a minted leaf
              record host, bytes,      read request line + headers + body
              duration, owning pid     run the §8.2 shape predicate
                        │                     │
              emit nothing per event   match → classify (§11.2) → envelope
              (rollup only, §4.4)      no match → count, emit nothing
```

- **The client believes it is talking to the real host**, because the leaf is minted for that host under a CA
  the device trusts. Certificate Transparency is not a constraint: CT covers publicly trusted CAs, and a
  locally trusted CA is outside a CT log's scope. **ASSUMPTION (A6):** recorded because a reviewer will ask,
  and because if a client ever requires SCTs this provider's reach shrinks and the coverage row shows it.
- **HTTP/1.1 and HTTP/2 are handled.** A client negotiating HTTP/3 never reaches the proxy: QUIC is disabled
  by policy and UDP/443 is blocked at the egress (E6, §14.1). The proxy does not pretend to support HTTP/3.
- **Non-443 TLS ports are not intercepted by default.** Mode F is plaintext on loopback (E11) and belongs to
  another provider; intercepting arbitrary high ports would widen the blast radius for a minority case. The
  set is per-tenant configurable if a customer's tooling needs it.
- **Bodies are bounded.** An over-cap body is not read into memory: the proxy records `size_bytes` and a digest
  of the first N bytes, classifies nothing, and emits with `confidence: degraded` — the honest signal that
  classification did not complete (C21), never a silent "clean".

### 5.4 Why it must fail open, and what "open" means

**Fail open** means the user's traffic is never blocked, degraded or delayed by this provider's inability to do
its job. Brief §5.5 gives it the largest blast radius in the product; [master §3] names failing open as the
mitigation.

| Trigger | Action | Reported as |
|---|---|---|
| Classifier unavailable or over budget | Carry the request unclassified | `degraded`, `detail=classifier_unavailable`; any event carries `confidence: degraded` |
| Spool unavailable or full | Carry the request; increment `dropped` | `degraded`, `detail=spool_unwritable` |
| Bundle signature failure | Keep enforcing the **previous** bundle; with none, stop reading content (M0, §13.3) | `tampered` |
| Upstream connect failure | Return the connection error the client would have seen anyway; never substitute a response | `degraded`, `detail=upstream_failure` |
| Handshake failure with a client | Stop intercepting that destination for that process; tunnel blind thereafter | `degraded`, `detail=client_pinned`, plus a `not_cooperative` count (§5.6) |
| Provider crash | Supervisor restores the system proxy to its pre-install value; egress continues direct | `absent` |
| Kill switch (server-side) | Enforcement and interception stop on the next policy poll; in-flight requests complete | `absent`, `detail=killed` |

A blocked prompt (policy `action: blocked`, E1) is **not** a failure to fail open: it is the product doing its
job under signed policy, recorded as `blocked` with `decided_locally: true` (brief §3.2 keeps `blocked`,
`warned` and `logged` distinct).

### 5.5 The kill switch, and clients that will not cooperate

**Kill switch.** A field in the signed bundle, not a code path needing a release:

```json
{ "provider": "proxy.tls", "mode": "disable", "effective_at": "2026-10-02T00:00:00Z",
  "reason_code": "fleet_regression_1234" }
```

`disable` propagates on the next policy poll (the `304`-aware `GET /v1/policy`, brief §4.2). It stops
**enforcement and interception first**, then restores the system proxy, then reports `absent` with
`detail=killed` — traffic is never left pointed at a proxy that has stopped intercepting, the one ordering that
would break egress while the kill switch is protecting it. The flip is a central audit entry (brief §1.1: a
mode that changes silently is a defect) and the reason code rides the next health report, so an operator
seeing a coverage cliff can attribute it in one step. `capture-core` stays installed and continues with
`proc.detect`, the spool and the ingest path: the kill switch removes the risky capability, not the product.

**Clients that will not cooperate.** E8 states the boundary — without a kernel component, only applications
honouring system proxy settings are reachable — and brief §5.2 states the rule: a pinned or misconfigured
client is never left broken to preserve collection.

| Detection | Meaning | Action | Recorded as |
|---|---|---|---|
| TLS alert during the minted-leaf handshake, repeated | The client pins a certificate or validates against a bundled CA list | Exclude the destination **for that process**; tunnel it blind from then on | `not_cooperative` plus a named gap (§15.3) |
| A promoted destination the client never connects to | The client ignores proxy configuration (Node.js before the shim; several native apps, E9) | Nothing to exclude at the proxy — it never arrived; the shim is the fix | Coverage gap attributed to the client, per process |
| A pinning client on a destination the tenant wants blocked | The tenant's intent is a block, not an observation | Block without interception, using the fingerprint from `proc.detect` | `blocked` with `decided_locally: true`, or an exclusion if policy says warn |

The first row matters operationally: the exclusion is per process and per destination, it is **automatic**, and
it is bounded in time — re-probed at a slow cadence because a client update can change its behaviour. It is
never a permanent silent omission.

### 5.6 Health, and what is deliberately not decrypted

**Health.** `healthy` requires all three of: bound on the configured ports, confirmed as the effective system
proxy, and a successful end-to-end probe through a minted leaf against a canary host. `degraded` covers
`tls_probe_failed`, `classifier_unavailable`, `spool_unwritable`, `upstream_failure`, `client_pinned`,
`not_effective_proxy`. `tampered` covers the system proxy changed away from us, the trusted root removed, and
the service stopped — the last detected by `proc.detect`, which is why it starts first and stops last.

**Never decrypted, at any destination** — brief §1.2's "no fleet-wide packet inspection" made concrete:
destinations in none of §5.1's three sources (tunnelled blind); **all** traffic from processes excluded for
pinning; non-TLS ports and TLS on ports other than those configured; loopback traffic, which is plaintext and
owned by `proxy.loopback` (E11/E12); any response body, since E4 makes them unavailable in the browser route
and brief §1.2 makes response capture a non-goal — the proxy streams responses through without buffering, and
the contract's `direction: ingress` value exists and stays unused; and WebSocket frames after the handshake
(E4), which are not readable through this mechanism and which the design does not keep a session to try to
read.

---

## 6. Loopback inference broker — `proxy.loopback`

**Observes** plaintext HTTP request bodies sent by a client to a local inference server while the broker holds
the role of that server and forwards to it (E11). Nothing to decrypt, no trust store involved; the whole
problem is being *in the path* (E12) without becoming a new failure mode for the user.

**Port strategy.** The broker occupies the port a local inference server would otherwise bind, which requires
the real server to move — brief §5.3 states this is generally possible through the server's own supported
configuration and explicitly flags it as **unvalidated** (E13/R1). Port assignment is therefore policy data
behind a lab gate: **ports are per-tool bundle entries** (fingerprint, port, mode), with brief §5.3's set
(11434, 1234, 8080) as the starting point rather than a hard-coded list; **two placement options** chosen by
what R1 finds — *client-side redirect* (the server keeps its default, the client is configured to a substitute,
available only if clients resolve a substitute, which is unvalidated) or *server-side relocation* (the server
moves off its default and the broker claims it, with the original value recorded so uninstall restores it
exactly); **every claimed port is declared**, alongside ports it wanted and could not get and ports whose
upstream was unreachable, so a tool with no held port is a named coverage gap rather than an invisible one; and
**the broker never claims a port it cannot serve** (E14), enforced by ordering rather than intention — binding
only after preflight, release before any retry.

### 6.2 The port-release contract

**The one path whose failure breaks the user rather than losing data** (brief §5.3, E14).

```
             preflight fails                ┌──────────────────────────┐
        ┌──────────────────────────────────►│        RELEASED          │
        │                                   │  port not bound by us    │
        │                                   │  upstream untouched      │
        │                                   └───────────┬──────────────┘
        │                                               │ preflight ok
        │                                               ▼
┌───────┴────────┐   serve error / watchdog fail   ┌──────────────────┐
│    HOLDING     │◄───────────────────────────────│     BINDING      │
│  port bound    │                                │ bind + probe     │
│  forwarding    │────────── shutdown ───────────►└──────────────────┘
└───────┬────────┘                                        │ bind fails
        │ process crash / kill                             ▼
        ▼                                          RELEASED (backoff)
   ┌─────────┐
   │ ORPHAN  │  socket closed by the OS on process death
   └─────────┘
```

1. **`RELEASED` is the default state.** The port is held only while the broker actively serves.
2. **Release precedes restart, always.** Every restart path — watchdog, crash recovery, configuration change,
   upgrade — closes the listening socket *first*; no code path rebinds while a previous socket may be open.
3. **Binding requires a passing preflight** (§6.3). If the upstream is not proven reachable on its relocated
   port, the broker stays `RELEASED` and backs off — inverting every other component's instinct, because here
   refusing to start is the safe behaviour.
4. **Process death is a release.** The OS closes the socket; the supervisor does not recreate it before
   re-running preflight.
5. **A stale socket is never inherited.** If something is listening that is not the expected upstream, the
   broker does **not** bind, does **not** kill the holder, and reports `tampered` with
   `detail=port_held_by_other` — the holder may be the user's own server, started on its default port because
   relocation failed.
6. **Uninstall restores.** The server's configuration returns to its original value, the broker stops, the
   port is free, and restoration is verified by the deployment rather than asserted.

**During a release window** a client configured to a relocated server may still address the broker's port. The
broker's answer is to **not be listening at all**: a connection to a closed loopback port fails immediately
with connection-refused, which every client surfaces as an error. This is chosen over a "fail fast" listener
that accepts and closes, because a listener that exists can appear healthy to a watchdog and invariant 1
depends on `RELEASED` being unambiguous. **ASSUMPTION (A7):** connection-refused is the least-bad signal
available; it makes a brief, visible user error possible during a release window, which is a real cost — the
alternative trades that error for an unhealthy state that is harder to detect and therefore likelier to leave
the port held when it should not be.

### 6.3 Upstream preflight, and the watchdog

**Preflight** is a single loopback HTTP request to the upstream's relocated port with a short deadline, then a
close — cheap enough to run often, specific enough to mean something.

| Property | Value | Rationale |
|---|---|---|
| Target | The upstream's relocated port on `127.0.0.1` | Loopback only; the broker probes nothing else (E12) |
| Request | A minimal, read-only request the server documents as safe | **ASSUMPTION (A8):** the path is per-tool bundle configuration; invoking a generation endpoint to test health would consume the user's resources and could itself be observed as usage |
| Deadline | Short — single-digit seconds | A slow preflight delays recovery; a generous one lets a dead server look alive |
| Success | Any well-formed HTTP response, including 4xx | A 4xx proves a server is listening and speaking HTTP; requiring 2xx would fail on a server wanting authentication |
| Failure | Release, back off, report `degraded`, retry with exponential backoff and jitter | Never bind on failure |

The preflight result is also mode F's health input: "port held **and** upstream reachable" is the only state in
which mode F is reported as captured. The **watchdog** is a separate goroutine inside `capture-core` — not a
separate process, because a watchdog that can die with its subject is not a watchdog. A short interval (order
of seconds) runs a TCP-connect liveness probe to the upstream's relocated port: cheap, no request, no side
effects. A longer one (order of a minute) runs the full preflight, so a momentarily busy server does not
trigger a release. Failure ladder: one missed probe → re-probe immediately; two consecutive → release the port,
report `degraded` with `detail=upstream_unreachable`, begin backoff; repeated cycles → backoff caps and the
broker stays released. **The watchdog never binds** — it reports, and the broker's state machine decides, which
keeps a single writer for the port and the invariants auditable in one place.

### 6.4 Crash recovery, repeated failure, and what is validated

| Event | Recovery | Reported |
|---|---|---|
| Broker goroutine panic | Recovered by the provider registry; marked `absent`; restarted after backoff; port re-bound only after preflight | `absent`, then `degraded` |
| `capture-core` killed | Supervisor restarts the service; startup ordering makes the broker the last to bind, so recovery cannot pre-empt the proxy | `absent` while down |
| Upstream server crashed by the user | Preflight fails, broker releases; when the user restarts the server the next preflight succeeds and it re-binds | `degraded`, `detail=upstream_unreachable` |
| Upstream relocated by the user | Preflight to the configured port fails; broker stays released; the coverage row shows the port unserved | `degraded` plus a named gap |
| Port taken by another process | Broker does not bind and does not fight for it | `tampered`, `detail=port_held_by_other` |
| Repeated failure past a threshold | Stop trying for a long cool-down — past a further threshold, until the next policy poll — so a broken configuration does not become an endless bind/release loop against the user's machine | `degraded`, `detail=cooling_down`; **one consolidated coverage gap**, not a stream |

That last row matters: an unbounded retry loop is a user-visible defect (brief §8: perceptible impact is a
defect) and a plausible way to make a user's local AI fail intermittently — E14's failure mode wearing a
different hat.

**The port-occupation hypothesis is unvalidated.** Brief §5.3 says so directly: whether every tool permits
relocation, and whether every client resolves a substitute, "is not yet validated — it needs a lab check
against the tools customers actually run". R1 tracks it, [master §7] Q5 owns it, and this document claims no
capability the brief has not established. The validation gates this provider shipping:

| # | Check | Pass condition |
|---|---|---|
| 1 | Each customer-relevant tool can be relocated by its own supported configuration, both platforms | Relocation survives a service restart and a machine reboot |
| 2 | Each customer-relevant client resolves the relocated port, configured explicitly and when discovering the server | Requests arrive at the relocated server without manual intervention |
| 3 | The broker holds the default port across a reboot, a crash of either side, and an upgrade | Port state matches §6.2's machine in every case |
| 4 | Uninstall restores the original configuration exactly | Byte-identical configuration, port free |
| 5 | A client that ignores relocation produces a clear error rather than a hang | Error within a bounded time |

If checks 1–2 fail for a tool, that tool is not in mode F's port set and F's coverage for it becomes
`detection_only` through `proc.detect` — [master §4.6]'s stated consequence, needing no architectural change.
The broker **observes** the request line, headers and body of requests to an intercepted port plus the client's
metadata; it **does not observe** responses (brief §1.2 non-goal — streamed through without buffering beyond
what forwarding requires), requests to ports it does not hold, non-HTTP local transports, or anything once the
client is reconfigured away from it. Coverage row: ports configured N, held M, upstream reachable K, requests
brokered R — a `K < M` condition is an alarm rather than a footnote, because it means the broker holds ports it
cannot serve, the state §6.2 exists to make impossible.

---

## 7. Browser extension

Manifest V3, Chromium only (E23), policy-installed.

### 7.1 Broad observation, narrow emission

E5 governs: a curated destination list cannot find tools nobody has enumerated, so observation must be broad
while emission stays narrow — separate layers, not a slogan.

| Layer | Breadth | Mechanism |
|---|---|---|
| Observation | Every host the browser visits, under a deployment-controlled host permission | `webRequest` listeners, `onBeforeRequest` with `requestBody` |
| Classification | Every observed request | The §8.2 shape predicate, in the WASM classifier |
| Emission | Only matches whose effective mode permits what was read | Envelope construction in `capture-core` |
| Storage | The extension holds nothing durable | Observations delivered over native messaging |

Three consequences. **The broad host permission is a real privacy surface**: nothing leaves the process except
matched observations and no body is ever written to extension storage — stated here so it appears in the
deployment's permission justification and the customer's notice (§13.4) rather than being discovered in a
review. **A match at a mode that forbids a read must still stop reading**: at M0 (§11.2) the predicate result
is still used — tool identity and volume are still reported — but the body is not retained, hashed or sent,
and the envelope contract *forbids* `content_digest`, `labels`, `classifier_version` and `confidence` on an M0
prompt, so a defect that read content at M0 is rejected at ingest rather than stored. **No request is modified,
and none is delayed beyond the blocking decision**: a prompt reaching the model is byte-identical to the one
the user sent.

### 7.2 `webRequest` + `requestBody`

Registered on `onBeforeRequest` with `requestBody`, where MV3 exposes bodies (E2). **Form encodings arrive as
parsed key/value pairs; everything else as raw bytes** (E2) — AI submissions are the "everything else" case in
practice, so the normal path is a strict UTF-8 decode falling back to treating the payload as binary on a
decode failure rather than lossily replacing characters, because a lossy decode corrupts the digest and breaks
dedup across routes ([master §7] Q3). **The listener must not block unless it is deciding to block**: it sits
on the request path, so any work beyond the predicate's fast path is deferred — body copied, request released,
classification afterwards. The only work permitted to delay a request is the inline warn/block decision
(§7.4), bounded by §9.4's decision budget. **Bodies are bounded** on the §5.3 discipline: an over-cap body is
hashed, sized, and emitted with `confidence: degraded`.

### 7.3 Request-shape classification, and attachment capture

The predicate is specified in §8.2. It runs **before** the mode is consulted for content, because whether a
request is a generative submission is not a content question — shape, size and structure are visible without
interpreting meaning, and they are M0-grade information. A **negative** match is counted, not emitted (§4.3):
that is what makes the predicate's behaviour auditable, since a provider that classifies nothing and one that
classifies everything are both visible in the counters and neither is visible from the event stream alone.

Brief §5.1's third bullet constrains attachments: **file uploads report a filename, never bytes** (E3), so
content must be read from the file object in page context before the request is constructed. A content script
in the extension's isolated world resolves the page's file input or drop target to the `File` objects the user
selected on a positive shape match on an upload-bearing request. **Bytes are read at send time, not at
selection time** — reading at selection would hold bytes for files the user never sends, and the brief's
framing is that the system sees a file *because a user attached it to an AI tool* (§1.2). Capture is
**snapshot-on-send, never read-through**: the file reference is held only long enough to read it in chunks,
hashing and handing bytes to `capture-core` chunk by chunk (§3.4), with nothing written to extension storage.
**A filename alone is not attachment capture**: where the composer builds the upload in a worker or canvas and
no `File` handle is reachable, the submission is recorded as `content_no_attachments` and counted in the
coverage row — the honest treatment of E3's harder half, and why §2's mode A row does not claim `content_full`
unconditionally. **A failed attachment read never fails the submission**: it is counted and reported, and the
prompt text is still captured.

### 7.4 Inline warn/block, and the WebSocket gap

E1 makes inline warn/block possible: `webRequestBlocking` was removed for ordinary extensions, but
**policy-installed extensions retain it**, so an enterprise deployment can warn or block *inline*, while the
request is still pending. **The decision is local**: the extension holds the bundle's rules and classifier
version, and a match resolves to `blocked`, `warned` or `logged` carrying `decided_locally: true` — the
schema's field for exactly this. **`blocked` cancels the request** through `webRequestBlocking`; **`warned`
requires explicit user confirmation** rendered before the request proceeds and recorded as part of the
decision, so the same rule deciding the same way twice is two events, because two prompts were sent. **The
budget is the 300 ms target** (brief §8), not the 150 ms classification target: this is the interactive
warn/block path and the only place the extension deliberately delays a user request. **A blocked request is
still an event** — otherwise the product could not answer "what did we stop", one of the questions the
dashboard exists for. And it **fails open on decision failure**: if evaluation errors or exceeds budget the
request proceeds (`logged`), the event carries `confidence: degraded`, and the failure is counted, because a
broken classifier must not become a broken browser (brief §6).

E4 governs WebSockets: `webRequest` exposes the handshake and nothing after it. **The handshake is captured**
(host, path, headers and timing on the upgrade request), so tool identity and a session count are obtainable
for WebSocket-delivered AI features; **the messages are not**, so such a submission contributes
`identity_volume`, never content. **Streaming is not the same as WebSocket**: where a chat UI posts the prompt
normally and streams the *response* over a socket nothing is lost, because response capture is a non-goal;
where the prompt itself travels over the socket the submission is genuinely uncapturable, and §15 counts it
through the handshake-versus-body ratio. **No workaround is attempted** — keeping a socket open and parsing
frames would mean behaving like the page's own code, and it is not available through the extension APIs. §16
lists this as a permanent structural gap.

### 7.5 Mode C and mode B inside one extension

Both are the same extension running the same predicate on a different host class. **Mode B**: the destination
is the SaaS's own domain, already broadly observed, so the predicate must be sensitive enough to find a
generative endpoint on a domain that mostly carries non-generative traffic and specific enough not to emit on
that traffic. The answer is a shape family (§8.2) requiring **both** a generative request structure and a
generative response contract, so a draft save and a chat call from the same origin are separated by their
bodies rather than their paths. Where the feature is chat-over-WebSocket (§7.4), the handshake plus the origin
still reports `identity_volume` for the tool, which is what makes those features visible at all rather than
absent.

**Mode C**: identity and volume are mandatory (brief §2) and are met with no DOM access, because the agent's
requests are ordinary browser requests and destination plus predicate already produce a fingerprint. Three
signals raise confidence that a run came from an agent rather than a person:

| Signal | What it is | Weight |
|---|---|---|
| Automation marker in request headers or client hints | A header the agent runtime sets on its own requests | High when present, never required |
| Request burst shape | Many similar requests from one tab with no corresponding form interaction | Medium |
| Rendering surface | The agent's UI renders in canvas or a closed shadow root, so `ext.dom` yields nothing | Used only to *explain* low `ext.dom` fidelity (R5), never to decide a submission happened |

The third row is how R5's failure mode is **measured** rather than assumed: where `ext.web_request` sees a
submission and `ext.dom` sees no corresponding composer, the difference is counted, and mode C's content
fidelity is reported as the ratio it actually is.

---

## 8. Behaviour-based tool fingerprinting

### 8.1 From observables to `tool_fingerprint`

The contract is explicit: `tool_fingerprint` is "a behaviour-derived identifier for the destination or
application, not a brand name", because discovery must classify behaviour rather than match a curated list
(brief §2). It is a deterministic function of observable signals, and stable enough that one tool seen through
two routes yields one value — otherwise dedup fails, since `dedup_key` includes the tool (brief §4.1).

| # | Signal | Source | Route-independent? | Notes |
|---|---|---|---|---|
| 1 | Destination host and registered domain where visible | Proxy SNI/CONNECT; extension request URL | For one tool reached from both browser and desktop, only if it uses one destination | An input, never an identity: "runs on `*.example-ai.invalid`" is a fact; "is ExampleAI" is a brand claim this design does not make |
| 2 | Endpoint path shape and method | All routes | Mostly | Normalised: numeric and UUID segments and query values become their shape, so `/v1/c/8f3…/chat` and `/v1/c/91a…/chat` collapse |
| 3 | Request body shape family | All routes | Yes | The structural signature of a generative request (§8.2) — member names and nesting, not values |
| 4 | Response contract shape | Proxy and extension (status, content type, streaming framing) | Yes | Separates a generative endpoint from a look-alike on the same origin |
| 5 | Process identity | `proc.detect`; proxy attribution | No — but it is the same tool across routes, so it feeds the same fingerprint | Image path, code-signature subject, product metadata |
| 6 | TLS ClientHello fingerprint | Proxy only | Yes where the client is the tool itself | Separates a desktop app from a browser on the same host. **ASSUMPTION (A9):** one of six signals, so deprecation degrades precision rather than breaking the fingerprint |
| 7 | Tenant-declared hints | Signed bundle | Yes | A tenant can pin a fingerprint to a name for their reporting (C8); this changes the *label*, never the fingerprint |

`tool_fingerprint = "tf1:" + base32(SHA-256(canonical(signal_vector)))` — a versioned prefix so the derivation
can change without silently re-keying every customer's history. **ASSUMPTION (A10):** the prefix plus a base32
SHA-256 fits the envelope's 128-character field; a schema-bounded field with an unbounded encoding is a defect
waiting to be written.

### 8.2 The predicate: a user-authored payload sent to a generative endpoint

One predicate for every provider, expressed as a weighted rule set `classifier-host` evaluates — data, not
code (§9.5), versioned with the classifier release. Each signal contributes evidence, not a verdict:

| Evidence | Positive indicator |
|---|---|
| Body | A member whose value is an ordered array of objects each carrying a role-like discriminator and a content-like payload |
| Body | A member carrying contiguous natural-language text of non-trivial length — especially the largest string in the payload |
| Body | Model or completion parameters alongside the text (sampling, length, modality selectors) |
| Body | A tool or function declaration array alongside the text |
| Method and body | `POST`, a JSON content type, and a body above a trivial size |
| Path | A segment matching conversational or completion vocabulary — **low weight, evidence only** |
| Destination | Host in the tenant's sanctioned or denied set, or in the seed set — **evidence, never sufficient** |
| Response | A streaming or completion-shaped response contract for the same request |
| Context | The request came from a tab with an active composer, or from a process whose modules indicate an agent runtime |

Tuned for the property the brief needs — **finding tools nobody has enumerated** — so the threshold favours
recall, with precision recovered downstream rather than at the collection point. A false positive that reaches
the classifier costs one event, not a false accusation: labels come from the content, and a non-generative
request with no sensitive content produces an uninteresting event. Where a false positive is expensive is
*enforcement* — a `blocked` decision on a legitimate request — so the enforcing decision is a separate,
higher-threshold policy rule. The asymmetry is deliberate: R6's failure mode ("a noisy classifier makes the
product worse than no product") is about enforcement, not discovery.

### 8.3 The same tool across routes, and `unknown` vs `unsanctioned`

If the extension's fingerprint for a web chat destination and the proxy's for the same provider's API
destination differ, one submission becomes two logical facts and R9 is violated. The answer is a **stability
mechanism, not a mapping table**:

1. **The signal vector is canonicalised** — sorted, normalised, shape-replaced (signals 2 and 3) — so two
   observations of one tool produce one vector even from different routes.
2. **Route-specific signals are excluded from the vector.** Where a signal exists on only one route (the
   extension sees the tab URL, the proxy sees the ClientHello) it may *resolve* a fingerprint but never
   *define* one; including it would guarantee two fingerprints for one tool.
3. **A device-local resolution cache** maps (route, destination, process) → fingerprint. It is a cache:
   rebuildable from observations, never authoritative, and its loss costs a re-derivation, not a wrong answer.
4. **The cloud side holds the authoritative per-tenant tool record** — `ops.tool`, keyed
   `tenant + fingerprint`, carrying the sanctioned state per C8 — so a device can report a fingerprint the
   tenant has already named without knowing the name.
5. **Where fingerprints genuinely differ, the events do not merge.** `dedup_key`'s derivation is normative and
   lives in [02-ingest-and-transport](02-ingest-and-transport.md) §4, and [master §4.4] already specifies the
   behaviour for non-reconcilable digests: stored but marked low merge confidence, never merged with an
   unrelated event, never discarded. This document's obligation is the upstream half — **compute the best
   vector the route permits, and record which route produced the record** so reconciliation can attribute the
   difference ([master §7] Q3).

Three sanctioned states, three behaviours, never merged in any device-side output (brief §3.2, C8):

| State | Where it comes from | Device behaviour |
|---|---|---|
| `sanctioned` | The bundle explicitly permits this fingerprint | No enforcement; full reporting |
| `unsanctioned` | The bundle explicitly denies this fingerprint | Enforcement — block or warn per rule, with `decided_locally: true` |
| `unknown` | No bundle entry, **including a tool just discovered** | **No enforcement.** Full reporting; `sanctioned_state = unknown` on the tool record; the tool appears in the discovery list for an administrator to classify |

The failure this prevents: a newly discovered tool treated as prohibited because it is not on a list, which
would turn the product's core discovery value into an accidental blanket ban. The device never emits a
sanctioned state — the envelope carries `tool_fingerprint`, and the state lives in `ops.tool` per tenant
([master §5.3]).

---

## 9. The classifier host

### 9.1 One source, two targets

Go compiled to both a native child process and `js/wasm` for the extension, per
[ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md), because **two
implementations of a classifier drift**, and drift between what the extension decides inline and what the
native host records would mean the product's own audit trail disagrees with its own enforcement. The
requirement is the shared source, not the language: ADR 0016 records the four constraints that decided
which language could deliver it here.

| Capability | Native | `js/wasm` in the extension |
|---|---|---|
| Rules, validators | Full | Full |
| Statistical model | Full | Full, on a worker within the extension's memory ceiling — exactly Q4's open question |
| Document parsing | **Delegated to the child process** (§10) | **Unavailable.** The extension may not parse a document; it sends bytes to the core, which routes them to the parser |
| Threads / SIMD | Available | No shared-memory threading; SIMD per browser support |

[Master §7] Q4 states the open measurement: whether the model meets 150 ms p95 inside a WASM sandbox is
unmeasured. §9.4's ladder is the contingency, and it changes the placement of one stage rather than the
architecture.

### 9.2 Pipeline: rules → validators → model

Strictly ordered, and the order is a requirement (brief §6: "deterministic rules first … statistical model for
fuzzy classes").

```
bytes ─► normalise ─► RULES ──────► VALIDATORS ──────► MODEL ──────► label set
         (§9.3)      regex and      checksums and      fuzzy classes  + confidence
                     context        structure          (customer_pii, + classifier
                     predicates     verification       source_code,   version
                                                        legal, health)
         │              │                │                 │
         └── every stage emits labels; a stage that does not run is recorded, never silently skipped ──┘
```

**Rules** produce labels carrying a `rule_id` (the schema's `label.rule_id`), which is what makes "why was this
flagged" answerable with a rule rather than a probability. **Validators** turn a pattern match into a
defensible verdict: a payment-card-shaped string is a payment card only if the checksum passes, a
government-identifier-shaped string only if its structure and, where applicable, its check digit hold — the
difference between a rule set a customer trusts and one that flags every 16-digit number. **The model** runs on
what the rules did not resolve, for the fuzzy classes brief §6 lists. **Output is a label set with confidences
and a version, never a boolean** (C19); an event with no labels is a legitimate output meaning the classifier
ran and found nothing, which is materially different from `confidence: degraded`, meaning it did not run
(§9.7).

**Normalisation** (the stage before rules) does decoding, Unicode normalisation, whitespace collapsing and text
extraction from structured bodies. **The same text yields the same digest and labels regardless of route** —
the digest computed here is the one in `content_digest` and inside the dedup key, so normalisation is part of
the dedup contract ([master §7] Q3) and is specified normatively in
[02-ingest-and-transport](02-ingest-and-transport.md) §4. And **normalisation is bounded**: it runs over
attacker-influenced input on the interactive path, so it is linear, allocation-bounded, and refuses
pathological inputs — deeply nested structures, enormous strings — by truncating and marking, with
`confidence: degraded` if the truncation changed what a rule could see.

### 9.4 Latency budget

Targets from brief §8: **≤150 ms p95 interactive**, **≤300 ms p95 warn/block**. Tiered, and every tier has a
defined exhaustion behaviour, because a stage that merely *can* take longer eventually will.

| Stage | p95 native | p95 WASM | On exhaustion |
|---|---|---|---|
| Normalise | 5 ms | 10 ms | Truncate, mark `degraded` |
| Rules (all families) | 20 ms | 35 ms | Stop after the current rule family, emit labels found, mark `degraded` |
| Validators | 10 ms | 15 ms | Same |
| Model | 60 ms | 90 ms | **Skip the model stage**, emit rules-only labels with `confidence: degraded` |
| Envelope construction and spool write | 10 ms | — (native side) | Spool writes are transactional and bounded; a slow write drops the event **with a counter** rather than blocking the submission |
| **Total, interactive** | **≤105 ms against a 150 ms target** | **≤150 ms against the same target** | |
| Inline block decision (extension) | — | 250 ms against the 300 ms target | Proceed (`logged`), mark `degraded`, count it |

Three rules make the budget real. **The host enforces it, not the stages**: the pipeline runs under a deadline
and returns whatever the stages produced when it expires, and a stage cannot extend it. **Exhaustion is never
"clean"**: every exhaustion path emits `confidence: degraded` (§9.7), and no path produces an empty label set
that looks like a negative result (C21). **Cold start is budgeted separately**: the host starts with the core
and stays resident (§3.5), so spawn cost is off the interactive path; if it is not ready when a submission
arrives, the WASM copy classifies it — same source, same version — or, failing that, it is emitted rules-only
with `degraded`, and never dropped silently. The device records the rolling p95 per stage on the health
channel, so Q4 is answered from production telemetry per device class rather than one benchmark.

### 9.5 The rules DSL is data, and why that matters twice

New rules and models must be promotable **without a software release**, evaluated non-enforcing first, and
reversible instantly (C20, brief §6). The mechanism is that rules are **signed data**, not code:

```json
{ "rule_id": "PAYMENT_CARD_PAN", "class": "payment_card", "score": 0.9,
  "when": [
    { "signal": "regex", "dialect": "linear", "pattern": "[0-9]{13,19}", "max_matches": 64 },
    { "signal": "validator", "validator": "luhn" },
    { "signal": "context", "predicate": "not_preceded_by", "value": "test" } ],
  "emits": { "excerpt_kind": "match_span" } }
```

- **Data only.** No expressions, loops, callbacks or host functions; a rule references a closed set of signals
  and a closed set of validators that ship in the binary. This is what keeps "promotable without a software
  release" from meaning "arbitrary code delivered to the fleet".
- **Signed with the bundle**, so a rule set and the policy using it cannot be separated by an attacker.
- **Versioned as a whole**: `classifier_version` identifies a (rules, validators, model, shape-predicate)
  tuple, so a behaviour change is visible as a version change (C19) and an old verdict is never silently
  reinterpreted under new logic.
- **Linear-time matching is a security property, not an optimisation.** The regex dialect is a linear-time
  engine with no backtracking constructs, and the input is attacker-controlled prompt text. With a
  backtracking engine a crafted prompt can pin a CPU inside the extension's WASM sandbox — which runs on the
  interactive path — and freeze the user's own submission: a denial-of-service vector against the user,
  delivered by the product, triggered by content the user pasted. In the native host the same input is a
  per-device CPU burn and a classification timeout. Both are unacceptable, and the fix is structural: **the
  engine cannot backtrack, so the attack does not exist**. Go's `regexp` is RE2-style and has no
  backtracking engine at all, so the property holds by construction rather than by a pattern-authoring
  rule; this is the mechanism, and [ADR 0016](adr/0016-the-classifier-host-is-one-go-source-built-for-native-and-js-wasm.md)
  is where the language was decided.
- **Bounded.** Rule count, matches per rule and pattern length are capped at load time; a signed release that
  violates them is rejected, reported, and the previous release retained (§9.6's retention rule applied to
  classifier data).

### 9.6 Release states: `shadow`, `enforcing`, `rolled_back`

Classification behaviour changes without a software release, so release *state* is policy data in the signed
bundle ([master §4.4]'s classifier-release failure row is this mechanism).

| State | Classification runs | Labels recorded | Enforcement acts | Use |
|---|---|---|---|---|
| `shadow` | Yes | Yes, in full | **No.** Decisions computed and recorded as `logged` with `decided_locally: true` and a shadow marker | Every new release's first state (C20, R6): this is how a false-positive rate is measured on real data before anyone is blocked by it |
| `enforcing` | Yes | Yes | Yes | Promotion, after the evaluation set ([master §7] Q7) and the shadow window agree |
| `rolled_back` | Yes, at the previous version | Yes | **No.** Enforcement stops on the next policy poll; in-flight decisions are not retroactively changed | Instant reversal without a software release |

**Promotion is a bundle field** — `classifier_release: { version, state, rules_digest, model_digest }`: the
device fetches the artefact by digest, verifies it against the signature, loads it, and switches; nothing is
installed, so nothing needs uninstalling. **Reversal is symmetric and immediate** — `rolled_back`, or simply
referencing the previous version, takes effect on the next poll, which is what [master §4.4] requires
("devices stop blocking immediately"). **The transition is a central audit entry**, and the device's health
report carries the release version and state, so a change in enforcement is attributable to a release rather
than a mystery. **Both states coexist**: a device can enforce release N while shadow-evaluating N+1 on the same
traffic, which is how the promotion decision gets evidence without a second deployment. And **a release that
fails to load** — digest mismatch, cap violation, signature failure, version handshake failure with the running
host — retains the previously loaded release, reports `degraded` with a specific cause, and never falls back to
"no rules": C10's retention rule applied to classifier data, on the principle that **the failure mode of a
security component is never "less inspection, silently".**

### 9.7 `confidence: degraded`, exactly

The schema defines it as "the classifier did not complete its work on this event" — the explicit signal C21
requires so a failed classifier is never reported as "no sensitive data found".

| Emitted when | Not emitted when |
|---|---|
| A stage was skipped because its budget was exhausted | The classifier ran and found nothing — a legitimate empty label set with `confidence: high` |
| The model artefact was missing, unloadable or failed to verify | The model ran and returned low scores |
| Normalisation truncated the payload such that a rule could not see all of it | The payload was large but fully processed |
| The document parser failed, timed out or was killed (§10) | The document parsed and contained no matching content |
| A provider handed over content the classifier could not process (over-cap body, undecodable bytes) | The body was decoded and processed |
| The host was unreachable and the event was emitted unclassified | The host answered, even if slowly but within budget |
| A release failed to load and rules-only labels came from the retained release | — |

**`degraded` is required at M1 and above** (the schema's M1+ branch), so a degraded event is valid and
storable — a data-quality signal, not an ingest error. **A degraded share is a first-class coverage metric**:
[master §4.4] requires an alert when it rises, and the per-device number belongs in the same report as the
provider states (§15.2), because "we were collecting but not classifying" is a coverage failure that provider
health would otherwise call healthy. And **M0 can never be degraded**: at M0 no classification was attempted
and no classifier ran, and the schema forbids `confidence` on an M0 prompt — conflating "we were told not to
read content" with "we tried to read content and could not" would corrupt the one metric telling an operator
whether the classifier works.

---

## 10. Document parsing isolation

R8 states the risk: the parser is the highest-risk code in the product, and brief §5.1 requires process
isolation, a memory cap and a hard timeout. Three properties arrive with its role. **The input is chosen by the
attacker, not the customer** — a document is parsed because a user attached it, so anyone wanting to reach a
parser can send a document to an employee and wait for them to paste it into a chat. **It is reached from the
interactive path**, so exploitation needs no unusual behaviour, since attaching a file is the normal case the
product exists to observe. And **what surrounds it is valuable** — a service holding a CA key (§5.2), the spool
key (§12), policy-signing trust and a device credential. The conclusion is one sentence: **the parser must be
assumed compromisable, so nothing valuable may be reachable from it.**

| Property | Specification |
|---|---|
| Placement | A child of `classifier-host`, one per document, spawned for the parse and reaped after it |
| Lifetime | Bounded: one document, one process, so a parser state bug is not cross-document |
| Privileges | The least the platform allows: no network, no service rights, no access to the spool directory, the CA key, the credential store or the policy bundle. macOS: a restricted sandbox profile; Windows: a job object with a low-integrity token. **ASSUMPTION (A11):** the primitives are implementation choices; the requirement is that the child has no filesystem path to key material, verifiable by inspection |
| Input | Document bytes over a pipe. The child has no path to the file's location and never opens a file itself, so it cannot be pointed at an unrelated document |
| Output | Extracted text, offsets, a status code — never a path, a handle or a command |
| Concurrency | Bounded fan-out, so many attachments cannot become a memory-exhaustion event |
| Keys | None. The child cannot encrypt, decrypt, sign or authenticate anything |

Both limits are enforced **by the parent**, because a limit a child enforces on itself is not a limit. A
parent-imposed **memory cap** (job object on Windows; an address-space limit plus residency monitoring on
macOS) kills the child on breach, emits the event with `confidence: degraded` and `detail=parser_memory`, and
does **not** retry the document. A parent-imposed **hard timeout**, well inside the interactive budget and
generous enough for legitimate large documents, kills it with `detail=parser_timeout`. An **output cap** bounds
the result size accepted, truncating and recording `degraded`, so a 500-page document does not become a
500-page label input. A **crash** — non-zero exit, signal, malformed result — is treated identically to a
timeout: `degraded`, counted, not retried, never "no sensitive content". A **per-format failure count** disables
parses for a repeatedly failing format for a period, records a coverage gap per format (§15.3), and leaves the
user's submission unaffected. **ASSUMPTION (A12):** concrete numbers — a memory cap in the tens of megabytes, a
timeout in the low hundreds of milliseconds, an output cap in the low megabytes — are parameters, not design:
R8 requires them finite, parent-enforced and breach-visible, while the values belong to the resource budget
(brief §8) and R8's test plan. **Not retried is deliberate**: a document that kills a parser once will kill it
again, and a retry loop turns an attacker-supplied file into a CPU or memory attack on the user's machine.

**Why parsing never happens in the browser context.** A parser bug in the extension is a bug in the page's
process: the extension lives inside the browser's process structure, so a memory-safety failure there is a
compromised browser holding the user's session on every site they are logged into — categorically worse than a
compromised parser child. The extension cannot be sandboxed further either: it runs under the browser's
privilege model and the deployment can add no memory cap and no killable boundary. And brief §5.1 says so
directly. The extension's role (E3) is to *read the bytes* from page context and hand them up; it never
interprets the format.

**Why not in `classifier-host`.** The host holds the loaded rules and model and is the component every label's
integrity depends on; a parser exploit inside it would sit adjacent to model weights, the rules release and the
classification protocol. The child boundary exists so the worst case is *one document's result being wrong*,
not the classifier being subverted — and the host is the component that must be trusted to say `degraded`
honestly, so it must not be the component an attacker can crash with a crafted PDF.

**Where parsing sits in the pipeline.** A document larger than an inline threshold is parsed **off the
interactive path**: the prompt text is classified and the request released, and the attachment's labels are
produced asynchronously and attached to the same logical submission through its `event_id` and `dedup_key`,
arriving as a correction to the observation rather than as a blocking wait. **ASSUMPTION (A13):** a large PDF
cannot be parsed inside 150 ms on a user's laptop, and blocking the submission on it would make the product
perceptible (brief §8), while always degrading would systematically lose exactly the content the product exists
to classify.

---

## 11. Mode enforcement

### 11.1 The scope matrix and the most-restrictive rule

Collection mode is "a first-class, per-tenant, per-scope setting — per tool, per data class, per user
population — not a global flag" (brief §1.1). The device resolves that matrix to **one effective mode per
observation**:

```
effective_mode(observation) = most_restrictive(
    mode_for_tool(tool_fingerprint),
    mode_for_population(user population),
    mode_for_device(device),
    class ceiling for every class the tool's prior admits     ← see below
)
```

Ordering is `m0 < m1 < m2 < m3` and "most restrictive" always means the lowest value. The subtle axis is data
class, because **a data class is a *result*, and the mode must be chosen *before* the content is read**. The
device therefore holds a **class-prior map** from the signed bundle: for each tool and population, which
classes are expected, with the tool's declared purpose as the prior — a coding assistant's prior includes
`source_code`, a legal-review tool's includes `legal_commercial`. The effective mode is the most restrictive
over those priors plus a **class ceiling** (the tenant's per-class mode for every class the prior admits),
which makes resolution conservative rather than optimistic: a tool whose prior admits `health` resolves against
the tenant's `health` mode even if this prompt contains none. The consequence is stated plainly: **the device
may apply a more restrictive mode than the strictest applicable data class would have required**, because it
cannot know the class without reading — and that over-restriction is visible, since the coverage report shows
the resolved mode per tool, so a customer who has over-restricted a tool can see which scope entry did it.
**ASSUMPTION (A14):** the class-prior map is this design's invention; brief §1.1 makes data class both a
scoping dimension and a classification output and does not say how a device selects a mode before knowing the
class, and priors plus a ceiling is the only ordering that satisfies both "the mode in force is applied before
a policy bundle is signed" (C3) and "applied before content is read" without reading content to decide whether
it may be read (O3).

### 11.2 The mode is applied before content is read

This ordering is what makes C5 structural rather than configurational.

```
observation arrives (network routes: body bytes exist in the provider's buffer — they must, to know a submission happened)
   │
   ├─ 1. resolve effective_mode from the signed bundle   ── no bundle and no previous bundle → M0 (§13.3)
   │
   ├─ 2. if M0:  compute size, compute tool fingerprint, build the M0 envelope,
   │              then DROP the body reference; never hash it, never label it, never excerpt it
   │
   ├─ 3. if M1+: compute the normalised digest, hand bytes to classifier-host
   │
   ├─ 4. if M2:  take the minimised excerpt from the classifier's match span
   │
   └─ 5. if M3:  write content to the local content store keyed by event; the envelope notes only that
                 content is held locally; the grant path (§13.4) is the only route out
```

Two honest observations a less careful document would skip. **"Before content is read" cannot mean "before
bytes are in memory" on the network routes**: to know a request is a generative submission at all — to produce
`tool_fingerprint` and `size_bytes`, both of which M0 requires — the provider must see the request. What M0
forbids is reading the content *as content*: decoding it to text, hashing it, classifying it, retaining it. The
enforcement point is the envelope contract: an M0 record carrying a digest or labels is **rejected at ingest**
(the schema's M0 branch) because it is evidence the device read content it was not permitted to read — the
audit trail for a collector defect, which is why the schema forbids those fields rather than ignoring them. And
**the browser route is the one place where content is genuinely not read at M0**: the extension's `requestBody`
read *is* the content read, so where the effective mode for a destination is already known to be M0 the
extension does not register the body-bearing listener for it and works from request metadata only, with any
resulting weakness in the shape predicate recorded as a coverage property of M0 rather than hidden.

### 11.3 Applying the mode, and the tenant ceiling

| Mode | May read | May retain | Envelope carries | Content leaving the device |
|---|---|---|---|---|
| **M0** | Request metadata, size, destination, process, timing — **not the body as content** | Nothing | identity, tool, times, mode, size, policy decision, and a dedup key derived from an occurred-at bucket and size (schema) | Nothing but the envelope |
| **M1** | Body bytes, for normalisation and classification | Nothing beyond the event | M0 fields + `content_digest`, `labels`, `classifier_version`, `confidence` | Classifications and digest |
| **M2** | As M1 | Nothing beyond the event | M1 fields + `content_excerpt` — minimised, schema-capped at 2048 characters, `match_span` or `redacted_window` | Classifications, digest and a minimised excerpt |
| **M3** | As M1 | **Content, locally**, keyed by event, within local retention | M1 fields, **no excerpt** (the schema forbids it at M3), and a local content-state marker | Nothing until a per-event grant exists (§13.4) |

The M3 row surprises people and is deliberate: **M3 does not put content in the envelope.** The envelope is
what crosses the network by default, and brief §1's defining property is that what crosses by default is "a
classification, a digest and dimensions". M3's content path is the approved retrieval path, and the schema
enforces the distinction by rejecting a `content_excerpt` on an M3 record.

**The M3 row's "local content-state marker" is device-local and does not enter the envelope**
([ADR 0017](adr/0017-the-m3-content-state-marker-is-device-local.md)). The contract is closed
(`additionalProperties: false`) so there is no field for it, and a device's claim to hold content is not
evidence that it does: the server learns content exists when a per-event grant is requested and the upload
arrives. The marker lives beside the content it describes, in the device's local store, and the device's
coverage row may report held-content *counts*.

"Why can't the system be put into an upload-everything state?" has five candidate answers and none of them
works:

| Candidate meaning | Why it is structurally unreachable |
|---|---|
| Every observed request is uploaded | The envelope has no content field at any mode except the M2 excerpt, schema-capped at 2048 characters and restricted to a match span or redacted window. There is no field to put a prompt in |
| M3 content is pushed to the server | Content moves only on an explicit per-event grant, requested by the device and decided by `control-api` against mode, budget, retention class and case reference (C14, [master §4.2] step 8). There is no bulk path |
| The mode is set to M3 everywhere with no budget | The mode is applied before the bundle is signed (C3) and the bundle carries the per-tenant content budget. **A device cannot exceed a ceiling it holds no bundle entry for**: an observation with no matching scope entry resolves to the tenant default, and a matrix that fails to resolve resolves *downward*. No value of "unset" means "everything" |
| A device claims a higher mode than allowed | The effective mode comes from the signed bundle; a device presenting M3 for a tenant capped at M1 produces events the ingest path rejects against the policy snapshot, and it cannot fabricate the bundle because the signature is verified before use (§13.2) |
| An operator exports content | The export path is D6 — metadata and labels — with content reachable only through the approved per-event path. There is no "export all content" operation to misconfigure |

The device's contribution is threefold and each part is checkable: it resolves the mode downward-only, never
sends content on the envelope, and moves content only through the per-event grant. The service-side half is in
[02-ingest-and-transport](02-ingest-and-transport.md).

**Mode-change attribution and the notice gate.** **Every mode change records who and when** (C4): the bundle
carries its own version, `effective_at` and an actor reference per scope change, the device stores the bundle
it is enforcing, and the health report carries that version — so a device enforcing bundle N is attributable to
the change that produced N, the device-side half of "a mode that changes silently is a defect". **The mode does
not change retroactively and cannot be raised over old data**: events are stamped with the mode in force when
observed, and a bundle raising a scope to M3 does not cause the device to seek out old content, because M1 and
M2 retained none. **Content-reading modes require a notice acknowledgement**: brief §3.2 lists notice
acknowledgement per tenant, user and version, and its enrolment entry says the record exists "so enrolment can
require a version before content-reading modes are enabled". The device therefore resolves the effective mode
for a user who has not acknowledged the current notice version to M0 and reports the reason — a named state
rather than a silent downgrade. The check is device-side because the device is what would do the reading; the
acknowledgement record itself lives in the control plane.

---

## 12. Local spool

The spool is the device's only durable store, bounded, encrypted and crash-safe (C22; [master §4.1]'s
SQLite/WAL choice). One writer (`capture-core`), and the bound is enforced on write, not on a timer.

```sql
CREATE TABLE spool_event (
  event_id         TEXT PRIMARY KEY,       -- uuid, minted by the provider
  dedup_key        TEXT NOT NULL,          -- computed per kind; the cloud enforces uniqueness (C12)
  kind             TEXT NOT NULL,          -- prompt | usage_rollup | model_detection (closed registry, D8)
  source           TEXT NOT NULL,          -- the seven-route vocabulary
  collection_mode  TEXT NOT NULL,
  tool_fingerprint TEXT NOT NULL,
  occurred_at      TEXT NOT NULL,          -- device clock (RFC3339)
  mono_offset_ms   INTEGER NOT NULL,       -- monotonic ordering, survives clock changes (C26)
  seq              INTEGER NOT NULL,       -- spool-local insertion order; the drop-oldest axis
  envelope         BLOB NOT NULL,          -- the deviceSubmission record, encrypted
  state            TEXT NOT NULL,          -- pending | in_flight | sent | rejected_terminal
  attempts         INTEGER NOT NULL DEFAULT 0,
  bytes            INTEGER NOT NULL,
  expires_at       TEXT NOT NULL           -- local retention, enforced independently of the network
);
CREATE INDEX spool_sendable ON spool_event(state, seq);
```

**Application-level encryption.** The payload is encrypted by the application, not merely by the disk: the
brief requires the buffer encrypted at rest (C22), and a device whose disk encryption is off, or whose spool
directory is readable by another local user, must not expose metadata or M2 excerpts. Each row's `envelope` is
sealed with an AEAD under a per-device spool key. **Key wrapping (DPAPI / Keychain).** The spool key is a random
per-device key wrapped by the platform's key protection — DPAPI scoped to the service account on Windows,
Keychain on macOS. **ASSUMPTION (A1):** these are the wrapping mechanism, because the brief requires encryption
at rest (C22) and per-object keys (C15) but names no mechanism, and they are the only facilities that work
unattended on a managed endpoint. The key is not derivable from the file, so copying the spool off the device
yields ciphertext, and it is not escrowed or recoverable by the vendor, so destroying the wrapping material
makes the spool unreadable — the correct outcome, and data loss confined to undelivered metadata. Uninstall
destroys the key as well as the file. **What is not in the spool:** at M3, prompt text and attachment bytes do
not live here as envelope data; they live in a separate local content store keyed by `event_id`, with its own
key, retention and destruction path, because the spool is dropped from aggressively and content must not be
silently evicted by event backpressure. At M1 and M2, no content is retained at all.

**The bound, and its default.** The bound is a bundle field ("spool bounds", brief §4.2), per-tenant tunable
without a release, with the default derived from the brief's own numbers rather than taste: 1–4 submissions per
active user per day (§3.1) at 1–2 KB per event row (§3.1) is an order of 10 KB per device per day, with M2
excerpts adding up to the schema's 2048-character cap per event and SQLite overhead of roughly 1.5–2× payload
at page granularity with a WAL index. **ASSUMPTION (A16):** the default bound is **~25 MB or ~25,000 rows,
whichever comes first**, tunable per tenant — two to three orders of magnitude above expected single-day
volume, so ordinary outages never approach it, while a device still survives an ingest outage measured in weeks
(ingest is 99.9% available with devices buffering through outages, brief §8) and the bound stays invisible in
disk terms.

### 12.2 Drop-oldest, crash safety, and a long outage

On reaching the bound, **the oldest `pending` rows are deleted first** (`ORDER BY seq`) — never in-flight or
already-sent rows. `dropped_total` is a **monotonic counter stored separately from the queue**, so it survives
the deletion that produces it; a counter kept in the dropped rows would count itself away. It is reported on
the health channel, surfaces as [master §4.4]'s `dropped_total`, and is rendered as an explicit undercount in
the coverage report — never as a footnote (C22) — and as a **windowed delta** as well as a total, because
"1,204 dropped since Tuesday" is actionable and "1,204 dropped since install" is not. **Drops are attributed**:
each records the kind and route of what was lost, so the operator sees "we lost 800 prompt events from the
proxy route" rather than a bare number. And **the device never silently reduces fidelity to avoid drops** — the
tempting move of dropping M2 excerpts when the spool is tight would make what the product collects depend on a
local condition, and the customer's numbers would then mean different things on different devices. The response
to pressure is to drop oldest and *say so*.

| Situation | Behaviour |
|---|---|
| Power loss or process kill mid-write | WAL gives atomicity: a partially written event is not visible after recovery. Recovery is WAL replay, not repair |
| Crash mid-drop | Deletion is transactional; the counter increment and the deletion commit together, so the count can neither exceed what was dropped nor lag behind it |
| Crash before the key is written | The spool is unreadable and is recreated, reported once as `spool_reinitialised` **with the count of events lost** — an unreadable spool is data loss and must be visible |
| Ingest unreachable for hours | Nothing special: events accumulate, batches retry with exponential backoff and jitter ([master §4.4]), and coverage reports spool depth rising |
| Ingest unreachable for weeks | The bound is reached and drop-oldest begins, by design; the operator sees depth at cap, `dropped_total` rising, and per-route attribution of what is being lost |
| Spool full **and** the outage continues | The collector keeps collecting: it does not stop observing because it cannot store — that would turn a transient outage into a permanent blind spot — and it does not buffer unboundedly in memory. It drops oldest on write, counts, and continues. **One cheap thing is added under pressure:** while at cap, the device emits a single consolidated `usage_rollup` per window per tool instead of per-event records, so the *volume* signal survives the outage even though individual events do not. The rollup is not reported as a substitute for the events — D8's `kind` discriminator makes the difference unambiguous |
| Local retention expiry during an outage | Rows past `expires_at` are deleted whether or not they were sent, counted **separately** from overflow drops: a retention expiry and an overflow drop are different failures with different fixes |
| Uninstall with undelivered events | The installer attempts a final drain with a bounded deadline, records what could not be sent as a dropped count attributed to `uninstall`, then destroys the key and the file |

---

## 13. Enrolment, credentials and policy

### 13.1 Enrolment and device identity

`POST /v1/enrol` ([master §5.1]) is the only call the device makes before it has an identity, and it is
mutually authenticated. **The device proves its entitlement**: a deployment secret from the MDM-delivered
configuration plus an on-device keypair whose public half is presented, the private half never leaving the
device and never escrowed. **The server proves itself**: the device validates the server certificate against a
pinned issuer established by deployment, not the platform default store alone, so a rogue endpoint cannot
enrol devices into a hostile tenant. **Result**: `device_id`, a per-device credential, a client certificate for
mTLS, and the tenant's initial signed bundle — stored under the same key protection as the spool (§12).

C11 requires re-enrolment after a re-image to be idempotent and to return the existing identity. **ASSUMPTION
(A17):** the MDM device identifier is the idempotency key — nothing on the endpoint itself reliably survives a
re-image, and the brief's device entity already carries an MDM id, so the field exists in the data model. The
server treats the seed as a natural key: a re-enrolment with the same seed returns the existing `device_id`, a
freshly minted credential (the old one revoked in the same transaction), and the current bundle. **A
re-enrolment with a new seed** — possible if the MDM record is recreated — returns a new device identity, and
the previous record becomes a stale identity the liveness job marks as not reporting ([master §4.4]). That is
the honest outcome: the system does not guess that two device records are one machine, because guessing wrong
merges two devices' data. **Re-enrolment never duplicates data**: events are keyed by `event_id` and deduped by
`dedup_key`, so where the seed is stable both survive the identity change, and where it is not, the two records
stay distinct and the customer sees two devices — visible and explainable beats silently merged.

**Credentials are per-device, individually revocable** (brief §4.2). Revocation is effective at the next
request: the ingest path rejects the batch with 401, the device stops sending, and the spool is retained rather
than discarded ([master §4.4]). **mTLS binding**: the credential is a client certificate and the private key is
sealed to the device, so a copied credential cannot impersonate a device — which is what makes `device_id`
trustworthy enough to be a leading dimension of the data model. **Rotation** happens at re-enrolment and on a
schedule, with an overlap window so a rotation cannot itself cause an outage; the schedule is deliberately
**not** tied to code-signing validity (E19), because that 460-day clock governs the installer identity and
coupling two unrelated expiries would create one outage date instead of two independent ones. **ASSUMPTION
(A18):** credential lifetime and rotation cadence are deployment parameters; the brief requires revocability
(C11) and is silent on lifetime. A revoked device is rejected and marked accordingly, the mark visible in
device inventory with the actor who revoked it.

### 13.2 Signed policy bundles

`GET /v1/policy` returns a signed, versioned bundle with an `ETag` keyed on bundle version, so unchanged policy
produces a **`304`** and no re-download (brief §4.2, C10). Contents are the brief's list — classifier version,
collection mode per scope, retention class, destination allowlist, spool bounds, feature state per collector —
plus, here, the classifier release state (§9.6), the provider kill switch (§5.5), the interception enumeration
(§5.1), the loopback port map (§6.1), and the shape-predicate parameters (§8.2). Verification stops at the
first failure:

```
bundle bytes ─► signature valid under the tenant's pinned policy key?  ── no ──► §13.3
             ─► schema valid?                                         ── no ──► retain previous, error
             ─► version not older than the one in force?              ── no ──► retain previous, error
             ─► artefact digests (rules, model) resolvable?            ── no ──► retain previous, error
             ─► apply per-provider diffs (never a restart, §4.1)
```

**`304` is not a failure and bumps nothing** — it refreshes the poll timer, so a device offline for a week
converges on its next successful poll. **The bundle in force is stored durably and survives a restart**,
encrypted under the same key protection as the spool; a device that boots without a reachable control plane
enforces the bundle it last had. **An older bundle is never accepted**: downgrade would be a way to weaken
policy, and the version check is what makes the signature meaningful over time.

### 13.3 Signature-failure behaviour

1. **A bundle whose signature does not verify is discarded.**
2. **The previous bundle is retained and remains in force** — not partially applied, not applied with the
   signature check skipped: the bundle in force does not change.
3. **The device never falls back to an unsigned or empty policy.** No code path lets the absence of a valid
   bundle widen what the device may do. This is the failure mode C10 names and the most dangerous possible
   default in a monitoring product: "no policy means no restriction" would turn an attacker who can block one
   HTTP request into an attacker who can switch the product off.
4. **An error is raised and reported**: `tampered` with a specific cause — `bundle_signature_invalid`,
   `bundle_schema_invalid`, `bundle_version_regression`, `bundle_artefact_missing` — while the retained bundle
   stays enforced.
5. **If there is no previous bundle** — a fresh install whose first bundle fails verification — the device
   **falls to M0**: metadata only, no content read, condition reported. [Master §4.4] states this and it is
   repeated because it is easy to get wrong: M0 is a *reduction* in capability, never an increase. A new device
   that cannot be given valid policy watches which tools are used and nothing more.
6. **The classifier release is subject to the same rule** (§9.6): an unloadable release retains the previous
   one, and with none, rules-only classification from the baseline with `confidence: degraded` — never
   unclassified-and-silent.
7. **Repeated failures escalate**: after a threshold the device reports at higher severity and backs off its
   polling, so a fleet-wide signing problem does not become a request storm. The bundle remains in force
   throughout.

### 13.4 Notice acknowledgement, and the content egress path

**Notice acknowledgement.** Brief §3.2 requires it per tenant, user and version. The bundle carries the
required notice version; the control plane reports which versions a `user_ref` has acknowledged; the device
resolves the effective mode for an unacknowledged user to **M0** and reports the reason (§11.3). This keeps
brief §1.1's M0→M1 permission boundary from being crossed by configuration alone.

**The content egress path** is the grant flow (C14, [master §4.2] steps 8–9). Device-side: the device requests
a grant **per event** with the case reference and the event's identifiers; on grant it receives a single-object
upload credential and key material, encrypts, and uploads ciphertext, never reusing the credential for a second
object and never requesting a second grant in a loop for the same event; on denial it keeps the content locally
within retention or discards it per policy, with the event retaining labels and digest and the denial reason —
`retention_expired`, `not_policy_relevant`, `over_budget`, `mode_not_permitted` — surfaced rather than
swallowed; on grant expiry mid-upload the object is refused, the grant is void, and the event's content state
returns to `local_only`, with no silent retry ([master §4.4]). The store-and-forward queue for content is
**separate from the event spool** and separately bounded, so backpressure on content never evicts events, and
vice versa.

---

## 14. Platform specifics

### 14.1 Windows

| Concern | Design | Source |
|---|---|---|
| Trust store | The root CA's public certificate into **Local Machine → Trusted Root** or the **Enterprise / Group Policy** store. Both are honoured; nothing else is | E7, brief §5.2 |
| Verification | A real handshake against a canary host through a minted leaf; failure reports `degraded`, because the wrong store fails silently | E7, §5.2 |
| System proxy | Machine policy (WinHTTP/WinINET) pointing at the local proxy's loopback port, with a bypass list for loopback and the ingest endpoint. The provider confirms it is the *effective* proxy, not merely that a setting was written | §5.6, brief §5.2 |
| QUIC | **Disabled by browser policy and UDP/443 blocked at the egress.** Browser policy alone is insufficient — a client that ignores it still speaks HTTP/3 | E6, brief §5.2 |
| Kernel driver | **None in v1** (D3): no WFP callout, no minifilter, no ETW content capture | D3 |
| Service | `capture-core` as a Windows service running as `LocalSystem`, restrictive service ACL so a non-admin cannot stop or reconfigure it, registered with restart-on-failure | §3.5 |
| Key protection | DPAPI scoped to the service account, for the spool key, the CA key and the device credential | A1, C22 |
| Extension deployment | Chrome and Edge **separately** — one policy per browser | E23 |
| Parser child | A job object with a memory cap and a low-integrity token | §10 |

**What the missing kernel component costs**, as a coverage boundary rather than a footnote (E8, D3):
applications that ignore system proxy settings are unreachable (§5.5) — several native apps, Electron builds
with custom networking, anything constructing raw sockets; per-flow attribution without interception is limited
to connection metadata the OS exposes without a driver, so blind-tunnelled flows are attributed at process
granularity from `proc.detect`; **HTTP/3 is not collected at all**, because it is disabled by policy so
interception works and a client bypassing policy and reaching UDP/443 is *blocked rather than observed*, which
is right for a client ignoring policy and means the traffic is a named gap; and nothing observes syscall- or
memory-level activity, so mode I detection is process-, module- and socket-based only.

### 14.2 macOS

| Concern | Design | Source |
|---|---|---|
| Root trust | A **configuration profile** installs the root CA into the **System** (or Default) keychain with *Always Trust* — the stores macOS honours | E7, brief §5.2 |
| System proxy | The same configuration profile sets the system proxy to the local proxy's loopback port | brief §5.2 |
| Service | `capture-core` as a **LaunchDaemon**, starting before login and surviving user-context changes, so the broker's port work happens in daemon context | §3.5 |
| Process enumeration | Unprivileged enumeration of processes, signatures, modules and sockets; `proc.detect` needs no entitlement | [master §4.1], D4 |
| Key protection | Keychain for the spool key, CA key and device credential, access restricted to the daemon | A1, C22 |
| Parser child | A restricted sandbox profile plus a parent-enforced memory cap | §10 |
| Extension deployment | Chromium (Chrome); Safari and Firefox unreachable in v1 | E23 |
| Code signing | Developer ID signing and notarisation; the 460-day validity governs the installer identity, rotation automated | E19 |

**v1 needs no Restricted Entitlements** (D4) — the design's largest schedule de-risk, and worth being explicit
about why each candidate is unnecessary:

| Entitlement | What it would add | Why v1 does not need it |
|---|---|---|
| **Network Extension** | Per-flow interception independent of system proxy settings; flow-level control; coverage past E8 | v1's interception is a userspace HTTP proxy reached through the **system proxy**, which is the reachable model brief §5.2 describes, and mode F is a userspace loopback port (E11/E12) needing nothing either. A Network Extension would *extend* coverage to clients that ignore the system proxy — a coverage upgrade, not a dependency |
| **Endpoint Security** | Exec, process, file-metadata, mount and authentication events (E15) | `proc.detect` reaches mode I with unprivileged process, module and socket enumeration; ES would add sharper exec telemetry and earlier detection. Its ceiling is explicit: **E15 says ES has no payload read**, so it could never contribute content. A detection upgrade, never a content mechanism |

Three operational consequences. **The applications are still filed immediately** (E18; [master §6]'s schedule
note): approval is a schedule gate with no published SLA, and declining to file because v1 does not need them
would put a future upgrade on the critical path — filing is free, needing them without having filed is not.
**Nothing depends on Screen Recording, ever**: E16 is unambiguous that it can never be pre-granted by MDM, that
it is deny-only, and that any dependency breaks zero-touch deployment permanently, so the design has no
screen-capture mechanism, no OCR path and no browser-agent fallback that would need one (§16 lists
screen-capture territory as permanently out of scope). And **Accessibility, Full Disk Access and
Files-and-Folders can be pre-granted (E17), but no v1 mechanism depends on Accessibility either**, because R3
warns that silent pre-granting of it may be changing; where it would help, it is an enhancement behind a
capability check.

### 14.3 Cross-platform summary

| Concern | Windows | macOS |
|---|---|---|
| Trust store | Local Machine → Trusted Root / Enterprise (E7) | System or Default keychain, Always Trust (E7) |
| Trust delivery | Installer plus machine policy | Configuration profile |
| System proxy | Machine policy (WinHTTP/WinINET) | Configuration profile |
| QUIC | Browser policy **and** UDP/443 egress block (E6) | Browser policy; whether the egress block is equally expressible by profile is **an open item** (O6), since brief §5.2 states it only for the Windows case |
| Service | Windows service, `LocalSystem`, restrictive ACL | LaunchDaemon |
| Kernel component | None (D3) | None (D4) |
| Restricted entitlements | n/a | **None required in v1** (D4) |
| Key wrapping | DPAPI | Keychain |
| Browsers | Chrome + Edge, separate policies (E23) | Chrome (E23) |
| Shell environment (E/G) | Machine-level environment plus a profile script | A daemon-delivered profile script plus path entries. **ASSUMPTION (A19):** exact file locations are deployment details; the requirement is a per-machine configuration every shell inherits, which is E10's lever |

---

## 15. Coverage measurement

### 15.1 Coverage state is an output of the collection path

R11 requires coverage as an output of the collection path "from the first version", because gaps that are not
measured get reported as success. The row is therefore not reconstructed later from event absence; it is
produced by the same code that produces events. **Every provider emits its own coverage row** from the §4.3
counters plus a state and `last_success`; **the row travels on the health channel**, upserted per device per
provider (D5's operational channel, `POST /v1/health`) and rolled up daily into the analytical store as one row
per device, not one row per counter per hour; **`last_success`'s zero value means "never"**, a different fact
from "not recently", rendered differently; and **a device that stops reporting becomes stale server-side** from
`last_seen`, so silence becomes a record rather than an inference ([master §4.4]).

### 15.2 The per-provider row, and the expected-versus-observed model

| Provider | `observed` counts | `emitted` counts | What the ratio means | Named gaps |
|---|---|---|---|---|
| `proxy.tls` | Connections to enumerated destinations | Envelopes emitted | Interception reach **and** predicate selectivity together; the two are separated by `skipped_not_generative` | Pinned clients, non-cooperating clients, non-443 ports, QUIC-blocked flows |
| `proxy.loopback` | Requests arriving on held ports | Envelopes emitted | Broker reach for mode F | Ports not held, upstream unreachable, tools that could not be relocated |
| `proc.detect` | Candidate processes per cycle | `model_detection` plus daily `usage_rollup` records | Detection reach for mode I, and identity/volume for D/E/H | Enumeration gaps, runtimes absent from the seed set |
| `cli.shim` | Shells started with the environment; children inheriting it | Envelopes from `cli.shim` routes | Shim adoption for modes E (CLI) and G | Shells that bypassed the profile, processes that cleared the environment |
| `capture-extension` (`ext.*`) | Requests observed under broad host permission | Envelopes emitted, split by route (`ext.web_request`, `ext.page_context`, `ext.dom`) | Browser reach and fidelity per route | WebSocket-only features, unreachable file handles, non-Chromium browsers |

**The degraded-confidence share belongs in this table too** (§9.7): a provider can be `healthy` while the
classifier exceeds its budget, and "we collected but did not classify" is a coverage failure no provider state
reveals.

A raw `emitted` count proves nothing without a denominator, and the denominator must not be circular — a system
defining "expected" as "what we saw" reports 100% coverage forever.

```
expected(provider, window)  = the population this provider should have seen, derived from a signal
                              the provider does NOT produce
observed(provider, window)  = what it accounted for, by disposition: emitted | skipped_not_generative |
                              blind_tunnelled | not_cooperative | dropped
coverage(provider, window)  = accounted_for / expected, reported with the disposition breakdown
```

| Provider | Expected population comes from | Independent because |
|---|---|---|
| `proxy.tls` | TCP connections to enumerated destinations, seen by `proc.detect`'s socket enumeration | A different provider using a different mechanism |
| `proxy.loopback` | Listening sockets attributable to local inference servers, plus the configured port map | From process and socket enumeration, not from brokered requests |
| `proc.detect` | Processes whose image signature or modules match an inference-runtime signature in the seed set | The *set* is bundle data; the *observation* is enumeration |
| `cli.shim` | Shell processes started, and their environments where readable | From `proc.detect` or the profile's own bookkeeping, cross-checked |
| `capture-extension` | Connections to the same destinations seen by `proxy.tls` or `proc.detect` | Cross-route comparison; the extension does not know what the proxy saw |

Three properties keep this honest. **`accounted_for`, not `emitted`** — a request the proxy handled but
classified as non-generative is accounted for, and treating it as a failure would punish correct behaviour and
push the team to tune the predicate to inflate the number. **The dispositions are never summed into one
number** — "94% coverage" hides whether the missing 6% was blind-tunnelled (by design), not cooperative (a
client problem) or dropped (an outage); the report shows all five, always. **The ratio is never presented
without its denominator size** — a provider with an expected population of three connections and 100% coverage
is not evidence of anything.

### 15.3 Pinned clients, and the 70–85% reality

Brief §5.2's rule is explicit: "A pinned or misconfigured client must never be left broken in order to preserve
collection. Detect the failure, exclude the process, and record that coverage was not achieved for it." So:
**detect** (the TLS handshake failure, the client's absence at the proxy after promotion, or the client's own
error surfaced to the user); **exclude** (stop intercepting that destination for that process, immediately and
automatically, and tunnel it blind — the exclusion is scoped to the process+destination pair, not global);
**record** (a named gap carrying process identity, destination and detection time, appearing as
`not_cooperative`, attributable to a process, re-probed at a slow cadence so a client update restores coverage
without operator action); and **never** leave the client broken, retry indefinitely producing repeating
user-visible errors, or omit it from the report. The distinction between `not_cooperative` and `absent`
matters: the provider is fine and the client will not participate, so merging them would hide a fleet-wide
client incompatibility inside a provider outage.

E24 says to plan for 70–85% management coverage, never 100% — a planning assumption about **how many devices
the customer manages at all**, a different axis from provider coverage on a device that has the collector. The
design keeps the axes separate and shows both: **managed-device coverage** (devices the customer's management
plane knows about versus devices with an active healthy collector, from server-side liveness plus an inventory
denominator) and **per-device coverage** (§15.2, computed on the device). The inventory denominator is an
**external dependency** — nothing on an endpoint can count devices the collector is not installed on — and
**ASSUMPTION (A20):** it comes from the customer's management plane (the same source as the device seed,
§13.1) or from the customer's directory ([master §5.3]'s organisational dimension), because without a
denominator the honest answer is "unmeasurable", which is still better than reporting 100%. Three presentation
rules make the number honest rather than decorative: **the unobserved remainder is a number, not an absence**
("4,180 of 5,000 managed devices reporting" is the answer to brief §3.6's question 7, and a dashboard showing
only the 4,180 is showing a success); **a device whose collector is present but whose providers are all
`absent` counts as unobserved**, not as observed-but-quiet — C25 applied at the reporting layer, because a
broken collector must not improve the coverage number; and **every mode in §2 has a stated coverage line**,
including modes with no provider at all (mode C on a non-Chromium browser per E23, any mode on an unmanaged
device), since "not covered, and here is why" is the output the product is judged on.

### 15.4 Tamper signals

C24 requires interference reported, not inferred. Each detection maps to a `tampered` state and a named cause:
`service_stopped` (detected by `proc.detect`, which is why it stops last, §3.5); `proxy_config_altered`;
`root_untrusted`; `shim_removed`; `port_held_by_other`; `extension_disabled` (native-messaging channel absence
plus a periodic liveness handshake); `bundle_signature_invalid` / `bundle_version_regression` (§13.3);
`spool_integrity`. An absence of events is ambiguous; each of these is a positive signal, reported within one
reporting interval of detection.

---

## 16. What is not captured, and why

Every entry is a structural limitation of the mechanism, a scope decision from brief §1.2, or a deferred
upgrade — each with its consequence for the product's numbers.

| # | Not captured | Why | Consequence, and how it is represented |
|---|---|---|---|
| 1 | **WebSocket messages after the handshake (E4)** | Not exposed by MV3's `webRequest`; the handshake is the last observable frame, and keeping a socket open to parse frames is not available through the extension APIs | Genuinely uncapturable. The handshake still yields tool identity and a session count, so these features appear as `identity_volume`, not as absence. Mode B's Slack-AI case, and part of mode C |
| 2 | **Response bodies, everywhere (E4, §1.2)** | An explicit v1 non-goal: the proxy streams responses without buffering and the extension has no API for them. `direction: ingress` exists in the contract and is deliberately unused | Nothing the product promises depends on them. The seam exists for a per-tenant opt-in later; building it now would multiply classification volume for a risk model the brief does not have |
| 3 | **Mode I prompts** | No socket, no request and no reachable buffer: an embedded model is linked into an application, which is §2's own description. Any content mechanism would require reading another process's memory — a kernel component, a debugger attach, or both | `detection_only` is the brief's own specification. The product reports that a model ran and how often, and does not claim to know what was asked |
| 4 | **Anything a kernel driver would be needed for** | D3 chooses no kernel component in v1; E20/E21/E22 make a driver commercially expensive and it carries the largest possible blast radius | Specifically: no interception of clients ignoring system proxy settings (E8); no per-flow observation independent of the proxy; no syscall- or memory-level model detection; no HTTP/3 collection, which is blocked rather than observed. Named gaps, not rounding errors |
| 5 | **Screen-capture territory of any kind** | E16: Screen Recording can never be pre-granted by MDM and is deny-only; any dependency breaks zero-touch deployment permanently | Permanently out of scope — no screen capture, no OCR, no pixel-level inference of prompts. Nothing in the design degrades if this never exists |
| 6 | **Keystroke content** | Brief §1.2: a hard requirement, not a preference | Never attempted. Listed explicitly because keystroke capture would trivially solve several fidelity problems in §2, and the answer is no |
| 7 | **Files at rest** | Brief §1.2: no filesystem indexing or file-at-rest scanning. The system sees a file because a user attached it to an AI tool | A sensitive document never attached to a model is invisible, by design. Coverage never claims otherwise |
| 8 | **Attachment contents where no `File` handle is reachable (E3)** | The network layer reports a filename, never bytes, and a composer building the upload in a worker or canvas offers no readable handle | Recorded as `content_no_attachments` and counted in mode A's coverage row (§7.3) — per-composer, measured, visible |
| 9 | **Content at M0** | The M0→M1 boundary is a permission boundary: M0 "requires no access to content at all" (brief §1.1) | By design. M0 answers "who uses which tools, how much, sanctioned or not" and nothing else. The schema *rejects* an M0 record carrying a digest or labels, so a defect that read content at M0 is caught at ingest rather than stored |
| 10 | **Any device without the collector installed** | Nothing on an endpoint can observe a device it is not on | The unobserved remainder of the 70–85% (E24, §15.3), reported against an external inventory denominator or as unmeasurable — never as 100% |
| 11 | **Non-Chromium browsers** | E23: deployment is one policy per browser, and Firefox and Safari are effectively unreachable | A, B and C coverage on those browsers is zero, and it is stated as zero. Tasks performed in them are not silently missing; they are missing from a coverage row that says so |
| 12 | **Personal or unmanaged devices, and off-network personal use** | Out of scope: the brief's collection model is managed devices | The remote/hybrid population on unmanaged hardware is the E24 remainder. A user captured on a managed laptop and not on a phone produces an undercount the product cannot measure — it can only report that the device population is incomplete |
| 13 | **Which account, conversation or vendor-side identity at the AI tool** | The device sees a destination and a payload, not the user's session at the vendor | `user_ref` is the device's signed-in user. **ASSUMPTION (A21):** acceptable, because the entity the customer needs is their own employee, not the vendor account |
| 14 | **Whether a prompt was user- or machine-authored (mode H)** | §2.2: not reliably observable on the device | Every egress submission is reported with equal weight. A customer wanting agent traffic separated gets it by tool fingerprint, which is observable, rather than by authorship, which is not |
| 15 | **Content existing only inside a browser-agent's rendering surface (R5)** | Canvas and closed shadow roots yield nothing to `ext.dom` | Mode C is best-effort by the brief's own specification and its content-fidelity ratio is measured (§7.5). The request-body route is unaffected, which is why C's mandatory identity/volume half is still met |
| 16 | **Anything outside the enumerated interception scope (§5.1)** | Brief §1.2 scopes interception to enumerated destinations; everything else is blind-tunnelled | An unknown AI tool reached by a desktop or CLI client on a destination never promoted is attributed by process and volume only. §5.1's bounded promotion shrinks this gap; the residual is visible as `blind_tunnelled` |

---

## 17. Component-to-requirement trace

| Requirement | Where |
|---|---|
| §2 modes A–I; discovery behavioural; sanctioned vs unknown | §2, §8.2, §8.3 |
| §1.1 scoped mode before signing; no upload-everything state; attribution | §11.1, §11.3 |
| §1.2 non-goals | §5.6, §16 |
| §4.2 signed bundle, `304`, signature failure retains previous, idempotent re-enrolment | §13.1–§13.3 |
| §5.1 MV3 specifics | §7 |
| §5.2 QUIC, trust store, proxy boundary, pinned clients | §14, §5.5, §15.3 |
| §5.3 local inference and port release | §6 |
| §5.4 entitlements and Screen Recording | §14.2, §16 row 5 |
| §5.5 kill switch and update safety | §5.5, §3.5 |
| §6 rules first, model for fuzzy classes, versioning, promotion, latency, degradation | §9.2, §9.4–§9.7 |
| §7 bounded buffer, drop oldest, reported counter, per-collector health, tamper detection | §12.2, §4.2–§4.3, §15.4 |
| R1 local-inference validation | §6.4 |
| R5 browser-agent fidelity | §2, §7.5, §15.2 |
| R7 process telemetry filtered at source | §4.3, §4.4 |
| R8 parser isolation | §10 |
| R9 double counting across routes | §8.3, §11.2 |
| R11 coverage is an output of the path | §15.1–§15.2 |

---

## 18. Assumptions

| # | Assumption | Justification |
|---|---|---|
| A1 | Platform key-protection facilities (DPAPI, Keychain) wrap the spool key, CA key and device credential | The brief requires encryption at rest (C22) and per-object keys (C15) but names no mechanism, and these are the only facilities that work unattended on a managed endpoint |
| A2 | Native messaging is the extension→core transport, with chunking for attachment bytes | MV3 offers no other supported channel from an extension to a privileged process, and the size ceiling is a browser fact the design must respect |
| A3 | A per-device root CA mints short-lived leaf certificates | The brief requires interception of enumerated destinations without saying where the key lives; per-device generation minimises the blast radius of key theft and removes a vendor-held interception capability |
| A4 | Agent-driven submissions (mode C) are attributed to the device's signed-in `user_ref` | The device cannot observe who authored text typed into an agent's runtime |
| A5 | Bounded, device-local *dynamic promotion* of a destination into the interception set | Reconciles §1.2's enumerated-scope rule with §2's behavioural-discovery requirement; without it, novel tools used by desktop and CLI clients are invisible |
| A6 | Certificate Transparency is not a constraint on a locally trusted CA, so clients accept minted leaves | A reviewer will ask; if a client ever requires SCTs, this provider's reach shrinks and the coverage row shows it |
| A7 | Connection-refused is the least-bad signal during a broker release window | Keeps `RELEASED` unambiguous, which §6.2's invariants depend on; a listening-but-erroring socket would look healthy to a watchdog |
| A8 | The upstream preflight path is per-tool configuration in the bundle | Invoking a generation endpoint to test health would consume the user's resources and could itself be observed as usage |
| A9 | A TLS ClientHello fingerprint is usable as one of six fingerprint signals | One input among six, so its deprecation degrades precision rather than breaking the fingerprint |
| A10 | `tf1:` plus a base32 SHA-256 fits the envelope's 128-character `tool_fingerprint` field | A schema-bounded field with an unbounded encoding is a defect waiting to be written |
| A11 | The parser child's confinement uses a job object (Windows) / sandbox profile (macOS) | The requirement is "no filesystem path to key material", which these satisfy; the specific primitive is an implementation choice |
| A12 | Parser memory cap, timeout and output cap are finite parameters (tens of MB, low hundreds of ms, low MB) | Only the *shape* is a design requirement (R8); the values belong to the resource budget (§8) and R8's test plan |
| A13 | Large attachments are parsed off the interactive path, labels correcting the event afterwards | A large PDF cannot be parsed inside 150 ms; blocking on it would make the product perceptible, and always-degrading would lose exactly the content the product exists to classify |
| A14 | A class-prior map resolves the data-class scoping axis before content is read | The brief makes data class both a scope dimension and a classifier output without ordering them; priors plus a ceiling is the only ordering satisfying C3 and "before content is read" together |
| A15 | Providers report the closed seven-counter set of §4.3 in `detail.counters`, rather than a free-form counter space | Anything richer is a high-cardinality stream the brief's §3.1 warning and R7 both forbid; a closed set is what makes the §15.2 coverage rows comparable across devices and versions |
| A16 | Default spool bound ~25 MB / ~25,000 rows per device, tunable per tenant | Two to three orders of magnitude above expected daily volume, so ordinary outages never approach it, and small enough not to be a customer decision |
| A17 | The MDM device identifier is the idempotency key for re-enrolment | Nothing on the endpoint reliably survives a re-image, and the brief's device entity already carries an MDM id |
| A18 | Credential lifetime and rotation cadence are deployment parameters, decoupled from code-signing validity | The brief requires revocability (§4.2, C11) and is silent on lifetime; coupling the two expiries would create one outage date instead of two independent ones |
| A19 | Shell-profile and environment delivery locations are per-platform deployment details | E10 fixes the *lever* (a managed shell environment), not the file paths |
| A20 | The managed-device coverage denominator comes from the customer's management plane or directory | Nothing on an endpoint can count devices the collector is not installed on; without a denominator the honest answer is "unmeasurable" |
| A21 | Attribution to the user, rather than their AI-vendor account, is sufficient | The customer's subject is their own employee; the vendor account is not observable from the device |

---

## 19. Open items

Distinct from [master §7]'s list: what must close for *this* document to be implemented as written.

| # | Item | Owner | Closes by | If unresolved |
|---|---|---|---|---|
| O1 | R1 lab validation against §6.4's acceptance criteria | Endpoint lead | The five checks against the customer-relevant tool set, both platforms | Mode F ships as `detection_only`; no architectural change ([master §4.6]) |
| O2 | Q4 — whether the WASM classifier meets 150 ms p95 in the extension | Classification lead | Benchmarking rules-only, rules+model and model-on-worker against recorded traffic | The model moves to the native host, or labels arrive on a follow-up record; either changes §9.4's ladder, not its structure |
| O3 | The class-prior map's initial content per tool family (A14) | Classification lead with product | Enumerating the classes each supported tool family plausibly carries, plus the tenant default | Resolution falls back to the tenant default — conservative, but coarser than the brief's per-class intent |
| O4 | Keeping the device's provider names identical to `ref.collector` as providers are added (§4.3) | Collection lead with data platform lead | A conformance check that every `Name()` a provider can report exists in `ref.collector`, run in CI | A renamed or added provider reports health the coverage report cannot attribute, which is the R11 failure arriving through a naming change |
| O5 | The dynamic-promotion window length and re-probe cadence (§5.1) | Endpoint lead | Measuring the false-promotion rate during the bake period | Either novel tools stay invisible (too narrow) or more plaintext than necessary is decrypted (too wide) |
| O6 | Whether the macOS UDP/443 egress block is equally expressible by configuration profile | Platform lead | Checking on a supervised test fleet — the same fleet R3 needs | macOS HTTP/3 coverage remains a named gap |
| O7 | The non-reconcilable-digest rate, and whether fingerprint canonicalisation (§8.3) reduces it | Collection lead | Instrumenting the rate from the pilot ([master §7] Q3 owns the canonicalisation spec) | R9's counts stay inflated on canvas and WebSocket surfaces, and the reconciliation report carries the difference |
| O8 | Notice-acknowledgement enforcement point: device-side (§11.3) or enrolment-side only | Product with legal | Confirming the deployment workstream supplies the notice version and the acknowledgement record | Content-reading modes could be enabled for a user who never saw the notice |
