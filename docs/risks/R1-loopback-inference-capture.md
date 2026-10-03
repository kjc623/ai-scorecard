# R1 — Loopback inference capture boundary: a measurement

Status: measured on one host, 2026-10-02. Owner: prober. Gated item: build step 8, the loopback
inference broker (mode F), which `docs/00-architecture.md` §6 says "ships only after lab
validation" and which E13 flags as an unvalidated hypothesis.

This is a measurement, not a design essay. It reports what was observed, what the observation
does **not** cover, and one defect it found. Where a claim rests on a stand-in, it says so in
the same sentence as the claim. The defect (§6) was fixed in the component after the measurement;
the transcript in §4 is the run that found it and has not been re-run against the fix.

---

## 1. What the subject is, and what it is not

The **broker is real**: the harness drives `endpoint/capture-core/proxy/loopback` exactly as
`capture-core` will — `loopback.New`, `Start`, `Health`, `Coverage`, `ApplyPolicy`, `Stop` —
with real TCP sockets on `127.0.0.1`. Nothing about the broker's behaviour below is simulated
or re-implemented.

The **upstream and the client are stand-ins**, and this is the boundary the whole document
turns on:

| Component | What it is here | What it is not |
|---|---|---|
| Broker (`proxy.loopback`) | The real package under test | — |
| "Local inference server" | A stand-in HTTP server the harness can stop and restart on another port | **Not** a vendor tool. No vendor tool was installed, configured, relocated or observed on this host. |
| "Client" | A raw TCP client the harness points at the broker's port | **Not** a vendor client. Nothing here shows that any real client resolves a substitute port. |
| "Relocation" | The harness moving its own server | **Not** relocation through a tool's supported configuration. |

Everything the harness does uses **ephemeral ports** chosen by the kernel. No vendor default
port (11434, 1234, 8080) is bound at any point, so these measurements say nothing about those
ports either way — deliberately, because the port number is not the property under test.

## 2. How to re-run

```
$env:GOCACHE="$PWD\.tools\gocache"; $env:GOPROXY="off"; $env:GOTOOLCHAIN="local"; $env:GOFLAGS="-mod=mod"
$env:GOTMPDIR="$PWD\.tools\tmp\go"; $env:TMP="$PWD\.tools\tmp"; $env:TEMP="$PWD\.tools\tmp"
cd docs/risks/R1-harness
go build ./...
go run .            # every claim;  go run . a|b|c|d|e for one
```

The run takes about 25 seconds (claim e measures a 7-second window, claim c waits out a
cool-down). Raw transcript of the run embedded below: `R1-harness/run-output.txt`. Three
consecutive runs produced identical verdicts; only the port numbers and the cool-down-bounded
recovery latency (1.013 s / 1.014 s / 1.015 s) differ — `R1-harness/stability.txt`.

## 3. What the harness does that a unit test cannot

Three of these claims need conditions a single-process test does not create, so the harness
uses the pattern from `endpoint/capture-spool`: it re-executes its own binary as a child.

- **A real process kill.** Claim (b) runs the broker in a child process and kills it with the
  platform's uncatchable kill — `TerminateProcess` on Windows, `SIGKILL` on POSIX. No deferred
  function runs and no socket is closed politely. The OS closing the socket *is* the property
  under §6.2 rule 4, and a graceful shutdown would have tested something else. The transcript
  shows the kill settling as exit status 1, which is what a killed process looks like here.
- **A second real process holding the port.** Claim (c) starts a separate process that binds
  the port and answers with a banner, like the user's own server started on its default port
  because relocation failed. The broker must refuse to bind and must not fight for it.
- **A repeated-failure sequence.** Claim (e) uses a black-hole upstream that accepts
  connections and never answers. Every accepted connection is one preflight attempt, so the
  attempt timeline — and the *absence* of attempts during a cool-down — is measured from
  outside the broker rather than asserted from its own state.

Claim (a) records the **raw bytes** the upstream receives (a tee under `http.ReadRequest`), so
"forwarded byte-identically" is checked against bytes rather than against a re-parsed request.

## 4. Raw output

