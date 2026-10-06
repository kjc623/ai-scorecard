package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

func TestPKIMaterialFitsTheLab(t *testing.T) {
	var out bytes.Buffer
	if err := writePKI(&out); err != nil {
		t.Fatal(err)
	}
	var p labPKI
	if err := json.Unmarshal(out.Bytes(), &p); err != nil {
		t.Fatalf("pki output is not JSON: %v", err)
	}
	parse := func(text string) *x509.Certificate {
		block, _ := pem.Decode([]byte(text))
		if block == nil || block.Type != "CERTIFICATE" {
			t.Fatalf("not a certificate PEM: %q", text)
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	ca, edge := parse(p.CACert), parse(p.EdgeCert)
	if !ca.IsCA {
		t.Fatal("the CA certificate is not a CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	for _, name := range []string{"edge", "127.0.0.1", "localhost"} {
		if _, err := edge.Verify(x509.VerifyOptions{Roots: pool, DNSName: name}); err != nil {
			t.Fatalf("the edge certificate does not verify as %s under the CA: %v", name, err)
		}
	}
	for _, key := range []string{p.CAKey, p.EdgeKey} {
		block, _ := pem.Decode([]byte(key))
		if block == nil || block.Type != "EC PRIVATE KEY" {
			t.Fatalf("a key is not an EC PRIVATE KEY PEM")
		}
		if _, err := x509.ParseECPrivateKey(block.Bytes); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSmokeConfiguration(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	env := map[string]string{"LAB_CA_CERT_PEM": "pem", "LAB_DEPLOYMENT_KEY": " sacdk_x \n"}
	cfg, err := smokeConfigFrom([]string{"-tenant", "t", "-analyst", "a@b.test", "-policy-key", hex.EncodeToString(pub),
		"-resolve", "127.0.0.1:8787=dashboard:8080, 127.0.0.1:8790=oidc:8080", "-dashboard", "http://127.0.0.1:8787/"},
		func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DeploymentKey != "sacdk_x" || cfg.Dashboard != "http://127.0.0.1:8787" || cfg.Resolve["127.0.0.1:8790"] != "oidc:8080" ||
		len(cfg.Resolve) != 2 || !bytes.Equal(cfg.PolicyKey, pub) || cfg.Wait != 90*time.Second {
		t.Fatalf("configuration is wrong: %+v", cfg)
	}
	if _, err := smokeConfigFrom([]string{"-policy-key", "zz", "-resolve", "nope"}, func(string) string { return "" }); err == nil ||
		!strings.Contains(err.Error(), "LAB_CA_CERT_PEM") || !strings.Contains(err.Error(), "-resolve") {
		t.Fatalf("an incomplete configuration must name every problem: %v", err)
	}

	// The policy check accepts exactly the envelope capture-core verifies.
	payload := []byte(`{"version":"7"}`)
	envelope, _ := json.Marshal(map[string]any{"key_id": "policy-key-1", "algorithm": "ed25519",
		"payload": json.RawMessage(payload), "signature": base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))})
	body, _ := json.Marshal(map[string]any{"schema_version": "1.0", "bundle_version": "7", "signed_bundle": json.RawMessage(envelope)})
	if !policyVerifies(body, pub, "policy-key-1") {
		t.Fatal("a correctly signed bundle must verify")
	}
	if policyVerifies(body, pub, "policy-key-2") {
		t.Fatal("a bundle naming another key id must not verify")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if policyVerifies(body, other, "policy-key-1") {
		t.Fatal("a bundle signed with another key must not verify")
	}
}

func TestEventBatchIsOneM1Prompt(t *testing.T) {
	raw, eventID := eventBatch("10ca1ab0-0000-4000-8000-000000000001", "22222222-2222-4222-8222-222222222222")
	var batch struct {
		EventCount int               `json:"event_count"`
		Events     []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &batch); err != nil || batch.EventCount != 1 || len(batch.Events) != 1 {
		t.Fatalf("batch: %s %v", raw, err)
	}
	var e map[string]any
	_ = json.Unmarshal(batch.Events[0], &e)
	if e["event_id"] != eventID || e["collection_mode"] != "m1" || e["kind"] != "prompt" ||
		!strings.HasPrefix(e["tool_fingerprint"].(string), "tls_") || e["labels"] == nil {
		t.Fatalf("envelope: %v", e)
	}
}
