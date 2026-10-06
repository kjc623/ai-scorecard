package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

// smokeConfig is what the smoke needs to know about the lab it runs in.
type smokeConfig struct {
	Edge          string            // the device ingress, https://edge:8443 on the lab network
	CAPEM         string            // the device CA, which also signs the edge's certificate
	Tenant        string            // the tenant the deployment key belongs to
	DeploymentKey string            // that tenant's deployment key
	PolicyKey     ed25519.PublicKey // the policy key devices pin
	PolicyKeyID   string
	Dashboard     string            // the dashboard's public URL, as a browser uses it
	Resolve       map[string]string // public host:port -> lab-network host:port, like curl --resolve
	Analyst       string            // an analyst account of the tenant at the lab identity provider
	Wait          time.Duration     // how long the services may take to become ready
}

// readiness is every service's readiness probe, by its address on the lab network.
var readiness = []struct{ name, url string }{
	{"control-api", "http://control-api:8080/readyz"},
	{"ingest-api", "http://ingest-api:8080/readyz"},
	{"content-vault", "http://content-vault:8080/readyz"},
	{"query-api", "http://query-api:8080/readyz"},
	{"dashboard", "http://dashboard:8080/readyz"},
	{"oidc", "http://oidc:8080/healthz"},
}

// checker prints one line per assertion and counts the failures.
type checker struct{ failures int }

func (c *checker) check(name string, ok bool, detail string) bool {
	mark := "  ok  "
	if !ok {
		mark = " FAIL "
		c.failures++
	}
	if detail != "" {
		name += " - " + detail
	}
	fmt.Println(mark + name)
	return ok
}

func runSmoke(cfg smokeConfig) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(cfg.CAPEM)) {
		return errors.New("the CA PEM holds no certificate")
	}
	ca, _ := pem.Decode([]byte(cfg.CAPEM))
	caCert, err := x509.ParseCertificate(ca.Bytes)
	if err != nil {
		return fmt.Errorf("the CA PEM: %w", err)
	}
	c := &checker{}

	fmt.Println("readiness")
	for _, p := range readiness {
		status, detail := waitReady(p.url, cfg.Wait)
		c.check(p.name+" is ready", status == http.StatusOK, detail)
	}

	fmt.Println("\nthe edge serves the device API only")
	for _, path := range []string{"/admin/v1/deployment", "/internal/v1/auth/begin", "/.well-known/jwks.json"} {
		status, _, err := do(edgeClient(roots, nil), http.MethodGet, cfg.Edge+path, nil, nil)
		c.check("GET "+path+" is refused at the edge", err == nil && status == http.StatusForbidden, statusDetail(status, err))
	}

	fmt.Println("\na device enrols with the deployment key and sends an event")
	dev := enrolDevice(c, cfg, roots, caCert)
	if dev != nil {
		devClient := edgeClient(roots, &dev.pair)
		batch, eventID := eventBatch(cfg.Tenant, dev.id)
		status, body, err := do(devClient, http.MethodPost, cfg.Edge+"/v1/events", batch, nil)
		ok, detail := batchAccepted(status, body, err, eventID)
		c.check("POST /v1/events with the device certificate is accepted", ok, detail)

		status, body, err = do(devClient, http.MethodGet, cfg.Edge+"/v1/policy", nil, nil)
		c.check("GET /v1/policy returns a bundle signed with the pinned policy key",
			err == nil && status == http.StatusOK && policyVerifies(body, cfg.PolicyKey, cfg.PolicyKeyID), statusDetail(status, err))

		status, _, err = do(edgeClient(roots, nil), http.MethodPost, cfg.Edge+"/v1/events", batch, nil)
		c.check("POST /v1/events without a certificate is refused", err == nil && status == http.StatusUnauthorized, statusDetail(status, err))

		other, err := foreignCertificate(dev.id, cfg.Tenant)
		if err == nil {
			status, _, err = do(edgeClient(roots, &other), http.MethodPost, cfg.Edge+"/v1/events", batch, nil)
		}
		c.check("POST /v1/events with a certificate from another CA is refused", err == nil && status == http.StatusUnauthorized, statusDetail(status, err))
	}

	fmt.Println("\nan analyst signs in through the identity provider and queries")
	signInAndQuery(c, cfg, dev)

	if c.failures > 0 {
		return fmt.Errorf("%d check(s) failed", c.failures)
	}
	return nil
}

// waitReady polls a probe until it answers 200 or the wait is over.
func waitReady(address string, wait time.Duration) (int, string) {
	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(wait)
	for {
		res, err := client.Get(address)
		status := 0
		if err == nil {
			status = res.StatusCode
			res.Body.Close()
		}
		if status == http.StatusOK || time.Now().After(deadline) {
			return status, statusDetail(status, err)
		}
		time.Sleep(time.Second)
	}
}

type device struct {
	id   string
	pair tls.Certificate
}

