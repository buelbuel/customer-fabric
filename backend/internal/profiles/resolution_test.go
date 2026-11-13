package profiles

import (
	"encoding/json"
	"testing"
	"time"
)

func contribution(record, source, value string, contributed, updated int64) Contribution {
	c := Contribution{Record: record, Source: source, ContributedAt: time.Unix(contributed, 0), UpdatedAt: time.Unix(updated, 0)}
	if value != "" {
		c.Value = json.RawMessage(value)
	}
	return c
}

func TestResolve(t *testing.T) {
	alice, alicia, ali := contribution("r1", "salesforce", `"Alice"`, 1, 5), contribution("r2", "shopify", `"Alicia"`, 2, 9), contribution("r3", "loyalty", `"Ali"`, 3, 3)
	withdrawn := contribution("r1", "salesforce", "", 1, 10)
	duplicate := contribution("r0", "salesforce", `"Al"`, 1, 1)
	priority := &ResolutionPolicy{Strategy: "source_priority", SourcePriority: []string{"shopify", "salesforce"}}
	recent := &ResolutionPolicy{Strategy: "most_recent"}
	for name, tc := range map[string]struct {
		policy *ResolutionPolicy
		in     []Contribution
		want   string
	}{
		"nothing live":                        {nil, []Contribution{withdrawn}, ""},
		"only value":                          {nil, []Contribution{alice}, "r1"},
		"without a policy the earliest stays": {nil, []Contribution{alicia, alice}, "r1"},
		"a withdrawal falls to the next":      {nil, []Contribution{withdrawn, ali, alicia}, "r2"},
		"equal times: lower record":           {nil, []Contribution{alice, duplicate}, "r0"},
		"priority wins over the earliest":     {priority, []Contribution{alice, alicia}, "r2"},
		"unlisted ranks last":                 {priority, []Contribution{ali, alice}, "r1"},
		"among unlisted the earliest":         {&ResolutionPolicy{Strategy: "source_priority", SourcePriority: []string{"erp"}}, []Contribution{ali, alicia}, "r2"},
		"most recent":                         {recent, []Contribution{alice, alicia, ali}, "r2"},
		"most recent ignores withdrawals":     {recent, []Contribution{withdrawn, ali}, "r3"},
	} {
		got, ok := Resolve(tc.policy, tc.in)
		if got.Record != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: got %q want %q", name, got.Record, tc.want)
		}
	}
	// The order contributions are listed in never changes the choice.
	for _, in := range [][]Contribution{{alice, alicia, ali}, {ali, alicia, alice}, {alicia, ali, alice}} {
		if got, _ := Resolve(nil, in); got.Record != "r1" {
			t.Fatalf("order dependent: %q", got.Record)
		}
	}
}
