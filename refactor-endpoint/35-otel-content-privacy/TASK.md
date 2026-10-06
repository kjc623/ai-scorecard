# 35. OTel content privacy

## Problem

OTel is the first collector that receives prompt text pushed at the device, rather than reading
it only when the mode allows. At `m1` the tool sends the text because the agent asked for it (to
classify locally). At `m0` a tool may still send it, if its own config was changed or a
normalizer mapped a field wrongly. The guarantee "metadata-only means no prompt text leaves the
device" (`DESIGN.md` §8) has to be proved end to end, not assumed from each normalizer. This
covers the spool, the drain, logs, health reports and error messages.

## Goal

A canary prompt sent through the receiver never appears in anything the device uploads or writes
at `m0`, `m1` or `m2`, beyond what each mode allows:
- `m0`: nothing;
- `m1`: a digest and labels;
- `m2`: a redacted excerpt.

A test proves this for every normalizer, so a future normalizer can't skip it.

## Scope

- **Test harness** (`device/capture-core/otlp/privacy_test.go`, plus a `device/integration` test
  for the full path):
  - Run a real `otlp` provider, the real pipeline, a real classifier-host (as
    `integration/attachment_path_test.go` builds it), the real spool, and a drain to a fake
    `/v1/events` (`integration/drain_path_test.go`'s `startTLSIngest`).
  - For each registered normalizer, use one fixture prompt event with the prompt text replaced by
    `SAC-CANARY-<random 16 hex>`. Include one fixture where the canary also sits in a non-prompt
    attribute the normalizer drops.
  - Run at `m0`, `m1`, `m2` and `m3`, then search:
    - every request body the fake ingest received;
    - the spool files (decrypted through the spool's own reader);
    - the content store;
    - every log line (a capturing logger);
    - every health report sent to the fake `/v1/health`.
  - Expected:
    - **`m0`**: zero hits anywhere, and the content reader is never called (instrumented).
    - **`m1`**: zero hits in uploads, logs and health; the envelope carries `content_digest` and
      labels.
    - **`m2`**: a hit only inside `content_excerpt.text`, and only as the classifier's excerpt
      allows.
    - **`m3`**: zero hits in uploads, logs and health. The canary exists only in the content
      store until a grant (not exercised here).
- **Fix what the test finds.** Fix any leak in the normalizer or receiver at fault, for example:
  - a decode error that quotes the body;
  - an attribute dump in a log line;
  - a content reader not wired through the mode gate.
- **The rule for future normalizers**: the test iterates over the registered normalizers, so a
  normalizer added later is covered without editing the test. Add one line to the `otlp` package
  doc saying so.

## Done when

- `cd device/capture-core && go test -race ./otlp/...` and `cd device/integration && go test ./...`
  pass, with the canary searches reporting zero unexpected hits per mode and normalizer. Print
  the per-mode hit table in verbose output and quote it.
- The test fails when a deliberate leak is introduced (log the prompt attribute in one normalizer
  temporarily). Show the failure, then revert.
- `node tools/accept.mjs` passes.
