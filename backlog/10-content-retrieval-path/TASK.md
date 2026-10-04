# 10. Content retrieval path

Depends on: nothing. Access control for content is task 11, not this one.

## Problem

Reading a stored prompt works, but not the way the design specifies.

1. Content transits `query-api`. The vault returns the decrypted content in its response body and
   `query-api` relays it to the browser (`query/query-api/src/http/content.js`, the comment headed
   "AS BUILT"). The design has the vault mint a short-lived retrieval URL so content never passes
   through `query-api` (`docs/02` section 11).
2. The vault fetches the stored ciphertext with an unauthenticated GET from
   `SAC_BLOB_CIPHERTEXT_ENDPOINT` (`vault/content-vault/cmd/content-vault/main.go` logs a warning
   about it). It presents no storage credential, so it only works against the lab's storage stand-in
   (`localdev` contentlab).

## Goal

The vault reads objects from real blob storage with its own identity, and serves a read through a
single-use, short-lived retrieval URL that the browser redeems directly. `query-api` hands the
browser the URL and never sees content. The audit-before-serve rule and the single-use grant stay.

## A recent product decision

A retrieval no longer requires a case reference or a second approver
(`vault/content-vault/internal/vault/service.go`, `Retrieve`). Opening an event in Search reads its
prompt. Do not reintroduce the approval.

## Read first

- `docs/02-ingest-and-transport.md` section 11, and `docs/06` on content custody.
- `azure/`: the storage and identity the deployment provides.
- `vault/content-vault/internal/vault/service.go`: `Retrieve`, `Redeem`, `serve`.

## Done when

In the lab the browser fetches content from the vault's retrieval URL; `query-api`'s response
carries no content; the lab still works offline, with a storage stand-in that requires a
credential; and the dashboard's Event drawer shows the prompt as it does today. Tests cover expiry,
single use and a replayed URL.
