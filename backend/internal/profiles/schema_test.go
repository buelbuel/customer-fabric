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

func TestCollectionDefinitionsAndRevisions(t *testing.T) {
	d := AttributeDefinition{Path: "interests", Type: TypeEnum, Values: []string{"music", "旅行"}, MaxItems: intPtr(2)}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`[]`, `["music","旅行"]`} {
		if err := d.Check(json.RawMessage(raw)); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{`null`, `"music"`, `["music",null]`, `["music","music","music"]`, `["secret"]`, `[["music"]]`} {
		if err := d.Check(json.RawMessage(raw)); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("accepted or disclosed %s: %v", raw, err)
		}
	}
	wider := d
	wider.MaxItems = intPtr(3)
	if err := wider.Supersedes(d); err != nil {
		t.Fatal(err)
	}
	for _, next := range []AttributeDefinition{
		{Path: d.Path, Type: d.Type, Values: d.Values},
		{Path: d.Path, Type: d.Type, Values: d.Values, MaxItems: intPtr(1)},
		{Path: d.Path, Type: d.Type, Values: []string{"music"}, MaxItems: intPtr(3)},
	} {
		if next.Supersedes(d) == nil {
			t.Errorf("accepted incompatible revision %+v", next)
		}
	}
	for _, n := range []int32{0, 101} {
		bad := d
		bad.MaxItems = &n
		if bad.Validate() == nil {
			t.Errorf("accepted max_items %d", n)
		}
	}
	for _, c := range []struct {
		t              AttributeType
		valid, invalid string
	}{
		{TypeInteger, `[1,2]`, `[1,2.0]`},
		{TypeDecimal, `[1,2.5]`, `[1,"2"]`},
		{TypeBoolean, `[true,false]`, `[true,1]`},
		{TypeDate, `["2016-02-29"]`, `["2017-02-29"]`},
		{TypeTimestamp, `["2026-01-01T10:00:00Z"]`, `["2026-01-01"]`},
	} {
		def := AttributeDefinition{Path: "history", Type: c.t, MaxItems: intPtr(2)}
		if def.Check(json.RawMessage(c.valid)) != nil || def.Check(json.RawMessage(c.invalid)) == nil {
			t.Errorf("%s collection validation", c.t)
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

func TestAccessPolicyValidation(t *testing.T) {
	valid := []AccessPolicy{
		{Path: "tier", Readers: []Reader{ReaderOperator, ReaderSegmentation}, Sources: []string{"crm", "shop"}},
		{Path: "tier", Readers: []Reader{}, Sources: []string{}},
	}
	for _, p := range valid {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
	invalid := []AccessPolicy{
		{Path: "tier", Sources: []string{}},
		{Path: "tier", Readers: []Reader{}},
		{Path: "tier", Readers: []Reader{"customer"}, Sources: []string{}},
		{Path: "tier", Readers: []Reader{ReaderOperator, ReaderOperator}, Sources: []string{}},
		{Path: "tier", Readers: []Reader{}, Sources: []string{"crm", "crm"}},
		{Path: "tier", Readers: []Reader{}, Sources: []string{"Not A Source"}},
	}
	for _, p := range invalid {
		if p.Validate() == nil {
			t.Errorf("accepted %+v", p)
		}
	}
	p := AccessPolicy{Readers: []Reader{ReaderSegmentation}, Sources: []string{"crm"}}
	if !p.Allows(ReaderSegmentation, "crm") || p.Allows(ReaderOperator, "crm") || p.Allows(ReaderSegmentation, "shop") {
		t.Fatal("policy decision")
	}
}