```
R1 loopback inference capture boundary - harness run 2026-10-02T14:36:16-04:00
go go1.27.0, host loopback only, ephemeral ports only (no vendor default port is bound)

### (a) the client's request is captured by the broker and forwarded
  upstream (stand-in local inference server) : 127.0.0.1:65008
  broker claimed port                       : 127.0.0.1:65009
  broker reached HOLDING after              : 0s
  client sent 284 bytes, response 250 bytes in 1ms: HTTP/1.1 200 OK
  the upstream saw 1 preflight request(s) and 1 forwarded request(s)
  request line as sent     : POST /v1/chat/completions HTTP/1.1
  request line as received : POST /v1/chat/completions HTTP/1.1
  headers as sent          : Content-Length: 149 | Content-Type: application/json | Host: 127.0.0.1:65009 | X-R1-Probe: harness
  headers as received      : Content-Length: 149 | Content-Type: application/json | Host: 127.0.0.1:65008 | User-Agent: Go-http-client/1.1 | X-R1-Probe: harness
  body sha256 as sent      : 72b5fcf015f2406143b6038795f91ed91f425ffb0cde4e7b0209dce5f43ea3f5 (149 bytes)
  body sha256 as received  : 72b5fcf015f2406143b6038795f91ed91f425ffb0cde4e7b0209dce5f43ea3f5 (149 bytes)
  pipeline observed        : route=proxy.loopback kind=prompt size_bytes=149 content_retained=149 bytes
  C1 extraction at the route: "R1 harness probe: summarise the attached contract."
  VERDICT (a): PASS - body byte-identical to the upstream (sha256 72b5fcf015f24061), method+path preserved, pipeline captured 1 observation(s) with the same bytes; headers re-serialized (Host rewritten to the upstream: true)

### (b) a real process kill releases the port; the client is refused, not hung
  broker running in child pid 47552, claiming 127.0.0.1:65015, upstream 127.0.0.1:65014
  before the kill: the client's request reached the upstream (upstream has 2 request(s))
  (kill returned exit status 1; on Windows a killed process settles as exit code 1)
  killed with the platform's uncatchable kill (TerminateProcess on Windows, SIGKILL on POSIX); no deferred code ran
  after the kill, first client error after 2ms: dial tcp 127.0.0.1:65015: connectex: No connection could be made because the target machine actively refused it. [typed errno WSAECONNREFUSED(10061)]
  the OS released the socket: another process can bind 127.0.0.1:65015: true
  VERDICT (b): PASS - connection refused (not a timeout) 2ms after the kill, error "dial tcp 127.0.0.1:65015: connectex: No connection could be made because the target machine actively refused it."; port immediately rebindable by another process: true

### (c) a port held by another real process: tampered, never fought for
  holder is a separate process, pid 31112, holding 127.0.0.1:65022
  holder's banner: "R1-HARNESS-HOLDER: this port belongs to another process"
  broker health after 0s: state=tampered detail=port_held_by_other counters=all zero
  coverage row: configured=1 held=0 upstream_reachable=0
  the holder still owns the port after the broker's attempt: true
  the broker never bound it: a client on 127.0.0.1:65022 still reaches the holder, not the broker
  VERDICT (c): PASS - reported tampered/port_held_by_other with coverage held=0 while a live upstream was reachable, and the holder process kept the port
  -- after the holder dies --
  (holder kill: exit status 1)
  the broker re-bound the freed port: true (after 1.014s)
  health after recovery: state=tampered detail=port_held_by_other counters=emitted=1 observed=1
  coverage after recovery: configured=1 held=1 upstream_reachable=1
  FINDING: stale tampered row after the port conflict ended - the broker re-bound 127.0.0.1:65022 and is serving it (coverage held=1 reachable=1, counters emitted=1 observed=1) but Health() still reports tampered/port_held_by_other
  VERDICT (c2): PASS - after the holder process died the broker re-bound the freed port (in 1.014s) and served a request through it

### (d) after the upstream moves, the broker stays released and says upstream_unreachable
  holding 127.0.0.1:65052, forwarding to 127.0.0.1:65051
  the upstream moved: 127.0.0.1:65051 stopped, a new server started on 127.0.0.1:65055 (nothing told the broker)
  the broker released the port after 159ms; a client now gets "dial tcp 127.0.0.1:65052: connectex: No connection could be made because the target machine actively refused it." in 0s
  health  : state=degraded detail=upstream_unreachable counters=all zero
  coverage: configured=1 held=0 upstream_reachable=0
  health 300ms later (still released, not flapping): state=degraded detail=upstream_unreachable counters=all zero
  VERDICT (d): PASS - stayed released (connection refused), reported degraded/upstream_unreachable, coverage held=0 reachable=0
  after the bundle names the new upstream port (65055): HOLDING again after 11ms
  a request now flows through the broker to the relocated server
  health after the policy change: state=healthy detail=(none) counters=emitted=1 observed=1
  VERDICT (d2): PASS - the broker followed the relocation through policy (HOLDING, healthy/(none))

### (e) a repeated-failure sequence reaches cool-down, not a bind/release loop
  upstream is a black hole (accepts, never answers): every preflight attempt is one accepted connection
  policy: preflight timeout 300ms, backoff base 100ms/max 400ms, max consecutive failures 3, cool-down 3s
  preflight attempts accepted by the black hole over 7s: 4
    attempt 1 at +0s
    attempt 2 at +400ms (gap 400ms)
    attempt 3 at +900ms (gap 500ms)
    attempt 4 at +4.2s (gap 3.3s)
  health timeline (state/detail changes only):
    +300ms    degraded/upstream_unreachable
    +1.22s    degraded/cooling_down
  the claimed port was never held during the sequence: connections accepted on 127.0.0.1:65083 = 0
  the claimed port is free after the sequence: true
  VERDICT (e): PASS - 4 attempts, largest gap 3.3s (>= cool-down 3s), reported cooling_down: true, port never held: true

================ summary ================
  [PASS] a    body byte-identical to the upstream (sha256 72b5fcf015f24061), method+path preserved, pipeline captured 1 observation(s) with the same bytes; headers re-serialized (Host rewritten to the upstream: true)
  [PASS] b    connection refused (not a timeout) 2ms after the kill, error "dial tcp 127.0.0.1:65015: connectex: No connection could be made because the target machine actively refused it."; port immediately rebindable by another process: true
  [PASS] c    reported tampered/port_held_by_other with coverage held=0 while a live upstream was reachable, and the holder process kept the port
  [PASS] c2   after the holder process died the broker re-bound the freed port (in 1.014s) and served a request through it
  [PASS] d    stayed released (connection refused), reported degraded/upstream_unreachable, coverage held=0 reachable=0
  [PASS] d2   the broker followed the relocation through policy (HOLDING, healthy/(none))
  [PASS] e    4 attempts, largest gap 3.3s (>= cool-down 3s), reported cooling_down: true, port never held: true
  [FINDING] stale tampered row after the port conflict ended - the broker re-bound 127.0.0.1:65022 and is serving it (coverage held=1 reachable=1, counters emitted=1 observed=1) but Health() still reports tampered/port_held_by_other
=========================================
```

