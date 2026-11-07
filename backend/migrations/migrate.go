// Package migrations runs embedded, versioned SQL with goose.
package migrations

import (
	"context"
	"embed"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed [0-9]*.sql
var files embed.FS

// Apply keeps database/sql confined to goose; application transactions use native pgx.
func Apply(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	locker, err := lock.NewPostgresSessionLocker(lock.WithLockID(7741001))
	if err != nil {
		return err
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files,
		goose.WithSessionLocker(locker), goose.WithDisableGlobalRegistry(true))
	if err != nil {
		return err
	}
	_, err = provider.Up(ctx)
	return err
}
