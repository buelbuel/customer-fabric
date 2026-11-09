package profiles

import (
	"encoding/json"
	"testing"
)

func TestMutationValidation(t *testing.T) {
	for _, body := range []string{`{}`, `{"set":{"name":null}}`, `{"set":{"name":[]}}`, `{"set":{"name":{}}}`, `{"set":{"bad.key":1}}`, `{"set":{"name":"x"},"remove":["name"]}`, `{"remove":["name","name"]}`} {
		var m Mutation
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatal(err)
		}
		if m.Validate(false) == nil {
			t.Errorf("accepted %s", body)
		}
	}
	var m Mutation
	_ = json.Unmarshal([]byte(`{"set":{"name":"synthetic","score":1,"active":true}}`), &m)
	if err := m.Validate(true); err != nil {
		t.Fatal(err)
	}
}
func TestLoneSurrogatesAreRefused(t *testing.T) {
	for raw, lone := range map[string]bool{
		`"\ud800"`: true, `"\udc00"`: true, `"\ud83dx"`: true, `"\ud83d\u0041"`: true,
		`"\ud83d\ude00"`: false, `"\\ud800"`: false, `"\u0041"`: false, `"\ufffd"`: false, `"plain"`: false,
	} {
		m := Mutation{Set: map[string]json.RawMessage{"name": json.RawMessage(raw)}}
		if (m.Validate(false) != nil) != lone {
			t.Errorf("%s: lone=%v", raw, lone)
		}
	}
}

func TestOwnershipIncludesRemovals(t *testing.T) {
	m := Mutation{Remove: []string{"name"}}
	owners := map[string]string{"name": "console"}
	if len(CheckOwnership(m, owners, "importer")) != 1 {
		t.Fatal("removal bypassed ownership")
	}
	if len(CheckOwnership(m, owners, "console")) != 0 {
		t.Fatal("owner denied")
	}
}
