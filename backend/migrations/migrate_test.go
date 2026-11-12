package migrations

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
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

func TestFreshInitializationAndRepeat(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := Apply(ctx, pool); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=1 AND is_applied`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("baseline missing or duplicated: %d %v", n, err)
	}
	for _, table := range []string{"sources", "profiles", "external_identifiers", "mutations", "attribute_ownership", "provenance", "outbox", "customer_identity_links", "linking_grants", "verified_associations"} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil || !exists {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
}

func TestUnexpectedExistingSchemaFailsWithoutDestruction(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TABLE sources(marker text); INSERT INTO sources VALUES('preserve-me')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool); err == nil {
		t.Fatal("accepted incompatible schema")
	}
	var marker string
	if err := pool.QueryRow(ctx, `SELECT marker FROM sources`).Scan(&marker); err != nil || marker != "preserve-me" {
		t.Fatal("existing data changed", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM goose_db_version WHERE version_id=1 AND is_applied`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed baseline recorded", err)
	}
}

func TestConcurrentMigrationRunners(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
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
	if _, err = provider.DownTo(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err = pool.QueryRow(ctx, `SELECT to_regclass('profiles') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("down failed", err)
	}
	if err = pool.QueryRow(ctx, `SELECT to_regclass('customer_identity_links') IS NOT NULL`).Scan(&exists); err != nil || exists {
		t.Fatal("identity link down failed", err)
	}
	if err = Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	// Build a broken migrations set from the real numbered files plus one failing migration, so
	// this test keeps working as migrations are added.
	broken := fstest.MapFS{}
	var highest int
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		data, err := files.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		broken[name] = &fstest.MapFile{Data: data}
	}
	for name := range broken {
		var n int
		if _, err := fmt.Sscanf(name, "%03d", &n); err == nil && n > highest {
			highest = n
		}
	}
	brokenName := fmt.Sprintf("%03d_broken.sql", highest+1)
	broken[brokenName] = &fstest.MapFile{Data: []byte("-- +goose Up\nCREATE TABLE rollback_probe(id integer);\nSELECT 1/0;\n-- +goose Down\nDROP TABLE rollback_probe;\n")}
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
	if version, err = provider.GetDBVersion(ctx); err != nil || version != int64(highest) {
		t.Fatal("failed migration advanced ledger", version, err)
	}
}

func TestCreatorBackfillWithoutOutbox(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO sources VALUES ('crm');
		INSERT INTO profiles(id,version,attributes) VALUES
			('00000000-0000-4000-8000-000000000001', 1, '{}'),
			('00000000-0000-4000-8000-000000000002', 1, '{}');
		INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result)
		VALUES ('00000000-0000-4000-8000-000000000003','crm','synthetic-backfill','hash',
			'00000000-0000-4000-8000-000000000001',201,'{}');
	`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var owner, unproven string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(MAX(creator_source) FILTER (WHERE id='00000000-0000-4000-8000-000000000001'),''),
		       COALESCE(MAX(creator_source) FILTER (WHERE id='00000000-0000-4000-8000-000000000002'),'')
		FROM profiles
	`).Scan(&owner, &unproven); err != nil || owner != "crm" || unproven != "" {
		t.Fatalf("unexpected creator backfill: %q / %q: %v", owner, unproven, err)
	}
}

// Upgrading keeps every plaintext signing secret, and rolling back refuses while any destination lacks
// one, so a downgrade never drops the only copy of a secret.
func TestDestinationSecretMigrationKeepsSecrets(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 30); err != nil {
		t.Fatal(err)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO destinations (name, url, signing_secret) VALUES ('legacy', 'https://hooks.example.test:443/x', $1)`, secret); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var kept []byte
	if err := pool.QueryRow(ctx, `SELECT signing_secret FROM destinations WHERE name = 'legacy'`).Scan(&kept); err != nil || string(kept) != string(secret) {
		t.Fatalf("the upgrade changed a plaintext secret: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO destinations (name, url) VALUES ('sealed_only', 'https://hooks.example.test:443/y')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 30); err == nil {
		t.Fatal("rolled back while a destination had no plaintext secret")
	}
	if _, err := pool.Exec(ctx, `UPDATE destinations SET signing_secret = $1 WHERE name = 'sealed_only'`, secret[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT signing_secret FROM destinations WHERE name = 'legacy'`).Scan(&kept); err != nil || string(kept) != string(secret) {
		t.Fatalf("the rollback changed a plaintext secret: %v", err)
	}
}
