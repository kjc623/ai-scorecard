package tlsproxy

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/state"
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
	key  crypto.Signer // in the platform keystore where there is one; it never leaves it
	cert *x509.Certificate
	der  []byte
	pool *x509.CertPool

	mu      sync.Mutex
	leaves  map[string]cachedLeaf
	leafTTL time.Duration
}

// cachedLeaf is a minted leaf and the window it is valid in.
type cachedLeaf struct {
	cert                *tls.Certificate
	notBefore, notAfter time.Time
}

// LeafTTL bounds a minted leaf's life. Short-lived is the point: a leaf that outlives the
// process's need for it is an interception capability lying around.
const LeafTTL = 12 * time.Hour

// leafRenewal is how much of a cached leaf's life must remain for it to be presented again; a leaf
// closer to its end is replaced, so no client is shown an expired or nearly expired certificate,
// which it would refuse as if it pinned.
const leafRenewal = time.Hour

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
	return mintCA(key, deviceID, now, validity)
}

// mintCA self-signs a per-device root over key.
func mintCA(key crypto.Signer, deviceID string, now time.Time, validity time.Duration) (*CA, error) {
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: self-signing device CA: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: parsing device CA: %w", err)
	}
	return newCA(cert, der, key), nil
}

func newCA(cert *x509.Certificate, der []byte, key crypto.Signer) *CA {
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &CA{key: key, cert: cert, der: der, pool: pool, leaves: map[string]cachedLeaf{}, leafTTL: LeafTTL}
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
// written to disk, and the cache is keyed by host so a busy destination costs one mint per
// leaf lifetime: a cached leaf is replaced once now is outside its validity or within leafRenewal
// of its end.
func (c *CA) Leaf(host string, now time.Time) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leaves[host]; ok && !now.Before(l.notBefore) && now.Add(leafRenewal).Before(l.notAfter) {
		return l.cert, nil
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
	c.leaves[host] = cachedLeaf{cert: cert, notBefore: tmpl.NotBefore, notAfter: tmpl.NotAfter}
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
	key, err := parseECKeyPEM(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA key: %w", err)
	}
	return caWithKey(certPEM, key)
}

// caWithKey pairs a PEM certificate with the key that signs for it.
func caWithKey(certPEM []byte, key crypto.Signer) (*CA, error) {
	cert, der, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: CA certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(key.Public()) {
		return nil, errors.New("tlsproxy: CA certificate and key do not match")
	}
	return newCA(cert, der, key), nil
}

// PEM returns the CA certificate as a PEM block. It is the public half and safe to install into
// the trust store.
func (c *CA) PEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.der})
}

// encodeKeyPEM is a file-kept CA key as a PKCS#8 PEM block.
func encodeKeyPEM(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: marshalling CA key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// The device root kept across restarts. Its certificate is a file in the state directory; its
// private key is in the platform keystore (caKeyStore), or a file beside the certificate where the
// platform has none wired in.
const (
	deviceCACertFile = "ca.pem"
	deviceCAKeyFile  = "ca.key"

	// deviceCAValidity is the root's life. It is installed once and reused across restarts.
	deviceCAValidity = 2 * 365 * 24 * time.Hour
	// deviceCARenewBefore is how close to expiry a start replaces the root. Renewal happens only
	// at start; an expired root would break every intercepted connection.
	deviceCARenewBefore = 60 * 24 * time.Hour
)

// caKeyStore keeps the device root's private key.
type caKeyStore interface {
	// Key returns the kept key. An error means there is no usable one.
	Key() (crypto.Signer, error)
	// Generate replaces the kept key with a new one.
	Generate() (crypto.Signer, error)
}

// RootTrust is the trust store as the device root's replacement uses it. The store removes only
// the root it last installed, so a root an earlier version left is adopted by installing it again
// (installation is idempotent) and then removed.
type RootTrust interface {
	Install(ctx context.Context, certDER []byte) error
	Verify(ctx context.Context, certDER []byte) (bool, error)
	Remove(ctx context.Context) error
}

// OpenDeviceCA returns the device root kept in dir, minting a new one when there is none, when it
// is unusable, or when it is near expiry; created reports a new root. Installing the root in the
// trust store is the proxy's Start, and toggling the proxy leaves the key where it is. A root
// whose key an earlier version kept as a file in dir, on a platform that now keeps it in the
// keystore, is replaced, and the old root is removed from trust.
func OpenDeviceCA(ctx context.Context, dir, label string, now time.Time, trust RootTrust) (ca *CA, created bool, err error) {
	keys, err := platformKeyStore(dir)
	if err != nil {
		return nil, false, err
	}
	return openDeviceCA(ctx, keys, dir, label, now, trust)
}

func openDeviceCA(ctx context.Context, keys caKeyStore, dir, label string, now time.Time, trust RootTrust) (*CA, bool, error) {
	if _, fileKept := keys.(fileKeyStore); !fileKept {
		if _, err := os.Lstat(filepath.Join(dir, deviceCAKeyFile)); err == nil {
			ca, err := replaceFileKeyRoot(ctx, keys, dir, label, now, trust)
			return ca, err == nil, err
		}
	}
	if ca, err := loadDeviceCA(keys, dir, now); err == nil {
		return ca, false, nil
	}
	ca, err := mintDeviceCA(keys, dir, label, now)
	return ca, err == nil, err
}

// loadDeviceCA returns the kept root, or why it cannot be reused.
func loadDeviceCA(keys caKeyStore, dir string, now time.Time) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, deviceCACertFile))
	if err != nil {
		return nil, err
	}
	key, err := keys.Key()
	if err != nil {
		return nil, err
	}
	ca, err := caWithKey(certPEM, key)
	if err != nil {
		return nil, err
	}
	if now.Add(deviceCARenewBefore).After(ca.cert.NotAfter) {
		return nil, errors.New("tlsproxy: device CA near expiry")
	}
	return ca, nil
}

