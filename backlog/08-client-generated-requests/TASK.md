# 08. Client-generated requests and classification scope

Depends on: nothing.

## Problem

Two symptoms, believed to share a cause.

1. Searching prompt text for "Australia" returns a hit reading "What is the capital of Australia
   Write the title in the predominant language of the session ...". That is Claude Code's own
   background request to title a session, which quotes the user's prompt inside a user-role message.
   It is stored, indexed and shown as if a person typed it.
2. The event for the prompt "What is the capital of Australia" is labelled `source_code` (score
   1.00) and `customer_pii` (0.50). The classifier appears to run over the whole captured request
   body, which includes the client's system prompt and tool definitions, rather than over the user's
   text. This is an inference from the labels; confirm it before building on it.

## What already exists

`vault/content-vault/internal/vault/typed.go` extracts what a person typed from a captured body (the
latest user message, with `<system-reminder>` blocks stripped), and the search index uses it. The
dashboard has the same logic in `query/dashboard/src/explore-model.js` (`exploreUserInput`). Both are
downstream patches. The endpoint should decide this once.

## Goal

- The endpoint (`endpoint/capture-core`, the classifier-host child and the proxy path) identifies the
  typed text of a request and classifies that, not the whole body.
- The endpoint marks each captured request with a kind: user prompt, client-generated (titling,
  summarisation, telemetry), or unknown. The envelope carries it. Use robust signals: request shape,
  absence of a typed turn, known client patterns per tool. Default to "user prompt" when unsure, so
  nothing a person typed is hidden.
- Ingest stores the kind; `query-api` exposes it as a filter; the vault does not index
  client-generated requests; Search hides them by default with a way to include them.

## Read first

- `docs/01-collectors.md`: classification on the device.
- `endpoint/protocol`: the envelope and classifier types.
- `contracts/`: the envelope contract. A new field is a contract change first.

## Done when

The titling request no longer appears in prompt search; the capital-of-Australia event carries no
`source_code` label; and a prompt that really contains source code or a card number is still
labelled. Tests cover the kind decision and the classification input. The agent change needs a new
build on the owner's machine: say in the report what the owner must run.
