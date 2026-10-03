# `contracts/tools/` — the generator and its suite

Turns [`../event-envelope.schema.json`](../event-envelope.schema.json) into the TypeScript and Go
types, and proves the result is what consumers can actually use.

```
node contracts/tools/generate.mjs           # write the generated files
node contracts/tools/generate.mjs --check   # exit 1 if the committed files differ
node contracts/tools/verify.mjs             # the suite
```

`--check` is gate 2 of `node tools/accept.mjs`: the generated output is committed so consumers never
run a generator, and this is what keeps the committed output honest.

| File | Responsibility |
|---|---|
| `generate.mjs` | Renders the two languages from the model and writes them, or compares them under `--check` |
| `schema-model.mjs` | Reads the schema and derives what both languages are rendered *from*: the closed `kind` registry, the per-kind and per-collection-mode field rules carried by the `if`/`then` branches, and the two shapes |
| `verify.mjs` | The suite |
| `index.js` | Re-exports the suite, which is how `node tools/verify-all.mjs` discovers it |

## Why the model is a separate file

The generator contains no contract knowledge of its own beyond how to *render* a model. Everything
that decides what the contract says lives in `schema-model.mjs`. That split is what makes the
generator trustworthy: if it also interpreted the schema, a mistake in interpretation would be
invisible in both the output and the diff.

`verify.mjs` is deliberately **not** a mirror of the generator. A test that reimplements the thing it
is testing agrees with it by construction — the failure mode this repository names as "the test that
agrees with the bug". It checks the generated output against the schema and against what a consumer
needs, not against the generator's own logic.
