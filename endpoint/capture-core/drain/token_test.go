package drain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/shadow-ai-capture/device/protocol"
)

func TestTokenFlow(t *testing.T) {
	var got protocol.TokenRequest
	d, _ := newRawDrainer(t, Config{
		AuthMode:     protocol.AuthModeDPoP,
		TenantID:     "tenant-1",
		DeviceID:     "device-1",
		AgentVersion: "test",
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(protocol.HeaderDPoP) == "" {
			t.Error("token request carried no DPoP proof")
		}
		body := gunzipBody(t, r)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("decode token request: %v", err)
		}
		resp := protocol.TokenResponse{AccessToken: "at-1", TokenType: protocol.TokenTypeDPoP, ExpiresIn: 900, ServerTime: time.Now().UTC()}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tok, exp, err := d.fetchToken(context.Background(), key, "device-1", "tenant-1")
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	if tok != "at-1" {
		t.Fatalf("token = %q, want at-1", tok)
	}
	if exp.Before(time.Now()) {
		t.Fatal("token expiry is in the past")
	}
	if got.GrantType != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		t.Fatalf("grant_type = %q", got.GrantType)
	}
	if got.Assertion == "" {
		t.Fatal("token request carried no assertion")
	}
	if got.DeviceID != "device-1" {
		t.Fatalf("device_id = %q, want device-1", got.DeviceID)
	}
}

func TestTokenRejectsNonDPoP(t *testing.T) {
	d, _ := newRawDrainer(t, Config{
		AuthMode:     protocol.AuthModeDPoP,
		TenantID:     "tenant-1",
		DeviceID:     "device-1",
		AgentVersion: "test",
	}, func(w http.ResponseWriter, r *http.Request) {
		resp := protocol.TokenResponse{AccessToken: "at-1", TokenType: "Bearer", ExpiresIn: 900, ServerTime: time.Now().UTC()}
		raw, _ := json.Marshal(resp)
		writeJSON(t, w, http.StatusOK, raw)
	})
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, _, err := d.fetchToken(context.Background(), key, "device-1", "tenant-1"); err == nil {
		t.Fatal("a bearer token response was accepted as a DPoP token")
	}
}
