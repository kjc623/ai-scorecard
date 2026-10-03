# dedup — the device half of the normative dedup contract

This package implements [docs/02-ingest-and-transport.md §4](../../../docs/02-ingest-and-transport.md),
"Deduplication — normative", on the device side: `content_digest` (C9) and the `dedup_key` ladder
(§4.5).

It is a separate package from the collectors for one reason. The digest a route computes and the
digest ingest recomputes must be the same function, so there is one implementation and one test, and
the device never invents its own. When two routes see the same submission — a proxy reading
`messages[]`, an extension reading a compose box — §4's tie-break collapses them into one logical
fact only if both computed identical keys.

```
content_digest = "sha256:" + hex(SHA-256(digest_input))          (§4.2 C9)
dedup_key      = sha256("sac-dedup-1" ␟ … ␟ tier ␟ material)     (§4.5 ladder)
```

## Why it exists rather than being inlined

Both keys are pinned to a canonicalisation version, `sac-canon-1`, and both are hashes over text that
two different code paths produced. Skipping a step fails silently in the worst way: the same content
normalised two ways becomes two digests, two Tier-T keys, two rows in the store — and the product's
answer to "how much did this happen" quietly doubles. So the ordering, separators and substitutions
are literal here and asserted by test rather than being a convention each caller follows.

## What is in it

| Element | Meaning |
|---|---|
| `CanonVersion` = `sac-canon-1` | First field of `digest_input`; a version change cannot silently collide old and new digests. |
| `LadderVersion` = `sac-dedup-1` | The ladder's own version. |
| `BucketWidth` = 300 s | The dedup time bucket, on the device's own uncorrected wall clock. Narrower splits one submission seen by two routes; wider merges two identical sends. |
| `Sep` = U+001F | The field separator of C9. C4 guarantees it cannot occur inside a field, which is what makes the layout unambiguous without escaping or length prefixes. |
| `Unreadable` = `~` | Substituted when a route is structurally unable to read an attachment's bytes (C7, E3). It is not "absent": "no attachment" and "an attachment we cannot read" are different coverage facts and must survive into the key. |
| Tiers `T`, `S`, `R`, `D` | Content-derived material, the payload-shape surrogate M0 is forced to use, a rollup window, and a detection basis. |
| `Normalizer` | Unicode NFC (C3), an interface because Go's standard library has no normalisation. `canon` is the real implementation; `IdentityNFC` is the offline default. |
| `Attachment` | One attachment's canonicalisation input: name, media type, size, and a content digest or `Unreadable`. |

`IdentityNFC` performs no normalisation and is named *Identity*, not *Default*, so a caller cannot
adopt it by accident. It is correct for already-composed text — the overwhelming majority of UTF-8 in
the wild — and wrong for decomposed sequences; the difference is reported rather than hidden, and the
pipeline that uses it degrades instead of claiming a Tier-T key.

## Tests

`TestDigestInput_C9LayoutIsLiteral` pins the field layout; `TestCanonicalText_C2ToC6` covers the
canonicalisation steps; `TestCanonicalText_SeparatorCannotSurvive` and
`TestDecode_IllFormedBytesBecomeReplacement` cover the two inputs that could otherwise make the
layout ambiguous; `TestCanonicalAttachments_C7`, `TestNamesDigest_UnreadableWhenNone` and
`TestBucketStart_300Seconds` cover the attachment and time inputs; `TestKeyLadder_TiersDifferAndRequireIdentity`,
`TestContentDigest_SameContentDifferentSpellingIsOneDigest` and
`TestContentDigest_AttachmentTierIsPartOfTheDigest` cover the two keys themselves.

## What it deliberately does not do

- **No envelope validation and no policy.** It computes keys from what it is handed; deciding whether
  a submission may be read at all is the pipeline's mode gate.
- **No attachment reading.** Callers supply a digest or `Unreadable`; a route that can read bytes
  hashes them itself.
- **No tie-break.** Which route wins when two observe one submission is `ref.route_fidelity`, a
  server-side fact; this package only makes the keys comparable.
- **No Unicode tables.** NFC proper lives in [`canon`](../../canon/README.md); the interface exists so
  this package can be compiled and its arithmetic tested where those tables are unavailable.
