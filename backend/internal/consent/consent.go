// Package consent is the pure effective-permission evaluator and policy rules. It has no
// database, HTTP or journal dependency (ADR 0009 sections 4 and 5; M2 incremental scope guardrail 4).
package consent

import (
	"fmt"
	"regexp"
	"sort"
	"time"
)

var Name = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var Jurisdiction = regexp.MustCompile(`^[A-Z]{2}$`)
var Locale = regexp.MustCompile(`^[a-z]{2}(-[A-Z]{2})?$`)

// LawfulBasis is a property of a policy version, never inferred.
type LawfulBasis string

const (
	Consent            LawfulBasis = "consent"
	Contract           LawfulBasis = "contract"
	LegalObligation    LawfulBasis = "legal_obligation"
	VitalInterests     LawfulBasis = "vital_interests"
	PublicTask         LawfulBasis = "public_task"
	LegitimateInterest LawfulBasis = "legitimate_interests"
)

func (b LawfulBasis) Valid() bool {
	switch b {
	case Consent, Contract, LegalObligation, VitalInterests, PublicTask, LegitimateInterest:
		return true
	}
	return false
}

// ConsentBased reports whether this basis is regulated by consent decisions. An objection
// applies to legitimate-interest and public-task versions instead (ADR 0009 section 4).
func (b LawfulBasis) ConsentBased() bool { return b == Consent }

type Purpose struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type PolicyVersion struct {
	ID                 string      `json:"id"`
	PurposeID          string      `json:"purpose_id"`
	Version            int32       `json:"version"`
	State              string      `json:"state"`
	LawfulBasis        LawfulBasis `json:"lawful_basis"`
	NoticeReference    string      `json:"notice_reference"`
	Jurisdiction       string      `json:"jurisdiction"`
	Locale             string      `json:"locale"`
	Channel            string      `json:"channel"`
	RetentionReference string      `json:"retention_reference"`
	CarryForward       bool        `json:"carry_forward"`
	CreatedAt          time.Time   `json:"created_at"`
}

func (p PolicyVersion) Validate() error {
	if !Name.MatchString(p.Channel) {
		return fmt.Errorf("invalid channel")
	}
	if !Jurisdiction.MatchString(p.Jurisdiction) {
		return fmt.Errorf("invalid jurisdiction")
	}
	if !Locale.MatchString(p.Locale) {
		return fmt.Errorf("invalid locale")
	}
	if !p.LawfulBasis.Valid() {
		return fmt.Errorf("invalid lawful basis")
	}
	if p.NoticeReference == "" || p.RetentionReference == "" {
		return fmt.Errorf("notice and retention references are required")
	}
	return nil
}

// Transition states for a policy version. Active and retired are terminal except retire.
func CanTransition(from, to string) bool {
	switch {
	case from == "draft" && to == "active":
		return true
	case from == "active" && to == "retired":
		return true
	}
	return false
}

// Decision is an append-only consent event.
type Decision string

const (
	Grant    Decision = "grant"
	Deny     Decision = "deny"
	Withdraw Decision = "withdraw"
	Object   Decision = "object"
)

func (d Decision) Valid() bool {
	switch d {
	case Grant, Deny, Withdraw, Object:
		return true
	}
	return false
}

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
