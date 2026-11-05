// Package migrations embeds immutable, versioned SQL migrations.
package migrations

import (
	"context"
	"embed"
	"fmt"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed *.sql
var files embed.FS

func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7741001)`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=1)`).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		b, e := files.ReadFile("001_foundation.sql")
		if e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, string(b)); e != nil {
			return fmt.Errorf("migration 1: %w", e)
		}
		if _, e = tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES(1)`); e != nil {
			return e
		}
	}
	return tx.Commit(ctx)
}
