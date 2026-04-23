package process

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/jmoiron/sqlx"
)

// postgresPortAllocator coordinates port allocation across blockyard
// peers via the blockyard_ports table (see #288, parent #262).
//
// The Redis variant fails open on a Redis blip — a transient outage
// during allocation can hand out a port already claimed by another
// peer, or falsely report exhaustion. Moving the source of truth into
// Postgres makes allocation correct under Redis restart.
//
// Each row is (port, owner). Owner is the hostname (matching the Redis
// SETNX value), so CleanupOwnedOrphans at startup can reclaim slots a
// previous crashed instance on the same host left behind.
type postgresPortAllocator struct {
	db       *sqlx.DB
	first    int
	last     int
	hostname string
}

func newPostgresPortAllocator(db *sqlx.DB, first, last int, hostname string) *postgresPortAllocator {
	return &postgresPortAllocator{
		db:       db,
		first:    first,
		last:     last,
		hostname: hostname,
	}
}

// Reserve picks a free port by inserting it into blockyard_ports, then
// attempts to bind a listener. On bind failure (a non-blockyard host
// process holds the port), the row is deleted and the scan advances
// past the failed index — matching the Redis variant's kernel-probe
// retry loop.
//
// The CTE INSERT is atomic: if two peers race for the same lowest free
// port, only one row inserts; the other returns no rows. The race
// branch re-probes for the next free slot rather than treating the
// no-row return as exhaustion.
func (p *postgresPortAllocator) Reserve() (int, net.Listener, error) {
	skipFrom := p.first
	for {
		if skipFrom > p.last {
			return 0, nil, errors.New("process backend: no free ports in range")
		}
		port, err := p.tryClaim(skipFrom)
		if err != nil {
			return 0, nil, fmt.Errorf("postgres port alloc: %w", err)
		}
		if port < 0 {
			next, err := p.findNextFree(skipFrom)
			if err != nil {
				return 0, nil, fmt.Errorf("postgres port probe: %w", err)
			}
			if next < 0 {
				return 0, nil, errors.New("process backend: no free ports in range")
			}
			skipFrom = next
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			return port, ln, nil
		}
		// Kernel says this port is externally busy. Drop the DB claim so
		// it can be re-used after the external holder releases, and
		// advance skip_from so the same index isn't re-probed.
		p.deleteRow(port)
		skipFrom = port + 1
	}
}

// Release returns a port to the pool via an ownership-checked DELETE.
// The owner guard prevents accidentally deleting a peer's row.
func (p *postgresPortAllocator) Release(port int) {
	if port < p.first || port > p.last {
		return
	}
	p.deleteRow(port)
}

// InUse counts entries owned by this host. Used by tests and diagnostic
// endpoints; not load-bearing for correctness.
func (p *postgresPortAllocator) InUse() int {
	ctx := context.Background()
	var n int
	if err := p.db.QueryRowxContext(ctx,
		`SELECT COUNT(*) FROM blockyard_ports WHERE owner = $1`,
		p.hostname,
	).Scan(&n); err != nil {
		slog.Error("postgres port in-use", "error", err)
		return 0
	}
	return n
}

// CleanupOwnedOrphans deletes every blockyard_ports row owned by this
// hostname. Called at startup to reclaim slots a previous crashed
// instance on the same host left behind. Workers from the previous run
// are dead (Pdeathsig killed them with the server), so the deletion is
// unconditional for owned rows.
func (p *postgresPortAllocator) CleanupOwnedOrphans(ctx context.Context) error {
	if _, err := p.db.ExecContext(ctx,
		`DELETE FROM blockyard_ports WHERE owner = $1`, p.hostname,
	); err != nil {
		return fmt.Errorf("postgres port cleanup: %w", err)
	}
	return nil
}

// tryClaim attempts to insert the lowest free port in [skipFrom, last].
// Returns the claimed port, or -1 if no row was inserted (either
// exhaustion or a race lost to a concurrent INSERT).
func (p *postgresPortAllocator) tryClaim(skipFrom int) (int, error) {
	ctx := context.Background()
	var port int
	err := p.db.QueryRowxContext(ctx, `
		INSERT INTO blockyard_ports (port, owner)
		SELECT p, $3
		FROM generate_series($1::int, $2::int) AS p
		WHERE NOT EXISTS (SELECT 1 FROM blockyard_ports b WHERE b.port = p)
		ORDER BY p
		LIMIT 1
		ON CONFLICT (port) DO NOTHING
		RETURNING port`,
		skipFrom, p.last, p.hostname,
	).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return port, nil
}

// findNextFree returns the lowest unclaimed port in [from, last], or -1
// if the range has no free slots. Used to distinguish "no rows" =
// exhaustion from "no rows" = race after tryClaim returns -1.
func (p *postgresPortAllocator) findNextFree(from int) (int, error) {
	ctx := context.Background()
	var port int
	err := p.db.QueryRowxContext(ctx, `
		SELECT p
		FROM generate_series($1::int, $2::int) AS p
		WHERE NOT EXISTS (SELECT 1 FROM blockyard_ports b WHERE b.port = p)
		ORDER BY p
		LIMIT 1`,
		from, p.last,
	).Scan(&port)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return 0, err
	}
	return port, nil
}

func (p *postgresPortAllocator) deleteRow(port int) {
	ctx := context.Background()
	if _, err := p.db.ExecContext(ctx,
		`DELETE FROM blockyard_ports WHERE port = $1 AND owner = $2`,
		port, p.hostname,
	); err != nil {
		slog.Error("postgres port delete", "port", port, "error", err)
	}
}
