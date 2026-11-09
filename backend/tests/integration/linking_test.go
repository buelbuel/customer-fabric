package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const (
	linkGrants    = "/v1/linking/grants"
	associations  = "/v1/customer/associations"
	linkIssuerA   = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	linkIssuerB   = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	linkCustomerA = "ory_st_customer_A_synthetic"
	linkCustomerB = "ory_st_customer_B_synthetic"
)

type linkingSuite struct {
	*suite
	customerIDs map[string]string
	profileID   string
}

func setupLinking(t *testing.T) linkingSuite {
	t.Helper()
	s := linkingSuite{
		suite:       setup(t),
		customerIDs: map[string]string{linkCustomerA: platform.UUID(), linkCustomerB: platform.UUID()},
	}
	// The live Kratos smoke covers the identity boundary. The adapter here
	// isolates database faults and races from account setup and network timing.
	whoami := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.customerIDs[r.Header.Get("X-Session-Token")]
		if r.URL.Path != "/sessions/whoami" || !ok {
			w.WriteHeader(401)
			return
		}
		fmt.Fprintf(w, `{"active":true,"expires_at":%q,"identity":{"id":%q,"state":"active","traits":{"email":"link@example.test"},"verifiable_addresses":[{"via":"email","value":"link@example.test","verified":true}]}}`,
			time.Now().Add(time.Hour).Format(time.RFC3339), id)
	}))
	t.Cleanup(whoami.Close)
	store := platform.Store{Pool: s.pool}
	handler, err := platform.NewAPI(store, platform.Config{
		Mode: "development", Tokens: map[string]string{"console": tokenA, "importer": tokenB},
		OperatorToken: strings.Repeat("o", 64), KratosPublicURL: whoami.URL,
		LinkingIssuers:         map[string]string{"console": linkIssuerA, "importer": linkIssuerB},
		CustomerViewAttributes: map[string][]string{"console": {"display_name", "tier", "plan", "status"}, "importer": {"tier"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h = handler
	s.profileID = s.create().ID
	for _, id := range s.customerIDs {
		if _, err := store.ResolveCustomerProfile(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func (s linkingSuite) customer(method, path, body, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-Session-Token", token)
	req.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	s.h.ServeHTTP(res, req)
	return res
}

func (s linkingSuite) expectStatus(res *httptest.ResponseRecorder, status int) {
	s.t.Helper()
	if res.Code != status {
		s.t.Fatalf("expected status %d, got %d", status, res.Code)
	}
}

type testLinkGrant struct {
	ID    string `json:"id"`
	Grant string `json:"grant"`
}

func (g testLinkGrant) payload() string {
	return fmt.Sprintf(`{"grant":%q}`, g.Grant)
}

func (s linkingSuite) issue(token string) testLinkGrant {
	s.t.Helper()
	body := fmt.Sprintf(`{"namespace":"crm","record":"synthetic-1","customer_subject":%q,"proof_method":"synthetic_challenge","proof_reference":%q}`,
		s.customerIDs[token], platform.UUID())
	res := s.request("POST", linkGrants, body, "", linkIssuerA, "")
	s.expectStatus(res, 201)
	s.expectStatus(s.request("POST", linkGrants, body, "", linkIssuerA, ""), 409)
	var grant testLinkGrant
	if err := json.Unmarshal(res.Body.Bytes(), &grant); err != nil || grant.ID == "" || grant.Grant == "" {
		s.t.Fatal("missing linking grant")
	}
	return grant
}

func (s linkingSuite) exec(query string, args ...any) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), query, args...); err != nil {
		s.t.Fatal(err)
	}
}

func TestVerifiedAssociationConcurrentRedemption(t *testing.T) {
	s := setupLinking(t)
	a := s.issue(linkCustomerA)
	b := s.issue(linkCustomerB)
	s.expectStatus(s.customer("POST", associations, a.payload(), linkCustomerB), 409)
	s.expectStatus(s.customer("POST", associations, fmt.Sprintf(`{"grant":%q,"customer_subject":%q}`, a.Grant, s.customerIDs[linkCustomerA]), linkCustomerA), 400)
	s.expectStatus(s.request("POST", linkGrants, "{}", "", tokenA, ""), 401)
	s.expectStatus(s.request("DELETE", linkGrants+"/"+a.ID, "", "", linkIssuerB, ""), 404)
	type result struct {
		code            int
		token, location string
	}
	results := make(chan result, 3)
	start := make(chan struct{})
	run := func(grant testLinkGrant, token string) {
		<-start
		res := s.customer("POST", associations, grant.payload(), token)
		results <- result{res.Code, token, res.Header().Get("Location")}
	}
	go run(a, linkCustomerA)
	go run(a, linkCustomerA)
	go run(b, linkCustomerB)
	close(start)
	var winner result
	for i := 0; i < 3; i++ {
		res := <-results
		switch res.code {
		case 201:
			if winner.location != "" {
				t.Fatal("more than one redemption succeeded")
			}
			winner = res
		case 409:
		default:
			t.Fatalf("unexpected redemption status %d", res.code)
		}
	}
	if winner.location == "" || s.count("verified_associations") != 1 {
		t.Fatal("one association must be committed")
	}
	other := linkCustomerA
	if winner.token == other {
		other = linkCustomerB
	}
	s.expectStatus(s.customer("GET", winner.location, "", other), 404)
	s.expectStatus(s.customer("DELETE", winner.location, "", other), 404)
	s.expectStatus(s.customer("GET", winner.location, "", winner.token), 200)
	s.expectStatus(s.customer("DELETE", winner.location, "", winner.token), 204)
	s.expectStatus(s.customer("DELETE", winner.location, "", winner.token), 204)
	s.expectStatus(s.customer("GET", winner.location, "", winner.token), 404)
	s.expectStatus(s.customer("POST", associations, a.payload(), linkCustomerA), 409)
	s.expectStatus(s.customer("POST", associations, b.payload(), linkCustomerB), 409)
	if s.count("verified_associations") != 1 || s.count("profiles") != 3 || s.count("outbox") != 1 || s.count("provenance") != 1 {
		t.Fatal("association changed profile state, attribute provenance or outbox")
	}
}

func TestVerifiedAssociationExpiryRevocationAndRollback(t *testing.T) {
	s := setupLinking(t)
	expired := s.issue(linkCustomerA)
	s.exec("UPDATE linking_grants SET issued_at=clock_timestamp()-interval '20 minutes', expires_at=clock_timestamp()-interval '10 minutes' WHERE id=$1", expired.ID)
	s.expectStatus(s.customer("POST", associations, expired.payload(), linkCustomerA), 409)

	cancelled := s.issue(linkCustomerA)
	s.expectStatus(s.request("DELETE", linkGrants+"/"+cancelled.ID, "", "", linkIssuerA, ""), 204)
	s.expectStatus(s.customer("POST", associations, cancelled.payload(), linkCustomerA), 409)

	deleted := s.issue(linkCustomerA)
	s.exec("DELETE FROM external_identifiers WHERE source='console' AND namespace='crm' AND value='synthetic-1'")
	s.expectStatus(s.customer("POST", associations, deleted.payload(), linkCustomerA), 409)
	s.exec("INSERT INTO external_identifiers(source, namespace, value, profile_id) VALUES('console','crm','synthetic-1',$1)", s.profileID)
	s.expectStatus(s.customer("POST", associations, deleted.payload(), linkCustomerA), 409)

	grant := s.issue(linkCustomerA)
	s.exec("ALTER TABLE verified_associations ADD CONSTRAINT test_rollback CHECK (source <> 'console') NOT VALID")
	s.expectStatus(s.customer("POST", associations, grant.payload(), linkCustomerA), 500)
	if s.count("verified_associations") != 0 {
		t.Fatal("failed redemption committed an association")
	}
	s.exec("ALTER TABLE verified_associations DROP CONSTRAINT test_rollback")
	res := s.customer("POST", associations, grant.payload(), linkCustomerA)
	s.expectStatus(res, 201)
	location := res.Header().Get("Location")
	s.expectStatus(s.customer("GET", location, "", linkCustomerA), 200)
	s.expectStatus(s.request("DELETE", linkGrants+"/"+grant.ID, "", "", linkIssuerA, ""), 204)
	s.expectStatus(s.customer("GET", location, "", linkCustomerA), 404)
	s.expectStatus(s.customer("POST", associations, grant.payload(), linkCustomerA), 409)
}

func TestVerifiedAssociationRequiresExistingCustomerAnchor(t *testing.T) {
	s := setupLinking(t)
	s.exec("DELETE FROM customer_identity_links WHERE kratos_identity_id=$1", s.customerIDs[linkCustomerA])
	grant := s.issue(linkCustomerA)
	res := s.customer("POST", associations, grant.payload(), linkCustomerA)
	s.expectStatus(res, 409)
	if !strings.Contains(res.Body.String(), "Profile required") || s.count("customer_identity_links") != 1 {
		t.Fatal("redemption must require an existing anchor")
	}
	s.expectStatus(s.customer("PUT", "/v1/customer/profile", "", linkCustomerA), 200)
	s.expectStatus(s.customer("POST", associations, grant.payload(), linkCustomerA), 201)
}
