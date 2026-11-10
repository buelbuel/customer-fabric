package identity

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOperatorSubjectRejectsExpiredAndInsufficientSessions(t *testing.T) {
	tests := []struct {
		name, aal, role string
		roles           []string
		expiry          time.Duration
		err             error
		want            []string
	}{
		{"active", "aal2", "", []string{"profile_reader"}, time.Minute, nil, []string{"profile_reader"}},
		{"legacy single role is not read", "aal2", "profile_reader", nil, time.Minute, ErrOperatorPermission, nil},
		{"scoped roles", "aal2", "", []string{"profile_reader", "audience_manager", "audience_manager", "superuser"}, time.Minute, nil, []string{"profile_reader", "audience_manager"}},
		{"expired", "aal2", "", []string{"profile_reader"}, -time.Minute, ErrInvalidSession, nil},
		{"password only", "aal1", "profile_reader", []string{"consent_admin"}, time.Minute, ErrOperatorPermission, nil},
		{"no permission", "aal2", "", nil, time.Minute, ErrOperatorPermission, nil},
		{"only unknown roles", "aal2", "admin", []string{"root"}, time.Minute, ErrOperatorPermission, nil},
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
					"identity": map[string]any{"id": id, "state": "active", "schema_id": "operator", "metadata_public": map[string]any{
						"customer_fabric_role": tc.role, "customer_fabric_roles": tc.roles,
					}},
				})
			}))
			defer server.Close()
			k, err := NewKratos(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			operator, err := k.OperatorSubject(t.Context(), token, nil)
			if !errors.Is(err, tc.err) || (tc.err == nil && (operator.Subject != id || !slices.Equal(operator.Roles, tc.want))) || (tc.err != nil && operator.Subject != "") {
				t.Fatalf("unexpected operator decision: %+v %v", operator, err)
			}
		})
	}
}
