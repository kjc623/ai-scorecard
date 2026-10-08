# 12. Device rule engine

## Problem

After task 11 the bundle carries enforcement rules, but no device code evaluates them. Every route
records the fixed decision `{rule_id: "policy.default", action: "logged"}` through a `Decide`
seam:
- `proxy/tlsproxy/provider.go`, around lines 120–124;
- `proxy/loopback/broker.go`, around lines 101–105;
- `cmd/capture-core/native.go`, around lines 273–276, when the extension sends no decision.

Hooks need a decision before the prompt is sent; the other routes need the decision recorded.

## Goal

One rule evaluator in capture-core decides allow, warn or block for an observation, from its
labels, tool, category, sanction state and route. Every route records the evaluator's decision.
Routes that can't stop a prompt (the proxy until task 46, the loopback broker) record what was
done, not what the rule asked for.

## Scope

- **New package `device/capture-core/enforce`**:
  - `Evaluate(b *policy.Bundle, in Input) Decision`.
  - `Input{Route protocol.Route, ToolFingerprint string, Labels []string, LabelsKnown bool}`.
  - `Decision{RuleID, Action (allow|warn|block), Message, Link}`.
  - Matching follows `DESIGN.md` §5 exactly:
    - `categories` resolve through the bundle's `catalog` (task 14). Until the catalog exists, a
      rule with a non-empty `categories` list never matches. Say so in a comment in the code and
      in the report.
    - `sanction` uses `sanctioned_tools`.
    - With `LabelsKnown` false (`m0`, or a degraded classification), a rule with a non-empty
      `labels` list doesn't match.
  - No match returns rule `policy.default`, action `allow`.
  - Pure functions, no I/O.
  - Table-driven tests cover every match field, empty lists, first-match-wins, unknown labels and
    the m0 case.
- **Record what happened**: add `RecordedAction(d Decision, canEnforce bool) protocol.Decision`:
  - `block` with `canEnforce` gives `blocked`;
  - `warn` with `canEnforce` gives `warned`;
  - anything without `canEnforce`, and `allow`, gives `logged`;
  - always `decided_locally: true`;
  - the rule id is kept on `logged`, so an analyst can see which rule would have acted.
- **Wire it in**:
  - Replace the default `Decide` in the TLS proxy, the loopback broker and the native endpoint's
    no-decision path. Each evaluates after classification, so labels are known. In
    `core.Pipeline.Process` this means the decision is computed after `classify`: change
    `Observation.Decision` from a value the provider passes to a value the pipeline computes
    through an `Observation.Enforce func(labels []string, known bool) protocol.Decision` hook.
  - Today none of these routes enforces (`canEnforce = false`).
  - The extension still sends its own decision; keep it when present.
- **Bundle swap**: the evaluator always uses the bundle in force at the moment of the decision
  (`pipe.Bundles`), so a rule change applies without a restart.

## Done when

- `cd device/capture-core && go test -race ./enforce/ ./core/ ./proxy/... ./cmd/capture-core/` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- Ask the owner to switch TLS inspection on for the test tenant and keep task 11's "block
  credential" rule (the test tenant's mode must be `m1` or higher); wait one policy poll.
1. `invm.ps1 -AsUser console -Command` runs `curl.exe` to an intercepted host, with an
   AWS-key-shaped test string (the classifier's `AWS_ACCESS_KEY_ID` rule) in a JSON body. It
   produces a `proxy.tls` event: the `egress_proxy` `emitted` counter rises and the batch is
   acknowledged (`invm.ps1 -AgentState`).
2. The owner confirms on the dashboard's event detail that the event's `policy_decision` is
   `{rule_id: <the rule>, action: "logged", decided_locally: true}`.
3. Ask the owner to switch TLS inspection off again.