// enrolDevice runs a first enrolment, then the refusals around it. The hardware identity is fixed,
// so every run re-enrols the same device rather than adding one.
func enrolDevice(c *checker, cfg smokeConfig, roots *x509.CertPool, ca *x509.Certificate) *device {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		c.check("generate a device key", false, err.Error())
		return nil
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "lab-smoke"}}, key)
	if err != nil {
		c.check("build a CSR", false, err.Error())
		return nil
	}
	hw := sha256.Sum256([]byte("lab-smoke-device"))
	request := func(deploymentKey string) []byte {
		b, _ := json.Marshal(protocol.EnrolmentRequest{
			SchemaVersion: protocol.EnrolmentSchemaVersion,
			DeploymentKey: deploymentKey,
			CSR:           string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
			Device: protocol.DeviceInfo{OS: "linux", AgentVersion: "lab-smoke", Hostname: "lab-smoke",
				ManagedState: string(protocol.ManagedStateUnknown), HardwareIdentityHash: "sha256:" + hex.EncodeToString(hw[:])},
		})
		return b
	}
	client := edgeClient(roots, nil)

	status, _, err := do(client, http.MethodPost, cfg.Edge+"/v1/enrol", request(""), nil)
	c.check("an enrolment without a deployment key or certificate is refused", err == nil && status == http.StatusUnauthorized, statusDetail(status, err))
	wrong := "sacdk_" + cfg.Tenant + "." + base64.RawURLEncoding.EncodeToString(randomBytes(32))
	status, _, err = do(client, http.MethodPost, cfg.Edge+"/v1/enrol", request(wrong), nil)
	c.check("an enrolment with an unknown deployment key is refused", err == nil && status == http.StatusUnauthorized, statusDetail(status, err))

	status, body, err := do(client, http.MethodPost, cfg.Edge+"/v1/enrol", request(cfg.DeploymentKey), nil)
	var resp protocol.EnrolmentResponse
	_ = json.Unmarshal(body, &resp)
	if !c.check("POST /v1/enrol with the deployment key issues a certificate",
		err == nil && status == http.StatusOK && resp.Credential.CertPEM != "" && resp.TenantID == cfg.Tenant,
		statusDetail(status, err)+", device "+resp.DeviceID) {
		return nil
	}
	block, _ := pem.Decode([]byte(resp.Credential.CertPEM))
	var leaf *x509.Certificate
	if block != nil {
		leaf, err = x509.ParseCertificate(block.Bytes)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	verified := leaf != nil && err == nil
	if verified {
		_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		verified = err == nil && leaf.Subject.CommonName == resp.DeviceID && slices.Contains(leaf.Subject.OrganizationalUnit, cfg.Tenant)
	}
	c.check("the certificate chains to the lab CA and names the device and tenant", verified, "")
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil
	}
	pair, err := tls.X509KeyPair([]byte(resp.Credential.CertPEM), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if err != nil {
		c.check("load the issued certificate", false, err.Error())
		return nil
	}
	return &device{id: resp.DeviceID, pair: pair}
}

// signInAndQuery signs the analyst in the way a browser does (the dashboard's sign-in, control-api,
// the identity provider, the callback) and then reads the device's events through the dashboard.
func signInAndQuery(c *checker, cfg smokeConfig, dev *device) {
	jar, _ := cookiejar.New(nil)
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	browser := &http.Client{Jar: jar, Timeout: 30 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if mapped, ok := cfg.Resolve[addr]; ok {
				addr = mapped
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}}

	start := cfg.Dashboard + "/signin/start?" + url.Values{"email": {cfg.Analyst}, "next": {"/session"}}.Encode()
	res, err := browser.Get(start)
	var session struct {
		Tenant string   `json:"tenant"`
		Actor  string   `json:"actor"`
		Roles  []string `json:"roles"`
	}
	where := ""
	if err == nil {
		where = res.Request.URL.Path
		_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&session)
		res.Body.Close()
	}
	if !c.check("the analyst signs in", err == nil && res.StatusCode == http.StatusOK && where == "/session" &&
		session.Tenant == cfg.Tenant && slices.Contains(session.Roles, "analyst"),
		fmt.Sprintf("%s, ended at %q as %q in %q", statusDetail(statusOf(res, err), err), where, session.Actor, session.Tenant)) {
		return
	}

	now := time.Now().UTC()
	params := map[string]any{"window": map[string]string{
		"from": now.Add(-time.Hour).Format(time.RFC3339), "to": now.Add(time.Hour).Format(time.RFC3339)}, "limit": 10}
	if dev != nil {
		params["device"] = dev.id
	}
	query, _ := json.Marshal(map[string]any{"query_version": "1", "template": "q8_activity", "params": params})
	status, body, err := do(browser, http.MethodPost, cfg.Dashboard+"/v1/query", query, map[string]string{"Origin": cfg.Dashboard})
	var answer struct {
		ResultState string            `json:"result_state"`
		Data        []json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(body, &answer)
	want := 0
	if dev != nil {
		want = 1
	}
	c.check("POST /v1/query with the session's product token returns the device's events",
		err == nil && status == http.StatusOK && answer.ResultState != "" && len(answer.Data) >= want,
		fmt.Sprintf("%s, result_state %q, %d row(s)", statusDetail(status, err), answer.ResultState, len(answer.Data)))
}

