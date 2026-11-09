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

func TestReferenceForwardsBrowserWriteHeadersToAPI(t *testing.T) {
	g := &gateway{}
	in := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:12345/account/profile", nil)
	in.Header.Set("Sec-Fetch-Site", "same-origin")
	in.AddCookie(&http.Cookie{Name: "cf_customer_12345", Value: "synthetic"})
	in.AddCookie(&http.Cookie{Name: "cf_customer_4433", Value: "sibling"})
	out := httptest.NewRequest(http.MethodPut, "http://api/v1/customer/profile", nil)
	g.authorizeAPI(in, out)
	cookies := out.Cookies()
	if out.Header.Get("Origin") != "http://127.0.0.1:12345" || out.Header.Get("Sec-Fetch-Site") != "same-origin" ||
		len(cookies) != 1 || cookies[0].Name != "cf_customer_12345" {
		t.Fatalf("unexpected forwarded headers: %v", out.Header)
	}
}

func TestReferenceDoesNotForwardSiblingSessions(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Cookies()
		if len(got) != 2 || got[0].Name != "cf_customer_12345" || got[1].Name != "csrf_token" {
			t.Error("only the local session and Kratos CSRF cookie should pass")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	handler, err := New(upstream.URL, "", "")
	if err != nil {
		t.Fatal(err)
	}
	in := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:12345/sessions/whoami", nil)
	for _, name := range []string{"cf_customer_12345", "cf_customer_4433", "cf_operator_session", "csrf_token"} {
		in.AddCookie(&http.Cookie{Name: name, Value: "synthetic"})
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, in)
	if out.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", out.Code)
	}
}
