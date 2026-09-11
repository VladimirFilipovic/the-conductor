package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"conductor/internal/storage/db"

	"github.com/jackc/pgx/v5/pgconn"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var ErrNotFound = errors.New("not found")

var ErrExists = errors.New("already exists")

// maskDSN redacts the password component of a postgres DSN so it's safe to log.
func maskDSN(dsn string) string {
	// Best-effort: a DSN that doesn't match falls through unmasked.
	if i := len("postgres://"); len(dsn) > i {
		if at := findByte(dsn[i:], '@'); at >= 0 {
			if colon := findByte(dsn[i:i+at], ':'); colon >= 0 {
				return "postgres://" + dsn[i:i+colon+1] + "***" + dsn[i+at:]
			}
		}
	}
	return dsn
}

func findByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// uniqueViolation reports whether err is a Postgres unique-constraint failure
// (SQLSTATE 23505), letting Insert paths map a duplicate to ErrExists without a
// separate existence probe racing the write.
func uniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// fkViolation reports whether err is a Postgres foreign-key failure (SQLSTATE
// 23503), i.e. a referenced parent row does not exist, so Insert paths can map
// a missing parent to ErrNotFound instead of leaking a driver error.
func fkViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

type querier struct {
	queries *db.Queries
}

// PostgresClient is the Querier backed by a database/sql pool over the pgx
// driver, plus the entry points that scope one to a transaction. SQL lives in
// db/queries, compiled by sqlc (the db package); this type only marshals values
// and maps results onto the sentinel errors.
type PostgresClient struct {
	querier
	pool *sql.DB
}

// NewPostgresClient opens a pool to dsn and verifies the connection. The schema
// is expected to already exist — run `make migrate` first. dsn is a libpq/pgx
// connection string or URL, e.g. "postgres://user:pass@host:5432/conductor".
func NewPostgresClient(ctx context.Context, dsn string) (*PostgresClient, error) {
	// sql.Open is lazy and never errors on a bad host; Ping forces a real
	// connection so construction fails fast on an unreachable database.
	pool, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("storage: open: %w", err)
	}
	if err := pool.PingContext(ctx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("storage: connect: %w", err)
	}
	slog.Info("storage: connected", "dsn", maskDSN(dsn))
	return &PostgresClient{querier: querier{queries: db.New(pool)}, pool: pool}, nil
}

// Close releases the underlying connection pool.
func (c *PostgresClient) Close() error { return c.pool.Close() }

// WithTx runs fn against a tx-scoped Querier and commits if it returns nil. Any
// error from fn (or commit) rolls the whole unit back, so multi-step workflows
// in the project package are all-or-nothing. The Querier must not be retained
// after fn returns, and nesting WithTx is not supported.
func (c *PostgresClient) WithTx(ctx context.Context, fn func(Querier) error) error {
	return c.withTx(ctx, nil, func(q querier) error { return fn(q) })
}

// WithReadTx runs fn against a tx-scoped Querier at REPEATABLE READ, so every
// read fn issues observes one frozen instant: the desired and observed halves
// of a reconcile pass can't drift between queries and tear the diff. The tx is
// read-only — it takes no row locks and exists only to pin the snapshot, so it
// always rolls back regardless of fn's error.
func (c *PostgresClient) WithReadTx(ctx context.Context, fn func(Querier) error) error {
	opts := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	return c.withTx(ctx, opts, func(q querier) error { return fn(q) })
}

// withTx is the shared begin/commit/rollback plumbing behind both With*Tx entry
// points; they differ only in isolation level and whether commit is attempted.
func (c *PostgresClient) withTx(ctx context.Context, opts *sql.TxOptions, fn func(querier) error) error {
	tx, err := c.pool.BeginTx(ctx, opts)
	if err != nil {
		return fmt.Errorf("storage: begin tx: %w", err)
	}
	// No-op after a successful Commit; guarantees rollback on error or panic.
	defer func() { _ = tx.Rollback() }()

	if err := fn(querier{queries: c.queries.WithTx(tx)}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("storage: commit: %w", err)
	}
	return nil
}
