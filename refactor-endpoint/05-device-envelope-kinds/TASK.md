# 05. Device: mint the new kinds

## Problem

capture-core can only mint prompt envelopes:
- `core.BuildEnvelope` (`device/capture-core/core/envelope.go`, around line 109) refuses any other
  kind.
- `core.Pipeline.Process` (`core/pipeline.go`) is built around prompt content.
- The device protocol constants (`device/protocol/envelope.go`) don't yet name the new routes and
  kinds.

The discovery collectors (tasks 15–21) and the OTLP normalizers (tasks 26 onward) need one call to
emit a record that carries no prompt content.

## Goal

A provider can emit a `discovery` or `agent_activity` record through the pipeline. The record is
spooled, drained to `/v1/events`, and accepted by the contract.

## Scope

- **`device/protocol/envelope.go`**:
  - Add `RouteToolHook`, `RouteToolOTel`, `RouteInvScan` and `RouteNetFlow`.
  - Replace `KindModelDetection` with `KindDiscovery`, and add `KindAgentActivity`.
  - Add typed constants for `discovery_type`, `detection_basis`, `activity_type` and `outcome`
    (`DESIGN.md` §3).
  - Keep `Route.Valid` and the spool's route handling (`protocol/spool.go`) in step.
  - Update `tools/check-vocab.mjs` only if the extension's `messages.js` mirrors routes. The
    extension never emits these routes, so if `check-vocab` requires every Go route to appear
    there, extend the check's allow-list of device-only routes instead of adding them to the
    extension.
- **`device/capture-core/core`**:
  - Add `Pipeline.Record(ctx, Fact) error`. `Fact` holds the metadata fields of a `discovery` or
    `agent_activity` record:
    - kind, route, tool fingerprint, person, occurred-at, monotonic offset;
    - the kind's own fields from §3;
    - the dedup key, computed by the caller.

    It resolves the mode (for `collection_mode`) and applies the identity gate. It then builds the
    envelope through `BuildEnvelope` and appends it to the sink like `finish` does. It never reads
    content.
  - Extend `BuildEnvelope` to build the two kinds, with the field rules of §3. Add unit tests that
    each kind refuses its forbidden fields before the contract sees them.
  - **Spool record log.** Every envelope appended to the spool, from `finish` and from `Record`,
    writes one `info` log line, `envelope spooled`.
    - It carries these fields:
      - `event_id`, `kind`, `source`, `tool_fingerprint`, `collection_mode`, `user_ref`;
      - `policy_decision.action` and `rule_id` when present;
      - `discovery_type`, `activity_type`, `app_version`, `publisher`, `host_app`,
        `destination_host`, `model_names` and `model` when present.
    - It never carries `content_digest`, `labels`, `content_excerpt`, attachment names, or any
      text.
    - This is how a device-side check shows what a device emitted, since the dashboard and
      database aren't available to the agents checking it.
    - A unit test asserts the field set, and that a prompt envelope's log line contains none of
      its content-derived values.
- **Tests**:
  - `device/integration/contract_test.go`: every new kind and every `discovery_type` minted
    through `Pipeline.Record` validates against `envelope.Schema`.
  - `device/capture-core/cmd/capture-core` (`cloud_test.go` `fakeCloud`): a `discovery` record
    recorded on an enrolled service reaches `fakeCloud`'s `/v1/events`.
- Nothing calls `Pipeline.Record` in production yet. Task 15 is its first production caller and
  lands right after this; say so in the report.

## Done when

- `cd device/integration && go test ./...` passes, including the new contract cases.
- The `fakeCloud` test shows the record arriving (plan E02: "a test event reaches the mock cloud").
- `node tools/accept.mjs` passes.
