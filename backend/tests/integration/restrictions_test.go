package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func restrictionPath(profile string) string {
	return "/v1/operator/profiles/" + profile + "/restriction"
}

func (s linkingSuite) restriction(profile string) platform.Restriction {
	s.t.Helper()
	res := s.request("GET", restrictionPath(profile), "", "", viewOperator, "")
	s.expect(res, 200)
	var r platform.Restriction
	if err := json.Unmarshal(res.Body.Bytes(), &r); err != nil {
		s.t.Fatal(err)
	}
	return r
}

func restrictBody(reason, reviewBy string) string {
	return fmt.Sprintf(`{"action":"restrict","reason":%q,"review_by":%q}`, reason, reviewBy)
}

// A restriction is reversible and takes effect everywhere the restricted flag is enforced, at once: the
// record accepts no source writes and takes no part in matching until the restriction is lifted. Every
// decision is kept, with its reason code and the operator who made it.
func TestProcessingRestrictionIsReversibleAndAudited(t *testing.T) {
	s := setupLinking(t)
	ctx := context.Background()
	record := s.viewRecord(tokenA, "restrict-ada", `{"tier":"gold"}`)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	if r := s.restriction(record); r.Restricted || len(r.Decisions) != 0 {
		t.Fatalf("before any decision %+v", r)
	}

	for _, body := range []string{
		restrictBody("because", nextMonth),
		restrictBody("legal_claims", time.Now().UTC().Format(time.DateOnly)),
		restrictBody("legal_claims", time.Now().UTC().AddDate(1, 1, 0).Format(time.DateOnly)),
		restrictBody("legal_claims", "next month"),
		`{"action":"lift","reason":"claim_resolved","review_by":"` + nextMonth + `"}`,
		`{"action":"lift","reason":"legal_claims"}`,
		`{"action":"erase","reason":"legal_claims"}`,
	} {
		s.expect(s.request("POST", restrictionPath(record), body, "", viewOperator, ""), 422)
	}
	s.expect(s.request("POST", restrictionPath(record), `{"action":"lift","reason":"claim_resolved"}`, "", viewOperator, ""), 409)
	s.expect(s.request("POST", restrictionPath(platform.UUID()), restrictBody("legal_claims", nextMonth), "", viewOperator, ""), 404)
	s.expect(s.request("POST", restrictionPath(record), restrictBody("legal_claims", nextMonth), "", tokenA, ""), 401)

	res := s.request("POST", restrictionPath(record), restrictBody("accuracy_contested", nextMonth), "", viewOperator, "")
	s.expect(res, 200)
	var restricted platform.Restriction
	if err := json.Unmarshal(res.Body.Bytes(), &restricted); err != nil || !restricted.Restricted || len(restricted.Decisions) != 1 ||
		restricted.Decisions[0].Reason != "accuracy_contested" || restricted.Decisions[0].ReviewBy != nextMonth || restricted.Decisions[0].Actor != roleOperator {
		t.Fatalf("restricted %s", res.Body.String())
	}
	s.expect(s.request("POST", restrictionPath(record), restrictBody("legal_claims", nextMonth), "", viewOperator, ""), 409)
	// Restricted: no source writes, no matching.
	s.expect(s.request("PATCH", "/v1/profiles/"+record, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"1"`), 409)
	s.expect(s.request("GET", "/v1/operator/profiles/"+record+"/match-candidates", "", "", viewOperator, ""), 404)

	s.expect(s.request("POST", restrictionPath(record), `{"action":"lift","reason":"accuracy_verified"}`, "", viewOperator, ""), 200)
	s.expect(s.request("PATCH", "/v1/profiles/"+record, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"1"`), 200)
	s.expect(s.request("GET", "/v1/operator/profiles/"+record+"/match-candidates", "", "", viewOperator, ""), 200)
	if r := s.restriction(record); r.Restricted || len(r.Decisions) != 2 || r.Decisions[0].Action != "lift" || r.Decisions[1].Action != "restrict" {
		t.Fatalf("after lifting %+v", r)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE processing_restriction_decisions SET reason = 'legal_claims'`); err == nil {
		t.Fatal("a restriction decision was edited")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM processing_restriction_decisions`); err == nil {
		t.Fatal("a restriction decision was deleted")
	}

	// An erasure fence is never loosened through a restriction.
	if _, err := s.pool.Exec(ctx, `UPDATE profiles SET lifecycle_state = 'erasure_pending' WHERE id = $1`, record); err != nil {
		t.Fatal(err)
	}
	s.expect(s.request("POST", restrictionPath(record), restrictBody("legal_claims", nextMonth), "", viewOperator, ""), 409)
}

