package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

func (s linkingSuite) contributors(profile string) []platform.ProfileContributor {
	s.t.Helper()
	res := s.request("GET", "/v1/operator/profiles/"+profile+"/contributors", "", "", viewOperator, "")
	s.expect(res, 200)
	var list struct {
		Items []platform.ProfileContributor `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
		s.t.Fatal(err)
	}
	return list.Items
}

func (s linkingSuite) authorize(profile, body string, status int) platform.ProfileContributor {
	s.t.Helper()
	res := s.request("POST", "/v1/operator/profiles/"+profile+"/contributors", body, "", viewOperator, "")
	s.expect(res, status)
	var c platform.ProfileContributor
	if status == 201 {
		if err := json.Unmarshal(res.Body.Bytes(), &c); err != nil {
			s.t.Fatal(err)
		}
	}
	return c
}

func (s linkingSuite) operatorProfile(id string) profiles.Profile {
	s.t.Helper()
	res := s.request("GET", "/v1/operator/profiles/"+id, "", "", viewOperator, "")
	s.expect(res, 200)
	var p profiles.Profile
	if err := json.Unmarshal(res.Body.Bytes(), &p); err != nil {
		s.t.Fatal(err)
	}
	return p
}

func (s linkingSuite) patch(token, id, body string) int {
	s.t.Helper()
	return s.request("PATCH", "/v1/profiles/"+id, body, platform.UUID(), token, fmt.Sprintf(`"%d"`, s.operatorProfile(id).Version)).Code
}

func shopEvent(id, value string, sequence int, change string) string {
	return fmt.Sprintf(`{"event_id":%q,"type":"record.updated.v1","record":{"namespace":"shop","value":%q},"source_epoch":1,"sequence":%d,"occurred_at":"2026-01-01T00:00:00Z",%s}`,
		id, value, sequence, change)
}

// Salesforce (console) creates a profile; Shopify (importer) contributes its own identifier and
// attributes to it through the public API once an identity reviewer authorizes it. Knowing the profile
// ID grants nothing, ownership and tombstones hold, and revocation stops every later write by Shopify
// without retracting what it wrote.
func TestSourcesContributeToOneProfileOnceAuthorized(t *testing.T) {
	s := setupLinking(t)
	ada := s.viewRecord(tokenA, "sf-003", `{"display_name":"Synthetic Ada","tier":"gold"}`)
	contribution := `{"set":{"loyalty_points":"120"},"identifiers":[{"namespace":"shop","value":"shop-77"}]}`

	// Without authorization, an existing and an unknown profile look the same.
	refused := s.request("PATCH", "/v1/profiles/"+ada, contribution, platform.UUID(), tokenB, `"1"`)
	unknown := s.request("PATCH", "/v1/profiles/"+platform.UUID(), contribution, platform.UUID(), tokenB, `"1"`)
	s.expect(refused, 403)
	s.expect(unknown, 403)
	if refused.Body.String() != unknown.Body.String() {
		t.Fatalf("a refusal disclosed whether the profile exists: %s / %s", refused.Body.String(), unknown.Body.String())
	}
	if len(s.contributors(ada)) != 0 {
		t.Fatal("contributors before any authorization")
	}

	verified := `{"source":"importer","basis":"verified_reference","verification_reference":"case:4711"}`
	s.expect(s.request("POST", "/v1/operator/profiles/"+ada+"/contributors", verified, "", tokenB, ""), 401)
	for _, body := range []string{
		`{"source":"importer","basis":"self_asserted"}`,
		`{"source":"importer","basis":"verified_reference"}`,
		`{"source":"importer","basis":"verified_reference","verification_reference":"has spaces"}`,
		`{"source":"importer","basis":"reviewed_match","match_review_id":"not-a-uuid"}`,
		`{"source":"nosuch","basis":"verified_reference","verification_reference":"case:1"}`,
	} {
		s.authorize(ada, body, 422)
	}
	s.authorize(ada, `{"source":"console","basis":"verified_reference","verification_reference":"case:1"}`, 409)
	s.authorize(platform.UUID(), verified, 404)
	granted := s.authorize(ada, verified, 201)
	if granted.Source != "importer" || granted.Basis != "verified_reference" || granted.AuthorizedBy == "" {
		t.Fatalf("authorization %+v", granted)
	}
	s.authorize(ada, verified, 409)

	// Shopify adds its own identifier and attributes; Salesforce's attributes stay Salesforce's.
	if code := s.patch(tokenB, ada, contribution); code != 200 {
		t.Fatalf("contribution %d", code)
	}
	if code := s.patch(tokenB, ada, `{"set":{"display_name":"Shopify name"}}`); code != 409 {
		t.Fatalf("a contributor overwrote another source's attribute: %d", code)
	}
	if code := s.patch(tokenA, ada, `{"remove":["tier"]}`); code != 200 {
		t.Fatalf("creator removal %d", code)
	}
	if code := s.patch(tokenB, ada, `{"set":{"tier":"platinum"}}`); code != 409 {
		t.Fatalf("a contributor resurrected a tombstone: %d", code)
	}
	p := s.operatorProfile(ada)
	if string(p.Attributes["loyalty_points"]) != `"120"` || string(p.Attributes["display_name"]) != `"Synthetic Ada"` || p.Attributes["tier"] != nil {
		t.Fatalf("attributes %v", p.Attributes)
	}
	attached := false
	for _, id := range p.Identifiers {
		attached = attached || (id.Source == "importer" && id.Namespace == "shop" && id.Value == "shop-77")
	}
	if !attached {
		t.Fatalf("identifiers %+v", p.Identifiers)
	}
	// Shopify's events for its record now reach the profile it was attached to.
	applied := s.request("POST", sourceEvents, shopEvent("shop-e1", "shop-77", 1, `"set":{"loyalty_points":"130"}`), "", tokenB, "")
	s.expect(applied, 201)
	if !strings.Contains(applied.Body.String(), `"outcome":"applied"`) || !strings.Contains(applied.Body.String(), ada) {
		t.Fatalf("event %s", applied.Body.String())
	}

	// Revocation stops Shopify's writes, by update or by event, and retracts nothing.
	revoke := "/v1/operator/profiles/" + ada + "/contributors/" + granted.ID + "/revoke"
	s.expect(s.request("POST", revoke, `{"reference":"no spaces allowed"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", revoke, `{"reference":"case:4712"}`, "", tokenA, ""), 401)
	first := s.request("POST", revoke, `{"reference":"case:4712"}`, "", viewOperator, "")
	s.expect(first, 200)
	again := s.request("POST", revoke, `{"reference":"case:4713"}`, "", viewOperator, "")
	s.expect(again, 200)
	if first.Body.String() != again.Body.String() || !strings.Contains(first.Body.String(), `"revocation_reference":"case:4712"`) {
		t.Fatalf("repeated revocation changed the record: %s / %s", first.Body.String(), again.Body.String())
	}
	s.expect(s.request("POST", "/v1/operator/profiles/"+ada+"/contributors/"+platform.UUID()+"/revoke", `{"reference":"case:1"}`, "", viewOperator, ""), 404)
	if code := s.patch(tokenB, ada, `{"set":{"loyalty_points":"999"}}`); code != 403 {
		t.Fatalf("a revoked contributor wrote: %d", code)
	}
	s.expect(s.request("POST", sourceEvents, shopEvent("shop-e2", "shop-77", 2, `"set":{"loyalty_points":"999"}`), "", tokenB, ""), 403)
	if got := s.operatorProfile(ada).Attributes["loyalty_points"]; string(got) != `"130"` {
		t.Fatalf("revocation retracted or changed a contribution: %s", got)
	}
	listed := s.contributors(ada)
	if len(listed) != 1 || listed[0].RevokedAt == nil || *listed[0].RevocationReason != "revoked" {
		t.Fatalf("contributors %+v", listed)
	}
	// A new authorization can follow a revocation.
	s.authorize(ada, `{"source":"importer","basis":"verified_reference","verification_reference":"case:4714"}`, 201)
}

// A same_person review between a profile and one another source created can authorize that source; a
// later different_people review of the pair revokes the authorization in the same transaction.
func TestReviewedMatchAuthorizesAndItsReversalRevokes(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "family_name"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	ada := s.viewRecord(tokenA, "m-ada", `{"email":"ada@example.com","family_name":"Muster"}`)
	adaShop := s.viewRecord(tokenB, "m-ada-shop", `{"email":"ADA@example.com","family_name":"MUSTER"}`)
	reviews := "/v1/operator/profiles/" + ada + "/match-reviews"
	reviewed := s.request("POST", reviews, fmt.Sprintf(`{"candidate_profile_id":%q,"decision":"same_person"}`, adaShop), "", viewOperator, "")
	s.expect(reviewed, 201)
	var review platform.MatchReview
	if err := json.Unmarshal(reviewed.Body.Bytes(), &review); err != nil {
		t.Fatal(err)
	}

	byReview := fmt.Sprintf(`{"source":"importer","basis":"reviewed_match","match_review_id":%q}`, review.ID)
	s.authorize(ada, fmt.Sprintf(`{"source":"importer","basis":"reviewed_match","match_review_id":%q}`, platform.UUID()), 422)
	granted := s.authorize(ada, byReview, 201)
	if granted.MatchReviewID == nil || *granted.MatchReviewID != review.ID {
		t.Fatalf("authorization %+v", granted)
	}
	if code := s.patch(tokenB, ada, `{"set":{"loyalty_points":"5"}}`); code != 200 {
		t.Fatalf("reviewed contribution %d", code)
	}

	s.expect(s.request("POST", reviews, fmt.Sprintf(`{"candidate_profile_id":%q,"decision":"different_people"}`, adaShop), "", viewOperator, ""), 201)
	listed := s.contributors(ada)
	if len(listed) != 1 || listed[0].RevokedAt == nil || *listed[0].RevocationReason != "match_review_reversed" {
		t.Fatalf("the reversal did not revoke: %+v", listed)
	}
	if code := s.patch(tokenB, ada, `{"set":{"loyalty_points":"6"}}`); code != 403 {
		t.Fatalf("a contributor wrote after the reversal: %d", code)
	}
	// The earlier same_person review is no longer the pair's latest decision.
	s.authorize(ada, byReview, 422)
}

// A write waiting for the profile's lock decides authorization only once it holds the lock, so it sees a
// revocation committed while it waited.
func TestAWriteWaitingOnTheProfileSeesARevocation(t *testing.T) {
	s := setupLinking(t)
	ada := s.viewRecord(tokenA, "lock-ada", `{"display_name":"Synthetic Ada"}`)
	granted := s.authorize(ada, `{"source":"importer","basis":"verified_reference","verification_reference":"case:1"}`, 201)
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT 1 FROM profiles WHERE id = $1 FOR UPDATE`, ada); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- s.request("PATCH", "/v1/profiles/"+ada, `{"set":{"loyalty_points":"1"}}`, platform.UUID(), tokenB, `"1"`).Code
	}()
	select {
	case code := <-done:
		t.Fatalf("the write did not wait for the profile lock: %d", code)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err = tx.Exec(ctx, `UPDATE profile_contributors SET revoked_at = now(), revoked_by = 'test', revocation_reason = 'revoked' WHERE id = $1`, granted.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 403 {
		t.Fatalf("a write that waited through a revocation got %d", code)
	}
}

// A customer's profile anchor has no creating source and never takes a contributor.
func TestCustomerAnchorsTakeNoContributors(t *testing.T) {
	s := setupLinking(t)
	anchor := s.anchor(linkCustomerA)
	s.authorize(anchor, `{"source":"importer","basis":"verified_reference","verification_reference":"case:1"}`, 409)
	if code := s.request("PATCH", "/v1/profiles/"+anchor, `{"set":{"tier":"gold"}}`, platform.UUID(), tokenB, `"1"`).Code; code != 403 {
		t.Fatalf("a source wrote a customer anchor: %d", code)
	}
}

// Only an identity reviewer authorizes or revokes a contributor; a profile reader may list them. The
// authorization records the reviewer's subject.
func TestContributorAuthorizationNeedsTheIdentityReviewerRole(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenA)
	issuer := newRolesIssuer(t, "profile_reader")
	h := s.kratosAPI(issuer.url)
	path := "/v1/operator/profiles/" + r.profile + "/contributors"
	body := `{"source":"importer","basis":"verified_reference","verification_reference":"case:1"}`
	if w := operatorSession(h, "POST", path, body); w.Code != 403 || !strings.Contains(w.Body.String(), "identity_reviewer") {
		t.Fatalf("a profile reader authorized a contributor: %d %s", w.Code, w.Body.String())
	}
	if w := operatorSession(h, "GET", path, ""); w.Code != 200 {
		t.Fatalf("a profile reader could not list contributors: %d", w.Code)
	}
	issuer.assign("profile_reader", "identity_reviewer")
	w := operatorSession(h, "POST", path, body)
	if w.Code != 201 || !strings.Contains(w.Body.String(), `"authorized_by":"`+roleOperator+`"`) {
		t.Fatalf("identity reviewer: %d %s", w.Code, w.Body.String())
	}
	var granted platform.ProfileContributor
	if err := json.Unmarshal(w.Body.Bytes(), &granted); err != nil {
		t.Fatal(err)
	}
	issuer.assign("profile_reader")
	if w := operatorSession(h, "POST", path+"/"+granted.ID+"/revoke", `{"reference":"case:2"}`); w.Code != 403 {
		t.Fatalf("a profile reader revoked a contributor: %d", w.Code)
	}
}