The harness exits 0 because its claims pass. The `[FINDING]` line does not change the exit
code, because it is an observation about the component rather than a failed measurement — §6
below reports it in full rather than burying it in a green run.

## 5. Verdict per claim

| # | Claim | Verdict | The measurement |
|---|---|---|---|
| a | The client's request is captured by the broker and forwarded | **PASS, with a caveat** | The body is byte-identical (same sha256 at the upstream). Method and path are preserved. The pipeline receives an observation with the same bytes and the C1 extraction returns the user-authored text. **Caveat, stated plainly: the request is not forwarded byte-identically as a whole request.** Go's HTTP writer re-serialises it: `Host` is rewritten to the upstream address and `User-Agent: Go-http-client/1.1` is added. Any claim of literal whole-message byte-identity would be false; what matters for capture and for `content_digest` — the body and the method/path — is preserved. |
| b | A killed broker releases the port and the client is refused, not hung | **PASS** | Real `TerminateProcess` of a real child; 2 ms later the client gets `WSAECONNREFUSED` (typed errno, not a message match); another process can bind the port immediately. No hang, and no "listening but broken" state. |
| c | A port held by another process → `tampered`/`port_held_by_other`, never fought for | **PASS** | With a *live* upstream reachable (so a refusal to bind can only be about the holder), the broker reports `tampered`/`port_held_by_other`, coverage `held=0`, and the holder process keeps the port and keeps answering. It does not kill, displace or even briefly bind the port. |
| c2 | After the holder dies, the broker recovers | **PASS (recovery) / see finding** | It re-binds within 1.01 s (the configured cool-down) and serves a request through the port. In this run its *health row* did not follow — §6, fixed since. |
| d | After the upstream moves, the broker stays released and reports `degraded`/`upstream_unreachable` | **PASS** | Released 159 ms after the upstream moved (two consecutive missed probes), port refused, `degraded`/`upstream_unreachable`, coverage `held=0 reachable=0`, stable 300 ms later (not flapping). |
| d2 | The broker follows a relocation through policy | **PASS** | A bundle naming the new upstream port returns it to `HOLDING` in 11 ms and requests flow again. This is §6.1's "ports are per-tool bundle entries" working as data rather than as guesswork. |
| e | A repeated-failure sequence reaches cool-down rather than a bind/release loop | **PASS** | Attempts at +0, +400 ms, +900 ms, then a 3.3 s gap; `degraded`/`cooling_down` from +1.22 s. The claimed port was never held at any point during the sequence (0 successful connections) and is free afterwards — so "never holds a port it cannot serve" (E14) held under the exact condition that would break it. |

