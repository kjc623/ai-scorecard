# Gemini API fixtures: documented, not captured

These request bodies are written by hand from the Gemini API's service definition. No client
produced them. A capture from the reference VM replaces this folder with one named after the
captured version.

- Source: https://github.com/googleapis/googleapis, `master` at `6553725b`, read 2026-10-08:
  `google/ai/generativelanguage/v1beta/generative_service.proto` (`GenerateContentRequest`, the
  HTTP bindings), `google/ai/generativelanguage/v1beta/content.proto` (`Content`, `Part`,
  `FunctionResponse`) and `google/ai/generativelanguage/v1/generative_service.proto`.
  ai.google.dev was not reachable from the build machine.
- The REST body is the proto3 JSON form of `GenerateContentRequest`, which accepts each field by
  its lowerCamelCase name and by its proto name (`inlineData` and `inline_data`).
- Endpoints: `POST https://generativelanguage.googleapis.com/{v1,v1beta}/{models,tunedModels,dynamic}/<model>:generateContent`
  and `:streamGenerateContent`; the body is the same for both.

## Files

| File | What it covers |
| - | - |
| `generate-content-text.json` | One content with no role and one text part |
| `generate-content-multi-turn.json` | `systemInstruction`; earlier turns, a model thought part; a final user content of text, `inlineData` and `fileData` parts |
| `generate-content-function-response.json` | A model `functionCall` (with a thought signature) answered by a user `functionResponse` |
| `generate-content-snake-case.json` | Proto field names: `system_instruction`, `inline_data`, `function_response`; two trailing user contents |

Every file's turn (the trailing user contents) reads, after canonicalisation, as the canary
`sac canary gemini-api AKIAIOSFODNN7EXAMPLE`: text parts, and the string values of a function
response. Where the turn has two text sources the canary is split between them. Everything else
is `SAC-PLACEHOLDER-<n>`; the inline data are the first bytes of a PNG and a PDF.
