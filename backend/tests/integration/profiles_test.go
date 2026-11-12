// Integration tests use a unique schema in the configured PostgreSQL database.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/customer-fabric/customer-fabric/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tokenA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const tokenB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

type suite struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    http.Handler
}

func setup(t *testing.T) *suite {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL unset: real PostgreSQL integration tests not run")
	}
	ctx := context.Background()
	admin, e := pgxpool.New(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	schema := "test_" + strings.ReplaceAll(platform.UUID(), "-", "")
	if _, e = admin.Exec(ctx, `CREATE SCHEMA `+schema); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		pool.Close()
		_, e := admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
		if e != nil {
			t.Error(e)
		}
	})
	if e = migrations.Apply(ctx, pool); e != nil {
		t.Fatal(e)
	}
	if e = migrations.Apply(ctx, pool); e != nil {
		t.Fatal("migration not repeatable", e)
	}
	if _, e = pool.Exec(ctx, `INSERT INTO sources(name) VALUES('console'),('importer')`); e != nil {
		t.Fatal(e)
	}
	h, e := platform.NewAPI(platform.Store{Pool: pool}, platform.Config{Mode: "development", OperatorToken: viewOperator, Tokens: map[string]string{"console": tokenA, "importer": tokenB}})
	if e != nil {
		t.Fatal(e)
	}
	return &suite{t, pool, h}
}
func (s *suite) request(method, path, body, key, token, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	if etag != "" {
		r.Header.Set("If-Match", etag)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}
func (s *suite) expect(w *httptest.ResponseRecorder, status int) {
	s.t.Helper()
	if w.Code != status {
		s.t.Fatalf("expected %d got %d: %s", status, w.Code, w.Body.String())
	}
}
func (s *suite) create() profiles.Receipt {
	s.t.Helper()
	w := s.request("POST", "/v1/profiles", `{"set":{"name":"Synthetic Ada"},"identifiers":[{"namespace":"crm","value":"synthetic-1"}]}`, platform.UUID(), tokenA, "")
	s.expect(w, 201)
	var r profiles.Receipt
	if e := json.Unmarshal(w.Body.Bytes(), &r); e != nil {
		s.t.Fatal(e)
	}
	return r
}

// contribute authorizes source, through the identity reviewer's API and on a recorded verification, to
// contribute to a profile it did not create (ADR 0018).
func contribute(t *testing.T, h http.Handler, operator, profile, source string) {
	t.Helper()
	body := fmt.Sprintf(`{"source":%q,"basis":"verified_reference","verification_reference":"test:verified-%s"}`, source, source)
	r := httptest.NewRequest("POST", "/v1/operator/profiles/"+profile+"/contributors", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+operator)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatalf("authorize contributor: %d %s", w.Code, w.Body.String())
	}
}

