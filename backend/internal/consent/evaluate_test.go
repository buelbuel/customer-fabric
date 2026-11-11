package consent

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func policy(version int32, basis LawfulBasis, carry bool) PolicyVersion {
	return PolicyVersion{Version: version, LawfulBasis: basis, CarryForward: carry}
}

func customer(d Decision, minute int) Evidence {
	return Evidence{Decision: d, EffectiveAt: at(minute), ActorKind: "customer", Version: 1, RecordedBasis: Consent}
}

func outcome(active PolicyVersion, versions []PolicyVersion, e ...Evidence) Outcome {
	return Evaluate(active, versions, e).Outcome
}

var v1 = []PolicyVersion{policy(1, Consent, false)}

func TestConsentNeedsAnAuthenticatedCustomerGrant(t *testing.T) {
	active := v1[0]
	if got := outcome(active, v1); got != FailingClosed {
		t.Fatalf("no evidence: %s", got)
	}
	if got := outcome(active, v1, customer(Grant, 1)); got != Permitted {
		t.Fatalf("customer grant: %s", got)
	}
	for _, actor := range []string{"operator", "source"} {
		g := customer(Grant, 1)
		g.ActorKind = actor
		if got := outcome(active, v1, g); got != FailingClosed {
			t.Fatalf("%s grant counted as consent: %s", actor, got)
		}
	}
	carried := customer(Grant, 1)
	carried.FromRecord = true
	if got := outcome(active, v1, carried); got != FailingClosed {
		t.Fatalf("grant carried from a record counted: %s", got)
	}
}

// A source's grant is consent only when verified under an active consent authority (ADR 0015), and only on
// the subject it was made about: an operator flag on it, or carrying it to a linked customer, never counts.
// A verified grant is lifted by a later withdrawal like any other and authorizes nothing under objection.
func TestVerifiedSourceGrants(t *testing.T) {
	active := v1[0]
	verified := customer(Grant, 1)
	verified.ActorKind, verified.VerifiedSource = "source", true
	if got := outcome(active, v1, verified); got != Permitted {
		t.Fatalf("verified source grant: %s", got)
	}
	for name, change := range map[string]func(*Evidence){
		"operator marked verified": func(e *Evidence) { e.ActorKind = "operator" },
		"carried to a customer":    func(e *Evidence) { e.FromRecord = true },
		"quarantined":              func(e *Evidence) { e.Quarantined = true },
		"authority revoked":        func(e *Evidence) { e.VerifiedSource = false },
	} {
		g := verified
		change(&g)
		if got := outcome(active, v1, g); got != FailingClosed {
			t.Errorf("%s counted as consent: %s", name, got)
		}
	}
	withdrawn := customer(Withdraw, 2)
	withdrawn.ActorKind = "source"
	if got := outcome(active, v1, verified, withdrawn); got != Blocked {
		t.Fatalf("a later source withdrawal: %s", got)
	}
}

// Precedence follows database receipt order only: a later customer grant lifts an earlier withdrawal,
// an equal or earlier one does not, whatever order the evidence arrives in.
func TestWithdrawalPrecedenceUsesReceiptOrder(t *testing.T) {
	active := v1[0]
	cases := []struct {
		name string
		e    []Evidence
		want Outcome
	}{
		{"grant then withdraw", []Evidence{customer(Grant, 1), customer(Withdraw, 2)}, Blocked},
		{"withdraw then later grant", []Evidence{customer(Withdraw, 1), customer(Grant, 2)}, Permitted},
		{"tie", []Evidence{customer(Grant, 1), customer(Withdraw, 1)}, Blocked},
		{"deny then grant then withdraw", []Evidence{customer(Deny, 1), customer(Grant, 2), customer(Withdraw, 3)}, Blocked},
		{"unsorted input", []Evidence{customer(Withdraw, 3), customer(Grant, 2)}, Blocked},
	}
	for _, c := range cases {
		if got := outcome(active, v1, c.e...); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestQuarantinedDecisions(t *testing.T) {
	active := v1[0]
	for _, d := range []Decision{Withdraw, Deny, Object} {
		q := customer(d, 2)
		q.Quarantined = true
		if got := outcome(active, v1, customer(Grant, 1), q); got != Blocked {
			t.Errorf("quarantined %s ignored: %s", d, got)
		}
	}
	q := customer(Grant, 1)
	q.Quarantined = true
	if got := outcome(active, v1, q); got != FailingClosed {
		t.Errorf("quarantined grant counted: %s", got)
	}
}

// A grant on a retired version authorises the active version only if every later version carries forward.
func TestRetiredVersionGrantNeedsCarryForward(t *testing.T) {
	grant := customer(Grant, 1)
	if got := outcome(policy(2, Consent, false), []PolicyVersion{policy(1, Consent, false), policy(2, Consent, false)}, grant); got != FailingClosed {
		t.Errorf("v1 grant authorised v2 without carry-forward: %s", got)
	}
	if got := outcome(policy(2, Consent, true), []PolicyVersion{policy(1, Consent, false), policy(2, Consent, true)}, grant); got != Permitted {
		t.Errorf("v1 grant with carry-forward into v2: %s", got)
	}
	broken := []PolicyVersion{policy(1, Consent, false), policy(2, Consent, false), policy(3, Consent, true)}
	if got := outcome(policy(3, Consent, true), broken, grant); got != FailingClosed {
		t.Errorf("carry-forward chain broken at v2 still authorised v3: %s", got)
	}
	later := customer(Grant, 1)
	later.Version = 3
	if got := outcome(policy(2, Consent, true), []PolicyVersion{policy(1, Consent, false), policy(2, Consent, true), policy(3, Consent, false)}, later); got != FailingClosed {
		t.Errorf("grant on a later version authorised an earlier active one: %s", got)
	}
}

// A protective decision survives a change of lawful basis; one recorded under a non-consent basis is ignored.
func TestDecisionsSurviveBasisChange(t *testing.T) {
	withdrawn := customer(Withdraw, 1)
	for _, b := range []LawfulBasis{LegitimateInterest, PublicTask, Contract, LegalObligation} {
		if got := outcome(policy(2, b, false), []PolicyVersion{policy(1, Consent, false), policy(2, b, false)}, withdrawn); got != Blocked {
			t.Errorf("withdrawal under consent bypassed by moving to %s: %s", b, got)
		}
	}
	objection := customer(Object, 1)
	objection.RecordedBasis = LegitimateInterest
	if got := outcome(policy(2, Consent, false), []PolicyVersion{policy(1, LegitimateInterest, false), policy(2, Consent, false)}, objection, customer(Grant, 2)); got != Blocked {
		t.Errorf("objection lifted by a grant after a basis change: %s", got)
	}
	meaningless := customer(Withdraw, 1)
	meaningless.RecordedBasis = Contract
	if got := outcome(policy(1, Contract, false), []PolicyVersion{policy(1, Contract, false)}, meaningless); got != Permitted {
		t.Errorf("a withdrawal recorded under a contract basis stopped contract processing: %s", got)
	}
	if got := outcome(policy(1, LegitimateInterest, false), []PolicyVersion{policy(1, LegitimateInterest, false)}, objection); got != Blocked {
		t.Errorf("objection on legitimate interest: %s", got)
	}
}
