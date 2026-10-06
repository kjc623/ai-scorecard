package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func caPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{EnvHTTPAddr, EnvRegion, EnvCACertPEM, "SAC_PG_HOST", "SAC_PG_PORT", "SAC_PG_DATABASE", "SAC_PG_USER", "SAC_PG_PASSWORD", "SAC_PG_SSLMODE"} {
		t.Setenv(name, env[name])
	}
}

func valid(t *testing.T) map[string]string {
	return map[string]string{
		EnvRegion:         "eastus",
		EnvCACertPEM:      caPEM(t),
		"SAC_PG_HOST":     "db.internal",
		"SAC_PG_DATABASE": "shadow",
		"SAC_PG_USER":     "ingest-api",
	}
}

func TestLoadConfig(t *testing.T) {
	setEnv(t, valid(t))
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.addr != defaultAddr || cfg.region != "eastus" || cfg.roots == nil || cfg.pg.User != "ingest-api" {
		t.Errorf("config = %+v", cfg)
	}

	env := valid(t)
	env[EnvHTTPAddr] = "0.0.0.0:8080"
	setEnv(t, env)
	if cfg, err := loadConfig(); err != nil || cfg.addr != "0.0.0.0:8080" {
		t.Errorf("addr = %q, err = %v", cfg.addr, err)
	}
}

func TestLoadConfigRefusesIncompleteConfiguration(t *testing.T) {
	cases := map[string]struct {
		mutate func(map[string]string)
		want   string
	}{
		"no region":     {func(e map[string]string) { delete(e, EnvRegion) }, EnvRegion},
		"no device CA":  {func(e map[string]string) { delete(e, EnvCACertPEM) }, EnvCACertPEM},
		"CA is not PEM": {func(e map[string]string) { e[EnvCACertPEM] = "not a certificate" }, EnvCACertPEM},
		"no database":   {func(e map[string]string) { delete(e, "SAC_PG_HOST") }, "SAC_PG_HOST"},
		"no db user":    {func(e map[string]string) { delete(e, "SAC_PG_USER") }, "SAC_PG_USER"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := valid(t)
			c.mutate(env)
			setEnv(t, env)
			if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want one naming %s", err, c.want)
			}
		})
	}
}