// mintDeviceCA replaces the kept root. The key is replaced first: a crash before the certificate
// is written leaves a certificate whose key does not match, which the next start replaces.
func mintDeviceCA(keys caKeyStore, dir, label string, now time.Time) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tlsproxy: device CA directory: %w", err)
	}
	key, err := keys.Generate()
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: device CA key: %w", err)
	}
	ca, err := mintCA(key, label, now, deviceCAValidity)
	if err != nil {
		return nil, err
	}
	if err := state.WriteFile(filepath.Join(dir, deviceCACertFile), ca.PEM()); err != nil {
		return nil, fmt.Errorf("tlsproxy: device CA certificate: %w", err)
	}
	return ca, nil
}

// replaceFileKeyRoot replaces a root whose key is a file in dir: it mints a new root in keys,
// removes the old root from the trust store, deletes the key file, then writes the new
// certificate. Until the key file is gone, every start tries again from the old certificate.
func replaceFileKeyRoot(ctx context.Context, keys caKeyStore, dir, label string, now time.Time, trust RootTrust) (*CA, error) {
	key, err := keys.Generate()
	if err != nil {
		return nil, fmt.Errorf("tlsproxy: device CA key: %w", err)
	}
	ca, err := mintCA(key, label, now, deviceCAValidity)
	if err != nil {
		return nil, err
	}
	certPath := filepath.Join(dir, deviceCACertFile)
	if old, err := os.ReadFile(certPath); err == nil {
		if err := retireRoot(ctx, trust, old); err != nil {
			return nil, fmt.Errorf("tlsproxy: removing the file-key device CA from the trust store: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(dir, deviceCAKeyFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("tlsproxy: deleting the device CA key file: %w", err)
	}
	if err := state.WriteFile(certPath, ca.PEM()); err != nil {
		return nil, fmt.Errorf("tlsproxy: device CA certificate: %w", err)
	}
	return ca, nil
}

// retireRoot removes the root in certPEM from the trust store when the store holds it.
func retireRoot(ctx context.Context, trust RootTrust, certPEM []byte) error {
	_, der, err := parseCertPEM(certPEM)
	if err != nil {
		return nil // not a certificate, so not one the store can hold
	}
	present, err := trust.Verify(ctx, der)
	if err != nil || !present {
		return err
	}
	if err := trust.Install(ctx, der); err != nil {
		return err
	}
	return trust.Remove(ctx)
}

// RetireDeviceRoot takes the device root kept in dir out of the trust store, when the store holds
// it. Without a kept root there is nothing to take out.
func RetireDeviceRoot(ctx context.Context, dir string, trust RootTrust) error {
	certPEM, err := os.ReadFile(filepath.Join(dir, deviceCACertFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("tlsproxy: reading the device CA certificate: %w", err)
	}
	if err := retireRoot(ctx, trust, certPEM); err != nil {
		return fmt.Errorf("tlsproxy: removing the device CA from the trust store: %w", err)
	}
	return nil
}

// DeleteDeviceKey deletes the device root's private key from the platform keystore (or the key
// file in dir where the platform has none). A key that does not exist is already deleted.
func DeleteDeviceKey(dir string) error { return deletePlatformKey(dir) }

// fileKeyStore keeps the key as a PEM file in the protected state directory.
type fileKeyStore struct{ path string }

func (s fileKeyStore) Key() (crypto.Signer, error) {
	// A key file another account can read, or could have planted, is not this device's secret.
	if err := state.CheckFile(s.path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	key, err := parseECKeyPEM(data)
	if err != nil {
		return nil, err
	}
	return key, nil
}

func (s fileKeyStore) Generate() (crypto.Signer, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKeyPEM(key)
	if err != nil {
		return nil, err
	}
	if err := state.WriteFile(s.path, keyPEM); err != nil {
		return nil, err
	}
	return key, nil
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
