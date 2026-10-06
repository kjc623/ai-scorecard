# 03. Envelope: endpoint routes and kinds

## Problem

The new collectors produce records the event envelope can't express:
- installed apps, running apps, CLIs, IDE extensions, local models and inference connections;
- model requests and tool calls from native telemetry;
- prompts collected by hooks and OpenTelemetry.

The envelope's kind and route sets are closed, so this is a contract change. It starts in
`contracts/`.

## Goal

`contracts/event-envelope.schema.json` defines the routes, kinds, fields and per-kind rules in
`DESIGN.md` §3, and the generated Go binding follows.

## Scope

- In `contracts/event-envelope.schema.json`:
  - Add the routes `tool.hook`, `tool.otel`, `inv.scan` and `net.flow` to `$defs/route`.
  - Replace kind `model_detection` with `discovery`, and add `agent_activity`.
  - Replace the `detection_basis` enum with the values in §3.
  - Add the new fields to `$defs/envelopeCore` with the types and bounds in §3.
  - Replace the `model_detection` `allOf` branch with a `discovery` branch, and add an
    `agent_activity` branch. Make `prompt` and `usage_rollup` forbid the new fields, following
    the existing branches' style: `if`/`then`, `not`/`anyOf`/`required`, and a `$comment` that
    says why.
- Regenerate: `node contracts/tools/generate.mjs`. Update the generator
  (`contracts/tools/generate.mjs`, `schema-model.mjs`) only where it can't express a new shape
  (for example the `model_names` string array).
- Update `contracts/tools/verify.mjs` and the generated package's tests
  (`contracts/generated/go/envelope/envelope_test.go`):
  - one valid record per new kind and discovery type;
  - one invalid record per forbidden field per kind.
- Update `contracts/README.md`: the kind list, and one sentence each on what `discovery` and
  `agent_activity` carry.
- Do not change ingest-api, the database or the device. Tasks 04 and 05 do that, and
  `accept.mjs` is allowed to fail in their checks until then: run only the contract gates for this
  task (see Done when).

## Done when

- `node contracts/tools/generate.mjs --check` passes.
- `node --test contracts/tools/` passes.
- `cd contracts/generated/go && go test ./...` passes.
- The report lists every field and rule added, against `DESIGN.md` §3.
