package main

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

func env(overrides map[string]string) func(string) string {
	base := map[string]string{
		EnvContentKeys: "v2:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)) +
			",v1:" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32)),
		EnvAuthIssuer: "https://control-api.internal.example",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.addr != "127.0.0.1:8080" || c.keys.Current() != "v2" || c.auth.Issuer != "https://control-api.internal.example" ||
		c.auth.Audience != "" || c.retrievalURLBase != "" {
		t.Fatalf("config = %+v", c)
	}
}

func TestTheKeyringIsRequiredAndValidated(t *testing.T) {
	short := base64.StdEncoding.EncodeToString(make([]byte, 16))
	for name, spec := range map[string]string{
		"missing":   "",
		"short key": "v1:" + short,
		"duplicate": "v1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)) + ",v1:" + base64.StdEncoding.EncodeToString(make([]byte, 32)),
		"malformed": "not-a-keyring",
	} {
		_, err := loadConfig(env(map[string]string{EnvContentKeys: spec}))
		if err == nil || !strings.Contains(err.Error(), EnvContentKeys) {
			t.Errorf("%s: loadConfig = %v, want a refusal naming %s", name, err, EnvContentKeys)
		}
	}
}

func TestTheIssuerIsRequired(t *testing.T) {
	if _, err := loadConfig(env(map[string]string{EnvAuthIssuer: ""})); err == nil || !strings.Contains(err.Error(), EnvAuthIssuer) {
		t.Fatalf("loadConfig without an issuer = %v", err)
	}
}

func TestURLsAndAddressAreValidated(t *testing.T) {
	for name, overrides := range map[string]map[string]string{
		"relative issuer":    {EnvAuthIssuer: "control-api"},
		"ftp jwks":           {EnvAuthJWKSURL: "ftp://control-api/jwks"},
		"relative base":      {EnvRetrievalURLBase: "/analyst"},
		"address no port":    {EnvHTTPAddr: "0.0.0.0"},
		"address bad port":   {EnvHTTPAddr: "0.0.0.0:http"},
		"address port range": {EnvHTTPAddr: ":70000"},
	} {
		if _, err := loadConfig(env(overrides)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	c, err := loadConfig(env(map[string]string{EnvHTTPAddr: "0.0.0.0:8080", EnvRetrievalURLBase: "https://analyst.example", EnvAuthAudience: "sac-vault"}))
	if err != nil || c.addr != "0.0.0.0:8080" || c.retrievalURLBase != "https://analyst.example" || c.auth.Audience != "sac-vault" {
		t.Fatalf("loadConfig = %+v, %v", c, err)
	}
}
