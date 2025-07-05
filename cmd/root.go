// Package cmd contains helpers common to all CLI implementations.
package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/blampe/rreading-glasses/internal"
	charm "github.com/charmbracelet/log"
)

// PGConfig configured a PostGres connection.
type PGConfig struct {
	PostgresHost         string `default:"localhost" env:"POSTGRES_HOST" help:"Postgres host."`
	PostgresUser         string `default:"postgres" env:"POSTGRES_USER" help:"Postgres user."`
	PostgresPassword     string `xor:"db-auth" env:"POSTGRES_PASSWORD" help:"Postgres password."`
	PostgresPasswordFile []byte `type:"filecontent" xor:"db-auth" env:"POSTGRES_PASSWORD_FILE" help:"File with the Postgres password."`
	PostgresPort         int    `default:"5432" env:"POSTGRES_PORT" help:"Postgres port."`
	PostgresDatabase     string `default:"rreading-glasses" env:"POSTGRES_DATABASE" help:"Postgres database to use."`
}

// DSN returns the database's DSN based on the provided flags.
func (c *PGConfig) DSN() string {
	if len(c.PostgresPasswordFile) > 0 {
		c.PostgresPassword = string(bytes.TrimSpace(c.PostgresPasswordFile))
	}

	dsn := fmt.Sprintf(
		"user=%s password=%s dbname=%s host=%s",
		c.PostgresUser,
		c.PostgresPassword,
		c.PostgresDatabase,
		c.PostgresHost,
	)

	// Unix sockets don't need a port.
	if !filepath.IsAbs(c.PostgresHost) {
		dsn = fmt.Sprintf("%s port=%d", dsn, c.PostgresPort)
	}

	return dsn
}

// LogConfig configures logging.
type LogConfig struct {
	Verbose bool `env:"VERBOSE" help:"increase log verbosity"`
}

// Run sets logging to DEBUG if verbose is enabled.
func (c *LogConfig) Run() error {
	if c.Verbose {
		internal.SetLogLevel(charm.DebugLevel)
	}
	return nil
}

// CloudflareConfig is optional and configures Cloudflare for cache busting.
type CloudflareConfig struct {
	CloudflareToken  string `and:"cf" help:"API token (not a legacy global API key) with permission to bust caches."`
	CloudflareZoneID string `and:"cf" help:"The Cloudflare zone ID"`
	CloudflareDomain string `and:"cf" help:"The domain name for constructing URLs to bust."`
}

// Cache returns the cloudflare cache, if it was configured, or nil otherwise.
func (c *CloudflareConfig) Cache() (*internal.CloudflareCache, error) {
	if c.CloudflareToken == "" {
		return nil, nil
	}

	// Reverse mapping from cache keys to URLs for busting.
	pather := func(key string) string {
		if strings.HasPrefix(key, "b") {
			return fmt.Sprintf("https://%s/book/%s", c.CloudflareDomain, key[1:])
		}
		if strings.HasPrefix(key, "w") {
			return fmt.Sprintf("https://%s/work/%s", c.CloudflareDomain, key[1:])
		}
		if strings.HasPrefix(key, "a") {
			return fmt.Sprintf("https://%s/author/%s", c.CloudflareDomain, key[1:])
		}
		return "https://" + c.CloudflareDomain
	}

	return internal.NewCloudflareCache(c.CloudflareToken, c.CloudflareZoneID, pather)
}

// Bust allows manually busting entries from the CLI.
type Bust struct {
	PGConfig
	LogConfig
	CloudflareConfig

	AuthorID int64 `arg:"" help:"author ID to cache bust"`
}

// Run busts a cache key.
func (b *Bust) Run() error {
	_ = b.LogConfig.Run()
	ctx := context.Background()

	cf, err := b.CloudflareConfig.Cache()
	if err != nil {
		return fmt.Errorf("setting up cloudflare: %w", err)
	}

	cache, err := internal.NewCache(ctx, b.DSN(), cf)
	if err != nil {
		return err
	}

	a, ok := cache.Get(ctx, internal.AuthorKey(b.AuthorID))
	if !ok {
		return nil
	}

	var author internal.AuthorResource
	err = json.Unmarshal(a, &author)
	if err != nil {
		return err
	}

	for _, w := range author.Works {
		for _, b := range w.Books {
			err = errors.Join(err, cache.Expire(ctx, internal.BookKey(b.ForeignID)))
		}
		err = errors.Join(err, cache.Expire(ctx, internal.WorkKey(w.ForeignID)))
	}
	err = errors.Join(err, cache.Expire(ctx, internal.AuthorKey(author.ForeignID)))

	return err
}

func init() {
	// Limit our memory to 90% of what's free. This affects cache sizes.
	_, err := memlimit.SetGoMemLimitWithOpts(
		memlimit.WithRatio(0.9),
		memlimit.WithLogger(slog.Default()),
		memlimit.WithProvider(
			memlimit.ApplyFallback(
				memlimit.FromCgroup,
				memlimit.FromSystem,
			),
		),
	)
	if err != nil {
		panic(err)
	}
}
