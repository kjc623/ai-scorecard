# classifier-host

Classifies prompt and document content on the device. It receives bytes and a collection mode —
never an identity — and returns labels with a confidence band; when a stage cannot finish (budget,
parser failure, unreadable body) the answer is `degraded`, carries no labels and names the stage.
The pipeline is normalisation (NFKC), the rules in [`rules/default.json`](rules/default.json) with
their validators, then the keyword model in [`rules/model.json`](rules/model.json) for classes no
rule labelled. Documents (docx, xlsx, pptx, odt, ods, zip, gzip) are parsed in a separate
`parse-child` process per document, under a parent-enforced timeout, memory and output limit.

## How it runs

capture-core starts it beside its own binary and speaks `endpoint/protocol` frames on its
stdin/stdout; the host exits when stdin closes:

    classifier-host serve --release DIR --pubkey HEX --transport stdio

| capture-core setting | Meaning |
|---|---|
| `SAC_CLASSIFIER_RELEASE` | The installed release directory (`manifest.json`, `rules.json`, `model.json`) |
| `SAC_CLASSIFIER_PUBKEY` | Hex Ed25519 key the release must verify under; the host refuses to start otherwise |

## Building a release

The package build signs the release with `cmd/classifier-release`, which requires every flag:

    go run ./cmd/classifier-release --rules rules/default.json --model rules/model.json \
      --key KEYFILE --version VERSION --out DIR

`KEYFILE` holds the signing key as a hex Ed25519 seed. The command prints `pubkey=<hex>`, the value
the package sets as `SAC_CLASSIFIER_PUBKEY`.

## Test

    go test ./...
