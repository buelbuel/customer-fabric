package segments

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

var definitions = map[string]profiles.AttributeDefinition{
	"tier":      {Path: "tier", Revision: 1, Type: profiles.TypeEnum, Values: []string{"gold", "silver"}},
	"ltv":       {Path: "ltv", Revision: 1, Type: profiles.TypeDecimal},
	"orders":    {Path: "orders", Revision: 1, Type: profiles.TypeInteger},
	"vip":       {Path: "vip", Revision: 1, Type: profiles.TypeBoolean},
	"birthday":  {Path: "birthday", Revision: 1, Type: profiles.TypeDate},
	"last_seen": {Path: "last_seen", Revision: 1, Type: profiles.TypeTimestamp},
	"city":      {Path: "city", Revision: 1, Type: profiles.TypeString},
}

func rule(t *testing.T, conditions string) Rule {
	t.Helper()
	var r Rule
	if err := json.Unmarshal([]byte(`{"purpose_id":"p","match":"all","conditions":`+conditions+`}`), &r); err != nil {
		t.Fatal(err)
	}
	if err := r.Validate(definitions); err != nil {
		t.Fatalf("%s: %v", conditions, err)
	}
	return r
}

// view builds a segmentation view; each value is "source=json".
func view(attributes map[string][]string) profiles.CustomerView {
	v := profiles.CustomerView{}
	for path, values := range attributes {
		a := profiles.ViewAttribute{Path: path, Type: definitions[path].Type}
		for _, sv := range values {
			source, raw, _ := strings.Cut(sv, "=")
			a.Values = append(a.Values, profiles.ViewValue{Value: json.RawMessage(raw), Source: source, AssociationID: source + "-assoc", MutationID: source + "-m"})
		}
		v.Attributes = append(v.Attributes, a)
	}
	return v
}

var at = time.Date(2026, 11, 10, 15, 0, 0, 0, time.UTC)

func TestValidateRefusesUnsafeOrIllTypedRules(t *testing.T) {
	for _, conditions := range []string{
		`[]`,
		`[{"attribute":"undefined","operator":"present"}]`,
		`[{"attribute":"tier","operator":"eq","value":"bronze"}]`,
		`[{"attribute":"tier","operator":"gt","value":"gold"}]`,
		`[{"attribute":"orders","operator":"eq","value":"1"}]`,
		`[{"attribute":"orders","operator":"eq","value":null}]`,
		`[{"attribute":"orders","operator":"eq"}]`,
		`[{"attribute":"orders","operator":"in","values":[]}]`,
		`[{"attribute":"orders","operator":"present","value":1}]`,
		`[{"attribute":"birthday","operator":"within_last_days","days":0}]`,
		`[{"attribute":"city","operator":"within_last_days","days":3}]`,
		`[{"attribute":"city","operator":"eq","value":"x","sources":["a","a"]}]`,
		`[{"attribute":"city","operator":"sql","value":"1=1"}]`,
	} {
		var r Rule
		if err := json.Unmarshal([]byte(`{"purpose_id":"p","match":"all","conditions":`+conditions+`}`), &r); err != nil {
			t.Fatal(err)
		}
		if r.Validate(definitions) == nil {
			t.Errorf("accepted %s", conditions)
		}
	}
	if (Rule{Match: "some", Conditions: []Condition{{Attribute: "vip", Operator: OpPresent}}}).Validate(definitions) == nil {
		t.Error("accepted an unknown match")
	}
	err := (Rule{Match: MatchAll, Conditions: []Condition{{Attribute: "city", Operator: OpEq, Value: json.RawMessage(`7`)}}}).Validate(definitions)
	if err == nil || strings.Contains(err.Error(), "7") {
		t.Errorf("a refusal must not echo the literal: %v", err)
	}
}

func TestMissingValuesMatchOnlyMissing(t *testing.T) {
	empty := view(nil)
	for conditions, want := range map[string]bool{
		`[{"attribute":"tier","operator":"missing"}]`:                  true,
		`[{"attribute":"tier","operator":"present"}]`:                  false,
		`[{"attribute":"tier","operator":"neq","value":"gold"}]`:       false,
		`[{"attribute":"tier","operator":"not_in","values":["gold"]}]`: false,
		`[{"attribute":"ltv","operator":"lt","value":100}]`:            false,
	} {
		if got, _ := rule(t, conditions).Evaluate(empty, at); got != want {
			t.Errorf("%s on a missing value: %v", conditions, got)
		}
	}
}

