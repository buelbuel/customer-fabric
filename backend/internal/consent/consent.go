// Package consent holds the pure consent rules. It has no database, HTTP or journal dependency
// (ADR 0009 sections 4 and 5; M2 incremental scope guardrail 4).
package consent

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

var Name = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var Channel = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
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
	Explanation        string      `json:"explanation"`
	Jurisdiction       string      `json:"jurisdiction"`
	Locale             string      `json:"locale"`
	Channel            string      `json:"channel"`
	RetentionReference string      `json:"retention_reference"`
	CarryForward       bool        `json:"carry_forward"`
	CreatedAt          time.Time   `json:"created_at"`
}

// Validate mirrors the database constraints so invalid input is a 422, never a constraint failure.
func (p PolicyVersion) Validate() error {
	if !Channel.MatchString(p.Channel) {
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
	if !reference(p.NoticeReference) || !reference(p.RetentionReference) {
		return fmt.Errorf("notice and retention references must be 1 to 256 bytes")
	}
	if len(p.Explanation) > 2048 || strings.TrimSpace(p.Explanation) == "" {
		return fmt.Errorf("explanation must be 1 to 2048 bytes of text")
	}
	return nil
}

func reference(s string) bool { return len(s) >= 1 && len(s) <= 256 }

// CanTransition allows only draft to active and active to retired. Nothing returns to draft or active.
func CanTransition(from, to string) bool {
	return (from == "draft" && to == "active") || (from == "active" && to == "retired")
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
