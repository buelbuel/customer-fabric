package consent

import "time"

// Evidence is one committed decision as evaluation sees it. EffectiveAt is the database receipt time,
// or for a decision carried from a linked source record, the later of its receipt and the link time.
// No application or source clock is involved, so precedence does not depend on source timestamps.
type Evidence struct {
	Decision      Decision
	EffectiveAt   time.Time
	ActorKind     string
	Quarantined   bool
	Version       int32       // policy version the decision was recorded under
	RecordedBasis LawfulBasis // lawful basis of that version
	FromRecord    bool        // carried from a linked source record; never a customer grant
	// VerifiedSource marks a source's grant made under a consent authority that is active now (ADR 0015):
	// the source was approved to collect consent for the purpose, and the grant names the notice, the
	// collection method, the source's receipt and a digest of the proof.
	VerifiedSource bool
}

type Outcome string

const (
	Permitted     Outcome = "permitted"
	Blocked       Outcome = "blocked"
	FailingClosed Outcome = "blocked_fail_closed"
)

// Evaluation is the effective permission plus the rule that decided it.
type Evaluation struct {
	Outcome Outcome  `json:"outcome"`
	Basis   string   `json:"basis"`
	Reasons []string `json:"reasons"`
}

func result(o Outcome, basis, reason string) Evaluation {
	return Evaluation{Outcome: o, Basis: basis, Reasons: []string{reason}}
}

// protective reports a withdrawal, denial or objection recorded where such decisions have meaning.
// Decisions recorded under a contract, legal-obligation or vital-interests version are ignored.
func (e Evidence) protective() bool {
	if e.Decision != Withdraw && e.Decision != Deny && e.Decision != Object {
		return false
	}
	switch e.RecordedBasis {
	case Consent, LegitimateInterest, PublicTask:
		return true
	}
	return false
}

// carries reports whether a grant on version v still authorises the active version: it is the active
// version, or every later version up to the active one carries earlier grants forward.
func carries(v int32, active PolicyVersion, versions []PolicyVersion) bool {
	if v == active.Version {
		return true
	}
	if v > active.Version {
		return false
	}
	byVersion := map[int32]PolicyVersion{}
	for _, p := range versions {
		byVersion[p.Version] = p
	}
	for w := v + 1; w <= active.Version; w++ {
		if p, ok := byVersion[w]; !ok || !p.CarryForward {
			return false
		}
	}
	return true
}

// Evaluate decides effective permission for one customer and purpose (ADR 0009 section 5).
//   - Withdrawals, denials and objections apply across every version of the purpose and survive a
//     change of lawful basis; quarantined ones still count.
//   - Only an authenticated customer grant, or a verified source grant on the subject itself, counts as
//     consent. Operator grants, unverified source grants and grants carried from a linked record never
//     do, and a grant authorises the active version only through carry-forward.
//   - A later customer grant lifts an earlier withdrawal or denial; a tie does not. An objection is lifted
//     only by the customer withdrawing it, which M2 does not model, so it stays.
//   - After a change from a consent or objectable basis to any other basis, an earlier protective
//     decision keeps processing stopped until a controller decision exists.
func Evaluate(active PolicyVersion, versions []PolicyVersion, evidence []Evidence) Evaluation {
	var blockedAt, grantedAt time.Time
	var blocked, granted, objected bool
	for _, e := range evidence {
		switch {
		case e.protective() && e.Decision == Object:
			objected = true
		case e.protective():
			if !blocked || e.EffectiveAt.After(blockedAt) {
				blockedAt, blocked = e.EffectiveAt, true
			}
		case e.Decision == Grant && (e.ActorKind == "customer" || e.ActorKind == "source" && e.VerifiedSource) && !e.Quarantined &&
			!e.FromRecord && carries(e.Version, active, versions):
			if !granted || e.EffectiveAt.After(grantedAt) {
				grantedAt, granted = e.EffectiveAt, true
			}
		}
	}
	switch active.LawfulBasis {
	case Consent:
		switch {
		case objected:
			return result(Blocked, "objection", "objection on record")
		case blocked && (!granted || !grantedAt.After(blockedAt)):
			return result(Blocked, "withdrawal", "withdrawal or denial not lifted by a later customer grant")
		case !granted:
			return result(FailingClosed, "no_grant", "no authenticated customer grant or verified source grant valid for the active policy version")
		}
		return result(Permitted, "consent", "authenticated customer grant or verified source grant")
	case LegitimateInterest, PublicTask:
		if objected || blocked {
			return result(Blocked, "objection", "objection, withdrawal or denial on record")
		}
		return result(Permitted, string(active.LawfulBasis), "no objection")
	default:
		if objected || blocked {
			return result(Blocked, "basis_changed", "an earlier withdrawal or objection needs a controller decision")
		}
		return result(Permitted, string(active.LawfulBasis), "non-consent basis")
	}
}
