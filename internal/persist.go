package internal

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
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
	Log(ctx).Info("finished loading author", "authorID", authorID)
	return p.cache.Delete(ctx, refreshAuthorKey(authorID))
}

// Persisted returns all in-flight author refreshes so they can be resumed. IDs
// are returned in FIFO order.
func (p *Persister) Persisted(ctx context.Context) ([]int64, error) {
	start := time.Now()

	rows, err := p.db.Query(ctx, "SELECT SUBSTRING(key, 3), expires FROM cache WHERE key LIKE 'ra%'")
	if err != nil {
		Log(ctx).Error("unable to recover in-flight refreshes", "err", err)
		return nil, err
	}
	defer rows.Close()

	m := map[int64]int64{}

	for rows.Next() {
		var id string
		var expires pgtype.Timestamptz
		err := rows.Scan(&id, &expires)
		if err != nil {
			continue
		}
		if authorID, err := strconv.ParseInt(id, 10, 64); err == nil {
			m[expires.Time.UnixNano()] = authorID
		}
	}

	authorIDs := make([]int64, 0, len(m))
	for _, key := range slices.Sorted(maps.Keys(m)) {
		authorIDs = append(authorIDs, m[key])
	}

	if len(authorIDs) > 0 {
		Log(ctx).Debug("recovered in-flight refreshes", "count", len(authorIDs), "duration", time.Since(start).String())
	}

	return authorIDs, err
}

func refreshAuthorKey(authorID int64) string {
	return fmt.Sprintf("ra%d", authorID)
}
