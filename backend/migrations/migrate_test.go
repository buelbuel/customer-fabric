package migrations

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

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
	for _, table := range []string{"sources", "profiles", "external_identifiers", "mutations", "attribute_contributions", "provenance", "outbox", "customer_identity_links", "linking_grants", "verified_associations"} {
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

// Sealed signing secrets are an unreleased schema change with no migration path: the migration refuses
// to drop destinations stored with a plaintext secret, and leaves them as they were.
func TestDestinationSecretMigrationNeverDropsDestinations(t *testing.T) {
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
	if err := Apply(ctx, pool); err == nil {
		t.Fatal("dropped a destination's plaintext secret")
	}
	var kept []byte
	if err := pool.QueryRow(ctx, `SELECT signing_secret FROM destinations WHERE name = 'legacy'`).Scan(&kept); err != nil || string(kept) != string(secret) {
		t.Fatalf("the refused migration changed a destination: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM destinations`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO destinations (name, url) VALUES ('sealed', 'https://hooks.example.test:443/y')`); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 30); err == nil {
		t.Fatal("rolled back over a destination whose secret exists only sealed")
	}
}

// The commit fence keeps every decision existing rows' times gave: for each change, the subscriptions
// selected by commit epoch are exactly those its time selected before, ties and suspensions included.
func TestCommitFenceBackfillKeepsTimeDecisions(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, files, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 32); err != nil {
		t.Fatal(err)
	}
	at := func(minute int) time.Time { return time.Date(2026, 11, 1, 10, minute, 0, 0, time.UTC) }
	for _, stmt := range []string{
		`INSERT INTO sources (name) VALUES ('console')`,
		`INSERT INTO destinations (name, url) VALUES ('d', 'https://hooks.example.test:443/x')`,
		`INSERT INTO consent_purposes (id, name) VALUES ('11111111-1111-4111-8111-111111111111', 'sync')`,
		`INSERT INTO consent_policy_versions (id, purpose_id, version, state, lawful_basis, notice_reference, jurisdiction, locale, channel, retention_reference, explanation)
		 VALUES ('22222222-2222-4222-8222-222222222222', '11111111-1111-4111-8111-111111111111', 1, 'active', 'legitimate_interests', 'n', 'DE', 'de-DE', 'email', 'r', 'Synthetic.')`,
		`INSERT INTO profiles (id, version, attributes, creator_source) VALUES ('33333333-3333-4333-8333-333333333333', 7, '{}', 'console')`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	subscription := func(id, name, state string, created time.Time, suspended *time.Time, replaces *string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO notification_subscriptions (id, name, destination, purpose_id, policy_version_id, basis_assessment, recipient_role,
		  event_types, data_categories, sources, created_by, state, created_at, suspended_at, replaces)
		  VALUES ($1, $2, 'd', '11111111-1111-4111-8111-111111111111', '22222222-2222-4222-8222-222222222222', 'LIA-1', 'processor',
		  '{profile.updated.v1}', '{record_reference}', '{console}', 'migration-test', $3, $4, $5, $6)`, id, name, state, created, suspended, replaces); err != nil {
			t.Fatal(err)
		}
	}
	suspended, replaced := at(20), "a0000000-0000-4000-8000-00000000000b"
	subscription("a0000000-0000-4000-8000-00000000000a", "first", "active", at(0), nil, nil)
	subscription(replaced, "second", "superseded", at(0), &suspended, nil)
	subscription("a0000000-0000-4000-8000-00000000000c", "third", "active", at(30), nil, &replaced)
	for i, minute := range []int{-1, 0, 10, 20, 25, 30, 35} {
		if _, err := pool.Exec(ctx, `INSERT INTO outbox (id, type, profile_id, version, source, occurred_at, payload)
		  VALUES (gen_random_uuid(), 'profile.updated.v1', '33333333-3333-4333-8333-333333333333', $1, 'console', $2, '{}')`, i+1, at(minute)); err != nil {
			t.Fatal(err)
		}
	}
	type decision struct {
		version int64
		ids     string
	}
	decide := func(predicate string) []decision {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT o.version, coalesce(string_agg(s.id::text, ',' ORDER BY s.id), '') FROM outbox o
		  LEFT JOIN notification_subscriptions s ON `+predicate+` GROUP BY o.version ORDER BY o.version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []decision
		for rows.Next() {
			var d decision
			if err := rows.Scan(&d.version, &d.ids); err != nil {
				t.Fatal(err)
			}
			out = append(out, d)
		}
		return out
	}
	byTime := decide(`s.created_at <= o.occurred_at AND (s.state IN ('active', 'needs_reapproval')
	  OR s.state = 'superseded' AND (s.suspended_at IS NULL OR s.suspended_at > o.occurred_at) AND EXISTS (
	    SELECT 1 FROM notification_subscriptions n WHERE n.replaces = s.id AND n.created_at > o.occurred_at))`)
	if err := Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	byEpoch := decide(`s.created_epoch <= o.commit_epoch AND (s.state IN ('active', 'needs_reapproval')
	  OR s.state = 'superseded' AND (s.suspended_epoch IS NULL OR s.suspended_epoch > o.commit_epoch) AND EXISTS (
	    SELECT 1 FROM notification_subscriptions n WHERE n.replaces = s.id AND n.created_epoch > o.commit_epoch))`)
	if !slices.Equal(byTime, byEpoch) || len(byTime) != 7 || byTime[0].ids != "" || byTime[6].ids == byTime[2].ids {
		t.Fatalf("decisions changed:\n by time  %v\n by epoch %v", byTime, byEpoch)
	}
	var epoch int64
	if err := pool.QueryRow(ctx, `SELECT epoch FROM commit_epoch`).Scan(&epoch); err != nil || epoch != 3 {
		t.Fatalf("epoch %d %v", epoch, err)
	}
}
