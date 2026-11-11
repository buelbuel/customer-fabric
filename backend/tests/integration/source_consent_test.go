package integration

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
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

// receiptKey is the synthetic Ed25519 key the console source signs its receipts with.
var receiptKey = ed25519.NewKeyFromSeed([]byte("synthetic-console-receipt-key-01"))

const receiptKeyID = "console-2026-1"

// sourceDecision is a source's consent report about one of its records; zero fields take valid defaults.
// signer and keyID default to receiptKey; unsigned sends no receipt.
type sourceDecision struct {
	record, policy, decision, method, notice, digest, keyID string
	collectedAt                                             time.Time
	epoch                                                   int64
	signer                                                  ed25519.PrivateKey
	unsigned                                                bool
	// tamper changes the decision after it was signed.
	tamper func(*platform.SourceConsentDecision)
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
	if d.signer == nil {
		d.signer = receiptKey
	}
	if d.keyID == "" {
		d.keyID = receiptKeyID
	}
	decision := platform.SourceConsentDecision{PolicyVersionID: d.policy, Decision: d.decision, CollectedAt: d.collectedAt.Format(time.RFC3339Nano), SourceEpoch: d.epoch}
	decision.Record.Namespace, decision.Record.Value = "crm", d.record
	decision.Proof.Method, decision.Proof.Reference, decision.Proof.NoticeReference, decision.Proof.SHA256 = d.method, "crm-receipt-"+key, d.notice, d.digest
	if !d.unsigned {
		decision.Receipt.KeyID = d.keyID
		decision.Receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(d.signer, platform.ConsentReceiptMessage("console", key, decision)))
	}
	if d.tamper != nil {
		d.tamper(&decision)
	}
	body, err := json.Marshal(decision)
	if err != nil {
		s.t.Fatal(err)
	}
	return s.request("POST", "/v1/source-consent-decisions", string(body), key, token, "")
}

// vetting is a valid review of a source for approving or renewing its authority.
func vetting() string {
	return fmt.Sprintf(`"arrangement_reference":"DPA-2026-CRM-7","vetting_reference":"VET-2026-031","review_by":%q`, time.Now().AddDate(0, 6, 0).Format(time.DateOnly))
}

func (s linkingSuite) approveAuthority(source, purpose string, methods ...string) string {
	s.t.Helper()
	return s.approveAuthorityWithKey(source, purpose, receiptKeyID, receiptKey, methods...)
}

// approveAuthorityWithKey approves an authority and registers the given receipt key on it; a public key
// is registered once, ever, so a later authority needs a key of its own.
func (s linkingSuite) approveAuthorityWithKey(source, purpose, keyID string, key ed25519.PrivateKey, methods ...string) string {
	s.t.Helper()
	quoted := make([]string, len(methods))
	for i, m := range methods {
		quoted[i] = fmt.Sprintf("%q", m)
	}
	res := s.request("POST", consentAuthoritiesPath, fmt.Sprintf(`{"source":%q,"purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":[%s],%s}`,
		source, purpose, strings.Join(quoted, ","), vetting()), "", viewOperator, "")
	s.expect(res, 201)
	var a struct{ ID string }
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		s.t.Fatal(err)
	}
	s.addReceiptKey(a.ID, keyID, key)
	return a.ID
}

