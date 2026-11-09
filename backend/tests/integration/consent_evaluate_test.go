package integration

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

type evaluationView struct {
	Outcome string   `json:"outcome"`
	Basis   string   `json:"basis"`
	Reasons []string `json:"reasons"`
}

func (s *consentSuite) evaluate(profile, purpose string) evaluationView {
	s.t.Helper()
	w := s.call("GET", "/v1/operator/consent/effective/"+profile+"/"+purpose, "", operatorToken)
	s.expect(w, 200)
	var v evaluationView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		s.t.Fatal(err)
	}
	return v
}

func TestEvaluateFailsClosedWithoutActivePolicy(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter") // no active version
	profile := s.seedProfile()
	if got := s.evaluate(profile, p.ID); got.Outcome != "blocked_fail_closed" {
		t.Fatalf("expected fail-closed with no active policy, got %+v", got)
	}
}

func TestEvaluateWithdrawalBlocksImmediately(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	profile := s.seedProfile()
	// Grant then evaluate: permitted.
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(v.ID, "grant", "customer"), operatorToken, "evt-eval-grant-01"), 201)
	if got := s.evaluate(profile, p.ID); got.Outcome != "permitted" {
		t.Fatalf("grant should permit, got %+v", got)
	}
	// Withdraw then evaluate on a fresh evaluation: blocked, no second step required.
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(v.ID, "withdraw", "customer"), operatorToken, "evt-eval-withdraw-01"), 201)
	if got := s.evaluate(profile, p.ID); got.Outcome != "blocked" {
		t.Fatalf("withdrawal must block immediately, got %+v", got)
	}
}

func TestEvaluateSourceGrantDoesNotPermitConsentProcessing(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("profiling")
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	profile := s.seedProfile()
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(v.ID, "grant", "source"), operatorToken, "evt-eval-src-01"), 201)
	if got := s.evaluate(profile, p.ID); got.Outcome != "blocked_fail_closed" {
		t.Fatalf("a source grant must not permit consent processing, got %+v", got)
	}
}

func TestEvaluateObjectionOnLegitimateInterest(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("profiling")
	body := `{"lawful_basis":"legitimate_interests","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"web","retention_reference":"r"}`
	w := s.call("POST", "/v1/operator/consent/policies/"+p.ID, body, operatorToken)
	s.expect(w, 201)
	var v policyView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		s.t.Fatal(err)
	}
	s.expect(s.transition(v.ID, "active"), 200)
	profile := s.seedProfile()
	if got := s.evaluate(profile, p.ID); got.Outcome != "permitted" {
		t.Fatalf("legitimate interest with no objection should permit, got %+v", got)
	}
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(v.ID, "object", "customer"), operatorToken, "evt-eval-obj-01"), 201)
	if got := s.evaluate(profile, p.ID); got.Outcome != "blocked" {
		t.Fatalf("objection must block legitimate interest, got %+v", got)
	}
}

func TestEvaluateRequiresAuth(t *testing.T) {
	s := consentSetup(t)
	w := s.call("GET", "/v1/operator/consent/effective/"+s.seedProfile()+"/"+s.purpose("x").ID, "", "")
	s.expect(w, 401)
	_ = httptest.NewRecorder
}
