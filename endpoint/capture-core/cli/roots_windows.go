//go:build windows

package cli

import (
	"crypto/x509"
	"encoding/pem"
	"syscall"
	"unsafe"
)

// systemRootsPEM returns the roots in the machine's ROOT store. SSL_CERT_FILE, REQUESTS_CA_BUNDLE
// and CURL_CA_BUNDLE replace a runtime's trust list rather than add to it, and the proxy
// blind-tunnels every destination outside the interception scope with its real certificate, so a
// bundle holding only the device root would fail verification for everything the device does not
// intercept. Windows fetches rarely-used roots on demand, so this is the set present now, not
// every root Windows would trust.
func systemRootsPEM() []byte {
	name, err := syscall.UTF16PtrFromString("ROOT")
	if err != nil {
		return nil
	}
	store, err := syscall.CertOpenSystemStore(0, name)
	if err != nil {
		return nil
	}
	defer syscall.CertCloseStore(store, 0)

	var out []byte
	var cert *syscall.CertContext
	for {
		// Each call frees the context it was handed, so there is nothing to release in the loop.
		cert, err = syscall.CertEnumCertificatesInStore(store, cert)
		if err != nil || cert == nil {
			return out
		}
		der := append([]byte(nil), unsafe.Slice(cert.EncodedCert, cert.Length)...)
		if _, err := x509.ParseCertificate(der); err != nil {
			continue
		}
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
}