func (s linkingSuite) addReceiptKey(authority, keyID string, key ed25519.PrivateKey) {
	s.t.Helper()
	public := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	s.expect(s.request("POST", consentAuthoritiesPath+"/"+authority+"/keys", fmt.Sprintf(`{"key_id":%q,"public_key":%q}`, keyID, public), "", viewOperator, ""), 201)
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
		"non-consent purpose": fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"],%s}`, interest, vetting()),
		"unknown source":      fmt.Sprintf(`{"source":"nowhere","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"],%s}`, marketing, vetting()),
		"boolean flag method": fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["crm_checkbox"],%s}`, marketing, vetting()),
		"no controller":       fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"","collection_methods":["double_opt_in"],%s}`, marketing, vetting()),
	} {
		if w := s.request("POST", consentAuthoritiesPath, body, "", viewOperator, ""); w.Code != 422 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	s.expect(s.request("POST", consentAuthoritiesPath, `{}`, "", tokenA, ""), 401)
	authority := s.approveAuthority("console", marketing, "double_opt_in")
	s.expect(s.request("POST", consentAuthoritiesPath, fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"],%s}`, marketing, vetting()), "", viewOperator, ""), 409)

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

	// A later withdrawal stops it. It needs no proof of consent and no current epoch: it names the
	// channel it came through, and a withdrawal queued before a re-key still counts.
	s.expect(s.request("POST", "/v1/source-consent-decisions", fmt.Sprintf(`{"record":{"namespace":"crm","value":"consent-ada"},"policy_version_id":%q,"decision":"withdraw","collected_at":%q,"source_epoch":%d,"proof":{"method":"unsubscribe_link","reference":"crm-unsubscribe-1"}}`,
		policy, time.Now().UTC().Format(time.RFC3339Nano), epoch+7), "later-withdrawal-00001", tokenA, ""), 201)
	if got := notified(record, 4); got != "purpose_not_permitted" {
		t.Fatalf("after the source's withdrawal: %s", got)
	}
	// A grant collected before that withdrawal, sent late or replayed under a new key, never lifts it.
	s.expect(s.reportConsent(tokenA, "late-grant-before-withdraw", grant), 409)
	s.expect(s.reportConsent(tokenA, "regrant-after-withdraw", sourceDecision{record: "consent-ada", policy: policy}), 201)
	if got := notified(record, 5); got != "delivered" {
		t.Fatalf("after a later grant: %s", got)
	}

	// Revoking the authority stops every grant made under it at once.
	if revoked := s.request("POST", consentAuthoritiesPath+"/"+authority+"/revoke", `{"reference":"REV-2026-1"}`, "", viewOperator, ""); revoked.Code != 200 || !strings.Contains(revoked.Body.String(), `"revoked_by":`) {
		t.Fatalf("revoking: %d %s", revoked.Code, revoked.Body.String())
	}
	s.expect(s.request("POST", consentAuthoritiesPath+"/"+authority+"/revoke", `{"reference":"REV-2026-1"}`, "", viewOperator, ""), 409)
	if got := notified(record, 6); got != "purpose_not_permitted" {
		t.Fatalf("after the authority was revoked: %s", got)
	}
	// The new authority has its own key; later grants are signed with it.
	secondKey, secondKeyID := ed25519.NewKeyFromSeed([]byte("synthetic-console-receipt-key-0b")), "console-2026-b"
	s.approveAuthorityWithKey("console", marketing, secondKeyID, secondKey, "double_opt_in")
	if got := notified(record, 7); got != "purpose_not_permitted" {
		t.Fatalf("a grant under the revoked authority counted under a new one: %s", got)
	}

	// A record with several identifiers has several lifetimes, and every one must permit on its own
	// evidence: one lifetime's grant neither speaks for another nor lifts another's withdrawal.
	several := s.viewRecord(tokenA, "consent-dan", `{"tier":"gold"}`, "consent-dan-shop")
	s.expect(s.reportConsent(tokenA, "several-first-grant-01", sourceDecision{signer: secondKey, keyID: secondKeyID, record: "consent-dan", policy: policy}), 201)
	if got := notified(several, 1); got != "purpose_not_permitted" {
		t.Fatalf("one lifetime's grant authorized another without one: %s", got)
	}
	s.expect(s.reportConsent(tokenA, "several-second-grant-1", sourceDecision{signer: secondKey, keyID: secondKeyID, record: "consent-dan-shop", policy: policy}), 201)
	if got := notified(several, 2); got != "delivered" {
		t.Fatalf("every lifetime granted: %s", got)
	}
	s.expect(s.reportConsent(tokenA, "several-second-withdraw", sourceDecision{signer: secondKey, keyID: secondKeyID, record: "consent-dan-shop", policy: policy, decision: "withdraw"}), 201)
	s.expect(s.reportConsent(tokenA, "several-first-regrant-1", sourceDecision{signer: secondKey, keyID: secondKeyID, record: "consent-dan", policy: policy}), 201)
	if got := notified(several, 3); got != "purpose_not_permitted" {
		t.Fatalf("a later grant on one lifetime lifted another's withdrawal: %s", got)
	}

	// An operator's grant on the record never counts.
	operatorRecord := s.viewRecord(tokenA, "consent-bob", `{"tier":"gold"}`)
	s.recordEvidence(operatorRecord, policy, "grant")
	if got := notified(operatorRecord, 1); got != "purpose_not_permitted" {
		t.Fatalf("an operator grant counted as consent: %s", got)
	}

	// Once the record is linked to a customer, the customer's own choice decides, not the source's.
	linked := s.viewRecord(tokenA, "consent-cy", `{"tier":"gold"}`)
	s.expect(s.reportConsent(tokenA, "linked-source-grant-01", sourceDecision{signer: secondKey, keyID: secondKeyID, record: "consent-cy", policy: policy}), 201)
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
