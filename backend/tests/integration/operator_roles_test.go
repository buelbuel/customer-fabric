package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const roleOperator = "4f1d2c3b-5a6e-4d7f-8a9b-0c1d2e3f4a5b"

// rolesIssuer is an operator issuer whose role assignment changes between requests, as the operator
// Kratos admin API changes metadata_public.
type rolesIssuer struct {
	mu    sync.Mutex
	roles []string
	url   string
}

func newRolesIssuer(t *testing.T, roles ...string) *rolesIssuer {
	issuer := &rolesIssuer{roles: roles}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issuer.mu.Lock()
		roles := append([]string(nil), issuer.roles...)
		issuer.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": true, "expires_at": time.Now().Add(time.Hour), "authenticator_assurance_level": "aal2",
			"authentication_methods": []map[string]string{{"method": "password"}, {"method": "totp"}},
			"identity": map[string]any{"id": roleOperator, "schema_id": "operator", "state": "active",
				"metadata_public": map[string]any{"customer_fabric_roles": roles}},
		})
	}))
	t.Cleanup(srv.Close)
	issuer.url = srv.URL
	return issuer
}

// newOperatorIssuer is an operator issuer that knows only the tests' operator session tokens, each an
// AAL2 operator holding every role; any other token is not a session.
func newOperatorIssuer(t *testing.T) string {
	t.Helper()
	tokens := []string{viewOperator, operatorToken}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/whoami" || !slices.Contains(tokens, r.Header.Get("X-Session-Token")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"active": true, "expires_at": time.Now().Add(time.Hour), "authenticator_assurance_level": "aal2",
			"authentication_methods": []map[string]string{{"method": "password"}, {"method": "totp"}},
			"identity": map[string]any{"id": roleOperator, "schema_id": "operator", "state": "active",
				"metadata_public": map[string]any{"customer_fabric_roles": identity.AllRoles()}},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (i *rolesIssuer) assign(roles ...string) {
	i.mu.Lock()
	i.roles = roles
	i.mu.Unlock()
}

func TestOperatorWritesNeedScopedRolesAndAreAudited(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "profile_reader")
	h := s.kratosAPI(issuer.url)
	purpose := func(name string) *httptest.ResponseRecorder {
		return operatorSession(h, "POST", "/v1/operator/consent/purposes", `{"name":"`+name+`"}`)
	}

	denied := purpose("newsletter")
	if denied.Code != 403 || !strings.Contains(denied.Body.String(), "consent_admin") {
		t.Fatalf("reader created a purpose: %d %s", denied.Code, denied.Body.String())
	}
	issuer.assign("profile_reader", "audience_manager")
	if w := purpose("newsletter"); w.Code != 403 {
		t.Fatalf("audience_manager created a purpose: %d", w.Code)
	}
	issuer.assign("profile_reader", "consent_admin")
	if w := purpose("newsletter"); w.Code != 201 {
		t.Fatalf("consent_admin: %d %s", w.Code, w.Body.String())
	}
	if w := purpose("newsletter"); w.Code != 409 {
		t.Fatalf("duplicate purpose: %d", w.Code)
	}
	// A revoked role applies to the very next request.
	issuer.assign("profile_reader")
	if w := purpose("profiling"); w.Code != 403 {
		t.Fatalf("revoked consent_admin still wrote: %d", w.Code)
	}

	if w := operatorSession(h, "GET", "/v1/operator/audit", ""); w.Code != 403 {
		t.Fatalf("audit without audit_reader: %d", w.Code)
	}
	issuer.assign("audit_reader")
	if w := operatorSession(h, "GET", "/v1/operator/consent/effective/"+roleOperator+"/"+roleOperator, ""); w.Code != 403 {
		t.Fatalf("audit_reader evaluated consent: %d", w.Code)
	}
	if w := operatorSession(h, "GET", "/v1/operator/attributes", ""); w.Code != 403 {
		t.Fatalf("audit_reader read profiles data: %d", w.Code)
	}
	trail := operatorSession(h, "GET", "/v1/operator/audit", "")
	if trail.Code != 200 {
		t.Fatalf("audit: %d", trail.Code)
	}
	var audit struct{ Items []platform.OperatorAuditEntry }
	if err := json.Unmarshal(trail.Body.Bytes(), &audit); err != nil {
		t.Fatal(err)
	}
	if len(audit.Items) != 1 || audit.Items[0].Actor != roleOperator || audit.Items[0].Method != "POST" ||
		audit.Items[0].Resource != "/v1/operator/consent/purposes" || audit.Items[0].Status != 201 {
		t.Fatalf("only the successful write is audited, by its operator: %s", trail.Body.String())
	}
	if strings.Contains(trail.Body.String(), "newsletter") {
		t.Fatal("the audit stored a request body")
	}

	// Every write is audited under the operator's subject.
	s.expect(s.call("POST", "/v1/operator/consent/purposes", `{"name":"another_purpose"}`, operatorToken), 201)
	var actor string
	if err := s.pool.QueryRow(context.Background(), `SELECT actor FROM operator_audit ORDER BY at DESC LIMIT 1`).Scan(&actor); err != nil || actor != roleOperator {
		t.Fatalf("audit actor %q %v", actor, err)
	}
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM operator_audit`); err == nil {
		t.Fatal("an audit record was deleted")
	}
}
