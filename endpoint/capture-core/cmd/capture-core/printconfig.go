package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/shadow-ai-capture/device/capture-core/credential"
	"github.com/shadow-ai-capture/device/capture-core/state"
)

// printConfig prints the resolved configuration and what the state directory says about the
// device, without starting anything or changing any file. Reading the state directory needs the
// service's rights (an administrator or root).
func printConfig(cfg Config, w io.Writer) error {
	p := func(label, format string, args ...any) {
		fmt.Fprintf(w, "%-18s %s\n", label+":", fmt.Sprintf(format, args...))
	}
	set := func(v string) string {
		if v == "" {
			return "not set"
		}
		return "set"
	}
	p("version", "%s", version)
	p("state directory", "%s", cfg.StateDir)
	p("tenant", "%s", cfg.TenantID)
	p("device endpoint", "%s", cfg.DeviceEndpoint)
	p("deployment key", "%s", set(cfg.DeploymentKey))
	p("extra CA file", "%s", orNone(cfg.CAFile))
	if cfg.PolicyKey == "" {
		p("policy", "no policy key: the device runs at M0 and fetches no policy")
	} else {
		p("policy", "fetched from the device endpoint, verified under key id %s", cfg.PolicyKeyID)
	}
	if cfg.ClassifierRelease == "" {
		p("classifier", "none: rules-only, confidence degraded")
	} else {
		p("classifier", "%s with release %s", classifierHostExe(), cfg.ClassifierRelease)
	}
	p("device identity", "%s until the server states the tenant's setting", cfg.DeviceIdentity)
	p("native messaging", "%s", platform.nativeAddr)

	root, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		p("enrolment", "the state directory does not exist yet; the device has not run")
		return nil
	}
	key, err := os.ReadFile(filepath.Join(root, state.SpoolKeyFile))
	if err != nil {
		p("enrolment", "unknown (%v)", err)
		return nil
	}
	store, err := credential.Open(filepath.Join(root, state.CredentialFile), key)
	if err != nil {
		return err
	}
	if c, err := store.Load(); err != nil {
		p("enrolment", "not enrolled (%v)", err)
	} else {
		p("enrolment", "device %s in tenant %s, certificate valid until %s", c.DeviceID, c.TenantID, c.NotAfter.Format(time.RFC3339))
	}
	if st, err := os.Stat(filepath.Join(root, state.HealthFile)); err == nil {
		p("last health", "%s, written %s", filepath.Join(root, state.HealthFile), st.ModTime().Format(time.RFC3339))
	}
	facts := collectHostFacts()
	p("attestation", "intune_device_id=%q entra_device_id=%q serial=%q", facts.Attestation.IntuneDeviceID, facts.Attestation.EntraDeviceID, facts.Attestation.SerialNumber)
	for _, n := range facts.Notes {
		p("attestation note", "%s", n)
	}
	return nil
}

func orNone(v string) string {
	if v == "" {
		return "none"
	}
	return v
}