func (s *suite) count(table string) int {
	s.t.Helper()
	var n int
	if e := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); e != nil {
		s.t.Fatal(e)
	}
	return n
}
func TestProfileLifecycle(t *testing.T) {
	s := setup(t)
	r := s.create()
	path := "/v1/profiles/" + r.ID
	w := s.request("GET", path, "", "", tokenA, "")
	s.expect(w, 200)
	var p profiles.Profile
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Version != 1 || len(p.Identifiers) != 1 || len(p.Provenance) != 1 {
		t.Fatalf("unexpected profile %+v", p)
	}
	w = s.request("PATCH", path, `{"set":{"name":"Synthetic Grace"}}`, platform.UUID(), tokenA, `"1"`)
	s.expect(w, 200)
	w = s.request("GET", path, "", "", tokenA, "")
	s.expect(w, 200)
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.Version != 2 || string(p.Attributes["name"]) != `"Synthetic Grace"` || len(p.Provenance) != 2 {
		t.Fatal("update or provenance inconsistent")
	}
	if s.count("outbox") != 2 || s.count("mutations") != 2 {
		t.Fatal("outbox not atomic")
	}
	w = s.request("GET", "/v1/profiles?limit=1", "", "", tokenA, "")
	s.expect(w, 200)
}
func TestIdempotencyAndConflict(t *testing.T) {
	s := setup(t)
	key := platform.UUID()
	body := `{"set":{"name":"Synthetic"}}`
	w := s.request("POST", "/v1/profiles", body, key, tokenA, "")
	s.expect(w, 201)
	replay := s.request("POST", "/v1/profiles", body, key, tokenA, "")
	s.expect(replay, 201)
	var first, second any
	_ = json.Unmarshal(w.Body.Bytes(), &first)
	_ = json.Unmarshal(replay.Body.Bytes(), &second)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if !bytes.Equal(a, b) || s.count("profiles") != 1 || s.count("outbox") != 1 {
		t.Fatal("duplicate effects or inconsistent receipt")
	}
	s.expect(s.request("POST", "/v1/profiles", `{"set":{"name":"Different"}}`, key, tokenA, ""), 409)
}
func TestIdentifierRollback(t *testing.T) {
	s := setup(t)
	s.create()
	before := s.count("provenance")
	s.expect(s.request("POST", "/v1/profiles", `{"set":{"name":"Other"},"identifiers":[{"namespace":"crm","value":"synthetic-1"}]}`, platform.UUID(), tokenA, ""), 409)
	if s.count("profiles") != 1 || s.count("mutations") != 1 || s.count("outbox") != 1 || s.count("provenance") != before {
		t.Fatal("conflicting identifier left partial state")
	}
	// Another source has an independent identifier namespace.
	s.expect(s.request("POST", "/v1/profiles", `{"identifiers":[{"namespace":"crm","value":"synthetic-1"}]}`, platform.UUID(), tokenB, ""), 201)
}
func TestOwnershipAndTombstones(t *testing.T) {
	s := setup(t)
	r := s.create()
	path := "/v1/profiles/" + r.ID
	key := platform.UUID()
	body := `{"set":{"name":"Rejected","new_field":"also rejected"}}`
	// Knowing the profile ID grants nothing: another source needs a verified authorization.
	s.expect(s.request("PATCH", path, body, platform.UUID(), tokenB, `"1"`), 403)
	contribute(t, s.h, viewOperator, r.ID, "importer")
	s.expect(s.request("PATCH", path, body, key, tokenB, `"1"`), 409)
	s.expect(s.request("PATCH", path, body, key, tokenB, `"1"`), 409)
	if s.count("provenance") != 2 || s.count("outbox") != 1 {
		t.Fatal("rejection audit duplicated or emitted outbox")
	}
	p, e := (platform.Store{Pool: s.pool}).Get(context.Background(), r.ID)
	if e != nil {
		t.Fatal(e)
	}
	if p.Version != 1 || len(p.Attributes) != 1 || p.Provenance[0].Outcome != "rejected" {
		t.Fatal("ownership conflict changed state")
	}
	s.expect(s.request("PATCH", path, `{"remove":["name"]}`, platform.UUID(), tokenA, `"1"`), 200)
	s.expect(s.request("PATCH", path, `{"set":{"name":"Resurrected"}}`, platform.UUID(), tokenB, `"2"`), 409)
	p, e = (platform.Store{Pool: s.pool}).Get(context.Background(), r.ID)
	if e != nil || len(p.Attributes) != 0 {
		t.Fatal("tombstone not enforced")
	}
}
func TestConcurrentUpdates(t *testing.T) {
	s := setup(t)
	r := s.create()
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.request("PATCH", "/v1/profiles/"+r.ID, `{"set":{"name":"Concurrent"}}`, platform.UUID(), tokenA, `"1"`)
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[200] != 1 || counts[412] != 1 || s.count("outbox") != 2 {
		t.Fatalf("lost-update protection failed: %v", counts)
	}
}
func TestConcurrentDuplicateCreation(t *testing.T) {
	s := setup(t)
	key := platform.UUID()
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.request("POST", "/v1/profiles", `{"set":{"name":"Duplicate"}}`, key, tokenA, "").Code
		}()
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 201 {
			t.Fatalf("status %d", c)
		}
	}
	if s.count("profiles") != 1 || s.count("outbox") != 1 {
		t.Fatal("concurrent duplicate effects")
	}
}
func TestOutboxFailureRollsBack(t *testing.T) {
	s := setup(t)
	r := s.create()
	ctx := context.Background()
	_, e := s.pool.Exec(ctx, `CREATE FUNCTION reject_outbox() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected failure'; END $$; CREATE TRIGGER fail_outbox BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_outbox()`)
	if e != nil {
		t.Fatal(e)
	}
	s.expect(s.request("PATCH", "/v1/profiles/"+r.ID, `{"set":{"name":"Must roll back"}}`, platform.UUID(), tokenA, `"1"`), 500)
	if s.count("mutations") != 1 || s.count("provenance") != 1 || s.count("outbox") != 1 {
		t.Fatal("partial transaction survived")
	}
	p, e := (platform.Store{Pool: s.pool}).Get(ctx, r.ID)
	if e != nil || p.Version != 1 {
		t.Fatal("state survived failed outbox")
	}
}
func TestValidationAndAuthorization(t *testing.T) {
	s := setup(t)
	cases := []struct {
		body   string
		status int
	}{{`{"source":"console","set":{"name":"X"}}`, 400}, {`{"set":{"list":[1]}}`, 422}, {`{"set":{"a":1,"a":2}}`, 400}, {`null`, 422}, {`{}`, 422}, {`{"set":{"name":"` + strings.Repeat("x", 70000) + `"}}`, 413}}
	for _, c := range cases {
		s.expect(s.request("POST", "/v1/profiles", c.body, platform.UUID(), tokenA, ""), c.status)
	}
	s.expect(s.request("POST", "/v1/profiles", `{"set":{"name":"X"}}`, platform.UUID(), "bad", ""), 401)
	r := s.create()
	s.expect(s.request("PATCH", "/v1/profiles/"+r.ID, `{"set":{"name":"X"}}`, platform.UUID(), tokenA, ""), 428)
	s.expect(s.request("GET", "/v1/profiles?limit=999", "", "", tokenA, ""), 400)
	s.expect(s.request("GET", "/v1/profiles/"+platform.UUID(), "", "", tokenA, ""), 404)
}
func TestCursorPagination(t *testing.T) {
	s := setup(t)
	for i := 0; i < 3; i++ {
		s.expect(s.request("POST", "/v1/profiles", fmt.Sprintf(`{"set":{"n":%d}}`, i), platform.UUID(), tokenA, ""), 201)
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		w := s.request("GET", "/v1/profiles?limit=1&cursor="+cursor, "", "", tokenA, "")
		s.expect(w, 200)
		var p platform.Page
		_ = json.Unmarshal(w.Body.Bytes(), &p)
		for _, i := range p.Items {
			if seen[i.ID] {
				t.Fatal("duplicate page record")
			}
			seen[i.ID] = true
		}
		cursor = p.NextCursor
		if cursor == "" {
			break
		}
	}
	if len(seen) != 3 {
		t.Fatal("pagination skipped records")
	}
}

