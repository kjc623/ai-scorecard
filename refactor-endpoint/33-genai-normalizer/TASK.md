# 33. Generic GenAI normalizer

## Problem

Tools the product has no normalizer for, such as in-house agents, SDK-based scripts and new
vendor tools, increasingly emit spans following the OpenTelemetry GenAI semantic conventions
(`gen_ai.*` attributes). Today the receiver counts them and drops them (task 23). They also don't
say which app they are in a way the catalog knows.

## Goal

Spans and log records with `gen_ai.*` attributes from a sender no other normalizer accepts become
`agent_activity` and prompt events. The app is identified from the sending process (task 24), not
from what the telemetry claims.

## Scope

- **`device/capture-core/otlp/genai`**, a `Normalizer`, registered last so it only sees what no
  tool-specific normalizer accepted:
  - `Accepts`: anything not accepted by another normalizer.
  - **Spans**:
    - a span with `gen_ai.operation.name` `chat`, `text_completion` or `generate_content` (or the
      older `gen_ai.system` plus a request model) → `agent_activity` / `model_request`, with
      `gen_ai.request.model` (else `gen_ai.response.model`), `gen_ai.usage.input_tokens`,
      `gen_ai.usage.output_tokens`, the span duration, and outcome from the span status;
    - `execute_tool` → `agent_activity` / `tool_call` with `gen_ai.tool.name`.
  - **Prompt text**: when a span or log event carries the latest user message
    (`gen_ai.input.messages`, or the older `gen_ai.prompt` / `gen_ai.user.message` event), call
    `Pipeline.Process` on route `tool.otel` with that message as the content reader, as task 26
    does. Earlier turns in the same messages array are not re-reported.
  - Verify the current semantic-convention attribute names
    (https://opentelemetry.io/docs/specs/semconv/gen-ai/) and record the semconv version
    followed in `DECISIONS.md`.
- **Which app**:
  - With the sender resolved (task 24), look up its image with `Bundle.AppByExe`. A catalog match
    gives `app:<app_key>`.
  - With no match, `tool_fingerprint` is `exe:<first 16 hex chars of sha256(lower-case image base name)>`.
  - An unresolved sender gives `exe:unknown`.
  - The `service.name` the telemetry declares is never used as the fingerprint, because a sender
    can claim any name.
  - The `exe:` form is defined in `DESIGN.md` §3. Check that ingest accepts it: the fingerprint
    pattern allows it, and it gets no `ref.tool_catalogue` row, so it shows as "Unrecognised tool".
- **Tests**:
  - spans built with the official Go SDK, using semconv attribute names, from a test process
    convert to the expected envelopes;
  - the fingerprint rules (catalog hit, miss, unresolved);
  - a span with no `gen_ai.*` attributes is counted `skipped_not_generative` and dropped;
  - at `m0` no message text is read.

## Done when

- `cd device/capture-core && go test -race ./otlp/...` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
1. On the PC, build a small Windows program outside the repository that uses the official
   OpenTelemetry Go SDK. It emits one `chat` span with `gen_ai.*` attributes and a user message
   to `http://127.0.0.1:47318`, with the token taken from its environment.
2. Copy it in with `invm.ps1 -CopyTo`.
3. Read the token with `invm.ps1 -Command` (don't print it in the report), and run the program
   as the console user with `invm.ps1 -AsUser console -Command`, passing the token in the
   environment.
4. The `otel_receiver` row's `emitted` counter rises by two, and the spool drains with the
   batch acknowledged (`invm.ps1 -AgentState`).
5. The owner confirms on the dashboard (event list, test tenant): an `agent_activity` /
   `model_request` and a `tool.otel` prompt event, with `tool_fingerprint`
   `exe:<hash of the program's name>` (shown as "Unrecognised tool"). Its `user_ref` is the one
   derived from the console user's UPN, or its `subject_name` is that UPN if device identity is
   clear.
