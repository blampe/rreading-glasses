package internal

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	_ "embed" // For schema.
	"errors"
	"fmt"
	"io"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver
	"go.uber.org/zap/buffer"
)

//go:embed schema.sql
var _schema string

// _buffers reduces GC.
var _buffers = buffer.NewPool()

var _ cache[[]byte] = (*pgcache)(nil)

func newPostgres(ctx context.Context, dsn string) (*pgcache, error) {
	db, err := newDB(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating db: %w", err)
	}
	return &pgcache{db: db}, nil
}

// newDB connects to our DB and applies our schema.
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

// pgcache implements a cacher for use with layeredcache.
type pgcache struct {
	db *sql.DB
}

func (pg *pgcache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, _, ok := pg.GetWithTTL(ctx, key)
	return val, ok
}

func (pg *pgcache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	var compressed []byte
	var expires time.Time
	err := pg.db.QueryRowContext(ctx, `SELECT value, expires FROM cache WHERE key = $1;`, key).Scan(&compressed, &expires)
	if err != nil {
		return nil, 0, false
	}

	// TODO: The client doesn't support gzip content-encoding, which is
	// bade because we could just return compressed bytes as-is.
	buf := _buffers.Get()
	defer buf.Free()

	err = decompress(ctx, bytes.NewReader(compressed), buf)
	if err != nil {
		return nil, 0, false
	}

	// We can't return the buffer's underlying byte slice, so make a copy.
	// Still allocates but simpler than returning the raw buffer for now.
	uncompressed := bytes.Clone(buf.Bytes())

	// Treat expired entries as a miss to force a refresh, but still return
	// the cached data because it can help speed up the refresh.
	ttl := time.Until(expires)
	if ttl <= 0 {
		return uncompressed, 0, false
	}

	return uncompressed, ttl, true
}

func (pg *pgcache) Set(ctx context.Context, key string, val []byte, ttl time.Duration) {
	expires := time.Now().Add(ttl)

	buf := _buffers.Get()
	defer buf.Free()

	err := compress(bytes.NewReader(val), buf)
	if err != nil {
		Log(ctx).Error("problem compressing value", "err", err, "key", key)
	}
	_, err = pg.db.ExecContext(ctx,
		`INSERT INTO cache (key, value, expires) VALUES ($1, $2, $3) ON CONFLICT (key) DO UPDATE SET value = $4, expires = $5;`,
		key, buf.Bytes(), expires, buf.Bytes(), expires,
	)
	if err != nil {
		Log(ctx).Error("problem setting cache", "err", err)
	}
}

// Expire can expire a row if provided the key as a tag.
func (pg *pgcache) Expire(ctx context.Context, key string) error {
	_, err := pg.db.ExecContext(ctx, `UPDATE cache SET expires = $1 WHERE key = $2;`, time.UnixMicro(0), key)
	return err
}

func compress(plaintext io.Reader, buf *buffer.Buffer) error {
	zw := gzip.NewWriter(buf)
	_, err := io.Copy(zw, plaintext)
	err = errors.Join(err, zw.Close())
	return err
}

func decompress(ctx context.Context, compressed io.Reader, buf *buffer.Buffer) error {
	zr, err := gzip.NewReader(compressed)
	if err != nil && !errors.Is(err, io.EOF) {
		Log(ctx).Warn("problem unzipping", "err", err)
		return err
	}

	_, err = io.Copy(buf, zr)
	if err != nil && !errors.Is(err, io.EOF) {
		Log(ctx).Warn("problem decompressing", "err", err)
		return err
	}
	if err := zr.Close(); err != nil {
		Log(ctx).Warn("problem closing zip write", "err", err)
	}

	return nil
}
