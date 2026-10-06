//go:build darwin || linux

package cli

import "os"

// systemRootBundles are the system CA bundles of the common Linux distributions and macOS, in the
// order crypto/x509 consults them.
var systemRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt",                // Debian, Ubuntu, Arch
	"/etc/pki/tls/certs/ca-bundle.crt",                  // Fedora, RHEL
	"/etc/ssl/ca-bundle.pem",                            // openSUSE
	"/etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem", // CentOS, RHEL
	"/etc/ssl/cert.pem",                                 // macOS, Alpine
}

// systemRootsPEM returns the first system CA bundle that exists. The CA-bundle variables replace a
// runtime's trust list rather than add to it, and the proxy tunnels every destination outside the
// interception scope with its real certificate, so a bundle holding only the device root would
// fail verification for everything the device does not intercept.
func systemRootsPEM() []byte {
	for _, p := range systemRootBundles {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return append([]byte("\n"), b...)
		}
	}
	return nil
}
