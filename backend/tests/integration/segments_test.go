package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const segmentsPath = "/v1/operator/segments"

type segmentPreview struct {
	Revision   int32  `json:"revision"`
	Population int    `json:"population"`
	Members    int    `json:"members"`
	NextCursor string `json:"next_cursor"`
	Excluded   struct {
		Unavailable, Consent, Criteria int
	} `json:"excluded"`
	Sample []struct {
		AnchorProfileID string `json:"anchor_profile_id"`
		Consent         struct {
			Outcome string `json:"outcome"`
		} `json:"consent"`
		Conditions []struct {
			Matched  bool `json:"matched"`
			Evidence []struct {
				Source     string `json:"source"`
				MutationID string `json:"mutation_id"`
			} `json:"evidence"`
		} `json:"conditions"`
	} `json:"sample"`
}

// consentPurpose creates a purpose with an active consent-based policy through the operator API.
func (s linkingSuite) consentPurpose(name string) (purpose, policy string) {
	s.t.Helper()
	res := s.request("POST", "/v1/operator/consent/purposes", `{"name":"`+name+`"}`, "", viewOperator, "")
	s.expect(res, 201)
	var p struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	res = s.request("POST", "/v1/operator/consent/policies/"+p.ID, `{"explanation":"Synthetic explanation.","lawful_basis":"consent","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`, "", viewOperator, "")
	s.expect(res, 201)
	var v struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &v)
	s.expect(s.request("POST", "/v1/operator/consent/policy-versions/"+v.ID+"/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	return p.ID, v.ID
}

// decideAsCustomer records a decision through the customer's own consent route, at the purpose
// revision the customer currently sees.
func (s linkingSuite) decideAsCustomer(customer, purpose, policy, decision string) {
	s.t.Helper()
	state := s.customer("GET", "/v1/customer/consent", "", customer)
	s.expect(state, 200)
	var page struct{ Items []consentItem }
	if err := json.Unmarshal(state.Body.Bytes(), &page); err != nil {
		s.t.Fatal(err)
	}
	var revision int64
	for _, item := range page.Items {
		if item.PurposeID == purpose {
			revision = item.Revision
		}
	}
	req := httptest.NewRequest("POST", "/v1/customer/consent/decisions", strings.NewReader(fmt.Sprintf(`{"policy_version_id":%q,"decision":%q}`, policy, decision)))
	req.Header.Set("X-Session-Token", customer)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "key-"+platform.UUID())
	req.Header.Set("If-Match", fmt.Sprintf(`"%d"`, revision))
	res := httptest.NewRecorder()
	s.h.ServeHTTP(res, req)
	s.expect(res, 201)
}

func (s linkingSuite) preview(name, query string) (segmentPreview, string) {
	s.t.Helper()
	res := s.request("GET", segmentsPath+"/"+name+"/preview"+query, "", "", viewOperator, "")
	s.expect(res, 200)
	var p segmentPreview
	if err := json.Unmarshal(res.Body.Bytes(), &p); err != nil {
		s.t.Fatal(err)
	}
	return p, res.Body.String()
}

