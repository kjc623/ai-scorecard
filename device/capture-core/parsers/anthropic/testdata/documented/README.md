# Anthropic Messages API fixtures: documented, not captured

These request bodies are written by hand from the Messages API reference. No client produced them.
A capture from the reference VM replaces this folder with one named after the captured version.

- Source: https://platform.claude.com/docs/en/api/messages/create and, for the beta block types,
  https://platform.claude.com/docs/en/api/beta/messages/create (the Markdown forms, `.md`), read
  2026-10-08. `docs.anthropic.com` was not reachable from the build machine.
- Cross-checked against the request types generated from the API's OpenAPI specification in
  `anthropics/anthropic-sdk-python` (`src/anthropic/types/`, `src/anthropic/types/beta/`), `main` at
  `50b78d17`, committed 2026-10-08.
- Endpoint: `POST https://api.anthropic.com/v1/messages`.

## Files

| File | What it covers |
| - | - |
| `messages-string.json` | One user message whose content is a string; a string `system` |
| `messages-multi-turn.json` | Earlier turns; `system` as text blocks; a final user message of an image and a text block; `stream` |
| `messages-prefill.json` | A trailing assistant message that prefills the reply |
| `messages-tool-result.json` | A `tool_use` answered by a `tool_result` whose content is a string |
| `messages-tool-result-blocks.json` | Parallel tool calls: a `tool_result` with text blocks and one with an image; a `thinking` block |
| `messages-document.json` | `document` blocks with a plain-text, a base64 PDF and a URL source, then a text block |
| `messages-search-result.json` | A `tool_result` holding a `search_result` and a `content` document |
| `messages-beta-mcp-tool-result.json` | Beta: an `mcp_tool_use` answered by an `mcp_tool_result` |

Every file's turn (the trailing user messages) reads, after canonicalisation, as the canary
`sac canary anthropic-api AKIAIOSFODNN7EXAMPLE`. Where the turn has two text sources the canary is
split between them. Everything else is `SAC-PLACEHOLDER-<n>`; the PDF and image data are the
first bytes of a PDF and a PNG.
