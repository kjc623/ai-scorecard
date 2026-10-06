// Package scim is control-api's SCIM 2.0 service provider (RFC 7643, RFC 7644): the one way a
// customer's people and groups reach the product, pushed by their identity provider. It is built to
// what Microsoft Entra ID's provisioning service and Okta actually send, not only to the RFC, because
// a provider that answers the RFC but refuses Entra's "Replace" or Okta's path-less PATCH provisions
// nobody.
//
// # The person, keyed the way devices key them
//
// A user's canonical user_ref is protocol.DeriveUserRef(upn, userName) under the tenant's
// user-reference key (directory.UserRefKeys), fixed when the user is created. Devices derive the same
// value from the signed-in UPN, so a person's events and their directory row meet without either side
// sending a name. Because a userName can change, and a device may only know the Entra object id,
// every upn-ref the person has held and the oid-ref of a GUID externalId (Entra admins map
// objectId to externalId) are kept in ops.user_ref_alias pointing at the canonical ref, and ingest
// resolves an alias before it stores an event. A upn-ref belongs to whoever holds that userName now:
// when a userName is reused, its alias moves to the new holder, and the new holder gets a canonical ref
// of their own if the old one is already a person's.
//
// ops.user_dim holds one row per canonical ref: the department from the enterprise extension, the
// display name only while the tenant's device_identity is 'clear', the sealed externalId (else
// userName) as directory_object_id_enc, and the status from `active`.
//
// # What is stored, and what is not
//
// ops.scim_user keeps the resource exactly as last provisioned, sealed (resource_enc), so a GET
// returns what the identity provider sent and the provider's own diffing stays quiet; the lookups
// a provider makes (userName eq, externalId eq) go through HMACs under the tenant key, so the table
// holds no clear identifier. A password, when a provider sends one, is dropped before anything is
// sealed. Groups are stored as the provider sends them; no product decision reads them.
//
// # Retire, never delete
//
// DELETE /Users/{id} deactivates: the row stays, ops.scim_user.active is false and the ops.user_dim
// row is 'inactive', so history stays attributable to a person who left. The resource stays
// readable with active false, which departs from RFC 7644 §3.6's 404-after-delete on purpose: a
// person who is rehired under the same userName is found by the provider's own userName query and
// reactivated, rather than refused as a duplicate forever.
//
// # Authentication
//
// A request carries `Authorization: Bearer sacscim_<tenant>.<secret>`. The tenant id is in the clear
// so the row-level-security session can be opened for it; the database's own answer
// (ops.tenant_for_scim_token over the token's sha256) must name the same tenant, compared in constant
// time, and the token must be unrevoked. Everything after that runs in that tenant's session.
package scim
