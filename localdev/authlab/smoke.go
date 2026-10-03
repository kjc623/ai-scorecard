package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// smokeConfig is the resolved input to the end-to-end check.
type smokeConfig struct {
	EdgeURL   string
	PKIDir    string
	Tenant    string
	TokenX509 string
	TokenDPoP string
}

// checker prints one line per assertion and counts failures, so the output is the evidence.
type checker struct{ failures int }

func (c *checker) check(name string, ok bool, detail string) {
	mark := "  ok  "
	if !ok {
		mark = " FAIL "
		c.failures++
	}
	if detail == "" {
		fmt.Printf("%s %s\n", mark, name)
		return
	}
	fmt.Printf("%s %s — %s\n", mark, name, detail)
}

// runSmoke drives both production modes through the edge and then the negatives.
func runSmoke(cfg smokeConfig) error {
	edge := strings.TrimRight(cfg.EdgeURL, "/")
	root, err := loadRootPool(cfg.PKIDir)
	if err != nil {
		return err
	}
	c := &checker{}

	fmt.Println("authlab: x509 mode — enrol through the edge, then ingest with the issued leaf")
	x509Flow, err := flowX509(c, root, edge, cfg.Tenant, cfg.TokenX509)
	if err != nil {
		return err
	}

	fmt.Println("\nauthlab: dpop mode — enrol, obtain a token, then ingest with DPoP")
	dpop, err := flowDPoP(c, root, edge, cfg.Tenant, cfg.TokenDPoP)
	if err != nil {
		return err
	}

	fmt.Println("\nauthlab: the negatives")
	negativeNoCredential(c, root, edge, cfg.Tenant, x509Flow.DeviceID)
	negativeReplay(c, root, edge, cfg.Tenant, dpop)
	negativeOtherCA(c, root, edge, cfg.Tenant, x509Flow.DeviceID)

	if c.failures > 0 {
		return fmt.Errorf("%d auth-lab check(s) failed", c.failures)
	}
	fmt.Println("\nauthlab: all auth-lab checks passed")
	return nil
}

type x509Result struct {
	DeviceID string
}

type dpopResult struct {
	DeviceID    string
	AccessToken string
	// Proof is the exact DPoP proof that authenticated the successful /v1/events request, so the
	// replay negative can present the same jti a second time.
	Proof   string
	LastHTU string
}

// flowX509 proves ADR 0020's x509 path end to end: a PKCS#10 CSR and a bootstrap token go to
// /v1/enrol through the edge; the issued leaf is then presented to /v1/events through the edge,
// where the edge forwards it in X-Client-Cert and the origin re-verifies it against the dev CA.
func flowX509(c *checker, root *x509.CertPool, edge, tenant, token string) (x509Result, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return x509Result{}, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "authlab-x509"},
	}, key)
	if err != nil {
		return x509Result{}, err
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	enrolBody, _ := json.Marshal(protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: token,
		Mode:           protocol.AuthModeX509,
		CSR:            csrPEM,
		Device: protocol.DeviceInfo{
			OS: "windows", AgentVersion: "authlab", HardwareIdentityHash: "authlab-x509",
		},
	})
	status, body, err := postJSON(baseClient(root), edge+"/v1/enrol", enrolBody, nil)
	if err != nil {
		return x509Result{}, err
	}
	var enrol protocol.EnrolmentResponse
	_ = json.Unmarshal(body, &enrol)
	c.check("x509 enrol through the edge returns an issued leaf",
		status == http.StatusOK && enrol.Credential.Mode == protocol.AuthModeX509 && enrol.Credential.CertPEM != "",
		fmt.Sprintf("status %d, mode %q, cert %t", status, enrol.Credential.Mode, enrol.Credential.CertPEM != ""))
	if status != http.StatusOK || enrol.Credential.CertPEM == "" {
		return x509Result{}, nil
	}

	leafKey, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return x509Result{}, err
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKey})
	pair, err := tls.X509KeyPair([]byte(enrol.Credential.CertPEM), leafPEM)
	if err != nil {
		return x509Result{}, fmt.Errorf("build the issued client key pair: %w", err)
	}

	batch, eventID := buildBatch(tenant, enrol.DeviceID)
	status, body, err = postJSON(clientWithCert(root, pair), edge+"/v1/events", batch, nil)
	if err != nil {
		return x509Result{}, err
	}
	ok, detail := batchAccepted(status, body, eventID)
	c.check("x509 POST /v1/events with the issued leaf is accepted", ok, detail)
	return x509Result{DeviceID: enrol.DeviceID}, nil
}