// edgeClient is a device's HTTPS client: it trusts the lab CA and presents pair when given one.
func edgeClient(roots *x509.CertPool, pair *tls.Certificate) *http.Client {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if pair != nil {
		cfg.GetClientCertificate = func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return pair, nil }
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}
}

func do(client *http.Client, method, address string, body []byte, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequest(method, address, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return res.StatusCode, b, err
}

func statusOf(res *http.Response, err error) int {
	if err != nil || res == nil {
		return 0
	}
	return res.StatusCode
}

func statusDetail(status int, err error) string {
	if err != nil {
		return err.Error()
	}
	return fmt.Sprintf("status %d", status)
}

// eventBatch is one batch holding one M1 prompt, as capture-core's TLS proxy reports a Claude Code
// request: a catalogued tool fingerprint, an empty label set and the default policy decision.
func eventBatch(tenant, deviceID string) ([]byte, string) {
	eventID := newUUID()
	digest := sha256.Sum256([]byte(eventID))
	now := time.Now().UTC()
	envelope := map[string]any{
		"schema_version":      "1.0",
		"event_id":            eventID,
		"tenant_id":           tenant,
		"device_id":           deviceID,
		"user_ref":            "lab-smoke",
		"tool_fingerprint":    "tls_b6681b043244c43f",
		"direction":           "egress",
		"kind":                "prompt",
		"occurred_at":         now.Add(-time.Second).Format(time.RFC3339),
		"monotonic_offset_ms": 1000,
		"source":              "proxy.tls",
		"collection_mode":     "m1",
		"size_bytes":          120,
		"confidence":          "high",
		"content_digest":      "sha256:" + hex.EncodeToString(digest[:]),
		"labels":              []any{},
		"classifier_version":  "lab-smoke",
		"prompt_kind":         "user",
		"policy_decision":     map[string]any{"rule_id": "policy.default", "action": "logged", "decided_locally": true},
		"dedup_key":           "sha256:" + hex.EncodeToString(digest[:]),
	}
	raw, _ := json.Marshal(envelope)
	batch, _ := json.Marshal(protocol.EventBatch{SchemaVersion: "1.0", BatchID: "lab-smoke-" + newUUID(),
		DeviceSentAt: now, EventCount: 1, Events: []json.RawMessage{raw}})
	return batch, eventID
}

func batchAccepted(status int, body []byte, err error, eventID string) (bool, string) {
	if err != nil || status != http.StatusOK {
		return false, statusDetail(status, err) + " " + strings.TrimSpace(string(body))
	}
	var resp protocol.EventBatchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return false, "unreadable response: " + err.Error()
	}
	if err := resp.Validate([]string{eventID}); err != nil {
		return false, "invalid response: " + err.Error()
	}
	detail := fmt.Sprintf("accepted %d, duplicate %d, rejected %d", resp.Counts.Accepted, resp.Counts.Duplicate, resp.Counts.Rejected)
	if resp.Counts.Rejected > 0 && len(resp.Results) > 0 {
		detail += fmt.Sprintf(" (%s)", resp.Results[0].Reason)
	}
	return resp.Counts.Accepted == 1, detail
}

// policyVerifies checks a GET /v1/policy body the way capture-core does: the signed envelope names
// the pinned key id, and the Ed25519 signature covers the payload bytes as received.
func policyVerifies(body []byte, pub ed25519.PublicKey, keyID string) bool {
	var resp protocol.PolicyResponse
	if json.Unmarshal(body, &resp) != nil {
		return false
	}
	var envelope struct {
		KeyID     string          `json:"key_id"`
		Algorithm string          `json:"algorithm"`
		Payload   json.RawMessage `json:"payload"`
		Signature string          `json:"signature"`
	}
	if json.Unmarshal(resp.SignedBundle, &envelope) != nil || envelope.KeyID != keyID || !strings.EqualFold(envelope.Algorithm, "ed25519") {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(envelope.Signature)
	return err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, envelope.Payload, sig)
}

// foreignCertificate is a client certificate naming the device and tenant, signed by a CA the lab
// does not trust.
func foreignCertificate(deviceID, tenant string) (tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	caTmpl := &x509.Certificate{SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: "Foreign CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true, IsCA: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafTmpl := &x509.Certificate{SerialNumber: randomSerial(),
		Subject:   pkix.Name{CommonName: deviceID, OrganizationalUnit: []string{tenant}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}

func newUUID() string {
	b := randomBytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
