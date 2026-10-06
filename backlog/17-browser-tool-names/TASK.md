# 17. Name the tools the browser extension observes

## Problem

The dashboard names a tool by looking its fingerprint up exactly (`ops.tool_display_name()`: the
tenant's own `ops.tool` names first, then `ref.tool_catalogue`, else "Unrecognised tool"). The two
capture routes compute fingerprints differently:

- `proxy.tls` in capture-core: `tls_` + 16 hex (`device/capture-core/proxy/tlsproxy/provider.go`).
  The catalogue has rows for these.
- The extension: `tf1:` + base32 SHA-256 of a signal vector that includes the body shape
  (`device/extension/src/tool-fingerprint.js`), so one site yields different fingerprints at M0 (`unread`)
  and at M1+. The catalogue has **no** `tf1:` rows.

So every prompt captured in the browser (chatgpt.com, claude.ai, gemini.google.com, …) shows as
"Unrecognised tool", and the extension's comment that one tool seen through either route yields one
fingerprint is not true.

## Goal

Every web AI tool the extension observes is named on the dashboard, and the names cannot silently
drift from what the extension emits.

## Decide first (ask the owner)

Present both, with your recommendation:

- **A. Catalogue the extension's fingerprints.** Add `ref.tool_catalogue` rows for every `tf1:`
  fingerprint the extension can produce for each tool it observes (every body-shape variant).
  Smallest change; the two routes keep different fingerprints for the same tool.
- **B. One fingerprint per tool across routes.** Make capture-core's proxy and the extension derive
  the same value, and rekey the catalogue. Cleaner, but it changes every stored `tool_fingerprint`
  (nothing is deployed yet, so no data migration is needed beyond the lab).

## Requirements (either way)

- A check that fails when the extension can emit a fingerprint for a tool that has no catalogue row:
  a test in `device/extension/test/` that computes the fingerprints and reads the seed in
  `services/database/schema.sql`, or a step in `tools/check-vocab.mjs`.
- Correct the comment in `device/extension/src/tool-fingerprint.js` to say what is true.
- A catalogue change goes in `services/database/schema.sql` (nothing is deployed, so no migration file).

## Done when

- The check passes, and fails if one catalogue row is removed.
- On the lab, a prompt sent from chatgpt.com in the browser is listed under "ChatGPT (web)" on Tools
  and in Search (observed in the browser).
- `node tools/accept.mjs` passes.
