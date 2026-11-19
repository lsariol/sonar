// Package store is Sonar's history in Postgres: migrations, the agent's writes, the API's reads.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a connection pool.
type Store struct {
	pool *pgxpool.Pool
}

// Connect opens a pool and checks the database answers; key names the Cove key for errors.
func Connect(ctx context.Context, connString, key string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("database: %s isn't a valid connection string: %w", key, err)
	}
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: %s: %w", key, err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: can't reach the database at %s: %w", key, err)
	}
	return &Store{pool: pool}, nil
}

// Ping checks that the database answers, giving up after 2 seconds.
func (s *Store) Ping(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return s.pool.Ping(ctx)
}

func (s *Store) Close() {
	s.pool.Close()
}

// IsUnreachable reports whether the database couldn't be reached at all, not an error from it.
func IsUnreachable(err error) bool {
	var connectErr *pgconn.ConnectError
	if !errors.As(err, &connectErr) {
		return false
	}
	var pgErr *pgconn.PgError
	return !errors.As(err, &pgErr)
}
