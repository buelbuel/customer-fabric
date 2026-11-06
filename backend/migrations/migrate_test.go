package migrations

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL unset: real PostgreSQL migration tests not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("migration_test_%x", random)
	if _, err = admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	return pool
}

func legacySchema(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	original, err := files.ReadFile("001_foundation.sql")
	if err != nil {
		t.Fatal(err)
	}
	// The initial SQL DDL is unchanged from the legacy release; goose markers
	// and a Down section are the only additions.
	originalUp := strings.Split(string(original), "-- +goose Down")[0]
	_, err = pool.Exec(context.Background(), originalUp+`
 CREATE TABLE schema_migrations(version integer PRIMARY KEY,applied_at timestamptz NOT NULL DEFAULT now());
 INSERT INTO schema_migrations(version) VALUES(1);`)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLegacyAdoptionPreservesData(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	legacySchema(t, pool)
	_, err := pool.Exec(ctx, `
 INSERT INTO sources(name) VALUES('console');
 INSERT INTO profiles(id,version,attributes) VALUES('00000000-0000-4000-8000-000000000001',1,'{"name":"Synthetic"}');
 INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('console','crm','synthetic','00000000-0000-4000-8000-000000000001');
 INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result) VALUES('00000000-0000-4000-8000-000000000002','console','legacy-request-001','hash','00000000-0000-4000-8000-000000000001',201,'{"version":1}');
 INSERT INTO attribute_ownership(profile_id,path,source,tombstone) VALUES('00000000-0000-4000-8000-000000000001','removed','console',true);
 INSERT INTO provenance(profile_id,path,source,mutation_id,outcome,rule) VALUES('00000000-0000-4000-8000-000000000001','removed','console','00000000-0000-4000-8000-000000000002','accepted','first-writer-owns');
 INSERT INTO outbox(id,type,profile_id,version,source,payload) VALUES('00000000-0000-4000-8000-000000000002','profile.created.v1','00000000-0000-4000-8000-000000000001',1,'console','{"version":1}');`)
	if err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, pool)
	for i := 0; i < 2; i++ {
		if err = Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	if after := snapshot(t, pool); after != before {
		t.Fatalf("legacy customer data changed\nbefore %s\nafter %s", before, after)
	}
	var versions int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=1 AND is_applied`).Scan(&versions); err != nil || versions != 1 {
		t.Fatalf("baseline duplicated or missing: %d %v", versions, err)
	}
}

func snapshot(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var result strings.Builder
	for _, table := range []string{"sources", "profiles", "external_identifiers", "mutations", "attribute_ownership", "provenance", "outbox", "schema_migrations"} {
		var data string
		if err := pool.QueryRow(context.Background(), "SELECT coalesce(jsonb_agg(to_jsonb(t) ORDER BY to_jsonb(t)::text),'[]'::jsonb)::text FROM "+table+" t").Scan(&data); err != nil {
			t.Fatal(err)
		}
		result.WriteString(data)
	}
	return result.String()
}

func TestLegacyAdoptionRejectsUnknownAndPartialSchema(t *testing.T) {
	for _, mutation := range []string{`INSERT INTO schema_migrations(version) VALUES(2)`, `ALTER TABLE profiles DROP COLUMN attributes`} {
		t.Run(mutation, func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			legacySchema(t, pool)
			if _, err := pool.Exec(ctx, mutation); err != nil {
				t.Fatal(err)
			}
			if err := Apply(ctx, pool); err == nil {
				t.Fatal("accepted incompatible legacy schema")
			}
			var exists bool
			if err := pool.QueryRow(ctx, `SELECT to_regclass('goose_db_version') IS NOT NULL`).Scan(&exists); err != nil || exists {
				t.Fatalf("failed adoption left a goose ledger: %v %v", exists, err)
			}
		})
	}
}

func TestConcurrentMigrationRunners(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint("legacy=", legacy), func(t *testing.T) {
			pool := testPool(t)
			ctx := context.Background()
			if legacy {
				legacySchema(t, pool)
			}
			var wg sync.WaitGroup
			results := make(chan error, 3)
			for i := 0; i < 3; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); results <- Apply(ctx, pool) }()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			var n int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=1 AND is_applied`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("migration not serialized: %d %v", n, err)
			}
		})
	}
}

func TestGooseDownUpAndMigrationRollback(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT to_regclass('profiles') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("down failed", err)
	}
	if err = Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	initial, err := files.ReadFile("001_foundation.sql")
	if err != nil {
		t.Fatal(err)
	}
	broken := fstest.MapFS{
		"001_foundation.sql": {Data: initial},
		"002_broken.sql":     {Data: []byte("-- +goose Up\nCREATE TABLE rollback_probe(id integer);\nSELECT 1/0;\n-- +goose Down\nDROP TABLE rollback_probe;\n")},
	}
	provider, err = goose.NewProvider(goose.DialectPostgres, db, broken, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err == nil {
		t.Fatal("broken migration succeeded")
	}
	if err = pool.QueryRow(ctx, `SELECT to_regclass('rollback_probe') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("failed migration left partial DDL", err)
	}
	var version int64
	if version, err = provider.GetDBVersion(ctx); err != nil || version != 1 {
		t.Fatal("failed migration advanced ledger", version, err)
	}
}