// A restriction past its review date is listed for review and stays in force until it is lifted.
func TestRestrictionsDueForReview(t *testing.T) {
	s := setupLinking(t)
	ctx := context.Background()
	overdue := s.viewRecord(tokenA, "due-ada", `{"tier":"gold"}`)
	current := s.viewRecord(tokenA, "due-bob", `{"tier":"gold"}`)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	s.expect(s.request("POST", restrictionPath(current), restrictBody("legal_claims", nextMonth), "", viewOperator, ""), 200)
	// A decision whose review date has since passed, as time would leave it.
	if _, err := s.pool.Exec(ctx, `UPDATE profiles SET restricted = true WHERE id = $1`, overdue); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO processing_restriction_decisions (id, profile_id, action, reason, review_by, actor)
		VALUES ($1, $2, 'restrict', 'objection_pending', current_date - 1, 'synthetic-operator')`, platform.UUID(), overdue); err != nil {
		t.Fatal(err)
	}
	due := func() []platform.RestrictionDue {
		t.Helper()
		res := s.request("GET", "/v1/operator/restrictions/due", "", "", viewOperator, "")
		s.expect(res, 200)
		var page struct{ Items []platform.RestrictionDue }
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		return page.Items
	}
	if items := due(); len(items) != 1 || items[0].ProfileID != overdue || items[0].Reason != "objection_pending" {
		t.Fatalf("due %+v", items)
	}
	if !s.restriction(overdue).Restricted {
		t.Fatal("an overdue restriction was lifted by its date passing")
	}
	s.expect(s.request("POST", restrictionPath(overdue), `{"action":"lift","reason":"objection_decided"}`, "", viewOperator, ""), 200)
	if items := due(); len(items) != 0 {
		t.Fatalf("a lifted restriction is still due: %+v", items)
	}
	s.expect(s.request("GET", "/v1/operator/restrictions/due?all=1", "", "", viewOperator, ""), 400)
	s.expect(s.request("GET", "/v1/operator/restrictions/due", "", "", tokenA, ""), 401)
}

// Deciding a restriction needs privacy_admin; reading it needs only profile_reader.
func TestRestrictionRoles(t *testing.T) {
	s := setupLinking(t)
	record := s.viewRecord(tokenA, "roles-ada", `{"tier":"gold"}`)
	issuer := newRolesIssuer(t, "profile_reader")
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorKratosPublicURL: issuer.url,
		KratosPublicURL: "http://customer-kratos.invalid:4433", Tokens: map[string]string{"console": tokenA, "importer": tokenB}})
	if err != nil {
		t.Fatal(err)
	}
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	if w := operatorSession(h, "GET", restrictionPath(record), ""); w.Code != 200 {
		t.Fatalf("profile_reader could not read a restriction: %d", w.Code)
	}
	if w := operatorSession(h, "POST", restrictionPath(record), restrictBody("legal_claims", nextMonth)); w.Code != 403 {
		t.Fatalf("profile_reader restricted a profile: %d", w.Code)
	}
	if w := operatorSession(h, "GET", "/v1/operator/restrictions/due", ""); w.Code != 403 {
		t.Fatalf("profile_reader listed due restrictions: %d", w.Code)
	}
	issuer.assign("profile_reader", "privacy_admin")
	if w := operatorSession(h, "POST", restrictionPath(record), restrictBody("legal_claims", nextMonth)); w.Code != 200 {
		t.Fatalf("privacy_admin could not restrict: %d %s", w.Code, w.Body.String())
	}
	var actor string
	if err := s.pool.QueryRow(context.Background(), `SELECT actor FROM processing_restriction_decisions WHERE profile_id = $1`, record).Scan(&actor); err != nil || actor != roleOperator {
		t.Fatalf("decision actor %q %v", actor, err)
	}
}

// Restricting a customer's anchor withholds the customer everywhere processing decides through their
// view: their unified view, segment previews, snapshots and activation dispatch, including deliveries
// queued before the restriction. Lifting it restores them.
func TestRestrictedCustomersAreWithheldFromAudiences(t *testing.T) {
	hook := newReceiver(t)
	s := setupActivation(t, hook)
	s.goldSegment()
	ada := s.anchor(linkCustomerA)
	s.destination("restrict_hook", hook)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	if p, _ := s.preview("gold", ""); p.Members != 2 {
		t.Fatalf("before restricting: %d members", p.Members)
	}
	queued := s.activate("gold", "restrict_hook")
	if queued.Queued != 2 {
		t.Fatalf("queued %d", queued.Queued)
	}

	s.expect(s.request("POST", restrictionPath(ada), restrictBody("objection_pending", nextMonth), "", viewOperator, ""), 200)
	s.expect(s.request("GET", viewPath(ada), "", "", viewOperator, ""), 409)
	if p, _ := s.preview("gold", ""); p.Members != 1 || p.Excluded.Unavailable != 1 {
		t.Fatalf("restricted customer previewed: %+v", p)
	}
	if snap := s.snapshot("gold"); snap.Members != 1 || snap.Excluded.Unavailable != 1 {
		t.Fatalf("restricted customer snapshotted: %+v", snap)
	}
	for s.dispatch() > 0 {
	}
	status := s.status(queued.ID)
	if d := deliveryByAnchor(status, ada); d.State != "suppressed" || d.Reason != "view_unavailable" {
		t.Fatalf("a delivery queued before the restriction: %+v", d)
	}
	if len(hook.deliveries()) != 1 {
		t.Fatalf("%d deliveries sent", len(hook.deliveries()))
	}

	s.expect(s.request("POST", restrictionPath(ada), `{"action":"lift","reason":"objection_decided"}`, "", viewOperator, ""), 200)
	s.expect(s.request("GET", viewPath(ada), "", "", viewOperator, ""), 200)
	if p, _ := s.preview("gold", ""); p.Members != 2 {
		t.Fatalf("after lifting: %d members", p.Members)
	}
}

// Only a profile's latest decision counts for review: an overdue restriction that was lifted and
// restricted again with a later review date is not due.
func TestRestrictionsDueUseTheLatestDecision(t *testing.T) {
	s := setupLinking(t)
	ctx := context.Background()
	record := s.viewRecord(tokenA, "latest-ada", `{"tier":"gold"}`)
	if _, err := s.pool.Exec(ctx, `INSERT INTO processing_restriction_decisions (id, profile_id, action, reason, review_by, actor, decided_at)
		VALUES ($1, $2, 'restrict', 'legal_claims', current_date - 10, 'synthetic-operator', now() - interval '20 days'),
		       ($3, $2, 'lift', 'claim_resolved', NULL, 'synthetic-operator', now() - interval '5 days')`, platform.UUID(), record, platform.UUID()); err != nil {
		t.Fatal(err)
	}
	s.expect(s.request("POST", restrictionPath(record), restrictBody("legal_claims", time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)), "", viewOperator, ""), 200)
	res := s.request("GET", "/v1/operator/restrictions/due", "", "", viewOperator, "")
	s.expect(res, 200)
	if !strings.Contains(res.Body.String(), `"items":[]`) {
		t.Fatalf("a restriction with a future review date is due: %s", res.Body.String())
	}
}
