# 46. Inline policy in the proxy

Needs, in the device phase (task 60): Claude Desktop installed on the reference VM and signed in as the console user; the owner
available for the console step and the test tenant's settings (`AGENTS.md`).

## Problem

The TLS proxy evaluates rules (task 12) but can't act on them: it records `logged` with the
rule's id, because `canEnforce` is false for `proxy.tls`. For desktop apps without hooks, the
proxy is the only place a prompt can be stopped. A blocked request needs a response the app
shows, and an OS notification for apps that hide errors.

## Goal

With a `block` rule matching, the proxy refuses the request with an error the app displays,
records `blocked`, and shows a Windows notification with the rule's message. With a `warn` rule,
the request goes through, a notification shows the message, and the event records `warned`.

## Scope

- **`proxy/tlsproxy`**: the decision is taken after the body is read and parsed (task 45) and
  classified, before anything is forwarded upstream.
  - For a `block`:
    1. Answer the client with `403` and the parser's error body in that API's own error shape
       (`Parser.BlockResponse(message, link string) (status int, contentType string, body []byte)`,
       added to the interface from task 45):
       - Anthropic `{"type":"error","error":{"type":"permission_error","message":...}}`;
       - OpenAI `{"error":{"message":...,"type":"policy_violation"}}`;
       - the desktop backends: the shapes their apps display, verified from captures, built
         with their parsers in the device phase (task 45);
       - `generic`: plain text.
    2. Close the upstream connection without sending anything.
  - For a `warn`: forward, then notify.
  - Set `canEnforce` true for `proxy.tls`, but only when a parser matched (`generic` can't produce
    a shape the app displays, so a `generic` match stays `logged`).
- **Notification**: through `userhelper.Notify` (task 10) to the session of the process that
  owns the connection (task 09):
  - title "Shadow AI Capture";
  - body the rule's message;
  - the rule's link.

  A notify failure doesn't change the decision; it counts `errors`.
- **Kill switch**: an existing kill switch on `proxy.tls` (`policy.Bundle.KillSwitchFor`) disables
  enforcement as it does today, and everything is `logged`. Task 47 adds the dashboard control.
- Tests:
  - each parser's block body is the documented shape;
  - a blocked request is never forwarded (a fake upstream sees zero requests);
  - warn forwards and notifies through a fake helper.

## Done when

- `cd device/capture-core && go test -race ./proxy/tlsproxy/ ./parsers/...` passes.
- `node tools/accept.mjs` passes.

## On the device

On the reference VM, running `main`'s release:
- Ask the owner to switch TLS inspection on for the test tenant, with a "block `credential`"
  rule and the mode at `m1` or higher (Settings page); wait one policy poll.
1. The owner sends an AWS-key-shaped test string in Claude Desktop at the VM's console. It shows the app's
   error with the rule's message, and a Windows notification appears in the console session.
   Capture both with `invm.ps1 -Screenshot`, taken while the toast is visible.
2. The same secret sent with `curl.exe` to `api.anthropic.com`
   (`invm.ps1 -AsUser console -Command 'curl.exe ...'`) returns 403 with the Anthropic error
   shape and the rule's message.
3. `invm.ps1 -AgentState` shows the `egress_proxy` `emitted` counter up by two and the spool
   drained. The owner confirms on the dashboard that both events show `source = proxy.tls`,
   `policy_decision.action = blocked`, and the console user's `user_ref`.
