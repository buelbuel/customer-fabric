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
