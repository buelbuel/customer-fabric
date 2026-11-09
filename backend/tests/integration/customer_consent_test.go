package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const (
	ownConsent = "/v1/customer/consent"
	ownHistory = ownConsent + "/history"
	decisions  = ownConsent + "/decisions"
)

type consentItem struct {
	PurposeID          string   `json:"purpose_id"`
	PurposeName        string   `json:"purpose_name"`
	Revision           int64    `json:"revision"`
	LatestDecision     string   `json:"latest_decision"`
	AvailableDecisions []string `json:"available_decisions"`
	Policy             struct {
		ID          string `json:"id"`
		Version     int    `json:"version"`
		State       string `json:"state"`
		Explanation string `json:"explanation"`
	} `json:"policy"`
	Effective evaluationView `json:"effective"`
}

type consentCustomer struct {
	profile, kratos, session string
}

func (s *customerAPI) customer() consentCustomer {
	s.t.Helper()
	profile, kratos := s.customerProfile()
	return consentCustomer{profile, kratos, s.session(kratos, "active")}
}

func (s *customerAPI) state(c consentCustomer, purposeID string) consentItem {
	s.t.Helper()
	w := s.send("GET", ownConsent, "", nativeSession(c.session))
	s.expect(w, 200)
	var page struct{ Items []consentItem }
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		s.t.Fatal(err)
	}
	for _, item := range page.Items {
		if item.PurposeID == purposeID {
			return item
		}
	}
	s.t.Fatalf("purpose %s is not listed", purposeID)
	return consentItem{}
}

func decisionBody(policy, decision string) string {
	return fmt.Sprintf(`{"policy_version_id":%q,"decision":%q}`, policy, decision)
}

func idempotent(key string) credential {
	return func(r *http.Request) { r.Header.Set("Idempotency-Key", key) }
}

func revision(n int64) credential {
	return func(r *http.Request) { r.Header.Set("If-Match", fmt.Sprintf(`"%d"`, n)) }
}

func (s *customerAPI) decide(c consentCustomer, policy, decision string, extra ...credential) *httptest.ResponseRecorder {
	all := append([]credential{nativeSession(c.session), idempotent("key-" + platform.UUID())}, extra...)
	return s.send("POST", decisions, decisionBody(policy, decision), all...)
}

func TestCustomerGrantAndWithdrawalDriveEffectivePermission(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()

	before := s.state(c, p.ID)
	if before.Policy.Explanation != "Synthetic explanation." || before.Policy.Version != 1 || before.Revision != 1 ||
		before.Effective.Outcome != "blocked_fail_closed" || strings.Join(before.AvailableDecisions, ",") != "grant,withdraw" {
		t.Fatalf("initial state %+v", before)
	}
	s.expect(s.decide(c, policy, "grant", revision(before.Revision)), 201)
	if got := s.outcome(c.profile, p.ID); got != "permitted" {
		t.Fatalf("after grant: %s", got)
	}
	granted := s.state(c, p.ID)
	if granted.LatestDecision != "grant" || granted.Revision != 2 {
		t.Fatalf("after grant %+v", granted)
	}
	s.expect(s.decide(c, policy, "withdraw"), 201)
	if got := s.outcome(c.profile, p.ID); got != "blocked" {
		t.Fatalf("after withdrawal: %s", got)
	}
	s.expect(s.decide(c, policy, "grant", revision(3)), 201)
	if got := s.outcome(c.profile, p.ID); got != "permitted" {
		t.Fatalf("a later customer grant did not lift the withdrawal: %s", got)
	}

	var actor, channel, proof string
	if err := s.pool.QueryRow(context.Background(), `SELECT actor_kind, proof_channel, proof_reference FROM consent_evidence ORDER BY received_at DESC LIMIT 1`).Scan(&actor, &channel, &proof); err != nil {
		t.Fatal(err)
	}
	if actor != "customer" || channel != "customer_native" || proof != "kratos_identity:"+c.kratos {
		t.Fatalf("recorded %s %s %s", actor, channel, proof)
	}
}

