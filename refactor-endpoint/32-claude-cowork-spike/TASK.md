# 32. Claude Cowork spike

Needs: on the reference host, Claude Desktop with Cowork available to the owner's account. If
Cowork isn't available on Windows, a Mac with it is acceptable for the research half; say which
was used.

## Problem

Harmonic reports that OTel is the only way to see Claude Cowork prompts. The plan doesn't know:
- how Cowork's OTel is configured;
- whether an administrator can enforce it;
- whether it runs on Windows at all.

This is research, with code only if the answer is clear and fits the existing pattern.

## Goal

`DECISIONS.md` records how Cowork's telemetry is configured and enforced, what it emits, and
whether the product can collect it the way it collects Claude Code (tasks 25–27). If it can, the
report names the follow-up tasks precisely.

## Scope

- **Research**:
  - the current Anthropic documentation for Cowork telemetry and administration (the monitoring
    page, the Claude Desktop enterprise configuration documentation, and any managed-settings or
    MDM keys);
  - Harmonic's OTel announcement (in `PLAN.md` sources) for what they configure.
- **Try it on a real install**:
  1. Point Cowork's OTel at an `otelcol-contrib` file exporter (as in task 25).
  2. Run one Cowork task.
  3. Capture the output.
  4. If output arrives, save it as fixtures under
     `device/capture-core/otlp/testdata/cowork/<version>/`, with task 25's placeholder rules and
     README.
- **Answer in `DECISIONS.md`**:
  - the platforms Cowork runs on;
  - where its OTel settings live;
  - whether an admin-managed location exists that users can't override (Windows registry
    policy, a managed settings file, an MDM profile);
  - the `service.name` and the event names;
  - whether prompt text is included and under which switch;
  - whether the existing Claude Code normalizer (task 26) already accepts the events.
- **Outcome**, exactly one of these:
  - **Covered by existing code**: the Claude Code normalizer and config writer already cover
    Cowork, for example because Cowork reads the same managed settings `env`. Show a Cowork
    prompt arriving as a `tool.otel` event, and stop.
  - **Needs follow-up**: write in the report the exact follow-up tasks (normalizer, config
    writer, the new `tools` key `cowork` in `DESIGN.md` §5, and the catalog row) for the owner
    to add. Write no product code beyond the fixtures.
  - **Not collectable**: record why (no OTel on Windows, no enforceable config, no prompt
    content) and what would change that.

## Done when

- The outcome and its evidence are in `DECISIONS.md`, and the report quotes it.
- Any fixtures added have their README, and `node tools/accept.mjs` passes.
