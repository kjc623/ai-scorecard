# internal/ — the service's seams

| Package | What it is |
|---|---|
| `store` | the persistence seam: an interface, the in-memory double, and the SQL implementation whose statement text the tagged test executes |
| `signer` | the `CertificateSigner` interface, the development `LocalCA`, and the `KeyVaultSigner` that refuses clearly until the vault client lands |
| `enrol` | `POST /v1/enrol`: token/credential validation, region pin, C11 idempotency, credential issuance |
| `token` | `POST /v1/token`: RFC 7523 assertion + RFC 9449 proof, and the ES256 `at+jwt` it issues; the `Verifier` is exposed for re-enrolment |
| `jose` | the stdlib-only compact JWS / EC JWK subset shared by the proof and the token |
| `dpop` | the RFC 9449 proof verifier, including `htu` reconstruction behind a gateway |
| `content` | `POST /v1/content/grant` and the finaliser: the grant decision over server-side state, the signed upload URL, the HTTP client for content-vault, and its own SQL statements (`sql.go`). It has a SQL store only |
| `httpapi` | the HTTP transport and §5's common error envelope. It also resolves the current credential a request presents; a certificate forwarded by the edge arrives percent-encoded in `X-Client-Cert` and is decoded here |

The dependency direction is one way: `httpapi` calls `enrol`, `token` and `content`; none knows the
others; `enrol` and `token` call `store` and `dpop`; `signer` is consumed only by `enrol`; `content`
has its own store interface and reaches the vault over HTTP. The device-side shapes come from
`github.com/shadow-ai-capture/device/protocol` and are consumed, never forked.
