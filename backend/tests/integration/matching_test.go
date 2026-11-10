package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func (s linkingSuite) candidates(profile string) (platform.MatchCandidates, string) {
	s.t.Helper()
	res := s.request("GET", "/v1/operator/profiles/"+profile+"/match-candidates", "", "", viewOperator, "")
	s.expect(res, 200)
	var c platform.MatchCandidates
	if err := json.Unmarshal(res.Body.Bytes(), &c); err != nil {
		s.t.Fatal(err)
	}
	return c, res.Body.String()
}

// Candidates are proposals over values granted to the matching reader, explained by signal names only.
func TestMatchCandidatesAreGovernedAndExplainedWithoutValues(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "phone", "given_name", "family_name", "customer_reference"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	ada := s.viewRecord(tokenA, "match-ada", `{"email":"ada@example.com","given_name":"Ada","family_name":"Muster","phone":"+44 1632 960001"}`)
	adaERP := s.viewRecord(tokenB, "match-ada-erp", `{"email":"ADA@example.com","family_name":"MUSTER"}`)
	lukas := s.viewRecord(tokenB, "match-lukas", `{"email":"ada@example.com","given_name":"Lukas","family_name":"Muster"}`)
	// The suite already holds one console record; with this one the population is five.
	other := s.viewRecord(tokenA, "match-other", `{"email":"ada@example.com"}`)

	c, body := s.candidates(ada)
	if c.Source != "console" || c.Population != 5 || c.Total != 2 || len(c.Candidates) != 2 {
		t.Fatalf("candidates %s", body)
	}
	if first := c.Candidates[0]; first.ProfileID != adaERP || first.Class != "probable" || strings.Join(first.Agree, ",") != "email,family_name" {
		t.Fatalf("probable %+v", first)
	}
	if second := c.Candidates[1]; second.ProfileID != lukas || second.Class != "conflicting" || !strings.Contains(strings.Join(second.Disagree, ","), "given_name") {
		t.Fatalf("household %+v", second)
	}
	for _, value := range []string{"ada@", "Muster", "MUSTER", "Lukas", "1632"} {
		if strings.Contains(body, value) {
			t.Fatalf("a candidate list disclosed %q: %s", value, body)
		}
	}

	// A review records a decision on what the operator saw; it is visible from both records and links nothing.
	reviews := "/v1/operator/profiles/" + ada + "/match-reviews"
	review := func(candidate, decision string) string {
		return fmt.Sprintf(`{"candidate_profile_id":%q,"decision":%q}`, candidate, decision)
	}
	s.expect(s.request("POST", reviews, review(adaERP, "same_person"), "", tokenA, ""), 401)
	s.expect(s.request("POST", reviews, review(adaERP, "maybe"), "", viewOperator, ""), 422)
	s.expect(s.request("POST", reviews, review(other, "same_person"), "", viewOperator, ""), 409)
	s.expect(s.request("POST", reviews, review(adaERP, "same_person"), "", viewOperator, ""), 201)
	s.expect(s.request("POST", reviews, review(lukas, "different_people"), "", viewOperator, ""), 201)
	if c, body := s.candidates(ada); c.Candidates[0].Review != "same_person" || c.Candidates[1].Review != "different_people" {
		t.Fatalf("reviewed candidates %s", body)
	}
	trail := s.request("GET", "/v1/operator/profiles/"+adaERP+"/match-reviews", "", "", viewOperator, "")
	s.expect(trail, 200)
	if !strings.Contains(trail.Body.String(), `"decision":"same_person","class":"probable","agree":["email","family_name"]`) {
		t.Fatalf("trail from the other record %s", trail.Body.String())
	}
	if s.count("verified_associations") != 0 {
		t.Fatal("a review linked records")
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE match_reviews SET decision='different_people'`); err == nil {
		t.Fatal("a review was edited")
	}

	// Withdrawing matching's grant on family_name leaves email alone, which is never enough.
	s.expect(s.request("POST", "/v1/operator/attributes/family_name/access", `{"readers":["operator"],"sources":["console","importer"]}`, "", viewOperator, `"1"`), 201)
	if c, body := s.candidates(ada); c.Total != 1 || c.Candidates[0].ProfileID != lukas {
		t.Fatalf("after narrowing family_name %s", body)
	}
	// A record never sees its own source's grant applied to another source's values.
	s.expect(s.request("POST", "/v1/operator/attributes/given_name/access", `{"readers":["matching"],"sources":["console"]}`, "", viewOperator, `"1"`), 201)
	if c, body := s.candidates(ada); c.Total != 0 {
		t.Fatalf("an ungranted source's given name took part %s", body)
	}

	s.expect(s.request("GET", "/v1/operator/profiles/"+s.anchor(linkCustomerA)+"/match-candidates", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", "/v1/operator/profiles/"+platform.UUID()+"/match-candidates", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", "/v1/operator/profiles/"+ada+"/match-candidates", "", "", tokenA, ""), 401)
	s.expect(s.request("POST", "/v1/operator/profiles/"+ada+"/match-candidates", "", "", viewOperator, ""), 405)
	s.expect(s.request("GET", "/v1/operator/profiles/"+ada+"/match-candidates?x=1", "", "", viewOperator, ""), 400)
	s.expect(s.request("POST", "/v1/operator/attributes/email/access", `{"readers":["matching","customer"],"sources":["console"]}`, "", viewOperator, `"1"`), 422)
}
