// Package database owns the PostgreSQL connection pool and schema migration.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/Keathan2609/nexus/internal/store/migrations"
)

type Config struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
}

func DefaultConfig(url string) Config {
	return Config{
		URL: url,
		// Sized for the control plane rather than for a web app. Workers hold a
		// connection only for the duration of a claim or a status write, so a modest
		// pool serves a large worker fleet; what matters more is that the ceiling is
		// low enough that N replicas cannot exhaust Postgres max_connections.
		MaxConns:        16,
		MinConns:        2,
		MaxConnLifetime: 30 * time.Minute,
		MaxConnIdleTime: 5 * time.Minute,
	}
}

type DB struct {
	*pgxpool.Pool
}

// Open establishes the pool and verifies it can actually reach the database. It does not
// return a half-usable handle: if the ping fails, so does Open.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	poolCfg.MaxConns = cfg.MaxConns
	poolCfg.MinConns = cfg.MinConns
	poolCfg.MaxConnLifetime = cfg.MaxConnLifetime
	poolCfg.MaxConnIdleTime = cfg.MaxConnIdleTime

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

func (db *DB) Close() { db.Pool.Close() }

// Migrate applies every pending migration.
//
// goose takes an advisory lock for the duration, so several API replicas starting at
// once cannot apply the same migration twice — the losers block, then observe there is
// nothing left to do. That is what makes running this on every boot safe.
func Migrate(ctx context.Context, url string) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}
	goose.SetLogger(goose.NopLogger())

	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return fmt.Errorf("open migration connection: %w", err)
	}
	defer sqlDB.Close()

	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// MigrateDown rolls back a single migration. Used by tests and by an operator with a
// reason; never called automatically.
func MigrateDown(ctx context.Context, url string) error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	goose.SetLogger(goose.NopLogger())
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	return goose.DownContext(ctx, sqlDB, ".")
}

// Version reports the currently applied schema version.
func Version(ctx context.Context, url string) (int64, error) {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return 0, err
	}
	goose.SetLogger(goose.NopLogger())
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return 0, err
	}
	defer sqlDB.Close()
	return goose.GetDBVersionContext(ctx, sqlDB)
}

// ensure the pgx stdlib driver is linked in for the migration path above.
var _ = stdlib.GetDefaultDriver
