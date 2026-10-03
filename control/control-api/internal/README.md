# internal/ — the service's seams

| Package | What it is |
|---|---|
| `store` | the persistence seam: an interface, the in-memory double, and the SQL implementation whose statement text the tagged test executes |
| `signer` | the `CertificateSigner` interface, the development `LocalCA`, and the `KeyVaultSigner` that refuses clearly until the vault client lands |
| `enrol` | `POST /v1/enrol`: token/credential validation, region pin, C11 idempotency, credential issuance |
| `token` | `POST /v1/token`: RFC 7523 assertion + RFC 9449 proof, and the ES256 `at+jwt` it issues; the `Verifier` is exposed for re-enrolment |
| `jose` | the stdlib-only compact JWS / EC JWK subset shared by the proof and the token |
| `dpop` | the RFC 9449 proof verifier, including `htu` reconstruction behind a gateway |
| `httpapi` | the HTTP transport and §5's common error envelope |

The dependency direction is one way: `httpapi` calls `enrol` and `token`; neither knows the other; both
call `store` and `dpop`; `signer` is consumed only by `enrol`. The device-side shapes come from
`github.com/shadow-ai-capture/device/protocol` and are consumed, never forked.
