# 0010. The event envelope is a discriminated union on `kind`, with a closed kind registry

Status: proposed
Date: 2026-10-02

## Context

Brief §4.1 fixes the event envelope field by field and requires that "every collection path must emit
the same record". It then shows a record containing `content_digest`, `labels`, `classifier_version`,
`size_bytes` and `policy_decision` — every one of which only makes sense for a prompt.

The product needs three different things to travel the same path:

- a **prompt** submission (usage modes A–H),
- a **usage rollup**, which is the only permitted exit for process-level observation (brief R7),
- a **model detection** for usage mode I, which by construction has no prompt because none is reachable.

Making the prompt-only fields nullable would satisfy "the same record" literally while losing the
ability to validate anything: an ingest path that accepts `labels: null` on a rollup also accepts it on
a prompt, and the failure is silent.

## Decision

One record, with a common core plus per-`kind` required fields, expressed in
[contracts/event-envelope.schema.json](../../contracts/event-envelope.schema.json) as JSON Schema
2020-12 with `if`/`then` branches on `kind`. `kind` is a **closed registry**: `prompt`, `usage_rollup`,
`model_detection`.

The kind-specific rules are enforced again as database check constraints, so a validation gap in a
service cannot admit a record the store should reject:

| Kind | Carries | Must not carry |
|---|---|---|
| `prompt` | tool, digest, size, labels, classifier version, policy decision | window fields, detection basis |
| `usage_rollup` | window start/end, submission count, bytes total | digest, labels, policy decision, size |
| `model_detection` | detection basis | digest, labels, policy decision, size, window |

Two further consequences of the union:

- **`received_at` is server-assigned and a device must not send it.** Brief §3.6 requires exactly two
  clocks, and a device-supplied receive time is neither. A separate `deviceSubmission` definition in the
  contract forbids it explicitly.
- **`schema_version` is added**, because brief §4.3 requires validation against a versioned schema and
  there was otherwise no field to version.

The closed registry is also what makes brief R7 **structural** rather than procedural. R7's mitigation is
"filter and roll up at the source; treat as a hard requirement", and a hard requirement that depends on
every collector behaving is not hard. Because the server rejects any `kind` outside the registry, a
collector defect **cannot** begin shipping raw per-process telemetry: there is no kind for it.

## Alternatives considered

- **One flat record with nullable prompt fields.** Rejected as above: it accepts a rollup with labels.
- **Three separate endpoints and record types.** Rejected: it multiplies the device-facing surface,
  which brief §4.3's "idempotency enforced by the store" and the batch model both argue against, and it
  fragments the per-path coverage accounting that brief §7 requires.
- **An open `kind` string with per-kind validation in the service.** Rejected: it puts R7's enforcement
  in application code, where a new kind can be added by a collector change rather than a server decision.

## Consequences

Easier: validation is exhaustive and mechanical; a malformed record is refused at the edge with a reason
code rather than stored; the collection-mode boundary is checkable (an M0 prompt carrying a content
digest is a defect and is refused, not ignored); R7 is enforced by the schema rather than by review.

Harder: adding a kind is a contract change requiring a `schema_version` bump and a coordinated server
release. That friction is deliberate.

We now maintain: the contract, its generated TypeScript and Go types, and a conformance test suite —
because the same schema now gates two collectors, two services and the dashboard.

Revisit if: the union grows past four or five kinds, at which point the common core is probably carrying
too little and the kinds should become separate records on separate paths.
