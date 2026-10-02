# 0018. `model_detection` carries no window, and the contract's kind branches are exhaustive

Status: proposed · Amends the `model_detection` branch of
[contracts/event-envelope.schema.json](../../contracts/event-envelope.schema.json) · Related:
[0010](0010-the-envelope-is-a-discriminated-union-with-a-closed-kind-registry.md)
Date: 2026-10-02

## Context

ADR 0010 makes `kind` a closed registry and gives each kind a branch that pins what it may and may
not carry. Generating language bindings from the schema (T1) surfaced a gap in one branch, and the
generator refusing to guess is what made it visible: `model_detection`'s `not.anyOf` list forbade
`window_start`, `content_digest`, `labels`, `classifier_version`, `content_excerpt`,
`policy_decision`, `size_bytes` and `attachments` — but not `window_end`, `submission_count` or
`bytes_total`. Those three were therefore legal, and optional, on a `model_detection` record.

Three sources say they should not be:

- ADR 0010 records `model_detection` as a per-detection record: one observation that a local model
  was used, not a bucket of usage.
- `docs/01-collectors.md` §4.4 emits exactly one `model_detection` per evidence-of-use and says the
  daily granularity requirement is satisfied by `usage_rollup`, which is the kind that carries a
  window.
- The schema's own `usage_rollup` branch forbids all three, because a windowed rollup is the only
  shape that has them. `model_detection` forbidding `window_start` while permitting `window_end` is
  incoherent on its face: a window with one end.

This is a single-authoring omission, not drift: no commit has touched either list since the
baseline. The generated bindings faithfully reproduced it, which is the correct behaviour for a
generator and the reason the gap reached review rather than being smoothed over.

## Decision

**`model_detection`'s branch forbids `window_end`, `submission_count` and `bytes_total`**, added to
the existing `not.anyOf` list, and the bindings are regenerated. This is a **tightening**: it
forbids three fields and requires none, so no record that validated before this change stops
validating. The generated TypeScript and Go therefore lose three optional fields on
`DeviceModelDetection` and `StoredModelDetection`, and no consumer needs to change to keep
compiling.

The general rule this instance stands for, which is what the record is really about: **a kind's
branch is exhaustive over the fields the other kinds own.** When a field is added to the contract
for one kind, the question "which other kinds must now forbid it?" is part of adding it, and the
generator is the wrong place to absorb the answer.

## Alternatives considered

- **Leave the schema and document the variance.** Rejected: the generated types would keep
  advertising three fields on a record type that must never carry them, every future consumer would
  have to know not to use them, and the contract's `additionalProperties: false` posture exists
  precisely so that "may this field be here?" is answered by the schema rather than by a reader.
- **Bump `schema_version` to 1.1.** Rejected: a version bump is for a change that alters what a
  conforming record looks like in a way deployed peers must coordinate on. Nothing deployed exists,
  and the change only removes permission for fields that should never have been permitted, so it
  cannot invalidate a record in the field. If a real 1.1 arrives for another reason, this decision
  is unaffected.
- **Require `submission_count: 0` on `model_detection`** to represent "this model ran and we captured
  no submissions". Rejected here, though the reasoning is worth recording: §4.4 does want that fact,
  but it belongs to `usage_rollup`, which already permits `submission_count: 0` and is the kind whose
  granularity is per device, per tool, per day. Duplicating it onto `model_detection` would put the
  same fact in two kinds, and the contract's kind registry exists to stop that.

## Consequences

**Easier.** A `model_detection` record has exactly one shape, and the generated types say so. The
temptation to stuff a rollup-shaped field into a detection record — which is what risk R7 is about,
raw telemetry arriving in a shape nobody designed — is now rejected at the schema rather than at
ingest.

**Harder.** Nothing operationally: the change happens before anything is deployed, and no consumer
compiles differently. The cost is one regeneration and a re-run of the codegen verification suite,
which is the mechanism that found the gap.

**We now maintain.** The exhaustiveness rule above, as a review question whenever a contract field is
added: which kinds must now forbid it, and does the generator's output still describe one shape per
kind. `contracts/tools/verify.mjs` asserts per-kind required/optional/forbidden sets in both
languages, so a future omission of this kind fails that suite rather than reaching a consumer.

**Revisit if:** a kind is added that legitimately carries a window for a reason other than periodic
aggregation. That would be a change to ADR 0010's registry, not to this record.
