package db

import (
	"errors"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// IsUniqueConstraintError reports whether err is a unique constraint
// violation, regardless of dialect.
func IsUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	// PostgreSQL: error code 23505 (unique_violation)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}

	// SQLite: string match
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}
