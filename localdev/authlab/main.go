// Command authlab is the development PKI and the end-to-end device client for the local auth lab
// (ADR 0020 decision 5). It has two modes:
//
//	authlab pki   -pki-dir DIR
//	    Generates the dev CA, the edge's server key pair and the access-token signing key pair with
//	    the standard library. The CA signs the device leaves control-api issues; the edge presents the
//	    server certificate; control-api signs tokens with the private half and ingest-api verifies
//	    with the public half. The material is written to DIR, which is gitignored and generated fresh
//	    on every run: no key is ever committed.
//
//	authlab smoke -edge-url URL -pki-dir DIR -tenant UUID -enrolment-token-x509 T -enrolment-token-dpop T
//	    Uses the real endpoint/protocol types and real crypto to prove both production auth modes
//	    through the edge: x509 enrol -> events accepted, and dpop enrol -> token -> events accepted,
//	    plus the negatives (no credential refused, a replayed DPoP jti refused, a certificate from
//	    another CA refused).
//
// The client is device-side code, not service code: it never imports a service package and the
// services never import it. Everything it does on the wire is what a real endpoint agent does.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	args := os.Args[2:]

	var err error
	switch mode {
	case "pki":
		fs := flag.NewFlagSet("pki", flag.ExitOnError)
		dir := fs.String("pki-dir", "", "directory to write generated key material into (required)")
		_ = fs.Parse(args)
		if *dir == "" {
			fmt.Fprintln(os.Stderr, "authlab pki: -pki-dir is required")
			os.Exit(2)
		}
		err = generatePKI(*dir)
	case "smoke":
		fs := flag.NewFlagSet("smoke", flag.ExitOnError)
		cfg := smokeConfig{}
		fs.StringVar(&cfg.EdgeURL, "edge-url", "https://127.0.0.1:8443", "edge base URL (scheme://host:port)")
		fs.StringVar(&cfg.PKIDir, "pki-dir", "", "directory with dev-ca.crt (required)")
		fs.StringVar(&cfg.Tenant, "tenant", "", "tenant uuid the enrolment tokens belong to (required)")
		fs.StringVar(&cfg.TokenX509, "enrolment-token-x509", "", "single-use enrolment token for the x509 flow (required)")
		fs.StringVar(&cfg.TokenDPoP, "enrolment-token-dpop", "", "single-use enrolment token for the dpop flow (required)")
		_ = fs.Parse(args)
		if cfg.PKIDir == "" || cfg.Tenant == "" || cfg.TokenX509 == "" || cfg.TokenDPoP == "" {
			fmt.Fprintln(os.Stderr, "authlab smoke: -pki-dir, -tenant, -enrolment-token-x509 and -enrolment-token-dpop are required")
			os.Exit(2)
		}
		err = runSmoke(cfg)
	case "help", "-h", "--help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "authlab: unknown mode %q\n", mode)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "authlab:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `authlab — development PKI and end-to-end client for the local auth lab

  authlab pki   -pki-dir DIR
  authlab smoke -edge-url URL -pki-dir DIR -tenant UUID \
                -enrolment-token-x509 TOKEN -enrolment-token-dpop TOKEN
`)
}
