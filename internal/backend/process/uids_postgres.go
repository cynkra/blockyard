package process

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"
)

// postgresUIDAllocator coordinates UID allocation across blockyard
// peers via the blockyard_uids table (see #288, parent #262). Same
// pattern as postgresPortAllocator but simpler: UIDs have no kernel-
// side probe analogous to net.Listen, so the Alloc path is straight-
// line — no kernel-bound retry, no tryClaim/probe distinction needed
// because a no-row return from the INSERT is always either exhaustion
// or a race (re-probed via findNextFree).
type postgresUIDAllocator struct {
	db       *sqlx.DB
	first    int
	last     int
	hostname string
}

func newPostgresUIDAllocator(db *sqlx.DB, first, last int, hostname string) *postgresUIDAllocator {
	return &postgresUIDAllocator{
		db:       db,
		first:    first,
		last:     last,
		hostname: hostname,
	}
}

// Alloc claims the lowest free UID via INSERT ... ON CONFLICT DO
// NOTHING RETURNING. On race (two peers picking the same lowest free
// UID), the loser returns no rows and re-probes for the next free slot
// rather than reporting exhaustion.
func (u *postgresUIDAllocator) Alloc() (int, error) {
	skipFrom := u.first
	for {
		if skipFrom > u.last {
			return 0, errors.New("process backend: no free UIDs in range")
		}
		uid, err := u.tryClaim(skipFrom)
		if err != nil {
			return 0, fmt.Errorf("postgres uid alloc: %w", err)
		}
		if uid >= 0 {
			return uid, nil
		}
		next, err := u.findNextFree(skipFrom)
		if err != nil {
			return 0, fmt.Errorf("postgres uid probe: %w", err)
		}
		if next < 0 {
			return 0, errors.New("process backend: no free UIDs in range")
		}
		skipFrom = next
	}
}

// Release deletes the row owned by this hostname. The owner guard
// prevents accidentally deleting a peer's UID.
func (u *postgresUIDAllocator) Release(uid int) {
	if uid < u.first || uid > u.last {
		return
	}
	ctx := context.Background()
	if _, err := u.db.ExecContext(ctx,
		`DELETE FROM blockyard_uids WHERE uid = $1 AND owner = $2`,
		uid, u.hostname,
	); err != nil {
		slog.Error("postgres uid release", "uid", uid, "error", err)
	}
}

// InUse counts entries owned by this host.
func (u *postgresUIDAllocator) InUse() int {
	ctx := context.Background()
	var n int
	if err := u.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM blockyard_uids WHERE owner = $1`,
		u.hostname,
	).Scan(&n); err != nil {
		slog.Error("postgres uid in-use", "error", err)
		return 0
	}
	return n
}

// CleanupOwnedOrphans deletes every blockyard_uids row owned by this
// hostname. Workers from a previous run are dead (Pdeathsig), so all
// owned rows at startup are stale.
func (u *postgresUIDAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	if _, err := u.db.ExecContext(ctx,
		`DELETE FROM blockyard_uids WHERE owner = $1`, u.hostname,
	); err != nil {
		return fmt.Errorf("postgres uid cleanup: %w", err)
	}
	return nil
}

func (u *postgresUIDAllocator) tryClaim(skipFrom int) (int, error) {
	ctx := context.Background()
	var uid int
	err := u.db.QueryRowxContext(ctx, `
		INSERT INTO blockyard_uids (uid, owner)
		SELECT u, $3
		FROM generate_series($1::int, $2::int) AS u
		WHERE NOT EXISTS (SELECT 1 FROM blockyard_uids b WHERE b.uid = u)
		ORDER BY u
		LIMIT 1
		ON CONFLICT (uid) DO NOTHING
		RETURNING uid`,
		skipFrom, u.last, u.hostname,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return uid, nil
}

func (u *postgresUIDAllocator) findNextFree(from int) (int, error) {
	ctx := context.Background()
	var uid int
	err := u.db.QueryRowxContext(ctx, `
		SELECT u
		FROM generate_series($1::int, $2::int) AS u
		WHERE NOT EXISTS (SELECT 1 FROM blockyard_uids b WHERE b.uid = u)
		ORDER BY u
		LIMIT 1`,
		from, u.last,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return uid, nil
}
