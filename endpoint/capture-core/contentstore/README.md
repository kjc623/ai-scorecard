# contentstore — the M3 local content store

This is the store [docs/01-collectors.md §11.3](../../../docs/01-collectors.md) gives an M3 device:
content keyed by event, held on the device within local retention, and never leaving it except
through a per-event grant ([docs/02 §3, §10](../../../docs/02-ingest-and-transport.md)). It
implements `core.ContentStore`, which is the one method the pipeline calls, and
`core.ContentStateReporter`.

It exists because of what the envelope is not. The envelope carries no prompt text at any mode, and
[ADR 0017](../../../docs/adr/0017-the-m3-content-state-marker-is-device-local.md) keeps even the
fact "this device holds content" off the wire: a device claiming it holds content is not evidence
that it does. So at M3 the content has to live somewhere on the device, with enough state beside it
to ask the server for a grant later. That is this package, and nothing in it enters an envelope.

## What it holds

Two things per event.

- **The content**, one file, `<event_id>.sealed`. It is sealed with AES-256-GCM under a 32-byte key,
  with a fresh nonce prepended and the event id as additional data, so a sealed file renamed to
  another event fails to open.
- **The grant state**, one entry in `index.json`: size, the local retention deadline, the state
  below, the last failure and when to try again, and the envelope facts a grant request repeats
  (mode, content digest, rule id). The index is not sealed. It holds no content, and it is written
  through a temporary file and a rename so a crash leaves the old index or the new one.

The key file must be **outside** the store directory, for the reason the spool's must (§12): the key
and what it seals do not travel together. `Open` refuses a key path inside the directory, and
creates a missing key file with 32 random bytes, mode `0600`. As with the spool key and the pinned
CA key, that is a file protected by filesystem ACLs: DPAPI and Keychain sealing are not implemented.

## The states

| State | Meaning | Leaves by |
|---|---|---|
| `held` | Stored. Its event has not been delivered, so no grant can be asked for | `MarkDelivered` |
| `ready` | The event was delivered; a grant may be requested | `Settle`, or stays `ready` through `Retry` |
| `uploaded` | A grant was issued and the object was written. Terminal | local retention |
| `denied` | The server decided against an upload. Terminal; the content stays on the device | local retention |

`Ready()` lists what may be requested now, oldest deadline first, and leaves out an object whose
retry time has not come. `Retry` records why an attempt failed and when to try again without
changing the state. `Expire` removes every object past its deadline whatever its state, because
local retention is a property of the content, not of the grant.

A denial keeps the content. That is deliberate and it is the design's: the four-value content state
distinguishes `local_only` from `not_captured`, and a denied grant leaves content `local_only`.

## Tests

`TestHeldContentIsSealedAndSurvivesReopen` proves the directory holds no plaintext and that content
and grant state survive a restart; `TestGrantStateMachine` proves content is not requestable before
its event is delivered, that a backed-off request is not offered early, and that a denial is
terminal and keeps the content; `TestExpiryAndKeyPlacement` proves retention removes held content
and that a key file inside the store directory is refused.

## What it deliberately does not do

- **No transport.** It never asks for a grant and never uploads. [`drain/`](../drain/README.md) owns
  the credential and the edge, and drives this store through a narrow interface.
- **No decision about what is content.** It stores the bytes the pipeline hands it: the prompt text
  where the route identified the user-authored segment, and the request body as observed where it
  did not.
- **No attachments.** Only the prompt content is held; attachment bytes are not.
- **No deletion on upload.** An uploaded object stays until local retention removes it. Whether a
  device should drop its copy once the server holds one is not decided anywhere, so this does the
  thing that loses nothing.
- **No bound.** Unlike the spool it has no byte or object cap, and nothing evicts under pressure;
  retention is the only thing that removes content. A device at M3 with a long retention and heavy
  use grows without limit.
- **No platform key wrapping.** See above.