func TestMutationReplayAfterLaterUpdatesAndSourceScope(t *testing.T) {
	s := setup(t)
	r := s.create()
	path := "/v1/profiles/" + r.ID
	key := platform.UUID()
	body := `{"set":{"name":"First update"}}`
	first := s.request("PATCH", path, body, key, tokenA, `"1"`)
	s.expect(first, 200)
	s.expect(s.request("PATCH", path, `{"set":{"name":"Later update"}}`, platform.UUID(), tokenA, `"2"`), 200)
	replay := s.request("PATCH", path, body, key, tokenA, `"1"`)
	s.expect(replay, 200)
	var a, b profiles.Receipt
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(replay.Body.Bytes(), &b)
	if a != b || s.count("outbox") != 3 {
		t.Fatal("replay changed state or receipt")
	}
	// The same key in another source's namespace grants nothing: without an authorization the source
	// cannot reach the profile, and with one it still cannot overwrite an owned attribute.
	s.expect(s.request("PATCH", path, `{"set":{"name":"Unauthorized overwrite"}}`, key, tokenB, `"3"`), 403)
	contribute(t, s.h, viewOperator, r.ID, "importer")
	s.expect(s.request("PATCH", path, `{"set":{"name":"Unauthorized overwrite"}}`, platform.UUID(), tokenB, `"3"`), 409)
	s.expect(s.request("PATCH", path, body, key, tokenA, `"2"`), 409)
	p, err := (platform.Store{Pool: s.pool}).Get(context.Background(), r.ID)
	if err != nil || p.Version != 3 || string(p.Attributes["name"]) != `"Later update"` {
		t.Fatal("replay or rejection changed current state", err)
	}
}

