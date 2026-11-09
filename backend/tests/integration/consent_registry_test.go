package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const operatorToken = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

type consentSuite struct {
	t    *testing.T
	pool *pgxpool.Pool
	h    http.Handler
}

func consentSetup(t *testing.T) *consentSuite {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL unset: real PostgreSQL integration tests not run")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	schema := "consent_" + strings.ReplaceAll(platform.UUID(), "-", "")
	if _, err = admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
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
		_, _ = admin.Exec(ctx, `DROP SCHEMA `+schema+` CASCADE`)
		admin.Close()
	})
	if err = migrations.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err = migrations.Apply(ctx, pool); err != nil {
		t.Fatal("migration not repeatable", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO sources(name) VALUES('console'),('importer')`); err != nil {
		t.Fatal(err)
	}
	h, err := platform.NewAPI(platform.Store{Pool: pool}, platform.Config{
		Mode: "development", OperatorToken: operatorToken,
		Tokens: map[string]string{"console": tokenA, "importer": tokenB},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &consentSuite{t, pool, h}
}

func (s *consentSuite) call(method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func (s *consentSuite) callHeader(method, path, body, token, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", key)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func (s *consentSuite) count(table string) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func (s *consentSuite) expect(w *httptest.ResponseRecorder, status int) {
	s.t.Helper()
	if w.Code != status {
		s.t.Fatalf("expected %d got %d: %s", status, w.Code, w.Body.String())
	}
}

type purposeView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *consentSuite) purpose(name string) purposeView {
	s.t.Helper()
	w := s.call("POST", "/v1/operator/consent/purposes", `{"name":"`+name+`"}`, operatorToken)
	s.expect(w, 201)
	var p purposeView
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		s.t.Fatal(err)
	}
	return p
}

type policyView struct {
	ID          string `json:"id"`
	Version     int32  `json:"version"`
	State       string `json:"state"`
	LawfulBasis string `json:"lawful_basis"`
}

func (s *consentSuite) draft(purposeID string) policyView {
	s.t.Helper()
	body := `{"lawful_basis":"consent","notice_reference":"notice-2026-01","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"retention-24m"}`
	w := s.call("POST", "/v1/operator/consent/policies/"+purposeID, body, operatorToken)
	s.expect(w, 201)
	var p policyView
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s *consentSuite) transition(id, to string) *httptest.ResponseRecorder {
	return s.call("POST", "/v1/operator/consent/policies/"+id+"/state", `{"state":"`+to+`"}`, operatorToken)
}

func TestRegistryRequiresOperatorAuth(t *testing.T) {
	s := consentSetup(t)
	// No token and a source token are both rejected.
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"newsletter"}`, ""), 401)
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"newsletter"}`, tokenA), 401)
}

func TestDraftThenActivateThenRetire(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	v1 := s.draft(p.ID)
	if v1.Version != 1 || v1.State != "draft" {
		t.Fatalf("unexpected first draft: %+v", v1)
	}
	s.expect(s.transition(v1.ID, "active"), 200)
	// A second draft gets the next version and does not disturb the active one.
	v2 := s.draft(p.ID)
	if v2.Version != 2 {
		t.Fatalf("expected version 2, got %d", v2.Version)
	}
	// Activating v2 while v1 is active is refused by the single-active constraint semantics.
	s.expect(s.transition(v2.ID, "active"), 409)
	// Retire v1, then activate v2.
	s.expect(s.transition(v1.ID, "retired"), 200)
	s.expect(s.transition(v2.ID, "active"), 200)
}

func TestActivePolicyIsImmutable(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("profiling")
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	// Direct SQL edit of an active version must fail at the database, not just the API.
	_, err := s.pool.Exec(context.Background(),
		`UPDATE consent_policy_versions SET notice_reference='changed' WHERE id=$1`, v.ID)
	if err == nil {
		t.Fatal("active policy version was editable; immutability trigger missing")
	}
	// A retired version cannot leave retired either.
	s.expect(s.transition(v.ID, "retired"), 200)
	if _, err = s.pool.Exec(context.Background(),
		`UPDATE consent_policy_versions SET state='active' WHERE id=$1`, v.ID); err == nil {
		t.Fatal("retired policy version reactivated in SQL")
	}
}

func TestOneActiveVersionPerPurpose(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("service_messages")
	v1 := s.draft(p.ID)
	s.expect(s.transition(v1.ID, "active"), 200)
	v2 := s.draft(p.ID)
	// The API refuses to activate v2 while v1 is active.
	s.expect(s.transition(v2.ID, "active"), 409)
	// The database refuses a direct second activation as well.
	if _, err := s.pool.Exec(context.Background(),
		`UPDATE consent_policy_versions SET state='active' WHERE id=$1`, v2.ID); err == nil {
		t.Fatal("two active versions coexisted")
	}
}

func TestInvalidPolicyRejectedAndDuplicatePurpose(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("marketing")
	bad := `{"lawful_basis":"made_up","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`
	s.expect(s.call("POST", "/v1/operator/consent/policies/"+p.ID, bad, operatorToken), 422)
	// Unknown purpose is a 404, not a silent create.
	w := s.call("POST", "/v1/operator/consent/policies/"+platform.UUID(), bad, operatorToken)
	if w.Code != 404 && w.Code != 422 {
		t.Fatalf("expected 404/422 for unknown purpose, got %d: %s", w.Code, w.Body.String())
	}
	// Duplicate purpose name conflicts.
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"marketing"}`, operatorToken), 409)
}
