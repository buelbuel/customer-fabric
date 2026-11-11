package consent

import (
	"fmt"
	"regexp"
	"time"
)

// Subject kinds and anchor rules (ADR 0009 section 3).
const (
	SubjectCustomer     = "customer"
	SubjectSourceRecord = "source_record"
)

var proofChannel = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
var idempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// Subject is a consent subject with exactly one anchor.
type Subject struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	LifecycleState string `json:"lifecycle_state"`
}

// Append is the input to record one consent decision. It carries no raw credentials or contact values.
type Append struct {
	Decision       Decision
	ActorKind      string // set by the authenticated route: customer, source or operator
	ProofChannel   string
	ProofReference string
	OccurredAt     time.Time
	IdempotencyKey string
	Quarantined    bool
	Source         string // set only for source-reported decisions
	// For source-reported decisions (ADR 0015): the consent authority they were made under, the notice
	// the subject was shown, a SHA-256 digest of the stored proof and the source's epoch at receipt.
	AuthorityID      string
	NoticeReference  string
	ProofDigest      []byte
	SourceGeneration int64
}

func (a Append) Validate() error {
	if !a.Decision.Valid() {
		return fmt.Errorf("invalid decision")
	}
	switch a.ActorKind {
	case "customer", "operator":
		if a.Source != "" {
			return fmt.Errorf("only a source actor names a source")
		}
	case "source":
		if a.Source == "" {
			return fmt.Errorf("a source actor must name its source")
		}
	default:
		return fmt.Errorf("invalid actor kind")
	}
	if !proofChannel.MatchString(a.ProofChannel) {
		return fmt.Errorf("invalid proof channel")
	}
	if a.ProofReference == "" || len(a.ProofReference) > 256 {
		return fmt.Errorf("invalid proof reference")
	}
	if !idempotencyKey.MatchString(a.IdempotencyKey) {
		return fmt.Errorf("invalid idempotency key")
	}
	if a.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	return nil
}
