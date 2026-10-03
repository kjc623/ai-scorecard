# vaultinvariants — the invariant tests, written from outside

`TestINV1_*` are the content vault's invariant-level tests, and they are deliberately not in
`internal/vault`. **INV-1** — "content stays put: the only content-egress path is a per-event grant" —
is a property of the whole product, not of one package's internals. A component cannot be the only
witness to the invariant it exists to enforce, so these tests are written from outside the package,
against the exported service API, where a change inside `vault` cannot quietly adjust the test that
would have caught it.

## What they prove

- **A fabricated grant produces nothing.** `Redeem` with a grant id that was never issued is refused
  with `grant_required`, and the log line records the reason rather than a generic error.
- **A grant is bound to its event.** A grant issued for one event cannot be redeemed against another
  (`grant_event_mismatch`), and a grant issued to one principal cannot be redeemed by another
  (`grant_principal_mismatch`).
- **A grant is single-use and time-bound.** Redeeming twice fails the second time
  (`grant_already_used`), and an expired grant fails with `grant_expired`.
- **The authorised path reports unavailability rather than empty success.** Where content is gone, the
  caller gets the §11 `no_longer_available` result and a reason, never a silent success with no bytes.
- **Search is refused when custody makes it impossible.** A `customer_held` tenant with `full_text` is
  refused at the service boundary, so a dropped database constraint or a bypassed migration cannot turn
  the vault into an indexer.

Every refusal is asserted to carry a reason from the closed vocabulary rather than a generic error,
because a refusal that cannot be grouped is a refusal nobody can count.

## What they deliberately do not prove

That the storage layer, the key backend or the HTTP surface behave correctly in detail. Those belong to
the component's own suite under `internal/` (`vault`, `keys`, `httpapi`, `store`), and this package
reaches them only through the exported service API. The two suites together are what the
[README](../README.md#build-and-test) means by the module's test run.
