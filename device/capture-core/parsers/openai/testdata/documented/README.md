# OpenAI API fixtures: documented, not captured

These request bodies are written by hand from OpenAI's published OpenAPI specification. No client
produced them. A capture from the reference VM replaces this folder with one named after the
captured version.

- Source: `openapi.json` in https://github.com/openai/openai-openapi (spec `info.version` 2.3.0),
  default branch at `c7224137`, read 2026-10-08: the `CreateChatCompletionRequest` and
  `CreateResponse` schemas and the schemas they reference. platform.openai.com and
  developers.openai.com were not reachable from the build machine.
- Endpoints: `POST https://api.openai.com/v1/chat/completions` (`chat-*` files) and
  `POST https://api.openai.com/v1/responses` (`responses-*` files).

## Files

| File | What it covers |
| - | - |
| `chat-string.json` | A developer message and a user message whose content is a string |
| `chat-parts.json` | Earlier turns; a final user message of `text`, `image_url`, `file` and `input_audio` parts; `stream` |
| `chat-tool.json` | An assistant `tool_calls` message answered by a `tool` message whose content is a string |
| `chat-parallel-tools.json` | Two `tool` messages, one with text parts and one with a string |
| `responses-string.json` | `input` as a string, with `instructions` |
| `responses-messages.json` | Message items with and without `type`; a final user message of `input_text`, `input_image` and `input_file` |
| `responses-function-output.json` | A `reasoning` and a `function_call` item answered by a `function_call_output` with a string output |
| `responses-previous-response.json` | `previous_response_id` with a `function_call_output` of text parts and a `custom_tool_call_output` |

Every file's turn (the trailing user, tool and function input) reads, after canonicalisation, as
the canary `sac canary openai-api AKIAIOSFODNN7EXAMPLE`. Where the turn has two text sources the
canary is split between them. Everything else is `SAC-PLACEHOLDER-<n>`; the file, image and audio
data are the first bytes of a PDF, a PNG and a WAV.
