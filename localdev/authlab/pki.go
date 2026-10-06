package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"time"
)

// labPKI is the lab's key material: the device CA, which control-api signs device certificates
// with and ingest-api and control-api verify them against, and the edge's TLS server certificate,
// which the CA also signs so a device trusts the edge through the one CA file it is given.
type labPKI struct {
	CACert   string `json:"ca_cert"`
	CAKey    string `json:"ca_key"`
	EdgeCert string `json:"edge_cert"`
	EdgeKey  string `json:"edge_key"`
}

// pkiValidity is long because the lab keeps its CA: devices installed against it hold
// certificates it signed.
const pkiValidity = 5 * 365 * 24 * time.Hour

// writePKI generates fresh material and writes it to w as JSON. It never touches a file: the
// caller decides whether material already exists and must be kept.
func writePKI(w io.Writer) error {
	p, err := newPKI(time.Now())
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(p)
}

func newPKI(now time.Time) (labPKI, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return labPKI{}, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "Shadow AI Capture Lab CA", Organization: []string{"Shadow AI Capture Lab"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(pkiValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return labPKI{}, fmt.Errorf("create the CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return labPKI{}, err
	}

	edgeKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return labPKI{}, err
	}
	edgeTmpl := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "edge", Organization: []string{"Shadow AI Capture Lab"}},
		// A device on the host reaches the edge at 127.0.0.1; a container reaches it as edge.
		DNSNames:    []string{"edge", "localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
		NotBefore:   now.Add(-time.Hour),
		NotAfter:    now.Add(pkiValidity),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	edgeDER, err := x509.CreateCertificate(rand.Reader, edgeTmpl, ca, &edgeKey.PublicKey, caKey)
	if err != nil {
		return labPKI{}, fmt.Errorf("create the edge certificate: %w", err)
	}
	caKeyPEM, err := keyPEM(caKey)
	if err != nil {
		return labPKI{}, err
	}
	edgeKeyPEM, err := keyPEM(edgeKey)
	if err != nil {
		return labPKI{}, err
	}
	return labPKI{
		CACert:   certPEM(caDER),
		CAKey:    caKeyPEM,
		EdgeCert: certPEM(edgeDER),
		EdgeKey:  edgeKeyPEM,
	}, nil
}

func certPEM(der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func keyPEM(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})), nil
}

func randomSerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		panic(err)
	}
	return n
}
