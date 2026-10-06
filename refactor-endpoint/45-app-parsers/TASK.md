# 45. Per-app protocol parsers

Needs: Claude Desktop and ChatGPT Desktop installed on the reference VM and signed in as the
console user, with TLS inspection on for the lab tenant.

## Problem

The proxy and the loopback broker each carry a copy of one generic JSON extractor
(`proxy/tlsproxy/serve.go`, around lines 76–143; `proxy/loopback/serve.go`, around lines
250–316). It takes:
- a top-level `prompt`; else
- the last `messages[]` entry with role `user`; else
- `input`.

It doesn't understand:
- Anthropic `tool_result` blocks;
- Gemini `contents[].parts`;
- OpenAI's Responses API;
- the private backends of Claude Desktop and ChatGPT Desktop.

When a vendor changes a body shape, nothing notices: extraction silently degrades.

## Goal

Each target app or API has its own parser, versioned and tested against real captured fixtures.
An unrecognised body shape is flagged (`confidence: degraded`, counted on the health row) instead
of mis-extracted.

## Scope

- **New package `device/capture-core/parsers`**:
  - `Parser` interface: `Match(host, path string) bool`,
    `Parse(body []byte, mediaType string) (Result, error)`, `Version() string`.
  - `Result{Text string, Attachments []dedup.Attachment, Shape string}`.
  - A `Registry` chooses by host and path, falling back to `generic`, which is the existing logic
    moved here once.
  - One subpackage per target:
    - `anthropic`: Messages API, `api.anthropic.com/v1/messages`, including `tool_result` and
      `document` blocks;
    - `openai`: Chat Completions and Responses, `api.openai.com/v1/chat/completions` and
      `/v1/responses`;
    - `claudeai`: Claude Desktop's backend on `claude.ai`;
    - `chatgpt`: ChatGPT Desktop's backend on `chatgpt.com`;
    - `gemini`: `generativelanguage.googleapis.com` `contents[].parts`.

    The private backends' endpoints and body shapes are verified from captures, not assumed.
    Record them in `DECISIONS.md` with the app versions.
- Each parser is isolated: a panic in one is recovered, the request falls back to `generic`, and
  it counts `errors`.
- A body that matches a parser's host and path but none of its known shapes returns
  `ErrUnknownShape`. The observation then goes on with `confidence: degraded` and the
  `content_unprocessable` detail on the provider row. This is the canary that flags format
  changes.
- **Fixtures** (`parsers/<target>/testdata/<app version>/`): request bodies captured on the
  reference VM through the proxy, with canary prompt text (no real prompts).
  - Send the canary prompts in each app at the VM's console as the console user.
  - Capture the bodies with a capture-only build of the agent on a scratch branch that is never
    merged, which writes intercepted request bodies to a file. Deploy it with
    `node localdev/testbed/deploy.mjs`, and redeploy the normal build afterwards.
  - Copy the files back with `invm.ps1 -CopyFrom`.
  - API fixtures (`anthropic`, `openai`, `gemini`) may instead come from requests sent through the
    proxy with `invm.ps1 -AsUser console -Command 'curl.exe ...'`. Streaming responses
  aren't parsed: the agent reads requests only, so `Result` has no response text. Record this in
  `DECISIONS.md` as a deviation from the plan's "and streamed responses".
- Replace both copies of the extractor in `tlsproxy` and `loopback` with the registry.
- `core/promptkind.go`'s Claude Code marker phrases stay where they are. They are prompt-kind
  logic, not parsing.
- Tests:
  - every fixture parses to its canary text;
  - an altered fixture (a renamed field) returns `ErrUnknownShape`;
  - a panicking test parser falls back.

## Done when

- `cd device/capture-core && go test -race ./parsers/... ./proxy/...` passes.
- On the reference VM, deployed with `node localdev/testbed/deploy.mjs`:
  1. A canary prompt sent at the VM's console in Claude Desktop, and one in ChatGPT Desktop, each
     produce a `proxy.tls` event at `m1`+, attributed to the console user.
  2. Each event's digest matches the canary text's digest. Compute that separately on the PC and
     show both.
  3. Add an `invm.ps1 -Screenshot` of each app after sending.
- `node tools/accept.mjs` passes.
