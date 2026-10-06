package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/shadow-ai-capture/control-api/internal/onboard"
)

// `control-api tenant create|invite` is the vendor operator's side of onboarding: create the
// product tenant, then issue the one-time link the customer's admin opens to connect their identity
// provider. They are commands rather than routes, because nothing a customer can reach may create a
// tenant or name its email domains. In Azure they run as the manual tenant-admin job, with the
// control-api image and database login.
//
//	control-api tenant create --name "Contoso" --region eastus --ceiling m1 --actor alice@vendor
//	control-api tenant invite --tenant <id> --domain contoso.com [--domain contoso.co.uk] \
//	  [--expires 168h] --actor alice@vendor
//
// create prints the tenant id and invite prints the onboarding URL, each alone on stdout. The URL
// is shown once: only its hash is stored. The database comes from SAC_PG_*, --region defaults to
// SAC_REGION, and the onboarding URL is built on SAC_PUBLIC_URL.
func runTenant(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: control-api tenant create|invite [flags]")
	}
	switch args[0] {
	case "create":
		return runTenantCreate(args[1:])
	case "invite":
		return runTenantInvite(args[1:])
	}
	return fmt.Errorf("unknown tenant command %q (want create or invite)", args[0])
}

func runTenantCreate(args []string) error {
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	var t onboard.NewTenant
	fs.StringVar(&t.Name, "name", "", "the customer's name (required)")
	fs.StringVar(&t.Region, "region", os.Getenv(EnvRegion), "the residency region the tenant is pinned to (default "+EnvRegion+")")
	fs.StringVar(&t.CeilingMode, "ceiling", "m1", "the collection ceiling: m0, m1, m2 or m3")
	fs.StringVar(&t.Actor, "actor", "", "the vendor operator recorded in the audit trail (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return inDatabase(func(ctx context.Context, db *sql.DB) error {
		id, err := onboard.CreateTenant(ctx, db, t)
		if err != nil {
			return err
		}
		fmt.Println(id)
		return nil
	})
}

func runTenantInvite(args []string) error {
	fs := flag.NewFlagSet("tenant invite", flag.ContinueOnError)
	var in onboard.NewInvite
	var domains domainList
	fs.StringVar(&in.TenantID, "tenant", "", "the tenant id `tenant create` printed (required)")
	fs.Var(&domains, "domain", "an email domain whose people sign in to this tenant; repeatable (required)")
	fs.DurationVar(&in.TTL, "expires", 7*24*time.Hour, "how long the link stays valid (at most 720h)")
	fs.StringVar(&in.Actor, "actor", "", "the vendor operator recorded in the audit trail (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(domains) == 0 {
		return errors.New("name at least one --domain: a work email is routed to its tenant by its domain")
	}
	in.Domains, in.PublicURL = domains, strings.TrimRight(os.Getenv(EnvPublicURL), "/")
	return inDatabase(func(ctx context.Context, db *sql.DB) error {
		inv, err := onboard.CreateInvite(ctx, db, in)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "onboarding link for tenant %s, single use, expires %s:\n", in.TenantID, inv.ExpiresAt.Format(time.RFC3339))
		fmt.Println(inv.URL)
		return nil
	})
}

// inDatabase opens the database from SAC_PG_* and runs fn with a bounded context.
func inDatabase(fn func(ctx context.Context, db *sql.DB) error) error {
	db, err := openDatabase()
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return fn(ctx, db)
}

// domainList collects repeated --domain flags.
type domainList []string

func (d *domainList) String() string { return strings.Join(*d, ",") }

func (d *domainList) Set(v string) error {
	*d = append(*d, v)
	return nil
}
