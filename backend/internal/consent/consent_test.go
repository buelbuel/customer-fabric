package consent

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ev(d Decision, at time.Time, actor string, q bool) Evidence {
	return Evidence{Decision: d, ReceivedAt: at, ActorKind: actor, Quarantined: q}
}

func TestConsentRequiresAuthenticatedGrant(t *testing.T) {
	if got := Evaluate(t0, Consent, nil); got.Outcome != FailingClosed {
		t.Fatalf("no evidence must fail closed, got %s", got.Outcome)
	}
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-time.Hour), "customer", false)}); got.Outcome != Permitted {
		t.Fatalf("customer grant must permit, got %s", got.Outcome)
	}
}

func TestSourceGrantDoesNotCountWithoutCustomer(t *testing.T) {
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-time.Hour), "source", false)}); got.Outcome != FailingClosed {
		t.Fatalf("source-only grant must not permit consent processing, got %s", got.Outcome)
	}
}

func TestWithdrawalBeatsEarlierGrant(t *testing.T) {
	e := []Evidence{
		ev(Grant, t0.Add(-2*time.Hour), "customer", false),
		ev(Withdraw, t0.Add(-time.Hour), "customer", false),
	}
	if got := Evaluate(t0, Consent, e); got.Outcome != Blocked {
		t.Fatalf("withdrawal must block, got %s", got.Outcome)
	}
}

func TestLateWithdrawalTakesEffectFromReceivedAt(t *testing.T) {
	// A withdrawal received after "now" is not yet effective; a backdated occurred_at does not delay it.
	late := Evidence{Decision: Withdraw, ReceivedAt: t0.Add(time.Hour), ActorKind: "customer"}
	onTime := Evidence{Decision: Withdraw, ReceivedAt: t0.Add(-time.Minute), ActorKind: "customer"}
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-2*time.Hour), "customer", false), late}); got.Outcome != Permitted {
		t.Fatalf("a not-yet-received withdrawal must not block before received_at, got %s", got.Outcome)
	}
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-2*time.Hour), "customer", false), onTime}); got.Outcome != Blocked {
		t.Fatalf("a received withdrawal must block immediately, got %s", got.Outcome)
	}
}

func TestQuarantinedRightsCount(t *testing.T) {
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-2*time.Hour), "customer", false), ev(Withdraw, t0.Add(-time.Hour), "source", true)}); got.Outcome != Blocked {
		t.Fatalf("quarantined withdrawal must count as effective, got %s", got.Outcome)
	}
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-time.Hour), "customer", true)}); got.Outcome != FailingClosed {
		t.Fatalf("quarantined grant must not permit, got %s", got.Outcome)
	}
}

func TestObjectionOnlyAffectsObjectableBases(t *testing.T) {
	obj := []Evidence{ev(Object, t0.Add(-time.Hour), "customer", false)}
	if got := Evaluate(t0, LegitimateInterest, obj); got.Outcome != Blocked {
		t.Fatalf("objection must block legitimate interest, got %s", got.Outcome)
	}
	if got := Evaluate(t0, PublicTask, obj); got.Outcome != Blocked {
		t.Fatalf("objection must block public task, got %s", got.Outcome)
	}
	if got := Evaluate(t0, Contract, obj); got.Outcome != Permitted {
		t.Fatalf("objection must not affect a contract basis, got %s", got.Outcome)
	}
	if got := Evaluate(t0, Consent, obj); got.Outcome != FailingClosed {
		t.Fatalf("a consent purpose with only an objection and no grant fails closed, got %s", got.Outcome)
	}
}

func TestDeniedBehavesLikeWithdrawal(t *testing.T) {
	if got := Evaluate(t0, Consent, []Evidence{ev(Grant, t0.Add(-2*time.Hour), "customer", false), ev(Deny, t0.Add(-time.Hour), "customer", false)}); got.Outcome != Blocked {
		t.Fatalf("denial must block, got %s", got.Outcome)
	}
}

func TestPolicyTransitionAndValidation(t *testing.T) {
	if !CanTransition("draft", "active") || !CanTransition("active", "retired") {
		t.Fatal("expected valid transitions")
	}
	if CanTransition("active", "draft") || CanTransition("retired", "active") {
		t.Fatal("active/retired must not silently revert")
	}
	p := PolicyVersion{Channel: "email", Jurisdiction: "DE", Locale: "de-DE", LawfulBasis: Consent, NoticeReference: "n", RetentionReference: "r"}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	bad := p
	bad.LawfulBasis = "made_up"
	if bad.Validate() == nil {
		t.Fatal("invalid lawful basis accepted")
	}
}

func TestSortIsDeterministicOnTies(t *testing.T) {
	e := []Evidence{ev(Grant, t0, "customer", false), ev(Withdraw, t0, "customer", false)}
	ids := []string{"b", "a"}
	out := SortForReplay(e, ids)
	if out[0].Decision != Withdraw {
		t.Fatalf("tie must resolve by id ascending; got first=%s", out[0].Decision)
	}
}
