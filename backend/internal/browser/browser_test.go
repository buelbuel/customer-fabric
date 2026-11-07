package browser

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReferenceRedirectStaysOnBrowserOrigin(t *testing.T) {
	for _, tc := range []struct {
		location string
		allowed  bool
	}{
		{"/account/login?flow=local", true},
		{"http://127.0.0.1:12345/account/login", true},
		{"http://127.0.0.1:4433/account/login", false},
		{"https://example.test/account/login", false},
		{"//example.test/account/login", false},
		{"/%2fexample.test/account/login", false},
		{"/\\example.test/account/login", false},
	} {
		t.Run(tc.location, func(t *testing.T) {
			res := &http.Response{Header: http.Header{"Location": {tc.location}}}
			if (checkResponse(res, "127.0.0.1:12345") == nil) != tc.allowed {
				t.Fatalf("unexpected redirect decision")
			}
		})
	}
}

func TestReferenceDoesNotForwardSiblingSessions(t *testing.T) {
	in := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:12345/", nil)
	for _, name := range []string{"cf_customer_12345", "cf_customer_4433", "cf_operator_session", "csrf_token"} {
		in.AddCookie(&http.Cookie{Name: name, Value: "synthetic"})
	}
	out := httptest.NewRequest(http.MethodGet, "http://kratos:4433/", nil)
	new(gateway).copyCookies(in, out)
	got := out.Cookies()
	if len(got) != 2 || got[0].Name != "cf_customer_12345" || got[1].Name != "csrf_token" {
		t.Fatal("only the local session and Kratos CSRF cookie should pass")
	}
}
