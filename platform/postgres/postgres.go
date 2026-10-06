// Package postgres opens the PostgreSQL connection pool every service and job uses.
//
// In Azure the server accepts only Microsoft Entra authentication: each connection presents an
// access token for the process's managed identity as its password, fetched (and refreshed) as the
// pool opens connections. A password is used instead when one is configured, which is how the
// local lab connects.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/shadow-ai-capture/platform/azureidentity"
)

// Environment variables read by ConfigFromEnv.
const (
	EnvHost     = "SAC_PG_HOST"
	EnvPort     = "SAC_PG_PORT"
	EnvDatabase = "SAC_PG_DATABASE"
	EnvUser     = "SAC_PG_USER"
	EnvPassword = "SAC_PG_PASSWORD"
	EnvSSLMode  = "SAC_PG_SSLMODE"
)

// Config locates a database and says how to authenticate to it.
type Config struct {
	Host     string
	Port     int // 5432 when zero
	Database string
	User     string
	// Password authenticates when set. When empty, every connection authenticates with an Entra
	// access token from Tokens.
	Password string
	SSLMode  string // "require" when empty
	// Tokens supplies Entra tokens when Password is empty. Nil uses the managed identity the
	// platform injected (azureidentity.FromEnvironment).
	Tokens azureidentity.TokenSource
	// MaxOpenConns bounds the pool; zero means 10.
	MaxOpenConns int
}

// ConfigFromEnv reads the SAC_PG_* variables.
func ConfigFromEnv() (Config, error) {
	cfg := Config{
		Host:     os.Getenv(EnvHost),
		Database: os.Getenv(EnvDatabase),
		User:     os.Getenv(EnvUser),
		Password: os.Getenv(EnvPassword),
		SSLMode:  os.Getenv(EnvSSLMode),
	}
	if p := os.Getenv(EnvPort); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return Config{}, fmt.Errorf("postgres: %s=%q is not a port", EnvPort, p)
		}
		cfg.Port = n
	}
	return cfg, cfg.Validate()
}

// Validate reports the first missing or malformed setting.
func (c Config) Validate() error {
	switch {
	case c.Host == "":
		return fmt.Errorf("postgres: no host (%s)", EnvHost)
	case c.Database == "":
		return fmt.Errorf("postgres: no database (%s)", EnvDatabase)
	case c.User == "":
		return fmt.Errorf("postgres: no user (%s)", EnvUser)
	case c.Port < 0 || c.Port > 65535:
		return fmt.Errorf("postgres: port %d is out of range", c.Port)
	}
	return nil
}

// String describes the target without any credential, for logs.
func (c Config) String() string {
	auth := "entra"
	if c.Password != "" {
		auth = "password"
	}
	return fmt.Sprintf("postgres://%s@%s/%s (auth=%s, sslmode=%s)", c.User, c.hostPort(), c.Database, auth, c.sslMode())
}

func (c Config) hostPort() string {
	port := c.Port
	if port == 0 {
		port = 5432
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(port))
}

func (c Config) sslMode() string {
	if c.SSLMode == "" {
		return "require"
	}
	return c.SSLMode
}

// Open returns a pool for cfg. It does not connect: reachability is a readiness question, answered
// by the caller's first query.
func Open(cfg Config) (*sql.DB, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.User(cfg.User),
		Host:     cfg.hostPort(),
		Path:     "/" + cfg.Database,
		RawQuery: url.Values{"sslmode": {cfg.sslMode()}}.Encode(),
	}
	if cfg.Password != "" {
		u.User = url.UserPassword(cfg.User, cfg.Password)
	}
	connConfig, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}

	var opts []stdlib.OptionOpenDB
	if cfg.Password == "" {
		tokens := cfg.Tokens
		if tokens == nil {
			cred, err := azureidentity.FromEnvironment()
			if err != nil {
				return nil, errors.Join(fmt.Errorf("postgres: no %s and no managed identity", EnvPassword), err)
			}
			tokens = cred
		}
		opts = append(opts, stdlib.OptionBeforeConnect(func(ctx context.Context, c *pgx.ConnConfig) error {
			token, err := tokens.Token(ctx, azureidentity.ResourcePostgres)
			if err != nil {
				return fmt.Errorf("postgres: entra token: %w", err)
			}
			c.Password = token
			return nil
		}))
	}

	db := stdlib.OpenDB(*connConfig, opts...)
	maxOpen := cfg.MaxOpenConns
	if maxOpen == 0 {
		maxOpen = 10
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}
