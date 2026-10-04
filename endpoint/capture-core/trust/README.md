# trust — the per-device root CA in the OS trust store

`trust` is the platform half of "the device trusts the CA that intercepts its traffic"
(docs/01-collectors.md §4.5, §5.2, §14). The CA is minted by `proxy.tls`; this package installs its
public certificate into the operating system's real trust store, verifies it is there, and removes
it when an uninstall or kill switch asks.

The defining property is **choose the store the platform actually honours**. Writing a CA to the
wrong store fails silently — the file write succeeds, the platform ignores it, and interception
breaks the user with no error. So each platform's command path targets exactly one store and
verifies by reading it back or by asking the platform to report the certificate, never by trusting
that a command returned exit code zero.

## Platform behaviour

| OS | Install | Verify | Remove |
|---|---|---|---|
| linux | write PEM to `<CertDir>/<Name>.crt`, run `update-ca-certificates`; if that is absent or fails, write to `/etc/pki/ca-trust/source/anchors/<Name>.crt` and run `update-ca-trust extract` | read the installed file back and compare bytes | delete the file and re-run the update |
| darwin | `security add-trusted-cert -d -r trustRoot -k <Keychain> <tmpPEM>` | `security find-certificate -a -c <CN> -Z <Keychain>`, confirm the SHA-1 in the output | `security delete-certificate -Z <sha1> <Keychain>`, then re-query to confirm gone |
| windows | `certutil -addstore -f Root <tmpFile>` (`Store: enterprise` adds `-enterprise`) | `certutil -store Root <sha1>`, confirm the thumbprint in the output | `certutil -delstore Root <sha1>`, then re-query to confirm gone |

## Design notes

- **No build tags.** Behaviour is selected by `Config.OS`, so a Linux test drives the macOS and
  Windows command construction through a fake `Runner`. `New` defaults `OS` to `HostOS()`.
- **Fingerprints.** SHA-256 (lowercase hex) is the identity in `Info`; SHA-1 (uppercase hex, no
  separators) is computed solely because it is the certificate identifier `security` and `certutil`
  use to address a cert — it is not a security primitive here.
- **`Remove` needs no argument.** The manager remembers the last installed DER, so `Remove(ctx)`
  satisfies `core.TrustRoot`. Install is idempotent; Remove errors when nothing was installed or
  when the removal cannot be verified.
- **Verify fails closed.** An unreadable store is an error, not a silent "not installed". The
  runner's exit code is never trusted alone: the fingerprint must be present in its output (or the
  bytes must match on disk, on Linux).
- **No panics.** Every error names the OS, the store and the cause.

## Tests

`trust_test.go` uses a fake `Runner` (recording exact argv, scripted stdout) and `t.TempDir()` to
cover all three OS values: exact command construction, temp staging and cleanup, idempotent
install, Verify true/false and unusable-store errors, Linux file deletion and update re-run, and
unknown-OS fail-closed. A real self-signed certificate is generated in-test.
