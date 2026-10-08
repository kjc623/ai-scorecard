# 41. Coaching messages

Needs, in the device phase (task 60): Claude Code and Cursor installed on the reference VM and signed in as the console user.

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

The observation needs the tools' interactive UIs, so it happens in the device phase (On the
device). The build gives `Render` a tested baseline to observe.

- **For the build**: golden tests of each adapter's `Render` (`capture-core/hooks/claudecode.go`,
  `cursor.go`) for a 280-character message with a link, for both `warn` and `block`, in the form
  each tool's current hooks documentation says it displays.
- **In the device phase**, for each tool and each of `warn` and `block`:
  1. Ask the owner to add a test-tenant rule with a 280-character message and a link, on the
     Settings page; wait for one policy poll.
  2. Trigger it on the reference VM, running `main`'s release, in the tool's interactive UI at the
     VM's console (an owner step). Record how the tool shows it: wrapping,
     truncation, Markdown or plain text, link clickable or not. Take screenshots with
     `invm.ps1 -Screenshot`.
- Adjust only the adapters' `Render`, on this task's fix branch:
  - the order of message and link;
  - a separator;
  - Markdown link syntax, only where the tool renders Markdown.

  Make no change to rules, the bundle or the relay.
- If a tool shows nothing for `warn`, record it in `DECISIONS.md`, and leave `warn` as an allow
  that is recorded `warned` only where something was shown (otherwise `logged`). Adjust the
  adapter's per-event `canEnforce` for warn accordingly.
- Update the golden tests to the observed rendering.

## Done when

- `cd device/capture-core && go test ./hooks/` passes with the golden tests.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release, with Claude Code and Cursor signed in as the
console user:
- The observation in Scope, for both tools and both actions. If `Render` changes, take the fix
  branch through merge and deploy and take the final screenshots on that release.
- `DECISIONS.md` has the observed rendering per tool and action, with versions.
- The report includes four `invm.ps1 -Screenshot` captures from the VM (2 tools × warn/block),
  showing the message and link, taken on the release that contains the final `Render`.
