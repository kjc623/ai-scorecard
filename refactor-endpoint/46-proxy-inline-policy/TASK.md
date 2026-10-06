# 46. Inline policy in the proxy

Needs: Claude Desktop installed on the reference host and signed in, with TLS inspection on for
the lab tenant.

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
       - the desktop backends: the shapes their apps display, verified from captures;
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
- On the reference host with a lab-tenant rule "block `credential`":
  1. Sending an AWS-key-shaped test string in Claude Desktop shows the app's error with the rule's
     message, and a Windows notification appears.
  2. The event shows `source = proxy.tls` and `policy_decision.action = blocked`.
  3. Screenshots of both, and of the dashboard event.
- `node tools/accept.mjs` passes.
