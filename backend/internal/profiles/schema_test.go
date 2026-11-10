package profiles

import (
	"encoding/json"
	"strings"
	"testing"
)

func intPtr(n int32) *int32 { return &n }

func TestDefinitionValidation(t *testing.T) {
	valid := []AttributeDefinition{
		{Path: "tier", Type: TypeEnum, Values: []string{"gold", "silver"}},
		{Path: "nickname", Type: TypeString, MaxLength: intPtr(64)},
		{Path: "nickname", Type: TypeString},
		{Path: "orders", Type: TypeInteger},
		{Path: "lifetime_value", Type: TypeDecimal},
		{Path: "active", Type: TypeBoolean},
		{Path: "signup_date", Type: TypeDate},
		{Path: "last_seen", Type: TypeTimestamp},
	}
	for _, d := range valid {
		if err := d.Validate(); err != nil {
			t.Errorf("%+v: %v", d, err)
		}
	}
	invalid := []AttributeDefinition{
		{Path: "Bad", Type: TypeString},
		{Path: "tier", Type: "json"},
		{Path: "tier", Type: TypeEnum},
		{Path: "tier", Type: TypeEnum, Values: []string{"gold", "gold"}},
		{Path: "tier", Type: TypeEnum, Values: []string{""}},
		{Path: "tier", Type: TypeString, Values: []string{"gold"}},
		{Path: "tier", Type: TypeString, Values: []string{}},
		{Path: "tier", Type: TypeEnum, Values: []string{}},
		{Path: "tier", Type: TypeInteger, MaxLength: intPtr(4)},
		{Path: "tier", Type: TypeString, MaxLength: intPtr(0)},
		{Path: "tier", Type: TypeString, MaxLength: intPtr(4097)},
	}
	for _, d := range invalid {
		if d.Validate() == nil {
			t.Errorf("accepted %+v", d)
		}
	}
}

func TestDefinitionChecksValues(t *testing.T) {
	cases := []struct {
		def    AttributeDefinition
		accept []string
		refuse []string
	}{
		{AttributeDefinition{Type: TypeString, MaxLength: intPtr(3)}, []string{`"abc"`, `"äöü"`, `""`}, []string{`"abcd"`, `1`, `true`}},
		{AttributeDefinition{Type: TypeInteger}, []string{`0`, `-12`, `9007199254740991`}, []string{`1.0`, `1e3`, `"1"`, `9007199254740992`, `true`}},
		{AttributeDefinition{Type: TypeDecimal}, []string{`0`, `-12.50`, `1e3`}, []string{`"1"`, `false`, `1e1000000`}},
		{AttributeDefinition{Type: TypeBoolean}, []string{`true`, `false`}, []string{`"true"`, `1`}},
		{AttributeDefinition{Type: TypeDate}, []string{`"2026-02-28"`, `"0001-01-01"`}, []string{`"2026-02-30"`, `"2026-2-1"`, `"2026-02-28T00:00:00Z"`, `20260228`, `"0000-12-31"`}},
		{AttributeDefinition{Type: TypeTimestamp}, []string{`"2026-02-28T10:00:00Z"`, `"2026-02-28T10:00:00.5+01:00"`}, []string{`"2026-02-28"`, `"2026-02-28T10:00:00"`, `"yesterday"`, `1`, `"0000-01-01T00:00:00Z"`}},
		{AttributeDefinition{Type: TypeEnum, Values: []string{"gold", "silver"}}, []string{`"gold"`}, []string{`"Gold"`, `"bronze"`, `1`}},
	}
	for _, c := range cases {
		for _, raw := range c.accept {
			if err := c.def.Check(json.RawMessage(raw)); err != nil {
				t.Errorf("%s %s: %v", c.def.Type, raw, err)
			}
		}
		for _, raw := range c.refuse {
			if c.def.Check(json.RawMessage(raw)) == nil {
				t.Errorf("%s accepted %s", c.def.Type, raw)
			}
		}
	}
}

// A refused value must never be echoed, because it is customer data.
func TestCheckErrorsOmitTheValue(t *testing.T) {
	def := AttributeDefinition{Path: "tier", Type: TypeEnum, Values: []string{"gold"}}
	err := def.Check(json.RawMessage(`"synthetic-secret-value"`))
	if err == nil || strings.Contains(err.Error(), "synthetic-secret-value") {
		t.Fatalf("error %v", err)
	}
}

func TestRevisionsOnlyWiden(t *testing.T) {
	enum := AttributeDefinition{Path: "tier", Revision: 1, Type: TypeEnum, Values: []string{"gold", "silver"}}
	str := AttributeDefinition{Path: "nickname", Revision: 1, Type: TypeString, MaxLength: intPtr(10)}
	compatible := []struct{ prev, next AttributeDefinition }{
		{enum, AttributeDefinition{Path: "tier", Type: TypeEnum, Values: []string{"silver", "gold", "bronze"}}},
		{str, AttributeDefinition{Path: "nickname", Type: TypeString, MaxLength: intPtr(20)}},
		{str, AttributeDefinition{Path: "nickname", Type: TypeString}},
	}
	for _, c := range compatible {
		if err := c.next.Supersedes(c.prev); err != nil {
			t.Errorf("%+v: %v", c.next, err)
		}
	}
	incompatible := []struct{ prev, next AttributeDefinition }{
		{enum, AttributeDefinition{Path: "tier", Type: TypeEnum, Values: []string{"gold"}}},
		{enum, AttributeDefinition{Path: "tier", Type: TypeEnum, Values: []string{"gold", "silver"}}},
		{enum, AttributeDefinition{Path: "tier", Type: TypeString}},
		{str, AttributeDefinition{Path: "nickname", Type: TypeString, MaxLength: intPtr(5)}},
		{str, AttributeDefinition{Path: "nickname", Type: TypeString, MaxLength: intPtr(10)}},
		{AttributeDefinition{Path: "nickname", Revision: 1, Type: TypeString}, AttributeDefinition{Path: "nickname", Type: TypeString, MaxLength: intPtr(10)}},
		{AttributeDefinition{Path: "orders", Revision: 1, Type: TypeInteger}, AttributeDefinition{Path: "orders", Type: TypeInteger}},
		{AttributeDefinition{Path: "orders", Revision: 1, Type: TypeInteger}, AttributeDefinition{Path: "orders", Type: TypeDecimal}},
	}
	for _, c := range incompatible {
		if c.next.Supersedes(c.prev) == nil {
			t.Errorf("accepted %+v after %+v", c.next, c.prev)
		}
	}
}
