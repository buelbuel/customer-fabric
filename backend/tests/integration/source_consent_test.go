package integration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const consentAuthoritiesPath = "/v1/operator/consent-authorities"

// sourceDecision is a source's consent report about one of its records; zero fields take valid defaults.
type sourceDecision struct {
	record, policy, decision, method, notice, digest string
	collectedAt                                      time.Time
	epoch                                            int64
}

func (s linkingSuite) reportConsent(token, key string, d sourceDecision) *httptest.ResponseRecorder {
	s.t.Helper()
	if d.decision == "" {
		d.decision = "grant"
	}
	if d.method == "" {
		d.method = "double_opt_in"
	}
	if d.notice == "" {
		d.notice = "n"
	}
	if d.digest == "" {
		sum := sha256.Sum256([]byte("synthetic proof " + key))
		d.digest = hex.EncodeToString(sum[:])
	}
	if d.collectedAt.IsZero() {
		d.collectedAt = time.Now().UTC()
	}
	if d.epoch == 0 {
		epoch, err := platform.Store{Pool: s.pool}.SourceEpoch(context.Background(), "console")
		if err != nil {
			s.t.Fatal(err)
		}
		d.epoch = epoch
	}
	body := fmt.Sprintf(`{"record":{"namespace":"crm","value":%q},"policy_version_id":%q,"decision":%q,"collected_at":%q,"source_epoch":%d,"proof":{"method":%q,"reference":"crm-receipt-%s","notice_reference":%q,"sha256":%q}}`,
		d.record, d.policy, d.decision, d.collectedAt.Format(time.RFC3339Nano), d.epoch, d.method, key, d.notice, d.digest)
	return s.request("POST", "/v1/source-consent-decisions", body, key, token, "")
}

func (s linkingSuite) approveAuthority(source, purpose string, methods ...string) string {
	s.t.Helper()
	quoted := make([]string, len(methods))
	for i, m := range methods {
		quoted[i] = fmt.Sprintf("%q", m)
	}
	res := s.request("POST", consentAuthoritiesPath, fmt.Sprintf(`{"source":%q,"purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":[%s]}`,
		source, purpose, strings.Join(quoted, ",")), "", viewOperator, "")
	s.expect(res, 201)
	var a struct{ ID string }
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		s.t.Fatal(err)
	}
	return a.ID
}

