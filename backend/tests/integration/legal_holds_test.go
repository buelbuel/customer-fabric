package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const holdApprover = "5a6b7c8d-9e0f-4a1b-8c2d-3e4f5a6b7c8d"

func holdBody(scope, profile, categories string) string {
	reviewBy := time.Now().UTC().AddDate(0, 6, 0).Format(time.DateOnly)
	return fmt.Sprintf(`{"scope":%q,"profile_id":%q,"categories":%s,"reason":"litigation","legal_reference":"CASE-2026-0042","review_by":%q}`,
		scope, profile, categories, reviewBy)
}

func decodeHold(t *testing.T, w *httptest.ResponseRecorder) platform.LegalHold {
	t.Helper()
	var hold platform.LegalHold
	if err := json.Unmarshal(w.Body.Bytes(), &hold); err != nil {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	return hold
}

// A hold is requested by one privacy admin and approved by another; it names its scope, categories,
// reason, legal reference and review date. Audit readers see it without the legal reference. Every
// decision is kept.
func TestLegalHoldsAreRequestedApprovedAndReleasedWithAudit(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "privacy_admin")
	h := s.kratosAPI(issuer.url)
	record := s.sourceRecord(tokenA)
	anchor, _ := s.customerProfile()
	holds := "/v1/operator/legal-holds"

	for body, status := range map[string]int{
		holdBody("account", record.profile, `["eventing"]`):                                                          422,
		holdBody("customer", record.profile, `["eventing"]`):                                                         422,
		holdBody("record", anchor, `["eventing"]`):                                                                   422,
		holdBody("record", record.profile, `["marketing"]`):                                                          422,
		holdBody("record", record.profile, `["eventing","eventing"]`):                                                422,
		holdBody("record", record.profile, `[]`):                                                                     422,
		strings.Replace(holdBody("record", record.profile, `["eventing"]`), "CASE-2026-0042", "case 42", 1):          422,
		strings.Replace(holdBody("record", record.profile, `["eventing"]`), `"review_by":"`, `"review_by":"2020`, 1): 422,
		holdBody("record", platform.UUID(), `["eventing"]`):                                                          404,
	} {
		if w := operatorSession(h, "POST", holds, body); w.Code != status {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	requested := operatorSession(h, "POST", holds, holdBody("record", record.profile, `["notifications","eventing"]`))
	hold := decodeHold(t, requested)
	if requested.Code != 201 || hold.Status != "requested" || !hold.InForce || hold.LegalReference != "CASE-2026-0042" || hold.RequestedBy != roleOperator {
		t.Fatalf("requested %d %+v", requested.Code, hold)
	}
	path := holds + "/" + hold.ID
	if w := operatorSession(h, "POST", path+"/approve", ""); w.Code != 403 {
		t.Fatalf("the requester approved its own hold: %d", w.Code)
	}
	issuer.as(holdApprover)
	approved := operatorSession(h, "POST", path+"/approve", "")
	if hold = decodeHold(t, approved); approved.Code != 200 || hold.Status != "active" || hold.ApprovedBy != holdApprover {
		t.Fatalf("approved %d %+v", approved.Code, hold)
	}
	if w := operatorSession(h, "POST", path+"/approve", ""); w.Code != 409 {
		t.Fatalf("approved twice: %d", w.Code)
	}

	// Reading: an audit reader without the reference, a profile reader not at all.
	issuer.assign("audit_reader")
	read := operatorSession(h, "GET", path, "")
	if read.Code != 200 || strings.Contains(read.Body.String(), "CASE-2026-0042") || !strings.Contains(read.Body.String(), `"reason":"litigation"`) {
		t.Fatalf("audit read %d %s", read.Code, read.Body.String())
	}
	if w := operatorSession(h, "POST", path+"/release", `{"reason":"matter_closed"}`); w.Code != 403 {
		t.Fatalf("an audit reader released a hold: %d", w.Code)
	}
	issuer.assign("profile_reader")
	if w := operatorSession(h, "GET", holds, ""); w.Code != 403 {
		t.Fatalf("a profile reader listed holds: %d", w.Code)
	}
	issuer.assign("privacy_admin")
	listed := operatorSession(h, "GET", holds+"?profile_id="+record.profile, "")
	if listed.Code != 200 || !strings.Contains(listed.Body.String(), "CASE-2026-0042") {
		t.Fatalf("list %d %s", listed.Code, listed.Body.String())
	}
	if w := operatorSession(h, "GET", holds+"?due=true&profile_id="+record.profile, ""); w.Code != 400 {
		t.Fatalf("conflicting query: %d", w.Code)
	}

	// Release, with a reason; it is final.
	if w := operatorSession(h, "POST", path+"/release", `{"reason":"because"}`); w.Code != 422 {
		t.Fatalf("release without a valid reason: %d", w.Code)
	}
	released := operatorSession(h, "POST", path+"/release", `{"reason":"matter_closed"}`)
	if hold = decodeHold(t, released); released.Code != 200 || hold.Status != "released" || hold.InForce || hold.ReleaseReason != "matter_closed" {
		t.Fatalf("released %d %+v", released.Code, hold)
	}
	if w := operatorSession(h, "POST", path+"/release", `{"reason":"matter_closed"}`); w.Code != 409 {
		t.Fatalf("released twice: %d", w.Code)
	}
	var actions []string
	for _, d := range hold.Decisions {
		actions = append(actions, d.Action+":"+d.Reason+":"+d.Actor)
	}
	if strings.Join(actions, ",") != "requested:litigation:"+roleOperator+",approved:approved:"+holdApprover+",released:matter_closed:"+holdApprover {
		t.Fatalf("decisions %v", actions)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE legal_hold_decisions SET reason = 'x'`); err == nil {
		t.Fatal("hold decisions are not append-only")
	}
}

// A request protects for 14 days unapproved, then lapses; an approved hold past its review date stays in
// force and is listed for review.
func TestLegalHoldsLapseUnapprovedAndComeDueForReview(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "privacy_admin")
	h := s.kratosAPI(issuer.url)
	ctx := context.Background()
	first, second := s.sourceRecord(tokenA), s.sourceRecord(tokenA)
	lapsing := decodeHold(t, operatorSession(h, "POST", "/v1/operator/legal-holds", holdBody("record", first.profile, `["eventing"]`)))
	due := decodeHold(t, operatorSession(h, "POST", "/v1/operator/legal-holds", holdBody("record", second.profile, `["eventing"]`)))
	issuer.as(holdApprover)
	if w := operatorSession(h, "POST", "/v1/operator/legal-holds/"+due.ID+"/approve", ""); w.Code != 200 {
		t.Fatalf("approve %d", w.Code)
	}
	// Time passes: grounds are immutable, so the test moves them past their dates with the guard off.
	for _, stmt := range []string{
		`ALTER TABLE legal_holds DISABLE TRIGGER legal_holds_forward_only`,
		`UPDATE legal_holds SET requested_at = now() - interval '15 days' WHERE id = '` + lapsing.ID + `'`,
		`UPDATE legal_holds SET review_by = current_date - 1 WHERE id = '` + due.ID + `'`,
		`ALTER TABLE legal_holds ENABLE TRIGGER legal_holds_forward_only`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	for _, stmt := range []string{
		`UPDATE legal_holds SET legal_reference = 'CASE-OTHER' WHERE id = '` + due.ID + `'`,
		`UPDATE legal_holds SET state = 'requested', approved_by = NULL, approved_at = NULL WHERE id = '` + due.ID + `'`,
		`DELETE FROM legal_holds WHERE id = '` + lapsing.ID + `'`,
	} {
		if _, err := s.pool.Exec(ctx, stmt); err == nil || !strings.Contains(err.Error(), "legal holds") {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if w := operatorSession(h, "POST", "/v1/operator/legal-holds/"+lapsing.ID+"/approve", ""); w.Code != 409 {
		t.Fatalf("a lapsed request was approved: %d", w.Code)
	}
	listed := operatorSession(h, "GET", "/v1/operator/legal-holds?due=true", "")
	var page struct{ Items []platform.LegalHold }
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Items) != 2 {
		t.Fatalf("due %d %s", listed.Code, listed.Body.String())
	}
	statuses := map[string]string{}
	for _, item := range page.Items {
		statuses[item.ID] = fmt.Sprint(item.Status, item.InForce)
	}
	if statuses[lapsing.ID] != "lapsedfalse" || statuses[due.ID] != "duetrue" {
		t.Fatalf("statuses %v", statuses)
	}
	var held []string
	rows, err := s.pool.Query(ctx, `SELECT profile_id::text FROM held_profiles('eventing')`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		held = append(held, id)
	}
	rows.Close()
	if len(held) != 1 || held[0] != second.profile {
		t.Fatalf("held %v", held)
	}
}

// Retention keeps exactly what a hold preserves: the categories it names, of the record or the
// customer's associated records, and nothing else. The report says how much a hold keeps.
func TestRetentionKeepsOnlyWhatAHoldPreserves(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "privacy_admin")
	h := s.kratosAPI(issuer.url)
	ctx := context.Background()
	store := platform.Store{Pool: s.pool}
	held, otherCategory, linked, free := s.sourceRecord(tokenA), s.sourceRecord(tokenA), s.sourceRecord(tokenB), s.sourceRecord(tokenA)
	anchor, kratos := s.customerProfile()
	s.link(kratos, linked.profile, linked.epoch)
	request := func(scope, profile, categories string) {
		t.Helper()
		issuer.as(roleOperator)
		hold := decodeHold(t, operatorSession(h, "POST", "/v1/operator/legal-holds", holdBody(scope, profile, categories)))
		issuer.as(holdApprover)
		if w := operatorSession(h, "POST", "/v1/operator/legal-holds/"+hold.ID+"/approve", ""); w.Code != 200 {
			t.Fatalf("approve %d %s", w.Code, w.Body.String())
		}
	}
	request("record", held.profile, `["eventing"]`)
	request("record", otherCategory.profile, `["notifications"]`)
	request("customer", anchor, `["eventing"]`)
	if _, err := s.pool.Exec(ctx, `UPDATE outbox SET occurred_at = now() - interval '400 days', relayed_at = now() - interval '400 days'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneEventing(ctx, platform.Retention{Relay: true}.WithDefaults(), time.Now()); err != nil {
		t.Fatal(err)
	}
	kept := func(profile string) bool {
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE profile_id = $1`, profile).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if !kept(held.profile) || !kept(linked.profile) || kept(otherCategory.profile) || kept(free.profile) {
		t.Fatalf("held %v linked %v other category %v free %v", kept(held.profile), kept(linked.profile), kept(otherCategory.profile), kept(free.profile))
	}
	issuer.assign("audit_reader")
	res := operatorSession(h, "GET", "/v1/operator/retention", "")
	var report platform.RetentionReport
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
		t.Fatalf("%d %s", res.Code, res.Body.String())
	}
	for _, c := range report.Categories {
		if c.Category == "outbox" && c.HeldPastHorizon != 2 {
			t.Fatalf("held past horizon %+v", c)
		}
	}
}