// The Epic 3 scenario: two customers with governed values from two sources, one segment that needs
// consent, a conflicting source, a segmentation-only attribute, and a withdrawal that changes the preview.
func TestSegmentPreviewSelectsConsentingGovernedCustomers(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["operator","segmentation"],"sources":["console","importer"]}`)
	s.govern("lifetime_value", `{"type":"decimal"}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.viewRecord(tokenA, "seg-ada", `{"tier":"gold","lifetime_value":120.5}`)
	s.viewRecord(tokenB, "seg-ada-erp", `{"tier":"silver"}`)
	s.viewRecord(tokenA, "seg-bob", `{"tier":"gold","lifetime_value":50}`)
	s.viewLink(linkIssuerA, linkCustomerA, "seg-ada")
	s.viewLink(linkIssuerB, linkCustomerA, "seg-ada-erp")
	s.viewLink(linkIssuerA, linkCustomerB, "seg-bob")
	ada, bob := s.anchor(linkCustomerA), s.anchor(linkCustomerB)
	purpose, policy := s.consentPurpose("segment_email")

	rule := fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"eq","value":"gold"},{"attribute":"lifetime_value","operator":"gte","value":100}]}`, purpose)
	s.expect(s.request("POST", segmentsPath+"/high_value", rule, "", viewOperator, ""), 201)

	// Without consent nobody is a member, however well they match.
	p, _ := s.preview("high_value", "")
	if p.Population != 2 || p.Members != 0 || p.Excluded.Consent != 2 {
		t.Fatalf("without consent %+v", p)
	}
	s.decideAsCustomer(linkCustomerA, purpose, policy, "grant")
	s.decideAsCustomer(linkCustomerB, purpose, policy, "grant")
	p, body := s.preview("high_value", "")
	if p.Members != 1 || p.Excluded.Criteria != 1 || len(p.Sample) != 1 || p.Sample[0].AnchorProfileID != ada || p.Sample[0].Consent.Outcome != "permitted" {
		t.Fatalf("high value preview %s", body)
	}
	for _, c := range p.Sample[0].Conditions {
		if !c.Matched || len(c.Evidence) != 1 || c.Evidence[0].Source != "console" || c.Evidence[0].MutationID == "" {
			t.Fatalf("explanation lost provenance %s", body)
		}
	}
	for _, leaked := range []string{"120.5", "gold", "silver"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("a preview disclosed %q: %s", leaked, body)
		}
	}

	// Importer's conflicting silver keeps Ada out of a negative criterion; a source filter can scope it.
	notSilver := fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"neq","value":"silver"}]}`, purpose)
	s.expect(s.request("POST", segmentsPath+"/not_silver", notSilver, "", viewOperator, ""), 201)
	if p, _ := s.preview("not_silver", ""); p.Members != 1 || p.Sample[0].AnchorProfileID != bob {
		t.Fatalf("conflict semantics %+v", p)
	}
	consoleOnly := strings.Replace(notSilver, `"value":"silver"}`, `"value":"silver","sources":["console"]}`, 1)
	s.expect(s.request("POST", segmentsPath+"/not_silver", consoleOnly, "", viewOperator, `"1"`), 201)
	if p, _ := s.preview("not_silver", "?limit=1"); p.Revision != 2 || p.Members != 2 || len(p.Sample) != 1 || p.NextCursor == "" {
		t.Fatalf("source-scoped revision %+v", p)
	} else if next, _ := s.preview("not_silver", "?limit=1&cursor="+p.NextCursor); len(next.Sample) != 1 || next.NextCursor != "" || next.Sample[0].AnchorProfileID == p.Sample[0].AnchorProfileID {
		t.Fatalf("second page %+v", next)
	}

	// A withdrawal takes effect on the next preview; nothing was stored to invalidate.
	s.decideAsCustomer(linkCustomerA, purpose, policy, "withdraw")
	if p, _ := s.preview("high_value", ""); p.Members != 0 || p.Excluded.Consent != 1 || p.Excluded.Criteria != 1 {
		t.Fatalf("after withdrawal %+v", p)
	}
	// A customer whose lifecycle no longer permits a view is never a member.
	s.decideAsCustomer(linkCustomerA, purpose, policy, "grant")
	if p, _ := s.preview("high_value", ""); p.Members != 1 {
		t.Fatalf("after the new grant %+v", p)
	}
	s.exec(`UPDATE consent_subjects SET lifecycle_state='erasure_pending' WHERE kind='customer' AND anchor_profile_id=$1`, ada)
	if p, _ := s.preview("high_value", ""); p.Members != 0 || p.Excluded.Unavailable != 1 {
		t.Fatalf("an unavailable customer stayed a member %+v", p)
	}
	s.exec(`UPDATE consent_subjects SET lifecycle_state='active' WHERE kind='customer' AND anchor_profile_id=$1`, ada)

	// Withdrawing segmentation's grant would make the value missing to every customer; the preview refuses
	// rather than silently change what the segment selects, until segmentation is granted again.
	s.expect(s.request("POST", "/v1/operator/attributes/lifetime_value/access", `{"readers":["operator"],"sources":["console"]}`, "", viewOperator, `"1"`), 201)
	s.expect(s.request("GET", segmentsPath+"/high_value/preview", "", "", viewOperator, ""), 409)
	s.expect(s.request("POST", "/v1/operator/attributes/lifetime_value/access", `{"readers":["segmentation"],"sources":["console"]}`, "", viewOperator, `"2"`), 201)
	if p, _ := s.preview("high_value", ""); p.Members != 1 {
		t.Fatalf("after granting segmentation again %+v", p)
	}
}