## 6. Finding: the `tampered` row did not clear when the conflict ended (fixed since)

**What was observed.** The port conflict in claim (c) ends — the holder process is killed —
and the broker **recovers correctly**: within 1.01 s it re-binds the port, and a client request
then flows through it to the upstream (`coverage: configured=1 held=1 upstream_reachable=1`,
`counters: emitted=1 observed=1`). Its health row nevertheless still reports
`state=tampered detail=port_held_by_other`, and kept reporting it.

**Why it matters.** `protocol.CollectorState`'s own documentation says `tampered` "is the
tamper signal and the only state that raises a security finding rather than an operations
one". At the revision under test a device that recovered from an ordinary port conflict
therefore raised a permanent security finding, and its health row contradicted its coverage row
about the same port: one said the port was held and reachable, the other said it was taken by
another process. `docs/01-collectors.md` §6.4's gate 3 requires that "port state matches §6.2's
machine in every case"; at that revision this case did not.

**Mechanism, with file:line** (behaviour of the revision under test, `endpoint/capture-core` at
2026-10-02 14:35; the line numbers locate the same code in the current files):

- `proxy/loopback/machine.go:222-223` sets `m.tampered = true` on `EvPortHeldByOther` from
  `BINDING`. At the revision under test the only place it was cleared was the `EvPolicyChanged`
  branch (`machine.go:174-179`); the other transitions of `Apply` (notably `RELEASED`/`ORPHAN` +
  `EvPreflightOK` → `ActBind` at 189-191, and `BINDING` + `EvBindOK` → `HOLDING` at 208-216)
  did not clear it.
- `proxy/loopback/broker.go:267-268` collects `tampered` and `broker.go:282-283` returns
  `StateTampered` **before** the `held > 0 && held == reachable` case at line 284, so a port
  that was genuinely held and serving was still reported as tampered while the flag stayed set.
- The runner does re-check after `CoolDown` (`broker.go:604`) and re-binds if the port is free
  — which is why the port recovered while the row did not.

**Resolution, as built.** The component now clears the flag on a successful bind:
`machine.go:208-216` sets `m.tampered = false` (line 215) on `BINDING` + `EvBindOK`, so `tampered`
is present-tense and a recovered, serving port no longer raises a security finding
(`Machine.Tampered`, `machine.go:104-113`, states the rule and cites this measurement). The
history is kept separately rather than lost: each conflict increments `ConflictCount`
(`machine.go:115-118`, incremented at 224). Two tests now drive the conflict to its end and
re-read the row — `TestMachine_6_2_RecoveredConflictIsNotTampered` (`machine_test.go:158`) and
`TestBroker_6_2_RecoveredPortConflictIsNotTamperedAndCoverageAgrees` (`broker_test.go:419`). The
harness itself has not been re-run against the fixed code, so §4's transcript still shows the
finding.

**Not fixed by the harness, deliberately.** This harness lives in `docs/risks/` and may not touch a component's
files. Two plausible fixes are the component owner's call, and they are different products:
either clear `tampered` when the broker next binds the port successfully (the row follows
reality, and the historical conflict is lost), or keep the conflict visible as a *sticky
detail* on an otherwise honest row (reality plus history, at the cost of a new field). I did
not pick between them; the component owner's choice is the resolution above.