func TestConflictingSourcesAreExplicit(t *testing.T) {
	v := view(map[string][]string{"tier": {"console=\"gold\"", "importer=\"silver\""}})
	matched, outcomes := rule(t, `[{"attribute":"tier","operator":"eq","value":"gold"}]`).Evaluate(v, at)
	if !matched || len(outcomes[0].Evidence) != 1 || outcomes[0].Evidence[0].Source != "console" {
		t.Fatalf("some source holds gold: %v %+v", matched, outcomes)
	}
	if matched, _ := rule(t, `[{"attribute":"tier","operator":"neq","value":"gold"}]`).Evaluate(v, at); matched {
		t.Fatal("neq ignored the conflicting gold value")
	}
	if matched, _ := rule(t, `[{"attribute":"tier","operator":"eq","value":"gold","sources":["importer"]}]`).Evaluate(v, at); matched {
		t.Fatal("a source filter counted another source's value")
	}
	if matched, _ := rule(t, `[{"attribute":"tier","operator":"neq","value":"gold","sources":["importer"]}]`).Evaluate(v, at); !matched {
		t.Fatal("importer alone holds silver")
	}
	raw, _ := json.Marshal(outcomes)
	if strings.Contains(string(raw), "gold") {
		t.Fatalf("evidence disclosed a value: %s", raw)
	}
}

func TestTypedComparisons(t *testing.T) {
	v := view(map[string][]string{
		"ltv": {"console=100.0"}, "orders": {"console=3"}, "vip": {"console=true"}, "city": {"console=\"Köln\""},
		"birthday": {"console=\"2026-11-03\""}, "last_seen": {"console=\"2026-11-10T16:30:00+02:00\""},
	})
	for conditions, want := range map[string]bool{
		`[{"attribute":"ltv","operator":"eq","value":100}]`:                           true,
		`[{"attribute":"ltv","operator":"gte","value":1e2}]`:                          true,
		`[{"attribute":"ltv","operator":"gt","value":99.99}]`:                         true,
		`[{"attribute":"orders","operator":"in","values":[1,3]}]`:                     true,
		`[{"attribute":"orders","operator":"lt","value":3}]`:                          false,
		`[{"attribute":"vip","operator":"eq","value":false}]`:                         false,
		`[{"attribute":"city","operator":"eq","value":"köln"}]`:                       false,
		`[{"attribute":"birthday","operator":"within_last_days","days":7}]`:           true,
		`[{"attribute":"birthday","operator":"within_last_days","days":6}]`:           false,
		`[{"attribute":"birthday","operator":"lt","value":"2026-11-04"}]`:             true,
		`[{"attribute":"last_seen","operator":"lte","value":"2026-11-10T14:30:00Z"}]`: true,
		`[{"attribute":"last_seen","operator":"within_last_days","days":1}]`:          true,
	} {
		if got, _ := rule(t, conditions).Evaluate(v, at); got != want {
			t.Errorf("%s: got %v", conditions, got)
		}
	}
	future := view(map[string][]string{"last_seen": {"console=\"2026-11-10T15:00:01Z\""}})
	if got, _ := rule(t, `[{"attribute":"last_seen","operator":"within_last_days","days":1}]`).Evaluate(future, at); got {
		t.Error("a future timestamp is not within the last day")
	}
}

func TestMatchAnyAndAll(t *testing.T) {
	v := view(map[string][]string{"orders": {"console=3"}})
	conditions := `[{"attribute":"orders","operator":"gt","value":5},{"attribute":"orders","operator":"present"}]`
	r := rule(t, conditions)
	if matched, outcomes := r.Evaluate(v, at); matched || len(outcomes) != 2 || outcomes[0].Matched || !outcomes[1].Matched {
		t.Fatalf("all: %v %+v", matched, outcomes)
	}
	r.Match = MatchAny
	if matched, _ := r.Evaluate(v, at); !matched {
		t.Fatal("any")
	}
}
