# internal — the content-vault packages

Six packages. The split follows the service's real seams: who the caller is, which keys exist, where
the wrapped key lives, what the policy decisions are, how they are spoken over HTTP, and what the tests
need to build a working service.

Nothing in here declares the wire vocabulary for collection modes: `m0`–`m3` come from
[endpoint/protocol](../../../endpoint/protocol/), consumed and never redefined, so this service cannot
fork a second spelling of "m3".

| Package | Why it exists |
|---|---|
| `auth` | The vault's caller identity, and a deliberate liability unless the network control holds. It has internal ingress only, so the identity it reads is what the ingress put there after authenticating the peer; the `HeaderAuthenticator` trusts `X-Sac-Service`/`X-Sac-Subject`/`X-Sac-Tenant` **only** because the ingress sets them from an authenticated peer and strips any inbound copy. The allowed callers are a closed set (`query-api`, `control-api`, `ops`), every missing fact is a refusal — no anonymous principal, no default tenant — and the tenant must be a uuid. A deployment that publishes this service has turned the package into an authentication bypass; the binary's non-loopback refusal is the most a process can check about its own ingress. |
| `httpapi` | The vault's one HTTP surface: `POST /v1/content/object`, `/object/finalise`, `/retrieval`, `/redeem`, `/shred`, `/rotate`, `/content-search`, and `GET /healthz`. The device-facing paths (including `/v1/content/grant`, which belongs to control-api) and the analyst paths are deliberately absent, and a test asserts they 404 rather than trusting that nobody will add them. It keeps two error kinds apart: a content refusal carries one of the closed denial reasons and 403, while a malformed body is a transport error (`bad_request`, 400) and is not a statement about content at all. Body tenant is checked against the principal in one place rather than in seven handlers. |
| `keys` | The content key hierarchy: `Wrap`, `Unwrap`, `CurrentVersion`, `NewVersion`, `Destroy`, `Kind` — six methods, no way to ask for KEK bytes, because "the KEK never leaves the key store" is only checkable if the interface cannot express the request. The wrapped DEK is bound to its row by GCM additional authenticated data (tenant, object, KEK id, KEK version), so a key moved to another row fails authentication. `LocalKeyWrapper` is AES-256-GCM software for tests and local development; `KMSKeyWrapper` is explicitly unimplemented and fails loudly, and its doc comment is the specification of what a real vault must provide. Destruction is a real operation: after `Destroy`, `Unwrap` returns `ErrKeyDestroyed`, which callers must report as a *result* rather than an error. |
| `store` | The persistence seam: the wrapped DEK in a database row, the ciphertext in a blob this service never sees. Every operation is one statement (or a short fixed sequence in one transaction), all of them in `sql.go`, which is what makes the audit-before-serve ordering and the single-use grant claim checkable by reading. `Memory` is the test double, and it is deliberately **permissive** about the schema's invariants — it will store a tenant whose custody and search tier contradict each other, exactly as a database whose constraint had been dropped would — so the service's own copy of the rule is what the tests exercise. `SQLStore` takes an already-opened `*sql.DB`, because this module carries no PostgreSQL driver and nothing in the binary opens one. |
| `vault` | The authorisation and key logic: prepare/finalise an object, retrieve and redeem under the grant matrix, shred and erase, rotate, and search. Two rules shape every method — content is read only through a recorded, unexpired, single-use grant bound to the principal and the event; and a read is audited *before* it is served and fails closed. Destruction is the third: destroying a key destroys the content, so the read path reports `no_longer_available` and never an error. `errors.go` holds the closed denial vocabulary, `search.go` the tier/scope rules and the three closed query forms. |
| `testrig` | Test-only fixtures: a tenant with a given custody mode and search tier, a service wired to the in-memory store and the local key wrapper, a controllable clock so grant and retention expiry are tested by moving time rather than sleeping, and a helper that walks the whole content path (prepare, finalise, retrieve, redeem). Nothing in the product imports it, and it never touches a database or a cloud service. |

## What is deliberately not here

No blob client: this build performs no blob I/O, and a deployment returns a short-lived storage URL
instead of serving bytes. No cloud SDK: mode 1's Key Vault and modes 2–3 are an interface and a refusal.
No content search over ciphertext: the index is plaintext-derived and lives in PostgreSQL, which is
exactly the trade [ADR 0014](../../../docs/adr/0014-content-search-is-a-per-tenant-capability.md)
records. See [../README.md](../README.md#not-verified-and-why) for what is asserted and what is not.