// flowDPoP proves ADR 0020's dpop path end to end: a public JWK plus a proof of possession enrols,
// the registered device key signs a token assertion, and the issued DPoP-bound access token plus a
// per-request proof is accepted by ingest-api.
func flowDPoP(c *checker, root *x509.CertPool, edge, tenant, token string) (dpopResult, error) {
	deviceKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return dpopResult{}, err
	}
	jwk := jwkFromPublic(&deviceKey.PublicKey)

	enrolProof, err := dpopProof(deviceKey, http.MethodPost, edge+"/v1/enrol", "")
	if err != nil {
		return dpopResult{}, err
	}
	enrolBody, _ := json.Marshal(protocol.EnrolmentRequest{
		SchemaVersion:  protocol.EnrolmentSchemaVersion,
		EnrolmentToken: token,
		Mode:           protocol.AuthModeDPoP,
		JWK:            &protocol.JWK{Kty: jwk.Kty, Crv: jwk.Crv, X: jwk.X, Y: jwk.Y},
		Device: protocol.DeviceInfo{
			OS: "windows", AgentVersion: "authlab", HardwareIdentityHash: "authlab-dpop",
		},
	})
	status, body, err := postJSON(baseClient(root), edge+"/v1/enrol", enrolBody, map[string]string{"DPoP": enrolProof})
	if err != nil {
		return dpopResult{}, err
	}
	var enrol protocol.EnrolmentResponse
	_ = json.Unmarshal(body, &enrol)
	c.check("dpop enrol through the edge registers the public key",
		status == http.StatusOK && enrol.Credential.Mode == protocol.AuthModeDPoP && enrol.Credential.JWK != nil,
		fmt.Sprintf("status %d, mode %q, jwk %t", status, enrol.Credential.Mode, enrol.Credential.JWK != nil))
	if status != http.StatusOK || enrol.DeviceID == "" {
		return dpopResult{}, nil
	}

	assertion, err := tokenAssertion(deviceKey, enrol.DeviceID, tenant)
	if err != nil {
		return dpopResult{}, err
	}
	tokenProof, err := dpopProof(deviceKey, http.MethodPost, edge+"/v1/token", "")
	if err != nil {
		return dpopResult{}, err
	}
	tokenBody, _ := json.Marshal(protocol.TokenRequest{
		GrantType: "urn:ietf:params:oauth:grant-type:jwt-bearer",
		Assertion: assertion,
		DeviceID:  enrol.DeviceID,
	})
	status, body, err = postJSON(baseClient(root), edge+"/v1/token", tokenBody, map[string]string{"DPoP": tokenProof})
	if err != nil {
		return dpopResult{}, err
	}
	var issued protocol.TokenResponse
	_ = json.Unmarshal(body, &issued)
	c.check("dpop POST /v1/token issues a DPoP-bound access token",
		status == http.StatusOK && issued.Validate() == nil,
		fmt.Sprintf("status %d, token_type %q, expires_in %d", status, issued.TokenType, issued.ExpiresIn))
	if status != http.StatusOK || issued.AccessToken == "" {
		return dpopResult{}, nil
	}

	eventsProof, err := dpopProof(deviceKey, http.MethodPost, edge+"/v1/events", athOf(issued.AccessToken))
	if err != nil {
		return dpopResult{}, err
	}
	batch, eventID := buildBatch(tenant, enrol.DeviceID)
	status, body, err = postJSON(baseClient(root), edge+"/v1/events", batch, map[string]string{
		"Authorization": "DPoP " + issued.AccessToken,
		"DPoP":          eventsProof,
	})
	if err != nil {
		return dpopResult{}, err
	}
	ok, detail := batchAccepted(status, body, eventID)
	c.check("dpop POST /v1/events with the access token and proof is accepted", ok, detail)
	return dpopResult{DeviceID: enrol.DeviceID, AccessToken: issued.AccessToken, Proof: eventsProof, LastHTU: edge + "/v1/events"}, nil
}

// negativeNoCredential proves the origin refuses a request that presents nothing, even through the
// edge: no client certificate and no DPoP token.
func negativeNoCredential(c *checker, root *x509.CertPool, edge, tenant, device string) {
	batch, _ := buildBatch(tenant, device)
	status, _, err := postJSON(baseClient(root), edge+"/v1/events", batch, nil)
	c.check("no credential through the edge is refused", err == nil && status == http.StatusUnauthorized,
		fmt.Sprintf("status %d", status))
}

