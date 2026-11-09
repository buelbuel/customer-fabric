package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// seedActivePolicy creates a purpose with one active policy version and returns its id.
func (s *consentSuite) seedActivePolicy(name string) string {
	s.t.Helper()
	p := s.purpose(name)
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	return v.ID
}

func TestEvidenceRequiresActivePolicy(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	draft := s.draft(p.ID) // draft, not active
	profile := s.seedProfile()
	w := s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(draft.ID, "grant", "customer"), operatorToken, "evt-0000001")
	s.expect(w, 409) // policy not active
}

func TestEvidenceRejectsForgedOrInvalid(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("profiling")
	profile := s.seedProfile()
	// Unknown policy version.
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(platform.UUID(), "grant", "customer"), operatorToken, "evt-0000002"), 422)
	// Invalid decision.
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "made_up", "customer"), operatorToken, "evt-0000003"), 422)
	// Missing idempotency key.
	s.expect(s.call("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "grant", "customer"), operatorToken), 422)
}

func TestEvidenceIdempotentUnderConcurrency(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	profile := s.seedProfile()
	body := s.evidenceBody(policy, "grant", "customer")
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, body, operatorToken, "evt-concurrent-0001").Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == 201 {
			created++
		} else if c != 200 {
			t.Fatalf("unexpected status %d during concurrent idempotent write", c)
		}
	}
	if created != 1 {
		t.Fatalf("expected exactly one created row, got %d", created)
	}
	if got := s.count("consent_evidence"); got != 1 {
		t.Fatalf("expected 1 evidence row, got %d", got)
	}
}

func TestEvidenceIsAppendOnly(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	profile := s.seedProfile()
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "grant", "customer"), operatorToken, "evt-append-0001"), 201)
	if _, err := s.pool.Exec(context.Background(), `UPDATE consent_evidence SET decision='withdraw'`); err == nil {
		t.Fatal("evidence row was updated; append-only trigger missing")
	}
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM consent_evidence`); err == nil {
		t.Fatal("evidence row was deleted; append-only trigger missing")
	}
}

// seedProfile creates a source profile directly so customer evidence can anchor on it.
func (s *consentSuite) seedProfile() string {
	s.t.Helper()
	id := platform.UUID()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO profiles(id,version,attributes) VALUES($1,1,'{}')`, id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

func (s *consentSuite) evidenceBody(policyID, decision, actor string) string {
	b, _ := json.Marshal(map[string]string{
		"policy_version_id": policyID, "decision": decision, "occurred_at": time.Now().UTC().Format(time.RFC3339),
		"proof_channel": "operator_console", "proof_reference": "synthetic-ref", "actor_kind": actor,
	})
	return string(b)
}
