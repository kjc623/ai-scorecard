package tlsproxy

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"
)

// CAInfo is the non-secret description of the device CA, for the trust store and for health.
type CAInfo struct {
	Subject     string
	NotBefore   time.Time
	NotAfter    time.Time
	Fingerprint string // sha256 of the DER, for the operator to check the right root is installed
}

// CA is the per-device certificate authority. There is one per device, never per tenant or per
// fleet: a stolen key covers only that device's minted leaves, and there is no vendor-held key
// that could be compelled to mint a certificate for a customer's hostname.
type CA struct {
	key  *ecdsa.PrivateKey
	cert *x509.Certificate
	der  []byte
	pool *x509.CertPool

	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
	leafTTL time.Duration
}

// LeafTTL bounds a minted leaf's life. Short-lived is the point: a leaf that outlives the
// process's need for it is an interception capability lying around.
const LeafTTL = 12 * time.Hour

// NewCA mints a per-device CA valid for three calendar months, the life of a CA minted for one
// run.
func NewCA(deviceID string, now time.Time) (*CA, error) {
	return NewCAValidFor(deviceID, now, now.AddDate(0, 3, 0).Sub(now))
}

// NewCAValidFor mints a per-device CA valid for validity. A CA the device keeps across restarts is
// installed in the trust store once and reused, so it lives longer than one minted per run; it is
// still per device, so its life bounds only that device's exposure.
func NewCAValidFor(deviceID string, now time.Time, validity time.Duration) (*CA, error) {
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

	return &CA{key: key, cert: cert, der: der, pool: pool, leaves: map[string]*tls.Certificate{}, leafTTL: LeafTTL}, nil
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

// ErrNoCA reports that interception was attempted without a device CA, which is a programming
// error rather than a runtime condition: a provider that cannot mint a leaf cannot intercept.
var ErrNoCA = errors.New("tlsproxy: no device CA")

func fingerprint(der []byte) string {
	sum := sha256Sum(der)
	return fmt.Sprintf("%x", sum[:])
}

// NewCAFromPEM rebuilds a device CA from a certificate and an EC private key in PEM form, so a CA
// the device keeps across restarts is the one the trust store already holds. The key is accepted
// in PKCS#8 ("PRIVATE KEY") or SEC1 ("EC PRIVATE KEY") form; anything else is refused, because a
// key that is not EC cannot mint the P-256 leaves this package signs.
func NewCAFromPEM(certPEM, keyPEM []byte, now time.Time) (*CA, error) {
	cert, der, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA certificate: %w", err)
	}
	key, err := parseECKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA key: %w", err)
	}
	if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != key.Curve ||
		pub.X.Cmp(key.X) != 0 || pub.Y.Cmp(key.Y) != 0 {
		return nil, errors.New("tlsproxy: CA certificate and key do not match")
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{
		key:     key,
		cert:    cert,
		der:     der,
		pool:    pool,
		leaves:  map[string]*tls.Certificate{},
		leafTTL: LeafTTL,
	}, nil
}

// PEM returns the CA certificate as a PEM block. It is the public half and safe to install into
// the trust store.
func (c *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}

// KeyPEM returns the CA private key as a PKCS#8 PEM block. It is an interception capability: the
// caller stores it where only the service can read it, and nothing in this package writes it.
func (c *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(c.key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: marshalling CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// parseCertPEM parses exactly one PEM x509 certificate.
func parseCertPEM(data []byte) (*x509.Certificate, []byte, error) {
	block, err := singlePEMBlock(data)
	if err != nil {
		return nil, nil, err
	}
	if block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("PEM block is %q, not a certificate", block.Type)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("not a parseable x509 certificate: %w", err)
	}
	return cert, block.Bytes, nil
}

// parseECKeyPEM parses exactly one EC private key in PKCS#8 or SEC1 form.
func parseECKeyPEM(data []byte) (*ecdsa.PrivateKey, error) {
	block, err := singlePEMBlock(data)
	if err != nil {
		return nil, err
	}
	switch block.Type {
	case "PRIVATE KEY": // PKCS#8
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("PKCS#8 key: %w", err)
		}
		ec, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS#8 key is %T, not an EC key", k)
		}
		return ec, nil
	case "EC PRIVATE KEY": // SEC1
		return x509.ParseECPrivateKey(block.Bytes)
	default:
		return nil, fmt.Errorf("PEM block is %q, not an EC private key (PKCS#8 or SEC1)", block.Type)
	}
}

// singlePEMBlock requires exactly one PEM block and no trailing data, because a CA key or
// certificate handed to the proxy must be unambiguous about which one it is.
func singlePEMBlock(data []byte) (*pem.Block, error) {
	block, rest := pem.Decode(data)
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("multiple PEM blocks or trailing data; exactly one block is required")
	}
	return block, nil
}
