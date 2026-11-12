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

// proofDocument is a synthetic proof of one double opt-in grant, as a source would produce it.
func proofDocument(record, policy, key, controller string, collectedAt time.Time) []byte {
	return []byte(fmt.Sprintf(`{"schema":"customer-fabric.consent-proof.v1","record":{"namespace":"crm","value":%q},"policy_version_id":%q,"notice_reference":"n","method":"double_opt_in","collected_at":%q,"controller":%q,"reference":"crm-receipt-%s","evidence":{"confirmation_sent_at":"2026-11-11T09:00:00Z","confirmed_at":"2026-11-11T09:05:00Z","message_reference":"synthetic-message-1"}}`,
		record, policy, collectedAt.Format(time.RFC3339Nano), controller, key))
}

func digestOf(document []byte) string {
	sum := sha256.Sum256(document)
	return hex.EncodeToString(sum[:])
}

func (s linkingSuite) submitProof(token, id string, document []byte) *httptest.ResponseRecorder {
	s.t.Helper()
	req := httptest.NewRequest("POST", "/v1/source-consent-verifications/"+id+"/proof", strings.NewReader(string(document)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	s.h.ServeHTTP(res, req)
	return res
}

// A source's grant is its attestation until its proof is checked (ADR 0017). An operator can ask for
// the proof of any grant, by audit sample or one at a time; the source answers with the structured proof
// document, which Customer Fabric checks against the recorded digest and the grant and then discards.
// A failed or unanswered verification voids the grant and suspends the authority. Under the verified
// trust policy a grant counts only once verified.
func TestSourceConsentEvidenceIsVerifiedIndependently(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	marketing, policy := s.purposeWithBasis("verified_marketing", "consent")
	subscription := s.subscribe("verified_marketing", subscriptionBody("crm_changes", marketing, "console"))
	n := platform.NewNotifier(s.store, s.config.Egress())
	version := map[string]int64{}
	notified := func(record string) string {
		t.Helper()
		version[record]++
		got, err := n.Notify(ctx, changeOf(record, version[record]), subscription)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome == platform.NotificationDelivered {
			return "delivered"
		}
		return got.Reason
	}
	authority := s.approveAuthority("console", marketing, "double_opt_in")
	authorityPath := consentAuthoritiesPath + "/" + authority
	collectedAt := time.Now().UTC()
	// granted reports a signed grant whose digest is that of its proof document, and returns the
	// evidence ID and the document.
	granted := func(record, key, controller string) (string, string, []byte) {
		t.Helper()
		profile := s.viewRecord(tokenA, record, `{"tier":"gold"}`)
		document := proofDocument(record, policy, key, controller, collectedAt)
		res := s.reportConsent(tokenA, key, sourceDecision{record: record, policy: policy, digest: digestOf(document), collectedAt: collectedAt})
		s.expect(res, 201)
		var receipt platform.EvidenceReceipt
		_ = json.Unmarshal(res.Body.Bytes(), &receipt)
		return profile, receipt.EvidenceID, document
	}
	verify := func(evidence string) platform.Verification {
		t.Helper()
		res := s.request("POST", "/v1/operator/consent-evidence/"+evidence+"/verifications", `{"reason":"dispute"}`, "", viewOperator, "")
		s.expect(res, 201)
		var v platform.Verification
		_ = json.Unmarshal(res.Body.Bytes(), &v)
		return v
	}
	review := func(evidence, id, decision string) *httptest.ResponseRecorder {
		t.Helper()
		return s.request("POST", "/v1/operator/consent-evidence/"+evidence+"/verifications/"+id+"/review",
			fmt.Sprintf(`{"decision":%q,"reference":"REVIEW-2026-7"}`, decision), "", viewOperator, "")
	}
	decided := func(res *httptest.ResponseRecorder) string {
		t.Helper()
		s.expect(res, 200)
		var v platform.Verification
		_ = json.Unmarshal(res.Body.Bytes(), &v)
		return v.State + ":" + v.Result
	}

	// Under attestation a signed grant counts before any verification.
	ada, adaEvidence, adaProof := granted("verify-ada", "verify-ada-grant-0001", "Synthetic Retail GmbH")
	if got := notified(ada); got != "delivered" {
		t.Fatalf("an attested grant: %s", got)
	}
	// Requesting: privacy_admin, a source grant, one open at a time.
	evidencePath := "/v1/operator/consent-evidence/" + adaEvidence + "/verifications"
	s.expect(s.request("POST", evidencePath, `{"reason":"dispute"}`, "", tokenA, ""), 401)
	s.expect(s.request("POST", evidencePath, `{"reason":"curiosity"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", "/v1/operator/consent-evidence/"+platform.UUID()+"/verifications", `{"reason":"dispute"}`, "", viewOperator, ""), 404)
	adaCheck := verify(adaEvidence)
	s.expect(s.request("POST", evidencePath, `{"reason":"dispute"}`, "", viewOperator, ""), 409)
	if adaCheck.State != "requested" || !adaCheck.DueAt.After(time.Now().Add(6*24*time.Hour)) {
		t.Fatalf("verification: %+v", adaCheck)
	}
	// The source sees what it must answer, by its own reference and record; another source does not.
	open := s.request("GET", "/v1/source-consent-verifications", "", "", tokenA, "")
	s.expect(open, 200)
	if body := open.Body.String(); !strings.Contains(body, adaCheck.ID) || !strings.Contains(body, `"reference":"crm-receipt-verify-ada-grant-0001"`) ||
		!strings.Contains(body, `"value":"verify-ada"`) {
		t.Fatalf("open verifications: %s", body)
	}
	if body := s.request("GET", "/v1/source-consent-verifications", "", "", tokenB, "").Body.String(); strings.Contains(body, adaCheck.ID) {
		t.Fatal("another source saw this source's verification")
	}
	s.expect(s.submitProof(tokenB, adaCheck.ID, adaProof), 404)
	// The exact proof document is consistent, which only checks it; it cannot be answered twice. A privacy
	// admin's review of the proof verifies it.
	if got := decided(s.submitProof(tokenA, adaCheck.ID, adaProof)); got != "checked:proof_consistent" {
		t.Fatalf("a matching proof: %s", got)
	}
	s.expect(s.submitProof(tokenA, adaCheck.ID, adaProof), 404)
	s.expect(review(adaEvidence, adaCheck.ID, "maybe"), 422)
	s.expect(s.request("POST", "/v1/operator/consent-evidence/"+adaEvidence+"/verifications/"+adaCheck.ID+"/review", `{"decision":"verified","reference":""}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", "/v1/operator/consent-evidence/"+adaEvidence+"/verifications/"+adaCheck.ID+"/review", `{"decision":"verified","reference":"R-1"}`, "", tokenA, ""), 401)
	if got := decided(review(adaEvidence, adaCheck.ID, "verified")); got != "verified:proof_consistent" {
		t.Fatalf("a reviewed proof: %s", got)
	}
	s.expect(review(adaEvidence, adaCheck.ID, "verified"), 409)
	if got := notified(ada); got != "delivered" {
		t.Fatalf("a verified grant: %s", got)
	}
	var stored int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM consent_evidence_verifications WHERE id = $1 AND document_digest = $2`,
		adaCheck.ID, func() []byte { sum := sha256.Sum256(adaProof); return sum[:] }()).Scan(&stored); err != nil || stored != 1 {
		t.Fatalf("only the digest of the document is kept: %d %v", stored, err)
	}

	// A document whose digest matches but which names another controller does not prove the grant: the
	// grant is void and the authority suspended, so every grant under it stops counting.
	bob, bobEvidence, bobProof := granted("verify-bob", "verify-bob-grant-0001", "Another Controller AG")
	bobCheck := verify(bobEvidence)
	if got := decided(s.submitProof(tokenA, bobCheck.ID, bobProof)); got != "failed:controller_mismatch" {
		t.Fatalf("a proof for another controller: %s", got)
	}
	if got := notified(ada); got != "purpose_not_permitted" {
		t.Fatalf("after a failed verification the authority still authorizes: %s", got)
	}
	s.expect(review(bobEvidence, bobCheck.ID, "verified"), 409)
	trail := s.request("GET", authorityPath+"/audit", "", "", viewOperator, "").Body.String()
	if !strings.Contains(trail, `"action":"suspended","reference":"verification:`+bobCheck.ID+`","actor":"automatic"`) {
		t.Fatalf("automatic suspension not audited: %s", trail)
	}
	// Renewed under the verified policy: the verified grant counts again, the failed one never. The audit
	// records the policy each review set.
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vettingWith("verified")+"}", "", viewOperator, ""), 200)
	if trail := s.request("GET", authorityPath+"/audit", "", "", viewOperator, "").Body.String(); !strings.Contains(trail, `"trust_policy":"verified"`) ||
		!strings.Contains(trail, `"trust_policy":"attestation"`) {
		t.Fatalf("trust policies not audited: %s", trail)
	}
	if got := notified(ada); got != "delivered" {
		t.Fatalf("a verified grant under the verified policy: %s", got)
	}
	if got := notified(bob); got != "purpose_not_permitted" {
		t.Fatalf("a grant whose verification failed: %s", got)
	}

	// Under the verified policy a new grant is asked for its proof at once and counts only once verified.
	cy := s.viewRecord(tokenA, "verify-cy", `{"tier":"gold"}`)
	cyProof := proofDocument("verify-cy", policy, "verify-cy-grant-00001", "Synthetic Retail GmbH", collectedAt)
	res := s.reportConsent(tokenA, "verify-cy-grant-00001", sourceDecision{record: "verify-cy", policy: policy, digest: digestOf(cyProof), collectedAt: collectedAt})
	s.expect(res, 201)
	var receipt platform.EvidenceReceipt
	_ = json.Unmarshal(res.Body.Bytes(), &receipt)
	if receipt.VerificationID == "" || receipt.Trust != "source_attested" {
		t.Fatalf("receipt under the verified policy: %+v", receipt)
	}
	if got := notified(cy); got != "purpose_not_permitted" {
		t.Fatalf("an unverified grant under the verified policy: %s", got)
	}
	tampered := []byte(strings.Replace(string(cyProof), "synthetic-message-1", "synthetic-message-2", 1))
	if got := decided(s.submitProof(tokenA, receipt.VerificationID, tampered)); got != "failed:digest_mismatch" {
		t.Fatalf("an altered document: %s", got)
	}
	if got := notified(cy); got != "purpose_not_permitted" {
		t.Fatalf("a grant with an altered proof: %s", got)
	}
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vettingWith("verified")+"}", "", viewOperator, ""), 200)
	dan := s.viewRecord(tokenA, "verify-dan", `{"tier":"gold"}`)
	danProof := proofDocument("verify-dan", policy, "verify-dan-grant-0001", "Synthetic Retail GmbH", collectedAt)
	res = s.reportConsent(tokenA, "verify-dan-grant-0001", sourceDecision{record: "verify-dan", policy: policy, digest: digestOf(danProof), collectedAt: collectedAt})
	s.expect(res, 201)
	_ = json.Unmarshal(res.Body.Bytes(), &receipt)
	// A retry of the grant names the same verification.
	retried := s.reportConsent(tokenA, "verify-dan-grant-0001", sourceDecision{record: "verify-dan", policy: policy, digest: digestOf(danProof), collectedAt: collectedAt})
	s.expect(retried, 200)
	if !strings.Contains(retried.Body.String(), receipt.VerificationID) {
		t.Fatalf("a retried grant: %s", retried.Body.String())
	}
	if got := decided(s.submitProof(tokenA, receipt.VerificationID, danProof)); got != "checked:proof_consistent" {
		t.Fatalf("dan's proof: %s", got)
	}
	if got := notified(dan); got != "purpose_not_permitted" {
		t.Fatalf("a checked but unreviewed grant under the verified policy: %s", got)
	}
	s.expect(review(receipt.EvidenceID, receipt.VerificationID, "verified"), 200)
	if got := notified(dan); got != "delivered" {
		t.Fatalf("a reviewed grant: %s", got)
	}

	// An unanswered verification, once overdue, stops the whole authority: ignoring a request is never
	// better than answering it. A privacy admin closes it as failed, which suspends the authority.
	danCheck := verify(receipt.EvidenceID)
	if _, err := s.pool.Exec(ctx, `UPDATE consent_evidence_verifications SET due_at = now() - interval '1 minute' WHERE id = $1`, danCheck.ID); err != nil {
		t.Fatal(err)
	}
	if got := notified(dan); got != "purpose_not_permitted" {
		t.Fatalf("an overdue verification: %s", got)
	}
	if got := notified(ada); got != "purpose_not_permitted" {
		t.Fatalf("another grant of an authority with an overdue verification: %s", got)
	}
	if got := grant(t, s.linkingSuite, "verify-ada", policy, "overdue-authority-grant"); got != 403 {
		t.Fatalf("an authority with an overdue verification took a grant: %d", got)
	}
	listed := s.request("GET", "/v1/operator/consent-evidence/"+receipt.EvidenceID+"/verifications", "", "", viewOperator, "")
	if !strings.Contains(listed.Body.String(), `"state":"overdue"`) {
		t.Fatalf("verifications: %s", listed.Body.String())
	}
	if got := decided(review(receipt.EvidenceID, danCheck.ID, "failed")); got != "failed:unanswered" {
		t.Fatalf("closing an overdue verification: %s", got)
	}
	if !strings.Contains(s.request("GET", authorityPath, "", "", viewOperator, "").Body.String()+s.request("GET", authorityPath+"/audit", "", "", viewOperator, "").Body.String(),
		`"reference":"review:`+danCheck.ID+`"`) {
		t.Fatal("closing an overdue verification did not suspend the authority")
	}

	// Audits sample grants never asked before.
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vettingWith("attestation")+"}", "", viewOperator, ""), 200)
	s.expect(s.request("POST", authorityPath+"/audits", `{"sample":0}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", authorityPath+"/audits", `{"sample":5}`, "", tokenA, ""), 401)
	granted("verify-eve", "verify-eve-grant-0001", "Synthetic Retail GmbH")
	sampled := s.request("POST", authorityPath+"/audits", `{"sample":5}`, "", viewOperator, "")
	s.expect(sampled, 201)
	var audit struct{ Items []platform.Verification }
	_ = json.Unmarshal(sampled.Body.Bytes(), &audit)
	if len(audit.Items) != 1 || audit.Items[0].Reason != "audit" {
		t.Fatalf("audit sample: %s", sampled.Body.String())
	}
	if !strings.Contains(s.request("GET", authorityPath+"/audit", "", "", viewOperator, "").Body.String(), `"action":"audit_sampled","reference":"sample:1"`) {
		t.Fatal("audit sample not audited")
	}

	// A source over its rate limit is refused before anything is decided.
	if _, err := s.pool.Exec(ctx, `INSERT INTO source_consent_rate (source, window_start, requests) VALUES ('console', date_trunc('minute', now()), 120)
		ON CONFLICT (source, window_start) DO UPDATE SET requests = 120`); err != nil {
		t.Fatal(err)
	}
	limited := s.reportConsent(tokenA, "rate-limited-grant-0001", sourceDecision{record: "verify-ada", policy: policy})
	if limited.Code != 429 || limited.Header().Get("Retry-After") == "" {
		t.Fatalf("over the rate limit: %d", limited.Code)
	}
	// A withdrawal never waits for the limit.
	s.expect(s.reportConsent(tokenA, "rate-limited-withdraw-1", sourceDecision{record: "verify-ada", policy: policy, decision: "withdraw"}), 201)
	s.expect(s.request("GET", "/v1/source-consent-verifications", "", "", tokenB, ""), 200)
}

// grant reports a signed grant on a record and returns the status.
func grant(t *testing.T, s linkingSuite, record, policy, key string) int {
	t.Helper()
	return s.reportConsent(tokenA, key+"-0001", sourceDecision{record: record, policy: policy}).Code
}
