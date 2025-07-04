package internal

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// persister records in-flight author refreshes so we can recover them on reboot.
type persister interface {
	Persist(ctx context.Context, authorID int64) error
	Persisted(ctx context.Context) ([]int64, error)
	Delete(ctx context.Context, authorID int64) error
}

// Persister tracks author refresh state across reboots.
type Persister struct {
	db *pgxpool.Pool
}

var _ persister = (*Persister)(nil)

// NewPersister creates a new Persister.
func NewPersister(ctx context.Context, dsn string) (*Persister, error) {
	db, err := newDB(ctx, dsn)
	return &Persister{db: db}, err
}

// Persist records an author's refresh as in-flight.
func (p *Persister) Persist(ctx context.Context, authorID int64) error {
	buf := make([]byte, 8)
	_ = binary.PutVarint(buf, authorID)

	_, err := p.db.Exec(ctx, "INSERT INTO cache (key, value) VALUES ($1, $2) ON CONFLICT DO NOTHING", fmt.Sprintf("ra%d", authorID), buf)
	return err
}

// Delete records an in-flight refresh as completed.
func (p *Persister) Delete(ctx context.Context, authorID int64) error {
	_, err := p.db.Exec(ctx, "DELETE FROM cache WHERE key = $1", fmt.Sprintf("ra%d", authorID))
	return err
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
