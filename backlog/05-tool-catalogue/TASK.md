# 05. Tool catalogue

Depends on: 01 (the Tools page needs aggregate rows).

## Problem

Tools appear everywhere as fingerprints such as `tls_b6681b043244c43f` and `tls_69f6ab029df3c019`.
The Tools page can never show sanctioned or unsanctioned, and the Unsanctioned view is always
empty, because `ops.tool` has no rows and nothing maps a fingerprint to a tool.

## Goal

1. A catalogue that resolves what the endpoint observed (TLS fingerprint, destination host, client
   process) to a named tool: "Claude Code", "ChatGPT (web)", "GitHub Copilot". Ship a seed catalogue
   for the common AI tools. An unknown fingerprint stays visible as "Unrecognised tool" with the raw
   fingerprint available.
2. A per-tenant sanction decision for each tool (sanctioned, unsanctioned, unknown) with an audited
   API to set it. Unknown stays unknown; it is never shown as unsanctioned.
3. `query-api` joins the name and the present-tense sanction state at read time for `q1` (tools
   ranked), `q2` (unsanctioned users) and the event list.

## Read first

- `docs/04-dashboard-and-query.md` section 3 (`q1`, `q2`).
- `query/dashboard/src/vocab.js`: the note that sanctioned state is joined at read time.
- `database/schema.sql`: `ops.tool`.
- `endpoint/capture-core`: how `tool_fingerprint` is derived.
- `query/query-api/src/registry.js`: `mart.v_tool_usage`, `mart.agg_tool_user_period`.

## Done when

The lab's Claude Code traffic shows as "Claude Code" on Tools, in Search rows and in search results.
Marking it unsanctioned through the API makes it appear in the Unsanctioned view with the people
using it, subject to k-suppression. Tests cover resolution, the unknown case and the sanction write.