**How it was found.** Only because the harness asked a question the unit tests of the time did
not: after the conflict ends, does the health row follow the port? The suite then covered the
tampered state itself (`TestBroker_6_2_PortHeldByOtherIsTamperedAndNeverFoughtFor`) and the release
rules (`TestMachine_6_2_Rule4_ProcessDeathIsARelease`), but nothing drove the conflict to its
end and re-read the health row. The two tests named in the resolution above now do.

## 7. The five §6.4 checks this host cannot validate

§6.4 lists five checks that gate the provider shipping. This is where R1's value actually is,
so each one is stated in terms of what was and was not observed.

### Check 1 — "Each customer-relevant tool can be relocated by its own supported configuration, both platforms" (pass = survives a service restart and a machine reboot)

**NOT VALIDATED. Nothing in this document bears on it.** The harness relocated *its own*
server by stopping it and starting it on another port. That is a demonstration that the broker
copes with a relocated upstream; it is not evidence that Ollama, LM Studio, llama.cpp, vLLM or
anything else can be moved by its own documented configuration, on Windows *and* macOS, or that
such a move survives a restart of the tool or a reboot of the machine. E13 is exactly this
hypothesis and it remains open.

*Evidence that would settle it:* a lab matrix, one row per tool per platform, running the real
tool: apply its documented relocation setting, verify the server listens on the substitute
port, restart the tool, reboot the machine, verify again, and record the tool's version. Plus
the negative control — verify that a client which was not reconfigured fails in a *bounded,
visible* way rather than hanging.

### Check 2 — "Each customer-relevant client resolves the relocated port" (explicitly configured, and when discovering the server)

**NOT VALIDATED.** The "client" here is a raw TCP client the harness points at a port it
already knows. It demonstrates nothing about whether a real client — the tool's own CLI, an
IDE plugin, a vendor app, or anything that *discovers* a server rather than being told where it
is — resolves a substitute. The discovery case cannot be configured at all, which is why it is
the half of §6.1's *client-side redirect* option that carries the risk.

*Evidence that would settle it:* for each client, point it at a relocated server and confirm
requests arrive with no manual intervention, and record what it does when the server is not
there. The harness's corpus mechanism (`nfc_oracle.mjs`'s pattern) does not transfer here: this
needs the actual client binaries.

### Check 3 — "The broker holds the default port across a reboot, a crash of either side, and an upgrade" (pass = port state matches §6.2's machine in every case)

**PARTIALLY VALIDATED, and one case currently fails.**

- *Crash of the broker* — **validated here** (claim b): the OS releases the socket on a real
  kill, the client is refused in 2 ms, the port is immediately rebindable.
- *Crash of the upstream* — **validated here** (claim d): release, `upstream_unreachable`,
  stay released, and re-bind after preflight when the upstream returns (through policy for a
  *relocation*, and via the watchdog's recovery path for a restart).
- *Port conflict ending* — **fails today** (§6): the port recovers, the health row does not.
- *Reboot* — **NOT validated.** No reboot was performed. §3.5's startup ordering (the broker
  binds last) is asserted by tests, but whether the default port is held *after a boot* depends
  on service start order, the tool's own start order and the policy load path — none of which
  this host exercised.
- *Upgrade* — **NOT validated.** There is no installer or service on this host to upgrade, so
  "the port survives an upgrade of capture-core" was not tested at all.

*Evidence that would settle it:* a VM with the service installed, cycled through reboot,
kill of each side, and a real upgrade, with the port state and the health row sampled
throughout; the health row must match the port's actual state at every sample.

### Check 4 — "Uninstall restores the original configuration exactly" (pass = byte-identical configuration, port free)

**NOT VALIDATED, and not this component's half.** The broker holds no vendor configuration
file and writes none: `policy.LoopbackPort.OriginalPort` (`endpoint/capture-core/policy/bundle.go:74`)
records the pre-relocation value so uninstall *can* restore it, but nothing in the harness
exercises restoration, and there is nothing on this host to restore.

