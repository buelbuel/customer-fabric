package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOperatorSubjectRejectsExpiredAndInsufficientSessions(t *testing.T) {
	tests := []struct {
		name, aal, role string
		expiry          time.Duration
		err             error
	}{
		{"active", "aal2", "profile_reader", time.Minute, nil},
		{"expired", "aal2", "profile_reader", -time.Minute, ErrInvalidSession},
		{"password only", "aal1", "profile_reader", time.Minute, ErrOperatorPermission},
		{"no permission", "aal2", "", time.Minute, ErrOperatorPermission},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const id = "8d7a991e-0e30-4919-8d5e-a58c75774738"
			token := "ory_st_" + strings.Repeat("a", 32)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Session-Token") != token || r.URL.Path != "/sessions/whoami" {
					t.Error("unexpected issuer request")
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"active": true, "expires_at": time.Now().Add(tc.expiry), "authenticator_assurance_level": tc.aal,
					"authentication_methods": []map[string]string{{"method": "password"}, {"method": "totp"}},
					"identity": map[string]any{"id": id, "state": "active", "schema_id": "operator", "metadata_public": map[string]string{
						"customer_fabric_role": tc.role,
					}},
				})
			}))
			defer server.Close()
			k, err := NewKratos(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			subject, err := k.OperatorSubject(t.Context(), token, nil)
			if !errors.Is(err, tc.err) || (tc.err == nil && subject != id) || (tc.err != nil && subject != "") {
				t.Fatalf("unexpected operator decision: %v", err)
			}
		})
	}
}
