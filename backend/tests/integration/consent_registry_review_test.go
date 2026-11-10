package integration

// Negative tests for defects found in the review of the consent registry (#22).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func (s *consentSuite) rows(table string) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+table).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// fakeOperatorKratos answers whoami with the given status and, for 200, an MFA profile_reader session.
func fakeOperatorKratos(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"active": true, "expires_at": time.Now().Add(time.Hour), "authenticator_assurance_level": "aal2",
				"authentication_methods": []map[string]string{{"method": "password"}, {"method": "totp"}},
				"identity": map[string]any{"id": "0b5c9a3e-6d1b-4c8e-9f2a-1d3e5f7a9b0c", "schema_id": "operator", "state": "active",
					"metadata_public": map[string]any{"customer_fabric_roles": []string{"profile_reader"}}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// kratosAPI builds the API in named operator mode against the given operator issuer.
func (s *consentSuite) kratosAPI(issuer string) http.Handler {
	s.t.Helper()
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorKratosPublicURL: issuer,
		KratosPublicURL: "http://customer-kratos.invalid:4433", Tokens: map[string]string{"console": tokenA, "importer": tokenB}})
	if err != nil {
		s.t.Fatal(err)
	}
	return h
}

func operatorSession(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer ory_st_"+strings.Repeat("a", 32))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A read-only profile_reader session must not create purposes, draft or transition policies.
func TestRegistryReaderCannotWrite(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	v := s.draft(p.ID)
	h := s.kratosAPI(fakeOperatorKratos(t, http.StatusOK))
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/v1/operator/consent/purposes", `{"name":"profiling"}`},
		{"POST", "/v1/operator/consent/policies/" + p.ID, `{"explanation":"Synthetic explanation.","lawful_basis":"consent","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`},
		{"POST", "/v1/operator/consent/policy-versions/" + v.ID + "/state", `{"state":"active"}`},
	} {
		if w := operatorSession(h, c.method, c.path, c.body); w.Code != 403 {
			t.Errorf("%s %s by profile_reader: got %d, want 403", c.method, c.path, w.Code)
		}
	}
	if s.rows("consent_purposes") != 1 || s.rows("consent_policy_versions") != 1 {
		t.Fatal("a profile_reader write reached the database")
	}
	// Reads stay available to the reader role.
	if w := operatorSession(h, "GET", "/v1/operator/consent/policies/"+p.ID, ""); w.Code != 200 {
		t.Fatalf("profile_reader read: %d", w.Code)
	}
}

// Operator identity failures keep the M1 semantics: 403 for permission, 503 for an outage.
func TestRegistryOperatorErrorSemantics(t *testing.T) {
	s := consentSetup(t)
	path := "/v1/operator/consent/policies/" + platform.UUID()
	if w := operatorSession(s.kratosAPI(fakeOperatorKratos(t, http.StatusForbidden)), "GET", path, ""); w.Code != 403 {
		t.Errorf("permission failure: got %d, want 403", w.Code)
	}
	if w := operatorSession(s.kratosAPI("http://127.0.0.1:1"), "GET", path, ""); w.Code != 503 {
		t.Errorf("identity outage: got %d, want 503", w.Code)
	}
}

// Malformed identifiers and over-long fields are client errors, never database errors.
func TestRegistryRejectsMalformedInput(t *testing.T) {
	s := consentSetup(t)
	for _, path := range []string{
		"/v1/operator/consent/policies/zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz",
		"/v1/operator/consent/policy-versions/zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz/state",
	} {
		method := "GET"
		if strings.HasSuffix(path, "/state") {
			method = "POST"
		}
		if w := s.call(method, path, `{"state":"active"}`, operatorToken); w.Code != 404 {
			t.Errorf("%s %s: got %d, want 404", method, path, w.Code)
		}
	}
	p := s.purpose("newsletter")
	for name, body := range map[string]string{
		"notice too long":  `{"explanation":"Synthetic explanation.","lawful_basis":"consent","notice_reference":"` + strings.Repeat("n", 300) + `","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`,
		"channel too long": `{"explanation":"Synthetic explanation.","lawful_basis":"consent","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"` + strings.Repeat("c", 40) + `","retention_reference":"r"}`,
	} {
		if w := s.call("POST", "/v1/operator/consent/policies/"+p.ID, body, operatorToken); w.Code != 422 {
			t.Errorf("%s: got %d, want 422", name, w.Code)
		}
	}
	v := s.draft(p.ID)
	if w := s.transition(v.ID, "made_up"); w.Code != 422 || strings.Contains(w.Body.String(), "made_up") {
		t.Errorf("invalid target state: got %d %s", w.Code, w.Body.String())
	}
	if w := s.call("GET", "/v1/operator/consent/policies/"+platform.UUID(), "", operatorToken); w.Code != 404 {
		t.Errorf("unknown purpose listing: got %d, want 404", w.Code)
	}
}

// Request bodies follow the M1 rules: charset parameters are accepted, trailing data and duplicate keys are not.
func TestRegistryBodyParsing(t *testing.T) {
	s := consentSetup(t)
	r := httptest.NewRequest("POST", "/v1/operator/consent/purposes", strings.NewReader(`{"name":"newsletter"}`))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set("Authorization", "Bearer "+operatorToken)
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	s.expect(w, 201)
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"profiling"} {"name":"other"}`, operatorToken), 400)
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"a_one","name":"b_two"}`, operatorToken), 400)
	if w := s.call("POST", "/v1/operator/consent/purposes", `{"name":"newsletter"}`, operatorToken); w.Code != 409 || strings.Contains(w.Body.String(), "identifier") {
		t.Errorf("duplicate purpose: got %d %s", w.Code, w.Body.String())
	}
	s.expect(s.call("DELETE", "/v1/operator/consent/purposes", "", operatorToken), 405)
}

// A version that was ever active is part of the notice record and cannot be deleted.
func TestActiveAndRetiredVersionsCannotBeDeleted(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM consent_policy_versions WHERE id=$1`, v.ID); err == nil {
		t.Fatal("active policy version deleted")
	}
	s.expect(s.transition(v.ID, "retired"), 200)
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM consent_policy_versions WHERE id=$1`, v.ID); err == nil {
		t.Fatal("retired policy version deleted")
	}
	draft := s.draft(p.ID)
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM consent_policy_versions WHERE id=$1`, draft.ID); err != nil {
		t.Fatalf("a draft must remain deletable: %v", err)
	}
}

// Concurrent activations of competing drafts leave exactly one active version and never a false success.
func TestConcurrentActivationsLeaveOneActive(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	drafts := []policyView{s.draft(p.ID), s.draft(p.ID), s.draft(p.ID)}
	codes := make(chan int, len(drafts))
	for _, d := range drafts {
		go func(id string) { codes <- s.transition(id, "active").Code }(d.ID)
	}
	ok := 0
	for range drafts {
		switch c := <-codes; c {
		case 200:
			ok++
		case 409:
		default:
			t.Fatalf("unexpected status %d", c)
		}
	}
	var active int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM consent_policy_versions WHERE purpose_id=$1 AND state='active'`, p.ID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if ok != 1 || active != 1 {
		t.Fatalf("successes=%d active=%d, want 1 and 1", ok, active)
	}
}
