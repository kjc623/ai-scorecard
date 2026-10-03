# credential — the sealed device credential

The per-device credential at rest: the issued x509 leaf (or the registered DPoP public key) plus the
EC private key that never leaves the device (ADR 0020 decision 3). The file is sealed with
AES-256-GCM under the key a `capture-spool` `KeyProvider` supplies — the same key and AEAD approach
the spool uses — so a credential file and a spool directory share one key-protection story.
