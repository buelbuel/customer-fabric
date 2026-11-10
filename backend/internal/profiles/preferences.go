package profiles

import (
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // timezones validate the same in every image, including ones without zoneinfo
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/language"
)

// Preferences are the fields a customer owns about themselves. They are separate from Kratos identity
// traits (verified contact details) and from source-owned business attributes; a nil field is unset.
type Preferences struct {
	DisplayName       *string `json:"display_name"`
	PreferredLanguage *string `json:"preferred_language"`
	Timezone          *string `json:"timezone"`
}

// Normalize validates and canonicalizes the fields: a display name is trimmed and free of control
// characters, a language is a well-formed BCP 47 tag in canonical form, and a timezone is an IANA zone.
// An empty string clears a field.
func (p Preferences) Normalize() (Preferences, error) {
	clean := func(v *string) *string {
		if v == nil || strings.TrimSpace(*v) == "" {
			return nil
		}
		s := strings.TrimSpace(*v)
		return &s
	}
	out := Preferences{DisplayName: clean(p.DisplayName), PreferredLanguage: clean(p.PreferredLanguage), Timezone: clean(p.Timezone)}
	if n := out.DisplayName; n != nil {
		if utf8.RuneCountInString(*n) > 100 || strings.IndexFunc(*n, unicode.IsControl) >= 0 {
			return out, fmt.Errorf("display_name is at most 100 characters without control characters")
		}
	}
	if l := out.PreferredLanguage; l != nil {
		tag, err := language.Parse(*l)
		if err != nil || len(*l) > 35 {
			return out, fmt.Errorf("preferred_language must be a BCP 47 language tag")
		}
		canonical := tag.String()
		out.PreferredLanguage = &canonical
	}
	if z := out.Timezone; z != nil {
		if _, err := time.LoadLocation(*z); err != nil || *z == "Local" || len(*z) > 64 {
			return out, fmt.Errorf("timezone must be an IANA time zone such as Europe/Berlin")
		}
	}
	return out, nil
}

// Changed names the fields that differ between two normalized versions.
func (p Preferences) Changed(prev Preferences) []string {
	differs := func(a, b *string) bool { return (a == nil) != (b == nil) || (a != nil && *a != *b) }
	var fields []string
	if differs(p.DisplayName, prev.DisplayName) {
		fields = append(fields, "display_name")
	}
	if differs(p.PreferredLanguage, prev.PreferredLanguage) {
		fields = append(fields, "preferred_language")
	}
	if differs(p.Timezone, prev.Timezone) {
		fields = append(fields, "timezone")
	}
	return fields
}
