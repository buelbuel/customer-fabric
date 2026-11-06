package migrations

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pressly/goose/v3/database"
)

// adoptLegacy is a one-time compatibility bridge for the original M0 ledger.
// It never executes schema DDL from a migration or touches customer data. All
// future version tracking and SQL execution belong to goose's built-in store.
func adoptLegacy(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Same lock as both the original runner and the goose provider.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7741001)`); err != nil {
		return err
	}
	var legacy, adopted bool
	err = tx.QueryRowContext(ctx, `SELECT
  EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='schema_migrations'),
  EXISTS(SELECT 1 FROM information_schema.tables WHERE table_schema=current_schema() AND table_name='goose_db_version')`).Scan(&legacy, &adopted)
	if err != nil {
		return err
	}
	if !legacy || adopted {
		return tx.Commit()
	}
	var valid bool
	if err = tx.QueryRowContext(ctx, `SELECT count(*)=1 AND coalesce(min(version)=1,false) FROM schema_migrations`).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return fmt.Errorf("cannot adopt unknown legacy migration history")
	}
	// Reject incomplete or incompatible legacy table shapes instead of silently
	// marking a fresh or partial database as migrated. Only the original, unmodified
	// M0 schema is supported; this is not a general schema drift reconciler.
	rows, err := tx.QueryContext(ctx, `SELECT s.name,
  p.id,p.version,p.attributes,p.created_at,p.updated_at,
  e.source,e.namespace,e.value,e.profile_id,
  m.id,m.source,m.key,m.request_hash,m.profile_id,m.status,m.result,m.created_at,
  a.profile_id,a.path,a.source,a.tombstone,
  v.sequence,v.profile_id,v.path,v.source,v.mutation_id,v.at,v.outcome,v.rule,
  o.id,o.type,o.profile_id,o.version,o.source,o.occurred_at,o.payload
  FROM sources s, profiles p, external_identifiers e, mutations m,
       attribute_ownership a, provenance v, outbox o LIMIT 0`)
	if err != nil {
		return fmt.Errorf("incompatible legacy schema: %w", err)
	}
	rows.Close()
	store, err := database.NewStore(database.DialectPostgres, "goose_db_version")
	if err != nil {
		return err
	}
	if err = store.CreateVersionTable(ctx, tx); err != nil {
		return err
	}
	for _, version := range []int64{0, 1} {
		if err = store.Insert(ctx, tx, database.InsertRequest{Version: version}); err != nil {
			return err
		}
	}
	// Preserve schema_migrations as read-only historical evidence.
	return tx.Commit()
}
