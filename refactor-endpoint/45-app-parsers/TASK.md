# 45. Per-app protocol parsers

Needs, in the device phase (task 60): Claude Desktop and ChatGPT Desktop installed on the reference VM and signed in as the
console user; the owner available for the console steps and the test tenant's TLS inspection
setting (`AGENTS.md`).

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

    The private backends' endpoints and body shapes are verified from captures, not assumed,
    and the captures are taken on the reference VM in the device phase (On the device). So
    `claudeai` and `chatgpt` are built then, on this task's fix branch; until then the registry
    falls back to `generic` for `claude.ai` and `chatgpt.com`, and `DECISIONS.md` says so.
    Record the verified endpoints and shapes in `DECISIONS.md` with the app versions.
- Each parser is isolated: a panic in one is recovered, the request falls back to `generic`, and
  it counts `errors`.
- A body that matches a parser's host and path but none of its known shapes returns
  `ErrUnknownShape`. The observation then goes on with `confidence: degraded` and the
  `content_unprocessable` detail on the provider row. This is the canary that flags format
  changes.
- **Canary text**: one fixed phrase per app plus an AWS-key-shaped test string, for example
  `sac canary claude-desktop AKIAIOSFODNN7EXAMPLE`. No real prompts.
- **Fixtures** (`parsers/<target>/testdata/<app version>/`):
  - For the build: the API parsers' bodies (`anthropic`, `openai`, `gemini`) written from the
    vendors' current API references, in a `documented/` folder with a README naming the
    reference page and the date.
  - In the device phase: request bodies captured on the reference VM with a test-only
    intercepting proxy, which replace the `documented/` sets and supply the desktop backends'.
    No product build captures bodies, and only `main`'s release is ever deployed to the VM.
    The capture:
  1. Ask the owner to switch TLS inspection off for the test tenant (Settings page); wait until
     `invm.ps1 -AgentState` shows the proxy stopped and no PAC is set.
  2. Copy a pinned release of mitmproxy's `mitmdump.exe` for Windows, plus a small addon script
     that writes the bodies of POST requests to the allow-listed hosts to files, into
     `C:\ProgramData\SacTestbed\mitm\` with `invm.ps1 -CopyTo`.
  3. Start `mitmdump` as the console user (`invm.ps1 -AsUser console`), which generates a
     throwaway CA. Add that CA to the machine `Root` store with `invm.ps1 -Command`.
  4. Point the console user's Internet Settings at `mitmdump` (`ProxyServer`, `ProxyEnable`,
     after recording the previous values) with `invm.ps1 -AsUser console`.
  5. The owner restarts both apps and sends the canary prompts at the VM's console.
  6. Copy the bodies back with `invm.ps1 -CopyFrom`.
  7. Undo everything: stop `mitmdump`, restore the Internet Settings values, remove the
     throwaway CA from `Root`, and delete `C:\ProgramData\SacTestbed\mitm\`. Ask the owner to
     switch TLS inspection back on.
  8. Record the tool, its version and the undo checks in `DECISIONS.md`.
  - API fixtures (`anthropic`, `openai`, `gemini`) may instead come from requests sent with
    `invm.ps1 -AsUser console -Command 'curl.exe ...'` through the same `mitmdump`.
  - Streaming responses aren't parsed: the agent reads requests only, so `Result` has no
    response text. Record this in `DECISIONS.md` as a deviation from the plan's "and streamed
    responses".
- Replace both copies of the extractor in `tlsproxy` and `loopback` with the registry.
- `core/promptkind.go`'s Claude Code marker phrases stay where they are. They are prompt-kind
  logic, not parsing.
- Tests:
  - every fixture parses to its canary text;
  - an altered fixture (a renamed field) returns `ErrUnknownShape`;
  - a panicking test parser falls back.

## Done when

- `cd device/capture-core && go test -race ./parsers/... ./proxy/...` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- First the fixture capture in Scope (steps 1–8). Then, on this task's fix branch, build the
  `claudeai` and `chatgpt` parsers against the captured bodies, replace the `documented/` API
  fixtures with captured ones, and take the fix through merge and deploy before the checks
  below.
- The fixture capture's undo is shown: `invm.ps1 -Command 'certutil -store Root'` lists no
  mitmproxy CA, the console user's `ProxyEnable` and `ProxyServer` have their recorded values,
  and the `mitm` folder is gone.

Then, with TLS inspection on and the test tenant at `m1` or higher (ask the owner to check the
Settings page; wait):
1. The owner sends a canary prompt in Claude Desktop and one in ChatGPT Desktop at the VM's
   console. Add an `invm.ps1 -Screenshot` of each app after sending.
2. `invm.ps1 -AgentState` shows the `egress_proxy` `emitted` counter up by two, no new
   `content_unprocessable`, and the spool drained.
3. The owner confirms on the dashboard two `proxy.tls` events (Claude Desktop and ChatGPT
   Desktop), attributed to the console user, each labelled with the `credential` data class.
   The label can only come from the parsed canary text.
