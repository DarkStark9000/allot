// Package store keeps orders, their history, their ledger, and their messages in PostgreSQL.
//
// Every change to an order happens in one transaction through Tx.Apply, which writes the
// order, one history row, its ledger postings, and its outbox messages together.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// ErrNotFound reports a row that does not exist.
var ErrNotFound = errors.New("store: not found")

// DB is a pool of connections to the allot database.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to the database at url and checks the connection.
func Open(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close closes every connection.
func (db *DB) Close() { db.pool.Close() }

// Ping checks that the database answers.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// Tx is one database transaction.
type Tx struct {
	tx pgx.Tx
}

// WithTx runs fn in a transaction. It commits when fn returns nil and rolls back otherwise.
func (db *DB) WithTx(ctx context.Context, fn func(*Tx) error) error {
	return pgx.BeginFunc(ctx, db.pool, func(tx pgx.Tx) error { return fn(&Tx{tx: tx}) })
}

// migrationLock is the advisory lock key that serializes migrations across processes.
const migrationLock = 4_202_610

// Migrate applies every embedded migration that has not been applied yet.
func (db *DB) Migrate(ctx context.Context) error {
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("store: migrations: %w", err)
	}
	slices.Sort(names)
	return db.WithTx(ctx, func(t *Tx) error {
		if _, err := t.tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLock); err != nil {
			return err
		}
		if _, err := t.tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version int PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
			return err
		}
		for _, name := range names {
			base := strings.TrimPrefix(name, "migrations/")
			version, err := strconv.Atoi(strings.SplitN(base, "_", 2)[0])
			if err != nil {
				return fmt.Errorf("store: migration %s: name must start with a number", base)
			}
			tag, err := t.tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				continue
			}
			sql, err := migrations.ReadFile(name)
			if err != nil {
				return err
			}
			if _, err := t.tx.Exec(ctx, string(sql)); err != nil {
				return fmt.Errorf("store: migration %s: %w", base, err)
			}
		}
		return nil
	})
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
