package main

import (
	"context"
	"database/sql"
	_ "embed" // For schema.
	"errors"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver

	"github.com/eko/gocache/lib/v4/cache"
	"github.com/eko/gocache/lib/v4/codec"
	"github.com/eko/gocache/lib/v4/store"
)

//go:embed schema.sql
var _schema string

func newPostgres(ctx context.Context, dsn string) (cache.SetterCacheInterface[[]byte], error) {
	db, err := newDB(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating db: %w", err)
	}
	return &pgcache{db: db}, nil
}

func newDB(ctx context.Context, dsn string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("dbinit: %w", err)
	}
	err = db.PingContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("establishing db connection: %w", err)
	}

	_logHandler.Info("ensuring DB schema")
	_, err = db.ExecContext(ctx, _schema)
	if err != nil {
		return nil, fmt.Errorf("ensuring schema: %w", err)
	}

	return db, nil
}

type pgcache struct {
	db *sql.DB
}

var _ cache.SetterCacheInterface[[]byte] = (*pgcache)(nil)

// Clear is a no-op.
func (pg *pgcache) Clear(_ context.Context) error {
	return nil
}

func (pg *pgcache) Delete(ctx context.Context, key any) error {
	_, err := pg.db.ExecContext(ctx, `DELETE FROM cache WHERE key = $1;`, key)
	return err
}

func (pg *pgcache) Get(ctx context.Context, key any) ([]byte, error) {
	val, _, err := pg.GetWithTTL(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.NotFoundWithCause(err)
	}
	return val, err
}

func (pg *pgcache) GetWithTTL(ctx context.Context, key any) ([]byte, time.Duration, error) {
	var val []byte
	var expires time.Time
	err := pg.db.QueryRowContext(ctx, `SELECT value, expires FROM cache WHERE key = $1;`, key).Scan(&val, &expires)
	if err != nil {
		return nil, 0, err
	}

	ttl := time.Until(expires)
	if ttl <= 0 {
		return nil, 0, sql.ErrNoRows // Treat expired entries as a miss to force a refresh.
	}

	return val, ttl, nil
}

func (pg *pgcache) Set(ctx context.Context, key any, val []byte, opts ...store.Option) error {
	o := store.ApplyOptions(opts...)
	expires := time.Now().Add(o.Expiration)
	_, err := pg.db.ExecContext(ctx,
		`INSERT INTO cache (key, value, expires) VALUES ($1, $2, $3) ON CONFLICT (key) DO UPDATE SET value = $4, expires = $5;`,
		key, val, expires, val, expires,
	)
	return err
}

func (pg *pgcache) GetType() string {
	return "cache" // ???
}

func (pg *pgcache) GetCodec() codec.CodecInterface {
	return nil // ???
}

// Invalidate can expire a row if provided the key as a tag.
func (pg *pgcache) Invalidate(ctx context.Context, opts ...store.InvalidateOption) error {
	o := store.ApplyInvalidateOptions(opts...)

	if len(o.Tags) != 1 {
		return nil // Nothing to do
	}
	_, err := pg.db.ExecContext(ctx, `UPDATE cache SET expires = $1 WHERE key = $2;`, time.UnixMicro(0), o.Tags[0])
	return err
}
