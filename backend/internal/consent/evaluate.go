package consent

import (
	"sort"
	"time"
)

// Evidence is the minimal verifiable record of one decision. It carries no raw credentials or contact values.
type Evidence struct {
	Decision    Decision
	ReceivedAt  time.Time
	ActorKind   string // customer, source, operator
	Quarantined bool
	Policy      PolicyVersion
}

// Outcome is the effective answer for one subject, purpose and channel at an evaluation time.
type Outcome string

const (
	Permitted Outcome = "permitted"
	Blocked   Outcome = "blocked"
	// FailingClosed: consent-based processing with missing, ambiguous or stale evidence.
	FailingClosed Outcome = "blocked_fail_closed"
)

// Evaluation is the effective permission plus the reason, so callers never see only a boolean.
type Evaluation struct {
	Outcome Outcome  `json:"outcome"`
	Basis   string   `json:"basis"` // which rule decided it
	Reasons []string `json:"reasons"`
}

// Evaluate computes effective permission for one subject and purpose from committed evidence.
// Rules (ADR 0009 section 5):
//   - Withdrawal, denial and objection apply across every policy version of the purpose,
//     regardless of carry-forward or a change of basis.
//   - A withdrawal takes effect from received_at, so a late or backdated event cannot delay it.
//   - Missing, ambiguous or quarantined evidence fails closed for consent-based purposes;
//     a quarantined withdraw or object counts as effective.
//   - Until #35 provides trustworthy ordering, a source-reported grant never supersedes an
//     earlier denial, withdrawal or objection; only an authenticated customer grant does.
func Evaluate(now time.Time, basis LawfulBasis, evidence []Evidence) Evaluation {
	reasons := []string{}
	// Consider only the newest active version's basis, but apply rights across all versions.
	withdrawn := false
	objected := false
	denied := false
	granted := false
	sourceGrant := false
	for _, e := range evidence {
		if e.ReceivedAt.After(now) {
			continue // not yet effective at evaluation time
		}
		if e.Quarantined && e.Decision != Withdraw && e.Decision != Object {
			continue // a quarantined grant or denial does not count; a quarantined withdraw/object does
		}
		switch e.Decision {
		case Withdraw:
			withdrawn = true
		case Object:
			objected = true
		case Deny:
			denied = true
		case Grant:
			if e.ActorKind == "customer" {
				granted = true
			} else {
				sourceGrant = true
			}
		}
	}
	_ = sourceGrant
	switch {
	case basis == Consent:
		// A withdrawal or denial blocks until a later authenticated customer grant lifts it.
		if withdrawn || denied {
			reasons = append(reasons, "withdrawal or denial on record")
			return Evaluation{Blocked, "withdrawal", reasons}
		}
		if !granted {
			reasons = append(reasons, "no authenticated grant")
			return Evaluation{FailingClosed, "no_grant", reasons}
		}
		return Evaluation{Permitted, "consent", append(reasons, "authenticated grant")}
	case basis == LegitimateInterest || basis == PublicTask:
		if objected {
			reasons = append(reasons, "objection on record")
			return Evaluation{Blocked, "objection", reasons}
		}
		return Evaluation{Permitted, string(basis), append(reasons, "no objection")}
	default:
		// Contract, legal obligation and vital interests do not depend on consent decisions.
		return Evaluation{Permitted, string(basis), append(reasons, "non-consent basis")}
	}
}

// EffectiveAt resolves same-time conflicts deterministically: later received_at wins; a true tie
// is resolved by the lexicographically greater id so the result is reproducible.
func SortForReplay(evidence []Evidence, ids []string) []Evidence {
	type pair struct {
		e  Evidence
		id string
	}
	p := make([]pair, len(evidence))
	for i := range evidence {
		id := ""
		if i < len(ids) {
			id = ids[i]
		}
		p[i] = pair{evidence[i], id}
	}
	sort.SliceStable(p, func(i, j int) bool {
		if p[i].e.ReceivedAt.Equal(p[j].e.ReceivedAt) {
			return p[i].id < p[j].id
		}
		return p[i].e.ReceivedAt.Before(p[j].e.ReceivedAt)
	})
	out := make([]Evidence, len(p))
	for i := range p {
		out[i] = p[i].e
	}
	return out
}
