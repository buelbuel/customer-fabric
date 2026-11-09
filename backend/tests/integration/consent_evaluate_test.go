package integration

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

type evaluationView struct {
	Outcome string   `json:"outcome"`
	Basis   string   `json:"basis"`
	Reasons []string `json:"reasons"`
}

func (s *consentSuite) outcome(profile, purpose string) string {
	s.t.Helper()
	w := s.call("GET", "/v1/operator/consent/effective/"+profile+"/"+purpose, "", operatorToken)
	s.expect(w, 200)
	var v evaluationView
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		s.t.Fatal(err)
	}
	return v.Outcome
}

// activePolicy drafts and activates a version of purpose with the given basis and carry-forward.
func (s *consentSuite) activePolicy(purpose, basis string, carry bool) string {
	s.t.Helper()
	body, _ := json.Marshal(map[string]any{"lawful_basis": basis, "notice_reference": "n", "jurisdiction": "DE",
		"locale": "de-DE", "channel": "email", "retention_reference": "r", "carry_forward": carry})
	w := s.call("POST", "/v1/operator/consent/policies/"+purpose, string(body), operatorToken)
	s.expect(w, 201)
	var v policyView
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	s.expect(s.transition(v.ID, "active"), 200)
	return v.ID
}

// customerGrant stores an authenticated customer grant the way the customer consent route (#25) will.
// No HTTP route can record one yet; operators cannot.
func (s *consentSuite) customerGrant(profile, policy string) {
	s.t.Helper()
	ctx := context.Background()
	var subject string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM consent_subjects WHERE kind='customer' AND anchor_profile_id=$1`, profile).Scan(&subject); err != nil {
		subject = platform.UUID()
		if _, err = s.pool.Exec(ctx, `INSERT INTO consent_subjects(id,kind,anchor_profile_id) VALUES($1,'customer',$2)`, subject, profile); err != nil {
			s.t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO consent_evidence(id,subject_id,policy_version_id,decision,actor_kind,occurred_at,proof_channel,proof_reference,idempotency_key)
		VALUES($1,$2,$3,'grant','customer',now(),'customer_session','synthetic',$4)`, platform.UUID(), subject, policy, "grant-"+platform.UUID()[:12]); err != nil {
		s.t.Fatal(err)
	}
}

// consentCustomer returns a customer anchor with an authenticated grant on a fresh consent purpose.
func (s *consentSuite) consentCustomer(name string) (profile, kratos, purpose, policy string) {
	s.t.Helper()
	p := s.purpose(name)
	policy = s.activePolicy(p.ID, "consent", false)
	profile, kratos = s.customerProfile()
	s.customerGrant(profile, policy)
	if got := s.outcome(profile, p.ID); got != "permitted" {
		s.t.Fatalf("baseline customer grant: %s", got)
	}
	return profile, kratos, p.ID, policy
}

// Operators can record decisions but never one that permits processing as customer consent.
func TestOperatorGrantNeverPermits(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	policy := s.activePolicy(p.ID, "consent", false)
	profile, _ := s.customerProfile()
	s.customerEvidence(profile, policy, "grant", "operator-grant-01")
	if got := s.outcome(profile, p.ID); got != "blocked_fail_closed" {
		t.Fatalf("operator grant evaluated as %s", got)
	}
}

// Invariant: a withdrawal made before linking is effective after linking.
func TestRecordWithdrawalBeforeLinkCarries(t *testing.T) {
	s := consentSetup(t)
	profile, kratos, purpose, policy := s.consentCustomer("newsletter")
	rp, epoch := s.record("", "before-link")
	s.recordEvidence(epoch, policy, "withdraw", "before-link-01")
	if got := s.outcome(profile, purpose); got != "permitted" {
		t.Fatalf("an unlinked record decided for the customer: %s", got)
	}
	s.link(kratos, rp, epoch)
	if got := s.outcome(profile, purpose); got != "blocked" {
		t.Fatalf("withdrawal made before the link: %s", got)
	}
}

