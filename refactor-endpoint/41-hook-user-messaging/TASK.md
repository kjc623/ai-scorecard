# 41. Coaching messages

Needs: Claude Code and Cursor installed on the reference host and signed in.

## Problem

Rules carry a message of up to 280 characters and an optional https link (`DESIGN.md` §5). The
whole point of "redirect rather than block" is that the user sees the approved alternative at the
moment of the block. Tasks 37 and 38 pass the message through, but nobody has checked how each
tool shows it:
- line breaks;
- length;
- whether the link is clickable;
- whether a `warn` is shown at all.

## Goal

On `warn` and `block`, the rule's message and link appear legibly in Claude Code and Cursor, and
the link is usable.

## Scope

- For each tool and each of `warn` and `block`:
  1. Write a lab-tenant rule with a 280-character message and a link.
  2. Trigger it, and record how the tool shows it: wrapping, truncation, Markdown or plain text,
     link clickable or not. Take screenshots.
- Adjust only the adapters' `Render` (`capture-core/hooks/claudecode.go`, `cursor.go`):
  - the order of message and link;
  - a separator;
  - Markdown link syntax, only where the tool renders Markdown.

  Make no change to rules, the bundle or the relay.
- If a tool shows nothing for `warn`, record it in `DECISIONS.md`, and leave `warn` as an allow
  that is recorded `warned` only where something was shown (otherwise `logged`). Adjust the
  adapter's per-event `canEnforce` for warn accordingly.
- Golden tests: each adapter's `Render` output for a long message with a link, for both actions.

## Done when

- `cd device/capture-core && go test ./hooks/` passes with the golden tests.
- `DECISIONS.md` has the observed rendering per tool and action, with versions.
- The report includes four screenshots (2 tools × warn/block) showing the message and link.
- `node tools/accept.mjs` passes.
