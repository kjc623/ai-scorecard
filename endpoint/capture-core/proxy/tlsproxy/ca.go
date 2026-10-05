package tlsproxy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// Sealer is the platform's at-rest key protection (A1: DPAPI scoped to the service account on
// Windows, Keychain on macOS). It is an interface because the real facility cannot be exercised
// in a test: the fake in this package's tests is an in-process AEAD, and the real implementations
// are NOT VERIFIED here.
//
// The CA private key is never written to disk **by this package**: the only form that leaves this
// process is Seal's output, and nothing in this package writes it anywhere. That is §3.3's "a CA
// key is an interception capability": escrowing it turns a per-device liability into a
// fleet-wide one.
//
// A deployment that pins a CA pair across restarts (cmd/sac-bundle + --ca-key) persists the key
// itself, as a 0600 file when no platform keystore is configured — the same file-key-provider
// deviation §14.3 records for the spool key, reported unsealed rather than implying protection the
// build does not have. DPAPI/Keychain sealing is not wired yet.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// CAInfo is the non-secret description of the device CA, for the trust store and for health.
type CAInfo struct {
	Subject     string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string // sha256 of the DER, for the operator to check the right root is installed
}

// CA is the per-device certificate authority. One per device, never per tenant and never per
// fleet (A3): a stolen key covers only that device's minted leaves, and there is no vendor-held
// key that could be compelled to mint a certificate for a customer's hostname.
type CA struct {
	key    *ecdsa.PrivateKey
	cert   *x509.Certificate
	der    []byte
	pool   *x509.CertPool
	sealed []byte

	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
	leafTTL time.Duration
}

// LeafTTL bounds a minted leaf's life. Short-lived is the point: a leaf that outlives the
// process's need for it is an interception capability lying around.
const LeafTTL = 12 * time.Hour

// NewCA mints a per-device CA. The key is generated here and never leaves the process in the
// clear; when a Sealer is supplied, the sealed form is retained for the platform to store.
func NewCA(deviceID string, sealer Sealer, now time.Time) (*CA, error) {
	// Three calendar months: the life of a CA minted for one run or one lab bundle.
	return NewCAValidFor(deviceID, sealer, now, now.AddDate(0, 3, 0).Sub(now))
}

// NewCAValidFor mints a per-device CA valid for validity. A CA the device keeps across restarts is
// installed in the trust store once and reused, so it lives longer than one minted per run; it is
// still per device, so its life bounds only that device's exposure.
func NewCAValidFor(deviceID string, sealer Sealer, now time.Time, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: generating device CA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "Shadow AI Capture Device CA " + deviceID,
			Organization: []string{"Shadow AI Capture"},
		},
		NotBefore:             now.Add(-time.Hour), // tolerate a small clock skew at install
		NotAfter:              now.Add(validity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: self-signing device CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: parsing device CA: %w", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)

	ca := &CA{key: key, cert: cert, der: der, pool: pool, leaves: map[string]*tls.Certificate{}, leafTTL: LeafTTL}
	if sealer != nil {
		// Only the sealed form is retained; the plain DER of the key is handed to Seal and
		// dropped. Nothing here writes it anywhere.
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			return nil, fmt.Errorf("tlsproxy: marshalling the device CA key: %w", err)
		}
		sealed, err := sealer.Seal(keyDER)
		if err != nil {
			return nil, fmt.Errorf("tlsproxy: sealing the device CA key: %w", err)
		}
		ca.sealed = sealed
	}
	return ca, nil
}

// DER returns the CA certificate for the platform trust store. Only the public half.
func (c *CA) DER() []byte { return append([]byte(nil), c.der...) }

// Pool returns a pool that trusts this CA: the interceptor's own end-to-end probe uses it, and a
// test client does the same instead of touching the OS trust store.
func (c *CA) Pool() *x509.CertPool { return c.pool }

// Info describes the CA for health and for the installer's verification step.
func (c *CA) Info() CAInfo {
	return CAInfo{
		Subject:     c.cert.Subject.CommonName,
		NotBefore:   c.cert.NotBefore,
		NotAfter:    c.cert.NotAfter,
		Fingerprint: fingerprint(c.der),
	}
}

// Leaf mints (or returns a cached) short-lived certificate for a hostname. Leaves are never
// written to disk, and the cache is keyed by host so a busy destination costs one mint.
func (c *CA) Leaf(host string, now time.Time) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cert, ok := c.leaves[host]; ok {
		return cert, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: leaf key for %s: %w", host, err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: leaf serial: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(c.leafTTL),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := parseIP(host); ip != nil {
		tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
	} else {
		tmpl.DNSNames = append(tmpl.DNSNames, host)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: minting leaf for %s: %w", host, err)
	}
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	c.leaves[host] = cert
	return cert, nil
}

// Sealed returns the sealed CA key, for the platform store. It is empty when no sealer was
// configured, which is a configuration the caller can detect rather than a silent no-op.
func (c *CA) Sealed() []byte { return append([]byte(nil), c.sealed...) }

// ErrNoCA reports that interception was attempted without a device CA, which is a programming
// error rather than a runtime condition: a provider that cannot mint a leaf cannot intercept.
var ErrNoCA = errors.New("tlsproxy: no device CA")

func fingerprint(der []byte) string {
	sum := sha256Sum(der)
	return fmt.Sprintf("%x", sum[:])
}
