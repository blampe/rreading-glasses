package internal

import (
	"bytes"
	"compress/gzip"
	"context"
	_ "embed" // For schema.
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/jackc/pgx/v4/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver
	"go.uber.org/zap/buffer"
)

//go:embed schema.sql
var _schema string

var _ cache[[]byte] = (*pgcache)(nil)

// _buffers reduces GC.
var _buffers = buffer.NewPool()

// _zipWriters caches gzip writers so we don't constantly re-alloc their
// internal buffers.
var _zipWriters = sync.Pool{New: func() any {
	return &gzip.Writer{}
}}

// _zipReaders caches gzip readers so we don't constantly re-alloc their
// internal buffers.
var _zipReaders = sync.Pool{New: func() any {
	return &gzip.Reader{}
}}

func newPostgres(ctx context.Context, dsn string) (*pgcache, error) {
	db, err := newDB(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("creating db: %w", err)
	}
	return &pgcache{db: db}, nil
}

// newDB connects to our DB and applies our schema.
func newDB(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing postgres config: %w", err)
	}

	cfg.MaxConns = 25
	db, err := pgxpool.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("establishing db connection: %w", err)
	}

	err = db.Ping(ctx)
	if err != nil {
		return nil, fmt.Errorf("pinging db: %w", err)
	}

	_logHandler.Info("ensuring DB schema")
	_, err = db.Exec(ctx, _schema)
	if err != nil {
		return nil, fmt.Errorf("ensuring schema: %w", err)
	}

	return db, nil
}

// pgcache implements a cacher for use with layeredcache.
type pgcache struct {
	db *pgxpool.Pool
}

func (pg *pgcache) Get(ctx context.Context, key string) ([]byte, bool) {
	val, _, ok := pg.GetWithTTL(ctx, key)
	return val, ok
}

func (pg *pgcache) GetWithTTL(ctx context.Context, key string) ([]byte, time.Duration, bool) {
	var compressed []byte
	var expires time.Time
	err := pg.db.QueryRow(ctx, `SELECT value, expires FROM cache WHERE key = $1;`, key).Scan(&compressed, &expires)
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
	_, err = pg.db.Exec(ctx,
		`INSERT INTO cache (key, value, expires) VALUES ($1, $2, $3) ON CONFLICT (key) DO UPDATE SET value = $4, expires = $5;`,
		key, buf.Bytes(), expires, buf.Bytes(), expires,
	)
	if err != nil {
		Log(ctx).Error("problem setting cache", "err", err, "key", key)
	}
}

// Expire can expire a row if provided the key as a tag.
func (pg *pgcache) Expire(ctx context.Context, key string) error {
	_, err := pg.db.Exec(ctx, `UPDATE cache SET expires = $1 WHERE key = $2;`, time.UnixMicro(0), key)
	return err
}

func compress(plaintext io.Reader, buf *buffer.Buffer) error {
	zw := _zipWriters.Get().(*gzip.Writer)
	zw.Reset(buf)
	defer _zipWriters.Put(zw)

	// gzip.Writer implements WriterTo so an intermediate buffer isn't needed.
	_, err := io.Copy(zw, plaintext)
	err = errors.Join(err, zw.Close())
	return err
}

func decompress(ctx context.Context, compressed io.Reader, buf *buffer.Buffer) error {
	zr := _zipReaders.Get().(*gzip.Reader)
	err := zr.Reset(compressed)
	if err != nil {
		return fmt.Errorf("problem resetting zip reader: %w", err)
	}
	defer _zipReaders.Put(zr)

	ibuf := _buffers.Get()
	defer ibuf.Free()
	intermediate := ibuf.Bytes()
	intermediate = intermediate[:cap(intermediate)]

	// buffer.Buffer doesn't implement WriterTo so we use an intermediate
	// buffer to spare an additional alloc.
	_, err = io.CopyBuffer(buf, zr, intermediate)

	if err != nil && !errors.Is(err, io.EOF) {
		Log(ctx).Warn("problem decompressing", "err", err)
		return err
	}
	if err := zr.Close(); err != nil {
		Log(ctx).Warn("problem closing zip writer", "err", err)
	}

	return nil
}