// Consent from a source counts only when the source holds an approved consent authority for the purpose
// and proves each grant: the notice shown, how it was collected, its own receipt and a digest of the
// stored proof. Operator entries and opt-in flags in the record never count. The verified grant is
// judged by the same consent engine, so a withdrawal, a revoked authority or a linked customer's own
// choice still decides.
func TestSourceConsentIsGovernedAndProven(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	marketing, policy := s.purposeWithBasis("source_marketing", "consent")
	interest, _ := s.purposeWithBasis("source_sync", "legitimate_interests")
	subscription := s.subscribe("source_marketing", subscriptionBody("crm_changes", marketing, "console"))
	n := platform.NewNotifier(platform.Store{Pool: s.pool}, s.config.WebhookHosts)
	notified := func(record string, version int64) string {
		t.Helper()
		got, err := n.Notify(ctx, changeOf(record, version), subscription)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome == platform.NotificationDelivered {
			return "delivered"
		}
		return got.Reason
	}
	// An opt-in flag in the source's own data is not consent.
	record := s.viewRecord(tokenA, "consent-ada", `{"tier":"gold","marketing_opt_in":true}`)
	if got := notified(record, 1); got != "purpose_not_permitted" {
		t.Fatalf("an opt-in attribute counted as consent: %s", got)
	}

	// Without an authority a source cannot grant, but it can always withdraw.
	s.expect(s.reportConsent(tokenA, "no-authority-grant-0001", sourceDecision{record: "consent-ada", policy: policy}), 403)
	s.expect(s.reportConsent(tokenA, "no-authority-withdraw-01", sourceDecision{record: "consent-ada", policy: policy, decision: "withdraw"}), 201)

	// Authorities: privacy_admin only, consent purposes only, one active per source and purpose.
	for name, body := range map[string]string{
		"non-consent purpose": fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"]}`, interest),
		"unknown source":      fmt.Sprintf(`{"source":"nowhere","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"]}`, marketing),
		"boolean flag method": fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["crm_checkbox"]}`, marketing),
		"no controller":       fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"","collection_methods":["double_opt_in"]}`, marketing),
	} {
		if w := s.request("POST", consentAuthoritiesPath, body, "", viewOperator, ""); w.Code != 422 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	s.expect(s.request("POST", consentAuthoritiesPath, `{}`, "", tokenA, ""), 401)
	authority := s.approveAuthority("console", marketing, "double_opt_in")
	s.expect(s.request("POST", consentAuthoritiesPath, fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"]}`, marketing), "", viewOperator, ""), 409)

	// A grant must be proven, current and the source's own.
	epoch, err := platform.Store{Pool: s.pool}.SourceEpoch(ctx, "console")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		d      sourceDecision
		token  string
		status int
	}{
		"method not approved": {sourceDecision{method: "signed_form"}, tokenA, 422},
		"another notice":      {sourceDecision{notice: "older-notice"}, tokenA, 422},
		"before the version":  {sourceDecision{collectedAt: time.Now().Add(-48 * time.Hour)}, tokenA, 422},
		"in the future":       {sourceDecision{collectedAt: time.Now().Add(time.Hour)}, tokenA, 422},
		"no proof digest":     {sourceDecision{digest: "not-a-digest"}, tokenA, 422},
		"stale source epoch":  {sourceDecision{epoch: epoch + 1}, tokenA, 409},
		"unknown record":      {sourceDecision{record: "consent-nobody"}, tokenA, 404},
		"another source":      {sourceDecision{}, tokenB, 404},
	} {
		if c.d.record == "" {
			c.d.record = "consent-ada"
		}
		c.d.policy = policy
		if w := s.reportConsent(c.token, "invalid-"+strings.ReplaceAll(name, " ", "-")+"-01", c.d); w.Code != c.status {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	if got := notified(record, 2); got != "purpose_not_permitted" {
		t.Fatalf("a refused grant counted: %s", got)
	}

	// A proven grant counts, is idempotent, and cannot be replayed with different proof.
	grant := sourceDecision{record: "consent-ada", policy: policy, collectedAt: time.Now().UTC()}
	s.expect(s.reportConsent(tokenA, "proven-grant-000000001", grant), 201)
	s.expect(s.reportConsent(tokenA, "proven-grant-000000001", grant), 200)
	other := grant
	other.digest = strings.Repeat("ab", 32)
	s.expect(s.reportConsent(tokenA, "proven-grant-000000001", other), 409)
	if got := notified(record, 3); got != "delivered" {
		t.Fatalf("a verified source grant: %s", got)
	}

	// A later withdrawal from the source stops it.
	s.expect(s.reportConsent(tokenA, "later-withdrawal-00001", sourceDecision{record: "consent-ada", policy: policy, decision: "withdraw"}), 201)
	if got := notified(record, 4); got != "purpose_not_permitted" {
		t.Fatalf("after the source's withdrawal: %s", got)
	}
	s.expect(s.reportConsent(tokenA, "regrant-after-withdraw", grant), 201)
	if got := notified(record, 5); got != "delivered" {
		t.Fatalf("after a later grant: %s", got)
	}

	// Revoking the authority stops every grant made under it at once.
	s.expect(s.request("POST", consentAuthoritiesPath+"/"+authority+"/revoke", "", "", viewOperator, ""), 200)
	s.expect(s.request("POST", consentAuthoritiesPath+"/"+authority+"/revoke", "", "", viewOperator, ""), 409)
	if got := notified(record, 6); got != "purpose_not_permitted" {
		t.Fatalf("after the authority was revoked: %s", got)
	}
	s.approveAuthority("console", marketing, "double_opt_in")
	if got := notified(record, 7); got != "purpose_not_permitted" {
		t.Fatalf("a grant under the revoked authority counted under a new one: %s", got)
	}

	// An operator's grant on the record never counts.
	operatorRecord := s.viewRecord(tokenA, "consent-bob", `{"tier":"gold"}`)
	s.recordEvidence(operatorRecord, policy, "grant")
	if got := notified(operatorRecord, 1); got != "purpose_not_permitted" {
		t.Fatalf("an operator grant counted as consent: %s", got)
	}

	// Once the record is linked to a customer, the customer's own choice decides, not the source's.
	linked := s.viewRecord(tokenA, "consent-cy", `{"tier":"gold"}`)
	s.expect(s.reportConsent(tokenA, "linked-source-grant-01", sourceDecision{record: "consent-cy", policy: policy}), 201)
	if got := notified(linked, 1); got != "delivered" {
		t.Fatalf("before linking: %s", got)
	}
	s.viewLink(linkIssuerA, linkCustomerA, "consent-cy")
	if got := notified(linked, 2); got != "purpose_not_permitted" {
		t.Fatalf("a source grant overrode the linked customer's own choice: %s", got)
	}
	s.decideAsCustomer(linkCustomerA, marketing, policy, "grant")
	if got := notified(linked, 3); got != "delivered" {
		t.Fatalf("after the customer's own grant: %s", got)
	}
}