// Invariants: a withdrawal made while linked survives unlinking; a later one does not reach an ex-customer.
func TestRecordWithdrawalWhileLinkedSurvivesUnlink(t *testing.T) {
	s := consentSetup(t)
	profile, kratos, purpose, policy := s.consentCustomer("newsletter")
	rp, epoch := s.record("", "while-linked")
	assoc := s.link(kratos, rp, epoch)
	s.recordEvidence(epoch, policy, "withdraw", "while-linked-01")
	s.unlink(assoc, epoch)
	if got := s.outcome(profile, purpose); got != "blocked" {
		t.Fatalf("withdrawal lost on unlink: %s", got)
	}

	other, otherKratos, otherPurpose, otherPolicy := s.consentCustomer("profiling")
	rp2, epoch2 := s.record("", "after-unlink")
	assoc2 := s.link(otherKratos, rp2, epoch2)
	s.unlink(assoc2, epoch2)
	s.recordEvidence(epoch2, otherPolicy, "withdraw", "after-unlink-01")
	if got := s.outcome(other, otherPurpose); got != "permitted" {
		t.Fatalf("withdrawal received after the unlink reached the former customer: %s", got)
	}
}

// Invariant: relinking cannot revive an older grant.
func TestRelinkDoesNotReviveOlderGrant(t *testing.T) {
	s := consentSetup(t)
	profile, kratos, purpose, policy := s.consentCustomer("newsletter")
	rp, epoch := s.record("", "relink")
	assoc := s.link(kratos, rp, epoch)
	s.recordEvidence(epoch, policy, "withdraw", "relink-wd-0001")
	s.unlink(assoc, epoch)
	s.customerGrant(profile, policy) // re-consents while unlinked
	if got := s.outcome(profile, purpose); got != "permitted" {
		t.Fatalf("a later customer grant should lift the withdrawal: %s", got)
	}
	s.link(kratos, rp, epoch) // the record's withdrawal is carried again from the new link time
	if got := s.outcome(profile, purpose); got != "blocked" {
		t.Fatalf("relink revived the grant made before it: %s", got)
	}
	s.customerGrant(profile, policy)
	if got := s.outcome(profile, purpose); got != "permitted" {
		t.Fatalf("a customer grant after the relink: %s", got)
	}
}

// Invariant: concurrent association changes and withdrawals cannot leave a permissive result.
func TestConcurrentLinkUnlinkAndWithdrawalNeverPermit(t *testing.T) {
	s := consentSetup(t)
	for i := 0; i < 20; i++ {
		profile, kratos, purpose, policy := s.consentCustomer("race_" + string(rune('a'+i)))
		rp, epoch := s.record("", "race-"+platform.UUID())
		assoc := s.link(kratos, rp, epoch)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.unlink(assoc, epoch) }()
		go func() { defer wg.Done(); s.recordEvidence(epoch, policy, "withdraw", "race-wd-"+platform.UUID()[:8]) }()
		wg.Wait()
		// Either order is legitimate, but the outcome must match it: a withdrawal received before the
		// unlink blocks, one received after does not.
		var before bool
		if err := s.pool.QueryRow(context.Background(), `SELECT e.received_at < a.revoked_at FROM verified_associations a
			JOIN consent_subjects rs ON rs.kind='source_record' AND rs.anchor_record_epoch=a.record_epoch
			JOIN consent_evidence e ON e.subject_id=rs.id WHERE a.id=$1`, assoc).Scan(&before); err != nil {
			t.Fatal(err)
		}
		want := map[bool]string{true: "blocked", false: "permitted"}[before]
		if got := s.outcome(profile, purpose); got != want {
			t.Fatalf("iteration %d: withdrawal before unlink=%v gave %s", i, before, got)
		}
	}
}

// Invariant: shared source profiles do not spread a decision to an unrelated person.
func TestSharedSourceProfileIsolatesRecords(t *testing.T) {
	s := consentSetup(t)
	shared := s.seedProfile()
	alice, aliceKratos, purpose, policy := s.consentCustomer("newsletter")
	bob, bobKratos := s.customerProfile()
	s.customerGrant(bob, policy)
	_, aliceEpoch := s.record(shared, "alice-record")
	_, bobEpoch := s.record(shared, "bob-record")
	s.link(aliceKratos, shared, aliceEpoch)
	s.link(bobKratos, shared, bobEpoch)
	s.recordEvidence(aliceEpoch, policy, "withdraw", "shared-wd-0001")
	if got := s.outcome(alice, purpose); got != "blocked" {
		t.Fatalf("alice's own record withdrawal: %s", got)
	}
	if got := s.outcome(bob, purpose); got != "permitted" {
		t.Fatalf("alice's withdrawal reached bob through the shared profile: %s", got)
	}
}

