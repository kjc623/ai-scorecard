// Command authlab is the lab's device-side tool. It runs on the lab network.
//
//	authlab pki
//	    Prints fresh lab key material as JSON: the device CA and the edge's server certificate. It
//	    writes no file; localdev/run.mjs stores it once and keeps it.
//
//	authlab smoke [flags]
//	    Proves the lab works end to end, the way a device and an analyst use it: every service is
//	    ready; the edge serves the device API only; a device enrols with the deployment key (and is
//	    refused without one), sends an event with its certificate, and fetches a policy bundle that
//	    verifies under the pinned key; and an analyst signs in through the identity provider and
//	    reads that device's events through the dashboard. The CA and the deployment key come from
//	    the environment (LAB_CA_CERT_PEM, LAB_DEPLOYMENT_KEY).
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "pki":
		err = writePKI(os.Stdout)
	case "smoke":
		var cfg smokeConfig
		cfg, err = smokeConfigFrom(os.Args[2:], os.Getenv)
		if err == nil {
			err = runSmoke(cfg)
		}
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "authlab:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: authlab pki | authlab smoke -tenant UUID -policy-key HEX [flags]")
}

func smokeConfigFrom(args []string, getenv func(string) string) (smokeConfig, error) {
	fs := flag.NewFlagSet("smoke", flag.ContinueOnError)
	cfg := smokeConfig{CAPEM: getenv("LAB_CA_CERT_PEM"), DeploymentKey: strings.TrimSpace(getenv("LAB_DEPLOYMENT_KEY"))}
	var policyKey, resolve string
	fs.StringVar(&cfg.Edge, "edge", "https://edge:8443", "the edge's address on the lab network")
	fs.StringVar(&cfg.Tenant, "tenant", "", "the tenant the deployment key belongs to")
	fs.StringVar(&policyKey, "policy-key", "", "the policy public key devices pin, hex")
	fs.StringVar(&cfg.PolicyKeyID, "policy-key-id", "policy-key-1", "the policy key id devices pin")
	fs.StringVar(&cfg.Dashboard, "dashboard", "http://127.0.0.1:8787", "the dashboard's public URL")
	fs.StringVar(&resolve, "resolve", "", "public host:port=lab host:port pairs, comma-separated")
	fs.StringVar(&cfg.Analyst, "analyst", "", "an analyst account of the tenant")
	fs.DurationVar(&cfg.Wait, "wait", 90*time.Second, "how long the services may take to become ready")
	if err := fs.Parse(args); err != nil {
		return smokeConfig{}, err
	}
	cfg.Edge = strings.TrimRight(cfg.Edge, "/")
	cfg.Dashboard = strings.TrimRight(cfg.Dashboard, "/")
	var errs []error
	if strings.TrimSpace(cfg.CAPEM) == "" {
		errs = append(errs, errors.New("LAB_CA_CERT_PEM is required"))
	}
	if cfg.DeploymentKey == "" {
		errs = append(errs, errors.New("LAB_DEPLOYMENT_KEY is required"))
	}
	if cfg.Tenant == "" || cfg.Analyst == "" {
		errs = append(errs, errors.New("-tenant and -analyst are required"))
	}
	if b, err := hex.DecodeString(strings.TrimSpace(policyKey)); err != nil || len(b) != ed25519.PublicKeySize {
		errs = append(errs, errors.New("-policy-key must be a 64-digit hex Ed25519 public key"))
	} else {
		cfg.PolicyKey = b
	}
	cfg.Resolve = map[string]string{}
	for _, pair := range strings.Split(resolve, ",") {
		if pair = strings.TrimSpace(pair); pair == "" {
			continue
		}
		from, to, ok := strings.Cut(pair, "=")
		if !ok || from == "" || to == "" {
			errs = append(errs, fmt.Errorf("-resolve %q is not host:port=host:port", pair))
			continue
		}
		cfg.Resolve[from] = to
	}
	return cfg, errors.Join(errs...)
}