// Under match any, a member's sample lists only the conditions that admitted it.
func TestSegmentSampleExplainsOnlyMatchedConditions(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.govern("plan", `{"type":"string"}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.viewRecord(tokenA, "any-ada", `{"tier":"gold","plan":"basic"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "any-ada")
	purpose, policy := s.consentPurpose("segment_any")
	s.decideAsCustomer(linkCustomerA, purpose, policy, "grant")
	rule := fmt.Sprintf(`{"purpose_id":%q,"match":"any","conditions":[{"attribute":"tier","operator":"eq","value":"gold"},{"attribute":"plan","operator":"neq","value":"basic"}]}`, purpose)
	s.expect(s.request("POST", segmentsPath+"/any_gold", rule, "", viewOperator, ""), 201)
	p, body := s.preview("any_gold", "")
	if p.Members != 1 || len(p.Sample[0].Conditions) != 1 || !p.Sample[0].Conditions[0].Matched {
		t.Fatalf("a failed condition was explained: %s", body)
	}
}

// Two operators revising from the same revision cannot both succeed.
func TestConcurrentSegmentRevisionsSerialize(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation"],"sources":["console"]}`)
	purpose, _ := s.consentPurpose("segment_race")
	rule := fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"present"}]}`, purpose)
	s.expect(s.request("POST", segmentsPath+"/race", rule, "", viewOperator, ""), 201)
	codes := make(chan int, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes <- s.request("POST", segmentsPath+"/race", rule, "", viewOperator, `"1"`).Code
		}()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for code := range codes {
		got[code]++
	}
	if got[201] != 1 || got[412] != 1 {
		t.Fatalf("concurrent revisions %v", got)
	}
}

func TestSegmentDefinitionsAreTypedVersionedAndOperatorOnly(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.govern("display_name", `{"type":"string"}`, `{"readers":["operator"],"sources":["console"]}`)
	purpose, _ := s.consentPurpose("segment_rules")
	rule := func(condition string) string {
		return fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[%s]}`, purpose, condition)
	}
	gold := rule(`{"attribute":"tier","operator":"eq","value":"gold"}`)

	s.expect(s.request("POST", segmentsPath+"/gold", gold, "", tokenA, ""), 401)
	s.expect(s.request("GET", segmentsPath+"/gold/preview", "", "", tokenA, ""), 401)
	s.expect(s.customer("GET", segmentsPath+"/gold/preview", "", linkCustomerA), 401)
	s.expect(s.request("GET", segmentsPath+"/gold/preview", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", segmentsPath+"/Gold", "", "", viewOperator, ""), 404)
	for _, invalid := range []string{
		rule(`{"attribute":"undefined","operator":"present"}`),
		rule(`{"attribute":"display_name","operator":"present"}`),
		rule(`{"attribute":"tier","operator":"eq","value":"bronze"}`),
		rule(`{"attribute":"tier","operator":"gt","value":"gold"}`),
		strings.Replace(gold, purpose, platform.UUID(), 1),
		strings.Replace(gold, purpose, "not-a-uuid", 1),
		strings.Replace(gold, `"all"`, `"most"`, 1),
	} {
		s.expect(s.request("POST", segmentsPath+"/gold", invalid, "", viewOperator, ""), 422)
	}
	s.expect(s.request("POST", segmentsPath+"/gold", strings.Replace(gold, `"match"`, `"sql":"SELECT 1","match"`, 1), "", viewOperator, ""), 400)

	created := s.request("POST", segmentsPath+"/gold", gold, "", viewOperator, "")
	s.expect(created, 201)
	if created.Header().Get("ETag") != `"1"` {
		t.Fatalf("etag %q", created.Header().Get("ETag"))
	}
	s.expect(s.request("POST", segmentsPath+"/gold", gold, "", viewOperator, ""), 428)
	s.expect(s.request("POST", segmentsPath+"/gold", gold, "", viewOperator, `"2"`), 412)
	s.expect(s.request("POST", segmentsPath+"/gold", rule(`{"attribute":"tier","operator":"in","values":["gold","silver"]}`), "", viewOperator, `"1"`), 201)
	history := s.request("GET", segmentsPath+"/gold", "", "", viewOperator, "")
	s.expect(history, 200)
	if strings.Count(history.Body.String(), `"revision"`) != 2 {
		t.Fatalf("history %s", history.Body.String())
	}
	list := s.request("GET", segmentsPath, "", "", viewOperator, "")
	s.expect(list, 200)
	if strings.Count(list.Body.String(), `"revision":2`) != 1 {
		t.Fatalf("current segments %s", list.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE segment_definitions SET rule='{}'`); err == nil {
		t.Fatal("a segment revision was edited in place")
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?cursor=x", "?sql=1", "?limit=1&limit=2"} {
		s.expect(s.request("GET", segmentsPath+"/gold/preview"+query, "", "", viewOperator, ""), 400)
	}
	s.expect(s.request("POST", segmentsPath+"/gold/preview", "", "", viewOperator, ""), 405)
}

// A preview evaluates every customer or refuses; it never samples a population silently.
func TestSegmentPreviewIsBounded(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation"],"sources":["console"]}`)
	purpose, _ := s.consentPurpose("segment_bound")
	s.expect(s.request("POST", segmentsPath+"/everyone", fmt.Sprintf(`{"purpose_id":%q,"match":"any","conditions":[{"attribute":"tier","operator":"missing"}]}`, purpose), "", viewOperator, ""), 201)
	anchors := func(n int) {
		if _, err := s.pool.Exec(context.Background(), `WITH p AS (INSERT INTO profiles(id,version,attributes) SELECT gen_random_uuid(),1,'{}' FROM generate_series(1,$1) RETURNING id)
			INSERT INTO customer_identity_links(kratos_identity_id,profile_id) SELECT gen_random_uuid(),id FROM p`, n); err != nil {
			t.Fatal(err)
		}
	}
	anchors(998)
	started := time.Now()
	if p, _ := s.preview("everyone", ""); p.Population != 1000 || p.Excluded.Consent != 1000 {
		t.Fatalf("full population %+v", p)
	}
	t.Logf("preview of 1000 customers took %s", time.Since(started))
	// An evaluation past the store's timeout is refused, not returned partial.
	hurried, err := platform.NewAPI(platform.Store{Pool: s.pool, EvaluationTimeout: time.Millisecond}, s.config)
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	req := httptest.NewRequest("GET", segmentsPath+"/everyone/preview", nil)
	req.Header.Set("Authorization", "Bearer "+viewOperator)
	hurried.ServeHTTP(res, req)
	s.expect(res, 503)
	anchors(1)
	s.expect(s.request("GET", segmentsPath+"/everyone/preview", "", "", viewOperator, ""), 409)
}
