// Package matching proposes explainable identity-match candidates between source records. A candidate
// grants nothing: it never links, merges or authorizes, and it is for an operator to review. Email
// equality alone is never sufficient, because people share and recycle addresses.
package matching

import (
	"encoding/json"
	"strings"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// fold compares names and addresses case-insensitively in every script: "WEISS" and "weiß" agree, and
// composed and decomposed accents agree.
func fold(s string) string { return cases.Fold().String(norm.NFC.String(s)) }

// Attribute paths a record's signals are read from. A missing or non-string value is no signal.
const (
	PathEmail      = "email"
	PathPhone      = "phone"
	PathGivenName  = "given_name"
	PathFamilyName = "family_name"
	PathReference  = "customer_reference"
)

// Signals are a record's normalized identity attributes.
type Signals struct {
	Email, Phone, GivenName, FamilyName, Reference string
}

// SignalsFrom normalizes a record's attributes: emails and names are trimmed and case-folded (a value not
// shaped like an address is no email), phones keep their last nine digits so national and international
// forms agree, and references are trimmed.
func SignalsFrom(attributes map[string]json.RawMessage) Signals {
	text := func(path string) string {
		var s string
		if json.Unmarshal(attributes[path], &s) != nil {
			return ""
		}
		return strings.TrimSpace(s)
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, text(PathPhone))
	if len(digits) > 9 {
		digits = digits[len(digits)-9:]
	} else if len(digits) < 7 {
		digits = ""
	}
	email := fold(text(PathEmail))
	if local, domain, ok := strings.Cut(email, "@"); !ok || local == "" || !strings.Contains(domain, ".") || strings.Contains(domain, "@") {
		email = ""
	}
	return Signals{Email: email, Phone: digits, GivenName: fold(text(PathGivenName)),
		FamilyName: fold(text(PathFamilyName)), Reference: text(PathReference)}
}

// Candidate classes. Strong and probable are proposals; conflicting means a shared identifier is
// contradicted by the names, which is exactly the shared-email or reused-reference case to review.
const (
	Strong      = "strong"
	Probable    = "probable"
	Conflicting = "conflicting"
)

// Candidate explains a comparison by signal names only, never by value.
type Candidate struct {
	Class    string   `json:"class"`
	Agree    []string `json:"agree"`
	Disagree []string `json:"disagree"`
}

// Compare decides whether two records are a candidate pair. Rules, in order:
//   - a shared reference, email or phone is conflicting when the given names differ, or when the family
//     names differ and a given name is missing to show a rename;
//   - a shared reference corroborated by a name or phone is strong;
//   - a shared reference with only the email, or a shared email or phone corroborated by the family name,
//     is probable, and so is email with phone and given name (a family name change keeps the rest);
//   - anything else, including email, phone, reference or a name alone, is no candidate.
func Compare(a, b Signals) (Candidate, bool) {
	c := Candidate{Agree: []string{}, Disagree: []string{}}
	check := func(name, x, y string) bool {
		if x == "" || y == "" {
			return false
		}
		if x == y {
			c.Agree = append(c.Agree, name)
			return true
		}
		c.Disagree = append(c.Disagree, name)
		return false
	}
	reference := check(PathReference, a.Reference, b.Reference)
	email := check(PathEmail, a.Email, b.Email)
	phone := check(PathPhone, a.Phone, b.Phone)
	given := check(PathGivenName, a.GivenName, b.GivenName)
	family := check(PathFamilyName, a.FamilyName, b.FamilyName)
	// Different given names mean different people even under one family name: a household shares a
	// landline, an address or a family email, and a reused reference carries someone else's name.
	givenDisagree := a.GivenName != "" && b.GivenName != "" && !given
	familyDisagree := a.FamilyName != "" && b.FamilyName != "" && !family
	switch {
	case (reference || email || phone) && (givenDisagree || familyDisagree && a.GivenName == "" || familyDisagree && b.GivenName == ""):
		c.Class = Conflicting
	case reference && (given || family || phone):
		c.Class = Strong
	case reference && email, (email || phone) && family, email && phone && given:
		c.Class = Probable
	default:
		return c, false
	}
	return c, true
}

// SharesIdentifier reports whether two records agree on a reference, email or phone. Every class
// Compare returns needs such an agreement, so records for which this is false are never candidates.
func (s Signals) SharesIdentifier(o Signals) bool {
	return s.Reference != "" && s.Reference == o.Reference || s.Email != "" && s.Email == o.Email || s.Phone != "" && s.Phone == o.Phone
}