func TestCustomerConsentIsScopedToTheSession(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	a, b := s.customer(), s.customer()
	s.expect(s.decide(a, policy, "grant", revision(1)), 201)

	if got := s.state(b, p.ID); got.LatestDecision != "" || got.Revision != 1 {
		t.Fatalf("another customer's decision leaked: %+v", got)
	}
	w := s.send("GET", ownHistory, "", nativeSession(b.session))
	s.expect(w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"items":[]}` {
		t.Fatalf("history leaked: %s", w.Body.String())
	}
	if got := s.outcome(b.profile, p.ID); got != "blocked_fail_closed" {
		t.Fatalf("grant crossed customers: %s", got)
	}

	var evidenceID string
	own := s.send("GET", ownHistory, "", nativeSession(a.session))
	s.expect(own, 200)
	var page struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal(own.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatalf("own history: %v %s", err, own.Body.String())
	}
	evidenceID = page.Items[0].ID
	for _, forbidden := range []string{"proof_reference", "kratos_identity", "idempotency"} {
		if strings.Contains(own.Body.String(), forbidden) {
			t.Fatalf("history disclosed %s", forbidden)
		}
	}
	foreign := s.send("GET", ownHistory+"?cursor="+evidenceID, "", nativeSession(b.session))
	s.expect(foreign, 200)
	if strings.TrimSpace(foreign.Body.String()) != `{"items":[]}` {
		t.Fatalf("a foreign cursor exposed data: %s", foreign.Body.String())
	}

	for _, field := range []string{`"customer_subject":"` + a.kratos + `"`, `"actor_kind":"operator"`, `"occurred_at":"2020-01-01T00:00:00Z"`} {
		body := strings.TrimSuffix(decisionBody(policy, "withdraw"), "}") + "," + field + "}"
		s.expect(s.send("POST", decisions, body, nativeSession(b.session), idempotent("key-"+platform.UUID())), 400)
	}
	s.expect(s.send("GET", ownConsent+"?subject="+a.kratos, "", nativeSession(b.session)), 400)
}

func TestConsentAudiencesStaySeparate(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	for name, token := range map[string]string{"operator": operatorToken, "source": tokenA} {
		bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }
		t.Run(name, func(t *testing.T) {
			s.expect(s.send("GET", ownConsent, "", bearer), 401)
			s.expect(s.send("GET", ownHistory, "", bearer), 401)
			s.expect(s.send("POST", decisions, decisionBody(policy, "grant"), bearer, idempotent("key-"+platform.UUID())), 401)
		})
	}
	session := nativeSession(c.session)
	s.expect(s.send("GET", "/v1/operator/consent/effective/"+c.profile+"/"+p.ID, "", session), 401)
	s.expect(s.send("POST", "/v1/operator/consent/purposes", `{"name":"customer_made"}`, session), 401)
	s.expect(s.send("POST", "/v1/operator/consent/evidence/customer/"+c.profile, "{}", session, idempotent("key-"+platform.UUID())), 401)
	s.expect(s.send("POST", "/v1/profiles", `{"set":{"a":"b"}}`, session), 401)
	cookie := browserCookie(c.session)
	s.expect(s.send("GET", "/v1/operator/consent/effective/"+c.profile+"/"+p.ID, "", cookie), 401)
	if s.count("consent_evidence") != 0 {
		t.Fatal("a rejected audience wrote evidence")
	}
}

func TestCustomerDecisionRejectsInvalidPolicies(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	active := s.activePolicy(p.ID, "consent", false)
	draft := s.draft(p.ID)
	contract := s.purpose("service_messages")
	contractPolicy := s.activePolicy(contract.ID, "contract", false)
	c := s.customer()

	for name, tc := range map[string]struct {
		policy, decision string
		status           int
	}{
		"malformed":    {"not-a-uuid", "grant", 422},
		"unknown":      {platform.UUID(), "grant", 422},
		"draft":        {draft.ID, "grant", 409},
		"non-consent":  {contractPolicy, "grant", 422},
		"objection":    {active, "object", 422},
		"denial":       {active, "deny", 422},
		"unrecognized": {active, "maybe", 422},
	} {
		t.Run(name, func(t *testing.T) {
			s.expect(s.decide(c, tc.policy, tc.decision, revision(1)), tc.status)
		})
	}
	s.expect(s.decide(c, active, "grant"), 428)
	s.expect(s.decide(c, active, "grant", func(r *http.Request) { r.Header.Set("If-Match", `"0"`) }), 400)
	s.expect(s.send("POST", decisions, decisionBody(active, "grant"), nativeSession(c.session), revision(1)), 422)
	if item := s.state(c, contract.ID); len(item.AvailableDecisions) != 0 {
		t.Fatalf("a non-consent purpose offered %v", item.AvailableDecisions)
	}
	if s.count("consent_evidence") != 0 {
		t.Fatal("an invalid decision wrote evidence")
	}
}

func TestRetiredVersionsAcceptWithdrawalsOnly(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	s.expect(s.decide(c, policy, "grant", revision(1)), 201)
	s.expect(s.transition(policy, "retired"), 200)

	item := s.state(c, p.ID)
	if item.Policy.State != "retired" || strings.Join(item.AvailableDecisions, ",") != "withdraw" {
		t.Fatalf("retired state %+v", item)
	}
	s.expect(s.decide(c, policy, "grant", revision(item.Revision)), 409)
	s.expect(s.decide(c, policy, "withdraw"), 201)

	// A customer who never decided has nothing to act on once the version is retired.
	other := s.customer()
	w := s.send("GET", ownConsent, "", nativeSession(other.session))
	s.expect(w, 200)
	if strings.Contains(w.Body.String(), p.ID) {
		t.Fatal("a retired purpose was shown to a customer who never decided on it")
	}
}

func TestStaleGrantIsRefusedButStaleWithdrawalIsNot(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	s.expect(s.decide(c, policy, "withdraw"), 201)

	// The first tab loaded the page before that withdrawal.
	s.expect(s.decide(c, policy, "grant", revision(1)), 412)
	if got := s.outcome(c.profile, p.ID); got != "blocked" {
		t.Fatalf("a stale grant changed the outcome: %s", got)
	}
	s.expect(s.decide(c, policy, "withdraw", revision(1)), 201)
	s.customerEvidence(c.profile, policy, "withdraw", "operator-after-view")
	if s.state(c, p.ID).Revision != 4 {
		t.Fatal("an operator decision did not advance the revision")
	}
	s.expect(s.decide(c, policy, "grant", revision(3)), 412)
}

func TestRecordWithdrawalMakesAStaleGrantFail(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	rp, epoch := s.record("", "carried-revision")
	s.link(c.kratos, rp, epoch)
	s.recordEvidence(epoch, policy, "withdraw", "record-withdraw-01")
	s.expect(s.decide(c, policy, "grant", revision(1)), 412)
	s.expect(s.decide(c, policy, "grant", revision(2)), 201)
}

func TestCustomerDecisionReplayReturnsOriginalReceipt(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	key := idempotent("key-" + platform.UUID())
	first := s.send("POST", decisions, decisionBody(policy, "grant"), nativeSession(c.session), key, revision(1))
	s.expect(first, 201)
	s.expect(s.decide(c, policy, "withdraw"), 201)

	replay := s.send("POST", decisions, decisionBody(policy, "grant"), nativeSession(c.session), key, revision(1))
	s.expect(replay, 200)
	var a, b platform.EvidenceReceipt
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(replay.Body.Bytes(), &b)
	if a.EvidenceID != b.EvidenceID || b.Created {
		t.Fatalf("replay %+v of %+v", b, a)
	}
	s.expect(s.send("POST", decisions, decisionBody(policy, "withdraw"), nativeSession(c.session), key), 409)
	if s.count("consent_evidence") != 2 {
		t.Fatal("a replay wrote evidence")
	}
}

func TestConcurrentGrantsWithOneRevisionRecordOnce(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	const writers = 8
	statuses := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = s.decide(c, policy, "grant", revision(1)).Code
		}()
	}
	wg.Wait()
	created := 0
	for _, status := range statuses {
		switch status {
		case 201:
			created++
		case 412:
		default:
			t.Fatalf("status %d", status)
		}
	}
	if created != 1 || s.count("consent_evidence") != 1 {
		t.Fatalf("created %d evidence rows from %v", created, statuses)
	}
}

// Whichever request wins the subject lock, a withdrawal racing a grant must leave processing stopped.
func TestConcurrentWithdrawalAlwaysStopsProcessing(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	for range 12 {
		c := s.customer()
		var wg sync.WaitGroup
		wg.Add(2)
		var grant, withdraw int
		go func() { defer wg.Done(); grant = s.decide(c, policy, "grant", revision(1)).Code }()
		go func() { defer wg.Done(); withdraw = s.decide(c, policy, "withdraw").Code }()
		wg.Wait()
		if withdraw != 201 || (grant != 201 && grant != 412) {
			t.Fatalf("grant %d withdraw %d", grant, withdraw)
		}
		if got := s.outcome(c.profile, p.ID); got != "blocked" {
			t.Fatalf("grant %d left outcome %s", grant, got)
		}
	}
}

func TestConsentRequiresAProvisionedCustomer(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	session := nativeSession(s.session(platform.UUID(), "active"))
	s.expect(s.send("GET", ownConsent, "", session), 404)
	s.expect(s.send("GET", ownHistory, "", session), 404)
	s.expect(s.send("POST", decisions, decisionBody(policy, "grant"), session, idempotent("key-"+platform.UUID()), revision(1)), 409)
}

func TestConsentRejectsUnusableSessions(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	for state, status := range map[string]int{"expired": 401, "unverified": 403, "outage": 503} {
		session := nativeSession(s.session(platform.UUID(), state))
		s.expect(s.send("GET", ownConsent, "", session), status)
		s.expect(s.send("POST", decisions, decisionBody(policy, "withdraw"), session, idempotent("key-"+platform.UUID())), status)
	}
}

func TestBrowserConsentWritesAreProtected(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	key := idempotent("key-" + platform.UUID())
	body := decisionBody(policy, "grant")
	foreign := func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }
	s.expect(s.send("POST", decisions, body, browserSession(c.session), foreign, key, revision(1)), 403)
	s.expect(s.send("POST", decisions, body, browserCookie(c.session), key, revision(1)), 403)
	s.expect(s.send("POST", decisions, body, nativeSession(c.session), browserCookie(c.session), key, revision(1)), 401)
	s.expect(s.send("POST", decisions, body, browserSession(c.session), key, revision(1), func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }), 415)
	if s.count("consent_evidence") != 0 {
		t.Fatal("a rejected browser write recorded evidence")
	}
	s.expect(s.send("POST", decisions, body, browserSession(c.session), key, revision(1)), 201)
	var channel string
	if err := s.pool.QueryRow(context.Background(), `SELECT proof_channel FROM consent_evidence`).Scan(&channel); err != nil || channel != "customer_browser" {
		t.Fatalf("channel %q: %v", channel, err)
	}
}

func TestConsentHistoryPaginatesNewestFirst(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	c := s.customer()
	for i := range 3 {
		decision := "withdraw"
		if i%2 == 1 {
			decision = "grant"
		}
		s.expect(s.decide(c, policy, decision, revision(int64(i+1))), 201)
	}
	var seen []string
	cursor := ""
	for {
		path := ownHistory + "?limit=2"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		w := s.send("GET", path, "", nativeSession(c.session))
		s.expect(w, 200)
		var page struct {
			Items []struct {
				Decision string `json:"decision"`
			} `json:"items"`
			NextCursor string `json:"next_cursor"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			seen = append(seen, item.Decision)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if strings.Join(seen, ",") != "withdraw,grant,withdraw" {
		t.Fatalf("history order %v", seen)
	}
}

func TestPolicyExplanationIsRequiredAndImmutable(t *testing.T) {
	s := customerSetup(t)
	p := s.purpose("newsletter")
	base := `"lawful_basis":"consent","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"`
	for name, body := range map[string]string{
		"missing":  `{` + base + `}`,
		"blank":    `{"explanation":"   ",` + base + `}`,
		"too long": `{"explanation":"` + strings.Repeat("e", 2049) + `",` + base + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			s.expect(s.call("POST", "/v1/operator/consent/policies/"+p.ID, body, operatorToken), 422)
		})
	}
	policy := s.activePolicy(p.ID, "consent", false)
	if _, err := s.pool.Exec(context.Background(), `UPDATE consent_policy_versions SET explanation='changed' WHERE id=$1`, policy); err == nil {
		t.Fatal("an active version's explanation changed")
	}
	s.expect(s.transition(policy, "retired"), 200)
	if _, err := s.pool.Exec(context.Background(), `UPDATE consent_policy_versions SET explanation='changed' WHERE id=$1`, policy); err == nil {
		t.Fatal("a retired version's explanation changed")
	}
}
