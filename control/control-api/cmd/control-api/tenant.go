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

// `control-api tenant create|invite` is the vendor operator's side of onboarding (the shared contract
// §0.1, §3): the vendor creates the product tenant, then issues a one-time link the customer's admin
// opens to connect their identity provider. It is a command, not an HTTP route, because nothing a
// customer can reach should be able to create a tenant or name its email domains.
//
//	control-api tenant create --dsn "$SAC_PG_DSN" --name "Contoso" --region eu-west \
//	  --key-custody vendor --ceiling m1
//	control-api tenant invite --dsn "$SAC_PG_DSN" --tenant <id> --domain contoso.com \
//	  [--domain contoso.co.uk] [--expires 168h] [--public-url https://app.example.com]
//
// create prints the tenant id and invite prints the onboarding URL, each alone on stdout, so a script
// can capture them. The URL is shown once: only its hash is stored.
func runTenant(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: control-api tenant create|invite [flags]")
	}
	switch args[0] {
	case "create":
		return runTenantCreate(args[1:])
	case "invite":
		return runTenantInvite(args[1:])
	default:
		return fmt.Errorf("unknown tenant command %q (want create or invite)", args[0])
	}
}

// operatorDB is the database flags both commands share, resolved the way sync-directory resolves them.
type operatorDB struct {
	dsn, driver, pgHost, pgPort, pgDatabase, role string
	actor                                         string
}

func (o *operatorDB) register(fs *flag.FlagSet) {
	fs.StringVar(&o.dsn, "dsn", "", "database DSN; the caller must register a driver")
	fs.StringVar(&o.driver, "driver", "", "database/sql driver name; must be registered in this binary")
	fs.StringVar(&o.pgHost, "pg-host", "", "database host (used to build the DSN, not a password)")
	fs.StringVar(&o.pgPort, "pg-port", "5432", "database port")
	fs.StringVar(&o.pgDatabase, "pg-database", "shadow", "database name")
	fs.StringVar(&o.role, "role", "control-api", "the identity this process connects as")
	fs.StringVar(&o.actor, "actor", operatorName(), "the vendor operator recorded in the audit trail (env SAC_OPERATOR)")
}

// operatorName defaults the audit actor to who is running the command.
func operatorName() string {
	for _, name := range []string{"SAC_OPERATOR", "USERNAME", "USER"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

func (o *operatorDB) open(ctx context.Context) (*sql.DB, error) {
	dsn := o.dsn
	if dsn == "" {
		dsn = postgresDSN(o.pgHost, o.pgPort, o.pgDatabase, o.role)
	}
	if dsn == "" {
		return nil, errors.New("no database: pass -dsn, or -pg-host (with the other -pg-* flags)")
	}
	driver := o.driver
	if driver == "" {
		driver = defaultDriverName
	}
	if driver == "" {
		return nil, errors.New("no PostgreSQL driver is registered in this binary; build with -tags sac_sql_driver")
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open database with driver %q: %w", driver, err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("database is not reachable: %w", err)
	}
	return db, nil
}

func runTenantCreate(args []string) error {
	fs := flag.NewFlagSet("tenant create", flag.ContinueOnError)
	var o operatorDB
	o.register(fs)
	var t onboard.NewTenant
	fs.StringVar(&t.Name, "name", "", "the customer's name (required)")
	fs.StringVar(&t.Region, "region", "", "residency region the tenant is pinned to (required)")
	fs.StringVar(&t.KeyCustody, "key-custody", "vendor", "vendor | customer_managed | customer_held")
	fs.StringVar(&t.CeilingMode, "ceiling", "m1", "collection ceiling: m0 | m1 | m2 | m3")
	fs.StringVar(&t.KEKID, "kek-id", "", "the tenant key-encryption key id (required for m3)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t.Actor = o.actor
	ctx := context.Background()
	db, err := o.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	id, err := onboard.CreateTenant(ctx, db, t)
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func runTenantInvite(args []string) error {
	fs := flag.NewFlagSet("tenant invite", flag.ContinueOnError)
	var o operatorDB
	o.register(fs)
	var in onboard.NewInvite
	var domains stringList
	fs.StringVar(&in.TenantID, "tenant", "", "the tenant id `tenant create` printed (required)")
	fs.Var(&domains, "domain", "an email domain whose people sign in to this tenant; repeatable")
	fs.DurationVar(&in.TTL, "expires", 7*24*time.Hour, "how long the link stays valid (at most 720h)")
	fs.StringVar(&in.PublicURL, "public-url", os.Getenv("SAC_PUBLIC_URL"), "the product's browser-facing base URL (env SAC_PUBLIC_URL)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(domains) == 0 {
		return errors.New("name at least one --domain: a work email is routed to its tenant by domain")
	}
	in.Domains, in.Actor = domains, o.actor
	ctx := context.Background()
	db, err := o.open(ctx)
	if err != nil {
		return err
	}
	defer db.Close()
	inv, err := onboard.CreateInvite(ctx, db, in)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "onboarding link for tenant %s, single use, expires %s:\n", in.TenantID, inv.ExpiresAt.Format(time.RFC3339))
	fmt.Println(inv.URL)
	return nil
}
