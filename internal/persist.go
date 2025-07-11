package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// persister records in-flight author refreshes so we can recover them on reboot.
type persister interface {
	Persist(ctx context.Context, authorID int64, current []byte) error
	Persisted(ctx context.Context) ([]int64, error)
	Delete(ctx context.Context, authorID int64) error
}

// Persister tracks author refresh state across reboots.
type Persister struct {
	db    *pgxpool.Pool
	cache cache[[]byte]
}

// nopersist no-ops persistence for tests.
type nopersist struct{}

var (
	_ persister = (*Persister)(nil)
	_ persister = (*nopersist)(nil)
)

func (*nopersist) Persist(ctx context.Context, authorID int64, current []byte) error {
	return nil
}

func (*nopersist) Persisted(ctx context.Context) ([]int64, error) {
	return nil, nil
}

func (*nopersist) Delete(ctx context.Context, authorID int64) error {
	return nil
}

// NewPersister creates a new Persister.
func NewPersister(ctx context.Context, cache cache[[]byte], dsn string) (*Persister, error) {
	db, err := newDB(ctx, dsn)
	return &Persister{db: db, cache: cache}, err
}

// Persist records an author's refresh as in-flight.
func (p *Persister) Persist(ctx context.Context, authorID int64, bytes []byte) error {
	p.cache.Set(ctx, refreshAuthorKey(authorID), bytes, 365*24*time.Hour)
	return nil
}

// Delete records an in-flight refresh as completed.
func (p *Persister) Delete(ctx context.Context, authorID int64) error {
	return p.cache.Delete(ctx, refreshAuthorKey(authorID))
}

// Persisted returns all in-flight author refreshes so they can be resumed.
func (p *Persister) Persisted(ctx context.Context) ([]int64, error) {
	rows, err := p.db.Query(ctx, "SELECT value FROM cache WHERE key LIKE $1", "ra%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var authorIDs []int64

	buf := make([]byte, 0, 8)
	for rows.Next() {

		err := rows.Scan(&buf)
		if err != nil {
			continue
		}

		authorID, err := binary.ReadVarint(bytes.NewReader(buf))
		if err != nil {
			continue
		}
		authorIDs = append(authorIDs, authorID)
	}

	return authorIDs, err
}

func refreshAuthorKey(authorID int64) string {
	return fmt.Sprintf("ra%d", authorID)
}