*Evidence that would settle it:* install on a machine with a real tool, relocate it, uninstall,
and diff the tool's configuration byte-for-byte against the pre-install copy, with the port
free afterwards. The port-free half is already measurable (claim b shows the OS frees the
socket); the byte-identical half is not.

### Check 5 — "A client that ignores relocation produces a clear error rather than a hang" (pass = error within a bounded time)

**PARTIALLY VALIDATED — for the broker's side of the contract, not the client's.** During a
release window the broker is not listening at all, and a client is refused in ~0–2 ms
(claims b and d). That is A7's chosen behaviour, measured.

Whether a *specific* client surfaces that as a clear error is a property of that client, and it
is **NOT validated** here: a client that retries silently, or that presents a spinner, would
still be a hang from the user's point of view.

*Evidence that would settle it:* run each real client against a deliberately released port and
record its user-visible behaviour and the time to error.

## 8. Verdict

**What is now validated, with evidence.** The broker's half of §6.2/§6.3/§6.4 behaves as
documented, measured with real sockets and real processes: it binds only after a passing
preflight; a real process kill releases the port with the OS closing the socket and the client
refused rather than hung; it refuses to bind a port a real second process holds and never
fights for it; when the upstream moves it releases, stays released, and reports
`degraded`/`upstream_unreachable`; a repeated-failure sequence reaches `cooling_down` with a
3.3 s gap in attempts instead of a bind/release loop; and it never held a port it could not
serve at any point in any of those sequences. A relocation is followed through policy, not
guesswork.

**What remains unvalidated.** Checks 1, 2 and 4 in full, the reboot and upgrade halves of
check 3, and the per-client half of check 5. All of them need something this host does not
have: a real vendor tool, a real vendor client, an installer, and a reboot. The one thing this
measurement *did* add to check 3 was a failure: the recovery case (§6), fixed since.

**Does anything here change the conclusion for mode F?** No — and it is important to be precise
about why. §6.4's own consequence is: "If checks 1–2 fail for a tool, that tool is not in mode
F's port set and F's coverage for it becomes `detection_only` through `proc.detect`". Checks 1
and 2 are not merely unvalidated *for some tool*; they are unvalidated **for every tool**,
because no tool was tested. So:

- **No tool's port-set entry should be enabled on the strength of this document.** For any tool
  whose relocation and client behaviour have not been lab-validated, F's coverage for it stays
  `detection_only` via `proc.detect` — which is a configuration change, not an architectural
  one, exactly as §6.4 says.
- **The broker should not ship as enabled-by-default either**, for a reason independent of the
  vendor checks: check 3's "port state matches §6.2's machine in every case" is not yet met in
  full. At the time of this run the cause was the stale `tampered` row (§6), under which a device
  that recovered from a port conflict raised a permanent security finding. That defect is fixed
  (`machine.go:215`), but the fixed case has not been re-measured here and the reboot and upgrade
  halves of check 3 remain unvalidated.
- **Server-side relocation is the strategy this host can speak to**, and it behaves correctly
  end to end. Client-side redirect rests entirely on check 2, which nothing here supports: the
  measurement shows only that a client arriving at a released port is refused quickly, not that
  any client would find the substitute.
- **The R1 blocker is now narrower than "the port-occupation hypothesis".** What remains is a
  vendor lab matrix (checks 1, 2, 4, 5-per-client) and a service lifecycle test (reboot and
  upgrade). Neither is code; both need machines and real tools.

## 9. Not verified — the short list

- No vendor tool was installed, configured, relocated, restarted or rebooted.
- No vendor client was used; nothing shows any real client resolving a substitute port.
- No reboot, no service upgrade, no uninstall, no installer.
- The "upstream" and "client" are stand-ins written for this harness (§1).
- One host, one platform (Windows), one machine: nothing here speaks to macOS.
- No vendor default port was bound, so the measurements say nothing about 11434/1234/8080
  specifically — by design, and it also means the port numbers in the transcript are
  meaningless outside it.
- The timings in the transcript are harness policy (300 ms preflight timeout, 100 ms backoff
  base, 3 s cool-down) chosen to make a 25-second run; they are not the production defaults and
  are not a performance measurement.
- Claim (a)'s forwarding evidence is one request of 149 bytes. Large bodies, chunked transfer
  encoding, streaming responses and client disconnects mid-body were not exercised.
