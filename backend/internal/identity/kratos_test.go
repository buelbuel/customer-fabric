package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestVerifiedSubjectRequiresCurrentVerifiedEmail(t *testing.T) {
	const id = "4b6eca32-f63c-47b8-b311-09ff4ec4556e"
	const sessionToken = "ory_st_example_native_token"
	expires := time.Now().Add(time.Hour).Format(time.RFC3339)
	active := fmt.Sprintf(`{"active":true,"expires_at":%q,"identity":{"id":%q,"state":"active","traits":{"email":"new@example.test"},"verifiable_addresses":[{"via":"email","value":"new@example.test","verified":true}]}}`, expires, id)

	for _, tc := range []struct {
		name, body string
		status     int
		want       error
	}{
		{"verified", active, 200, nil},
		{"normalized trait", strings.Replace(active, `"email":"new@example.test"`, `"email":"New@Example.test"`, 1), 200, nil},
		{"old address", strings.Replace(active, `"value":"new@example.test"`, `"value":"old@example.test"`, 1), 200, ErrUnverifiedAddress},
		{"unverified", strings.Replace(active, `"verified":true`, `"verified":false`, 1), 200, ErrUnverifiedAddress},
		{"inactive identity", strings.Replace(active, `"state":"active"`, `"state":"inactive"`, 1), 200, ErrInvalidSession},
		{"expired", strings.Replace(active, expires, "2000-01-01T00:00:00Z", 1), 200, ErrInvalidSession},
		{"revoked", "", 401, ErrInvalidSession},
		{"malformed", active + "{}", 200, ErrUnavailable},
		{"outage", "", 503, ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/sessions/whoami" ||
					r.Header.Get("X-Session-Token") != sessionToken || r.Header.Get("Authorization") != "" {
					t.Error("unexpected Kratos session request")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer upstream.Close()
			client, err := NewKratos(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.VerifiedSubject(context.Background(), sessionToken)
			if !errors.Is(err, tc.want) || (err == nil && got != id) || (err != nil && got != "") {
				t.Fatalf("unexpected verification result %q, %v", got, err)
			}
		})
	}
}

func TestKratosDoesNotForwardSessionOnRedirect(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions/whoami" {
			t.Error("session request followed a redirect")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/other", http.StatusFound)
	}))
	defer upstream.Close()
	client, err := NewKratos(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.VerifiedSubject(context.Background(), "ory_st_example_native_token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unexpected verification error %v", err)
	}
}
