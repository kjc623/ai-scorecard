# 13. Extension policy hand-off

Needs: Chrome or Edge on the reference host with the lab extension force-installed (as for the
browser gate).

## Problem

The browser extension never receives usable policy (traced, not yet run):

1. capture-core's `handlePolicySync` (`device/capture-core/cmd/capture-core/native.go`, around
   lines 214–229) sends `store.InForceRaw()`. That is the signed envelope
   `{key_id, algorithm, payload, signature}`, not the decoded bundle.
2. The service worker (`device/extension/background/service-worker.js`, around lines 115–118)
   applies that object as the bundle.
3. `device/extension/src/mode-policy.js` reads `scope`, `default_mode`, `mode_caps`,
   `sanctioned_hosts`, `denied_hosts`, `seed_hosts`, `body_lane_patterns`, `rules`,
   `classifier_release.state` and `confirmation_window_ms`. None of these exist in the bundle the
   server signs.

So every browser observation resolves to `m0`, there are no rules, and the extension's release
state is `rolled_back`: it never warns or blocks.

## Goal

The extension resolves its mode from the same bundle fields the device uses, and enforces the
same enforcement rules (task 11) with the same matching (task 12).

## Scope

- **First, confirm the defect**: on the reference host, record the extension's resolved mode and
  rule count before the fix, from its health row or a service-worker console log.
- **Protocol** (`device/protocol/native.go`): `PolicyBundleMessage.Bundle` carries the verified,
  decoded payload, the exact JSON object the signature covered. Never the envelope. The extension
  trusts capture-core, which verified it.
- **capture-core**: `handlePolicySync` sends the decoded payload of the bundle in force. Update
  `native_test.go` and the golden frames (`device/extension/test/golden-frames.test.mjs` and
  `device/extension/tools/emit-frames.mjs`).
- **Extension `mode-policy.js`**:
  - Read the real bundle shape.
  - Mode: `tenant_default_mode` and `tool_modes[tool_fingerprint]`, the most restrictive applying,
    as `core/mode.go` `Resolve` does for those two inputs. The extension never sees population or
    device scopes, so it asks capture-core with the existing `mode_query` when those are present.
  - Rules: `rules` and `sanctioned_tools`.
  - Seed hosts: `interception.seed_hosts`.
  - Remove the fields that no bundle carries:
    - `scope`, `default_mode`, `mode_caps`, `sanctioned_hosts`, `denied_hosts`,
      `body_lane_patterns`;
    - the `classifier_release.state` release gating (`shadow`/`rolled_back`). There is no release
      state in the bundle, so rules are always enforcing.
  - Keep `confirmation_window_ms` as an extension constant (20 s).
- **Extension `enforce.js`**: replace the old matchers (`hosts`, `paths`, `tools`, `modes`,
  `min_size_bytes`) with the §5 matcher. Port the Go evaluator's table tests (task 12) to
  `node --test` so both implementations pass the same cases. Put the shared cases in one JSON
  file under `device/integration/testdata/enforce/` and have both test suites read it.
- **Messages**:
  - `warn` uses the existing confirmation overlay with the rule's `message` and `link`.
  - `block` cancels and shows the overlay with the rule's `message` and `link`.
  - Remove the hard-coded default text in `pipeline.js` (around line 476) and
    `content/content-script.js`. A rule without a message can't be `warn` or `block` (task 11).
- `check-vocab.mjs` stays green. If it compares the removed vocabularies, update it.

## Done when

- `cd device/extension && npm test` passes, including the shared evaluator cases.
- `cd device/capture-core && go test ./enforce/ ./cmd/capture-core/` passes against the same
  shared cases.
- On the reference host (lab MSI rebuilt, extension updated), with a lab-tenant rule
  "block `credential` on route `ext.web_request`" and the tenant at `m1` or higher:
  1. Pasting an AWS-key-shaped test string into ChatGPT in the browser is blocked, and the overlay
     shows the rule's message.
  2. The event carries `policy_decision.action = "blocked"`.
  3. Screenshot both.
- `node tools/accept.mjs` passes, including the `browser` gate.
