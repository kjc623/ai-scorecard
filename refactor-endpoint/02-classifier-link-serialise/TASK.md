# 02. Serialise classifier requests

## Problem

capture-core talks to `classifier-host` over one stdio pipe, and `classifier-host` serves one
request at a time (`device/classifier-host/classify/server.go`, around lines 51–72).
`classifierlink.Client.Classify` (`device/capture-core/classifierlink/link.go`, around lines
120–180) does not serialise callers:

- Two concurrent callers can interleave their frames, and each can read the other's response.
- When a caller's context ends first, its goroutine keeps reading, and can take the response meant
  for the next caller.

Today the proxy, the loopback broker and the native endpoint call it concurrently. The OTLP
receiver and the hook relay will add many more callers.

## Goal

Each `Classify` call gets the response to its own request, or a degraded fallback. Never another
caller's.

## Scope

- In `classifierlink/link.go`, hold a request mutex from writing the frame until reading its
  response (or giving up).
- When a call gives up (budget or context) before its response is read, close and drop the
  connection, so a late response can't be read by the next call. The next call reconnects through
  the existing `Connect` path. The giving-up call returns the existing degraded fallback
  (`rulesOnlyFallback`).
- A caller waiting for the mutex counts its wait against its own budget. If the budget runs out
  while waiting, it returns the fallback without sending.
- Keep the public API of `Client` unchanged.
- Tests in `classifierlink/link_test.go`:
  - 50 concurrent `Classify` calls against a fake host that echoes a request-specific marker:
    every caller gets its own marker.
  - A call that times out, followed by another call: the second gets its own response, not the
    late one.
  - Run both under `-race`.

## Done when

- `cd device/capture-core && go test -race ./classifierlink/` passes with the new tests, and the
  report shows both new tests failing before the fix.
- `node tools/accept.mjs` passes.
