package consent

import (
	"strings"
	"testing"
)

func TestPolicyTransitionAndValidation(t *testing.T) {
	if !CanTransition("draft", "active") || !CanTransition("active", "retired") {
		t.Fatal("expected valid transitions")
	}
	for _, bad := range [][2]string{{"active", "draft"}, {"retired", "active"}, {"draft", "retired"}, {"active", "active"}, {"draft", "made_up"}} {
		if CanTransition(bad[0], bad[1]) {
			t.Fatalf("%s to %s must be refused", bad[0], bad[1])
		}
	}
	p := PolicyVersion{Channel: "email", Jurisdiction: "DE", Locale: "de-DE", LawfulBasis: Consent, NoticeReference: "n", Explanation: "e", RetentionReference: "r"}
	if err := p.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	for name, mutate := range map[string]func(*PolicyVersion){
		"lawful basis":       func(v *PolicyVersion) { v.LawfulBasis = "made_up" },
		"channel too long":   func(v *PolicyVersion) { v.Channel = strings.Repeat("a", 33) },
		"notice too long":    func(v *PolicyVersion) { v.NoticeReference = strings.Repeat("n", 257) },
		"retention empty":    func(v *PolicyVersion) { v.RetentionReference = "" },
		"jurisdiction lower": func(v *PolicyVersion) { v.Jurisdiction = "de" },
	} {
		bad := p
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}
