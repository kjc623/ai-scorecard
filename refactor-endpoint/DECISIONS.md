# Decisions

Settled decisions for the endpoint refactor, newest last. A task that deviates from `DESIGN.md` or
`PLAN.md`, or settles a vendor fact, adds an entry here: the date, the task number, what was
decided and why, and for a vendor fact the product version checked.

## 2026-10-06, owner, before task 01

- **TLS inspection becomes opt-in.** A tenant setting, off by default, gates the TLS proxy, the
  CLI shim's proxy environment, the Windows desktop-app PAC and the device root's trust-store
  installation (`DESIGN.md` §10). With it on, tools covered by an enabled native collector are not
  decrypted.
- **Scope.** Phases 0–6 of the plan, including Phase 4 (network inspection, reworked against the
  existing proxy) and E37 (generic child-process supervision, as a seam for a future MCP gateway).
  E38 (signed and notarised packages, Jamf) and E41 (nightly vendor-tool matrix) are deferred.
- **Platforms.** Windows first: each collector is built and verified on Windows, compiles
  elsewhere and reports `absent` there. Tasks 53–56 port to macOS and Linux and need those
  machines.
- **Rules.** Block and warn rules are tenant data: an ordered rule list in the signed bundle,
  edited on the dashboard's Settings page and enforced by hooks, the extension and the proxy. The
  extension's broken policy hand-off is fixed as part of this.

- **Install once, control from the dashboard** (`DESIGN.md` §0):
  - devices are installed through Intune with the standard MSI and tenant file;
  - every collection capability and policy can be turned on and off from the dashboard at any
    time after enrolment, with no install-time flags;
  - this brings the loopback broker (task 57) and the kill switch (task 47) under dashboard
    control too.

## 2026-10-06, orchestrator, while writing the tasks

- **The existing envelope is extended, not replaced** (`DESIGN.md` §3). `aigov.event.v1` is the
  plan's name for the repository's envelope. `model_detection`, which nothing emits, becomes
  `discovery`; `agent_activity` is added for OTel model requests and tool calls.
  `endpoint.config_tampered` is a health state, not an event.
- **The hook binary is `capture-core --hook`**, a relay to the running service over the native
  endpoint, like the browser's native messaging host. The policy cache and the detectors live in
  the service. The state directory is not readable by users, so a standalone binary could not read
  the cached policy; the relay keeps everything local and makes no network call.
- **Redaction is not an action.** No hook can rewrite a prompt, and rewriting proxied bodies is
  outside the plan's tasks. Rules allow, warn or block.
- **OTLP ports are 47318 (HTTP) and 47317 (gRPC) on 127.0.0.1**, not 4318/4317, so a developer's own
  collector on the standard ports is not displaced.
- **No policy version gating.** Nothing is deployed, so new bundle fields land in the server and
  the device together, and the lab agent is reinstalled.
- **Budgets** are as in `DESIGN.md` §13. The owner can change them before task 51.
- **Tool order.** Claude Code, then Codex, then Copilot, then Cursor.
- **Network inspection reads requests only** (task 45). The product records prompts; nothing
  stores model responses, so parsing streamed responses (plan E34) is left out.
