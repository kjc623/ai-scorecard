# 57. Local model capture in policy

Needs: on the reference host, Ollama for Windows installed with two models pulled (the same setup
as task 20).

## Problem

The loopback broker (`device/capture-core/proxy/loopback`, route `proxy.loopback`) captures
prompts sent to local model servers. It takes the server's well-known port and forwards to the
server, which it expects to find relocated to another port. Today it can't be turned on:

- **No policy reaches it.** Its configuration is the bundle's `loopback.ports` list, and
  control-api never emits a `loopback` section, so on every device it holds nothing.
- **Nothing relocates the model server.** `LoopbackPort.OriginalPort` is recorded, but no code
  moves the server to `upstream_port`, so the broker could only fight the server for its port.
- **Its timing settings are ignored.** The bundle's timing fields (`probe_interval_seconds` and the
  others) are not passed to the broker (`cmd/capture-core/service.go`, around lines 345–351).

`DESIGN.md` §0 requires every collection capability to be switchable from the dashboard.

## Goal

An admin turns "Local model capture" on for Ollama on the Settings page. On the next policy poll
the device does three things:
1. moves Ollama to its upstream port;
2. holds Ollama's well-known port;
3. captures prompts sent to it as `proxy.loopback` events at the tool's mode.

Turning the setting off restores Ollama's original port and releases the broker's.

## Scope

- **Supported runtimes:** Ollama only, in this task. It is relocated through the `OLLAMA_HOST`
  environment variable.
  - Verify how Ollama for Windows reads `OLLAMA_HOST` (machine or user environment, and whether
    the tray app must restart), and record the version you checked in `DECISIONS.md`.
  - Check whether LM Studio's server port can be relocated by the agent, from a machine-wide
    setting rather than per-user app preferences. Record the finding in `DECISIONS.md`. Add LM
    Studio only if it can, otherwise leave it out of the setting.
- **Database and control-api**:
  - Add `ops.endpoint_tool_setting.loopback boolean`, defaulting to false.
  - Accept `ollama` as a `tool_key`.
  - `compose()` emits `loopback.ports` for each enabled runtime, with:
    - `tool_fingerprint` `app:ollama`;
    - `port` from the catalog's `listen_port` signal (11434);
    - `upstream_port` a fixed server constant (`21434`);
    - `preflight_path` `/api/version`;
    - `mode` the resolved tool mode.

    The timing fields are server constants:

    | Field | Value |
    |---|---|
    | probe interval | 5 s |
    | preflight interval | 60 s |
    | preflight timeout | 3,000 ms |
    | max consecutive failures | 3 |
    | cool-down | 300 s |

  - Settings API: `PUT /admin/v1/settings/endpoint/tools/ollama` accepts `{loopback}`.
- **Dashboard**: an "Ollama" row in the endpoint tools table with a "Local model capture" switch.
  Give it a one-line explanation that the device moves Ollama to another port while capture is on.
- **Device**:
  - Pass the timing fields to the broker.
  - Make the broker a `core.Toggled` provider, enabled when `loopback.ports` is non-empty.
  - Add relocation to `capture-core/toolconfig` (create the package if task 27 hasn't yet; follow
    `DESIGN.md` §8's backup rule):
    1. Record `OLLAMA_HOST`'s original value in `toolconfig/ollama/original`.
    2. Set the machine-wide `OLLAMA_HOST` to `127.0.0.1:<upstream_port>`.
    3. Start the broker on 11434 only once the preflight to the upstream succeeds.

    The broker's existing state machine already refuses to fight a port another process holds.
  - On disable: release the port first, then restore `OLLAMA_HOST`, as the existing shutdown
    order does.
- Tests:
  - compose emits the section only when the runtime is on;
  - the device relocates and restores `OLLAMA_HOST` in a fake environment seam;
  - the broker start waits for the upstream preflight.

## Done when

- On the reference host, with the lab tenant's Ollama "Local model capture" switched on in the
  dashboard and no service restart:
  1. After the next policy poll, `curl http://127.0.0.1:11434/api/generate` with a prompt
     produces a `proxy.loopback` event for `app:ollama` in the lab tenant, visible on the
     dashboard.
  2. Switching it off restores `OLLAMA_HOST` to its original value, and Ollama answers on 11434
     directly.
- `node tools/accept.mjs` passes.
