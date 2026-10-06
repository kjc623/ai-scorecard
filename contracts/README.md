# contracts

The wire contract for the event envelope: what a device sends to `POST /v1/events`.

| Path | What it is |
|---|---|
| `event-envelope.schema.json` | The contract, JSON Schema 2020-12. The only source of envelope field names |
| `generated/go/` | Go module `github.com/shadow-ai-capture/contracts`, package `envelope`: generated, committed |
| `tools/` | The generator and its verification suite |

## The envelope

One record per observation, discriminated on `kind` (`prompt`, `usage_rollup`, `model_detection`)
and, for prompts, on `collection_mode` (`m0`–`m3`). The kind registry is closed: a kind the schema
does not name is an invalid record, not a new kind. Every field is declared once in
`$defs/envelopeCore` with `additionalProperties: false`; the `if`/`then` branches decide which
fields each kind and mode requires and forbids. At `m0` no content-derived field may appear — the
device may report that a submission happened and how large it was, nothing about its content. A
device never sends `received_at`; the server assigns it.

## The Go binding

`node contracts/tools/generate.mjs` renders `generated/go/envelope/envelope.go` from the schema and
copies the schema beside it as `event-envelope.schema.json`, which the package embeds as
`envelope.Schema`. The package provides one struct per device variant (a required field is a value,
a permitted one a pointer, a forbidden one absent), the closed enums, and `DecodeDeviceSubmission`,
which dispatches on `kind` and `collection_mode` and refuses undeclared fields. It does not check
value constraints: ingest-api validates every record against `envelope.Schema` with a JSON Schema
library first, then decodes it.

The output is committed so consumers never run a generator; `--check` fails when it drifts.

## Commands

```sh
node contracts/tools/generate.mjs           # regenerate
node contracts/tools/generate.mjs --check   # exit 1 if the committed output differs
node --test contracts/tools/                # drift, registries, field sets, gofmt, vet, Go tests
cd contracts/generated/go && go test ./...  # the Go package's own tests
```

Changing the schema changes every consumer: the endpoint (`device/capture-core` mints envelopes),
ingest-api (validates and decodes them) and the database (`ingest.record_event` stores them).
