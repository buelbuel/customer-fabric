package matching

import (
	"encoding/json"
	"testing"
)

func signals(t *testing.T, raw string) Signals {
	t.Helper()
	var attributes map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &attributes); err != nil {
		t.Fatal(err)
	}
	return SignalsFrom(attributes)
}

func TestSignalsNormalize(t *testing.T) {
	s := signals(t, `{"email":" Ada@Example.COM ","phone":"+44 1632 960017","given_name":"Élodie","family_name":"EXEMPLE","customer_reference":" retail-1 ","tier":"gold"}`)
	if s != (Signals{Email: "ada@example.com", Phone: "632960017", GivenName: "élodie", FamilyName: "exemple", Reference: "retail-1"}) {
		t.Fatalf("%+v", s)
	}
	if a, b := signals(t, `{"family_name":"WEISS","given_name":"Élodie"}`), signals(t, `{"family_name":"weiß","given_name":"E\u0301lodie"}`); a.FamilyName != b.FamilyName || a.GivenName != b.GivenName {
		t.Fatalf("folding %+v %+v", a, b)
	}
	if national := signals(t, `{"phone":"01632 960017"}`); national.Phone != s.Phone {
		t.Fatalf("national and international forms disagree: %q", national.Phone)
	}
	if s := signals(t, `{"email":"not-an-email","phone":"12"}`); s != (Signals{}) {
		t.Fatalf("a malformed email and a short phone are no signal: %+v", s)
	}
	if s := signals(t, `{"email":7,"phone":"12"}`); s != (Signals{}) {
		t.Fatalf("non-strings and short phones are no signal: %+v", s)
	}
}

func TestCompare(t *testing.T) {
	ada := Signals{Email: "ada@example.com", Phone: "632960001", GivenName: "ada", FamilyName: "muster", Reference: "retail-1"}
	for name, tc := range map[string]struct {
		other Signals
		class string
	}{
		"reference with a name":          {Signals{Reference: "retail-1", FamilyName: "muster"}, Strong},
		"reference with only the email":  {Signals{Reference: "retail-1", Email: "ada@example.com"}, Probable},
		"email and family name":          {Signals{Email: "ada@example.com", FamilyName: "muster"}, Probable},
		"phone and family name":          {Signals{Phone: "632960001", FamilyName: "muster"}, Probable},
		"renamed, email phone and given": {Signals{Email: "ada@example.com", Phone: "632960001", GivenName: "ada", FamilyName: "muster-renamed"}, Probable},
		"shared email, another person":   {Signals{Email: "ada@example.com", GivenName: "lukas", FamilyName: "beispiel"}, Conflicting},
		"reused reference":               {Signals{Reference: "retail-1", GivenName: "lukas", FamilyName: "beispiel"}, Conflicting},
		"household landline":             {Signals{Phone: "632960001", GivenName: "lukas", FamilyName: "muster"}, Conflicting},
		"household email":                {Signals{Email: "ada@example.com", GivenName: "lukas", FamilyName: "muster"}, Conflicting},
		"household reference":            {Signals{Reference: "retail-1", GivenName: "lukas", FamilyName: "muster"}, Conflicting},
		"renamed without a given name":   {Signals{Email: "ada@example.com", FamilyName: "beispiel"}, Conflicting},
	} {
		c, ok := Compare(ada, tc.other)
		if !ok || c.Class != tc.class {
			t.Errorf("%s: %v %+v", name, ok, c)
		}
	}
	for name, other := range map[string]Signals{
		"email alone":      {Email: "ada@example.com"},
		"reference alone":  {Reference: "retail-1"},
		"phone alone":      {Phone: "632960001"},
		"family name only": {FamilyName: "muster"},
		"nothing shared":   {Email: "lukas@example.com", FamilyName: "beispiel"},
	} {
		if c, ok := Compare(ada, other); ok {
			t.Errorf("%s became a candidate: %+v", name, c)
		}
	}
	if c, _ := Compare(ada, Signals{Email: "ada@example.com", FamilyName: "muster"}); len(c.Agree) != 2 || c.Agree[0] != PathEmail {
		t.Fatalf("explanation %+v", c)
	}
}
