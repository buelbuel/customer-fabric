package platform

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConfigurationFailsClosed(t *testing.T) {
	for _, c := range []Config{{}, {Mode: "production", Tokens: map[string]string{"console": strings.Repeat("x", 32)}}, {Mode: "development", Tokens: map[string]string{"console": "weak"}}, {Mode: "development", Tokens: map[string]string{"a": strings.Repeat("x", 32), "b": strings.Repeat("x", 32)}}} {
		if c.Validate() == nil {
			t.Fatal("unsafe configuration accepted")
		}
	}
}
func TestUnauthorizedBeforeStorage(t *testing.T) {
	h, e := NewAPI(Store{}, Config{Mode: "development", Tokens: map[string]string{"console": strings.Repeat("x", 32)}})
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{"/v1/profiles", "/v1/profiles/unknown"} {
		r := httptest.NewRequest("GET", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("got %d", w.Code)
		}
	}
}
func TestDuplicateKeys(t *testing.T) {
	for _, s := range []string{`{"set":{"a":1,"a":2}}`, `{"set":{},"set":{}}`, `{"identifiers":[{"value":"1","value":"2"}]}`} {
		if !duplicateKeys([]byte(s)) {
			t.Fatal("missed duplicate")
		}
	}
	if duplicateKeys([]byte(`{"set":{"a":1,"b":2}}`)) {
		t.Fatal("false duplicate")
	}
}