// Invariant: a recreated identifier has a new record epoch and inherits nothing from the old one.
func TestRecreatedIdentifierInheritsNothing(t *testing.T) {
	s := consentSetup(t)
	alice, aliceKratos, purpose, policy := s.consentCustomer("newsletter")
	rp, oldEpoch := s.record("", "recreated")
	s.link(aliceKratos, rp, oldEpoch)
	s.recordEvidence(oldEpoch, policy, "withdraw", "old-epoch-wd-01")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM external_identifiers WHERE record_epoch=$1`, oldEpoch); err != nil {
		t.Fatal(err)
	}
	_, newEpoch := s.record(rp, "recreated")
	if newEpoch == oldEpoch {
		t.Fatal("recreated identifier kept its epoch")
	}
	carol, carolKratos := s.customerProfile()
	s.customerGrant(carol, policy)
	s.link(carolKratos, rp, newEpoch)
	if got := s.outcome(carol, purpose); got != "permitted" {
		t.Fatalf("new epoch inherited the old epoch's withdrawal: %s", got)
	}
	if got := s.outcome(alice, purpose); got != "blocked" {
		t.Fatalf("the old epoch's withdrawal stopped applying to its own customer: %s", got)
	}
}

// Invariant: evaluation fails closed when evidence is too large to evaluate safely.
func TestEvaluationFailsClosedOnEvidenceVolume(t *testing.T) {
	s := consentSetup(t)
	profile, _, purpose, policy := s.consentCustomer("newsletter")
	s.customerEvidence(profile, policy, "withdraw", "volume-wd-0001")
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO consent_evidence(id,subject_id,policy_version_id,decision,actor_kind,occurred_at,proof_channel,proof_reference,idempotency_key)
		SELECT gen_random_uuid(), s.id, $2, 'grant','customer', now(), 'customer_session','x','bulk-key-'||g FROM consent_subjects s, generate_series(1,500) g WHERE s.anchor_profile_id=$1`, profile, policy); err != nil {
		t.Fatal(err)
	}
	if got := s.outcome(profile, purpose); got != "blocked_fail_closed" {
		t.Fatalf("over the evidence limit: %s", got)
	}
}

// Retired versions never authorise new processing without carry-forward.
func TestRetiredGrantNeedsCarryForward(t *testing.T) {
	s := consentSetup(t)
	profile, _, purpose, v1 := s.consentCustomer("newsletter")
	s.expect(s.transition(v1, "retired"), 200)
	s.activePolicy(purpose, "consent", false)
	if got := s.outcome(profile, purpose); got != "blocked_fail_closed" {
		t.Fatalf("v1 grant authorised v2 without carry-forward: %s", got)
	}

	other, _, otherPurpose, w1 := s.consentCustomer("profiling")
	s.expect(s.transition(w1, "retired"), 200)
	s.activePolicy(otherPurpose, "consent", true)
	if got := s.outcome(other, otherPurpose); got != "permitted" {
		t.Fatalf("carry-forward grant: %s", got)
	}
}

// A withdrawal survives a change of lawful basis.
func TestBasisChangeKeepsWithdrawal(t *testing.T) {
	s := consentSetup(t)
	profile, _, purpose, v1 := s.consentCustomer("profiling")
	s.customerEvidence(profile, v1, "withdraw", "basis-chg-0001")
	s.expect(s.transition(v1, "retired"), 200)
	s.activePolicy(purpose, "legitimate_interests", false)
	if got := s.outcome(profile, purpose); got != "blocked" {
		t.Fatalf("withdrawal bypassed by a basis change: %s", got)
	}
}

func TestEvaluationTargetsAndAuth(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("service_messages")
	s.activePolicy(p.ID, "contract", false)
	profile, _ := s.customerProfile()
	if got := s.outcome(profile, p.ID); got != "permitted" {
		t.Fatalf("contract basis: %s", got)
	}
	empty := s.purpose("newsletter")
	if got := s.outcome(profile, empty.ID); got != "blocked_fail_closed" {
		t.Fatalf("no active policy: %s", got)
	}
	for name, path := range map[string]string{
		"source profile":  "/v1/operator/consent/effective/" + s.seedProfile() + "/" + p.ID,
		"unknown profile": "/v1/operator/consent/effective/" + platform.UUID() + "/" + p.ID,
		"unknown purpose": "/v1/operator/consent/effective/" + profile + "/" + platform.UUID(),
	} {
		if w := s.call("GET", path, "", operatorToken); w.Code != 404 {
			t.Errorf("%s: got %d, want 404", name, w.Code)
		}
	}
	s.expect(s.call("GET", "/v1/operator/consent/effective/"+profile+"/"+p.ID, "", ""), 401)
}
