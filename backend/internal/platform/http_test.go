package platform

import (
	"fmt"
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

func TestCustomerSessionNamespaceCannotBecomeOperatorCredential(t *testing.T) {
	source := strings.Repeat("x", 32)
	cfg := Config{Mode: "development", Tokens: map[string]string{"importer": source}, KratosPublicURL: "http://kratos:4433"}
	for _, token := range []string{"", source, "ory_st_" + strings.Repeat("z", 32)} {
		cfg.OperatorToken = token
		if cfg.Validate() == nil {
			t.Fatal("accepted a missing or non-distinct operator credential")
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

func TestSecurityHeadersAndCORS(t *testing.T) {
	h, err := NewAPI(Store{}, Config{Mode: "development", Tokens: map[string]string{"console": strings.Repeat("x", 32)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"GET", "OPTIONS", "POST"} {
		r := httptest.NewRequest(method, "/v1/profiles", nil)
		r.Header.Set("Origin", "https://untrusted.example")
		r.Header.Set("Access-Control-Request-Headers", "authorization")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 || w.Header().Get("Access-Control-Allow-Origin") != "" || w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Fatalf("cross-origin access enabled: %d %v", w.Code, w.Header())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" || w.Header().Get("Content-Type") != "application/problem+json" {
			t.Fatal("missing safe error headers")
		}
	}
}

func TestAmbiguousHeadersAndRateLimit(t *testing.T) {
	h, err := NewAPI(Store{}, Config{Mode: "development", Tokens: map[string]string{"console": strings.Repeat("x", 32)}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Authorization", "X-Session-Token", "Idempotency-Key", "If-Match"} {
		r := httptest.NewRequest("GET", "/v1/profiles", nil)
		r.Header.Add(name, "one")
		r.Header.Add(name, "two")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("ambiguous %s accepted", name)
		}
	}
	for i := 0; i < 601; i++ {
		r := httptest.NewRequest("GET", "/v1/profiles", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 600 && (w.Code != 429 || w.Header().Get("Retry-After") == "") {
			t.Fatal("limiter did not bound invalid credentials")
		}
	}
}

func TestInternalErrorsAreRedacted(t *testing.T) {
	w := httptest.NewRecorder()
	fail(w, fmt.Errorf("database password=secret synthetic-customer-data"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "synthetic-customer-data") {
		t.Fatal("internal details leaked")
	}
}