func TestConcurrentIdentifierClaim(t *testing.T) {
	s := setup(t)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.request("POST", "/v1/profiles", `{"set":{"name":"Synthetic"},"identifiers":[{"namespace":"crm","value":"concurrent-id"}]}`, platform.UUID(), tokenA, "").Code
		}()
	}
	wg.Wait()
	close(codes)
	counts := map[int]int{}
	for c := range codes {
		counts[c]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		t.Fatal(counts)
	}
	for _, table := range []string{"profiles", "external_identifiers", "mutations", "provenance", "outbox", "attribute_ownership"} {
		if s.count(table) != 1 {
			t.Fatalf("partial or duplicate claim in %s", table)
		}
	}
}

func TestFailedMutationKeyCanBeRetried(t *testing.T) {
	s := setup(t)
	r := s.create()
	ctx := context.Background()
	key := platform.UUID()
	path := "/v1/profiles/" + r.ID
	_, err := s.pool.Exec(ctx, `CREATE FUNCTION reject_outbox_retry() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'synthetic failure'; END $$; CREATE TRIGGER fail_outbox_retry BEFORE INSERT ON outbox FOR EACH ROW EXECUTE FUNCTION reject_outbox_retry()`)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"set":{"name":"Recovered"},"identifiers":[{"namespace":"support","value":"retry-id"}]}`
	s.expect(s.request("PATCH", path, body, key, tokenA, `"1"`), 500)
	if s.count("external_identifiers") != 1 || s.count("mutations") != 1 {
		t.Fatal("failed transaction reserved identifier or key")
	}
	if _, err = s.pool.Exec(ctx, `DROP TRIGGER fail_outbox_retry ON outbox`); err != nil {
		t.Fatal(err)
	}
	s.expect(s.request("PATCH", path, body, key, tokenA, `"1"`), 200)
	if s.count("external_identifiers") != 2 || s.count("outbox") != 2 {
		t.Fatal("retry did not recover atomically")
	}
}

func TestStrictRequestContract(t *testing.T) {
	s := setup(t)
	for _, body := range []string{`{"SET":{"name":"Alias"}}`, `{"set":{"name":"A"},"Set":{"name":"B"}}`, `{"identifiers":[{"Namespace":"crm","value":"1"}]}`, `{"identifiers":[{"source":"console","namespace":"crm","value":"1"}]}`} {
		s.expect(s.request("POST", "/v1/profiles", body, platform.UUID(), tokenA, ""), 400)
	}
	s.expect(s.request("POST", "/v1/profiles", `{"set":{"name":"A"},"remove":[]}`, platform.UUID(), tokenA, ""), 422)
	r := s.create()
	for _, etag := range []string{`"01"`, `"+1"`, `W/"1"`, `*`, `"9223372036854775808"`} {
		s.expect(s.request("PATCH", "/v1/profiles/"+r.ID, `{"set":{"name":"B"}}`, platform.UUID(), tokenA, etag), 400)
	}
	s.expect(s.request("GET", "/v1/profiles?limit=1&limit=2", "", "", tokenA, ""), 400)
	if s.count("profiles") != 1 || s.count("outbox") != 1 {
		t.Fatal("invalid request changed state")
	}
}