// negativeReplay proves ops.dpop_replay does its job: the exact proof that just authenticated one
// request is refused when it is presented again with a different body.
func negativeReplay(c *checker, root *x509.CertPool, edge, tenant string, dpop dpopResult) {
	if dpop.AccessToken == "" || dpop.Proof == "" {
		c.check("a replayed DPoP jti is refused", false, "the dpop flow did not get far enough to replay")
		return
	}
	// The body is irrelevant to authentication; keep it well-formed so the failure is the replay.
	batch, _ := buildBatch(tenant, dpop.DeviceID)
	status, _, err := postJSON(baseClient(root), edge+"/v1/events", batch, map[string]string{
		"Authorization": "DPoP " + dpop.AccessToken,
		"DPoP":          dpop.Proof,
	})
	c.check("a replayed DPoP jti is refused", err == nil && status == http.StatusUnauthorized,
		fmt.Sprintf("status %d", status))
}

// negativeOtherCA proves the edge is a filter and the origin is the authority: a certificate the
// configured CA did not sign is forwarded and then refused by ingest-api, not trusted by the edge.
func negativeOtherCA(c *checker, root *x509.CertPool, edge, tenant, device string) {
	leaf, err := throwawayClientCert()
	if err != nil {
		c.check("a certificate from another CA is refused", false, err.Error())
		return
	}
	batch, _ := buildBatch(tenant, device)
	status, _, err := postJSON(clientWithCert(root, leaf), edge+"/v1/events", batch, nil)
	c.check("a certificate from another CA is refused", err == nil && status == http.StatusUnauthorized,
		fmt.Sprintf("status %d", status))
}

// throwawayClientCert generates a CA and a client leaf that the lab's trust bundle did not sign.
func throwawayClientCert() (tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          mustSerial(),
		Subject:               pkix.Name{CommonName: "Auth Lab Throwaway CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: mustSerial(),
		Subject:      pkix.Name{CommonName: newUUID(), OrganizationalUnit: []string{newUUID()}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}, nil
}

func mustSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err)
	}
	return n
}

// --- shared transport helpers ---------------------------------------------------------------

func loadRootPool(dir string) (*x509.CertPool, error) {
	pemBytes, err := os.ReadFile(filepath.Join(dir, fileCACert))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (run `authlab pki` first)", fileCACert, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("%s carries no certificates", fileCACert)
	}
	return pool, nil
}

func baseClient(root *x509.CertPool) *http.Client {
	return &http.Client{
		Timeout:   20 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: root}},
	}
}

// clientWithCert sends one client certificate. GetClientCertificate is set so the leaf is presented
// even if the edge's advertised CA names would otherwise make Go skip it.
func clientWithCert(root *x509.CertPool, pair tls.Certificate) *http.Client {
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      root,
		Certificates: []tls.Certificate{pair},
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &pair, nil
		},
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

func postJSON(client *http.Client, url string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, b, nil
}

// buildBatch makes one valid §5.3 batch. The digests are fresh per call so a rerun is accepted
// rather than reported as a duplicate of an earlier run's submission.
func buildBatch(tenant, device string) ([]byte, string) {
	eventID := newUUID()
	sum := sha256.Sum256([]byte(eventID))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	envelope := map[string]any{
		"schema_version":      "1.0",
		"event_id":            eventID,
		"tenant_id":           tenant,
		"device_id":           device,
		"user_ref":            "user-1",
		"tool_fingerprint":    "chat-tool",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		"monotonic_offset_ms": 5,
		"source":              "ext.web_request",
		"collection_mode":     "m1",
		"size_bytes":          120,
		"policy_decision":     map[string]any{"rule_id": "RULE_1", "action": "logged", "decided_locally": true},
		"dedup_key":           digest,
		"confidence":          "high",
		"content_digest":      digest,
		"labels":              []any{},
		"classifier_version":  "2026.01.0-shadow",
	}
	raw, _ := json.Marshal(envelope)
	batch := protocol.EventBatch{
		SchemaVersion: "1.0",
		BatchID:       "authlab-" + newUUID(),
		DeviceSentAt:  time.Now().UTC(),
		EventCount:    1,
		Events:        []json.RawMessage{raw},
	}
	out, _ := json.Marshal(batch)
	return out, eventID
}

// batchAccepted reports whether a 200 response carries the event as accepted or duplicate (never
// rejected), and validates the response against the protocol the device is about to trust.
func batchAccepted(status int, body []byte, eventID string) (bool, string) {
	if status != http.StatusOK {
		return false, fmt.Sprintf("status %d", status)
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, fmt.Sprintf("unreadable response: %v", err)
	}
	if err := resp.Validate([]string{eventID}); err != nil {
		return false, fmt.Sprintf("invalid response: %v", err)
	}
	return resp.Counts.Accepted+resp.Counts.Duplicate == 1 && resp.Counts.Rejected == 0,
		fmt.Sprintf("accepted %d, duplicate %d, rejected %d", resp.Counts.Accepted, resp.Counts.Duplicate, resp.Counts.Rejected)
}
