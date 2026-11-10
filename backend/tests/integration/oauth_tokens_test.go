package integration

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const testIssuer = "http://127.0.0.1:4433"

// An application acts for a customer with a Hydra access token, limited to the scopes the customer granted.
func TestApplicationsReadAndWritePreferencesWithScopedTokens(t *testing.T) {
	s := setupLinking(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "hydra", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(jwks.Close)
	cfg := s.config
	cfg.HydraPublicURL, cfg.HydraIssuer = jwks.URL, testIssuer
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "hydra"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	token := func(subject string, scopes ...string) string {
		raw, err := jwt.Signed(signer).Claims(map[string]any{"iss": testIssuer, "sub": subject, "aud": []string{"customer-fabric-api"},
			"client_id": "synthetic-shop", "scp": scopes, "exp": time.Now().Add(time.Hour).Unix()}).Serialize()
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	call := func(method, body, bearer, etag string, extra ...string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "/v1/customer/profile/preferences", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.Header.Set("Content-Type", "application/json")
		if etag != "" {
			req.Header.Set("If-Match", etag)
		}
		if len(extra) == 2 {
			req.Header.Set(extra[0], extra[1])
		}
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		return res
	}
	ada := s.customerIDs[linkCustomerA]
	reader, writer := token(ada, "openid", "profile.read"), token(ada, "openid", "profile.read", "profile.write")

	if res := call("GET", "", reader, ""); res.Code != 200 || !strings.Contains(res.Body.String(), `"version":0`) {
		t.Fatalf("read with profile.read: %d %s", res.Code, res.Body.String())
	}
	if res := call("PUT", `{"timezone":"Europe/Berlin"}`, reader, ""); res.Code != 403 || !strings.Contains(res.Header().Get("WWW-Authenticate"), "insufficient_scope") {
		t.Fatalf("write with only profile.read: %d", res.Code)
	}
	if res := call("PUT", `{"timezone":"Europe/Berlin"}`, writer, ""); res.Code != 200 {
		t.Fatalf("write with profile.write: %d %s", res.Code, res.Body.String())
	}
	// The customer sees the application's change in their own session.
	if p := decodePreferences(t, s.preferences("GET", "", linkCustomerA, "")); p.Timezone == nil || *p.Timezone != "Europe/Berlin" {
		t.Fatalf("the customer does not see the application's write: %+v", p)
	}
	if res := call("GET", "", token("0b5c9a3e-6d1b-4c8e-9f2a-1d3e5f7a9b0c", "profile.read"), ""); res.Code != 404 {
		t.Fatalf("a token for a customer without a profile: %d", res.Code)
	}
	tampered := reader[:len(reader)-4] + "AAAA"
	if res := call("GET", "", tampered, ""); res.Code != 401 || !strings.Contains(res.Header().Get("WWW-Authenticate"), "invalid_token") {
		t.Fatalf("tampered token: %d", res.Code)
	}
	if res := call("GET", "", reader, "", "X-Session-Token", linkCustomerA); res.Code != 400 {
		t.Fatalf("session and token together: %d", res.Code)
	}
	// Customer Fabric's lifecycle state, not just the token, decides whether an application may act.
	s.exec(`UPDATE profiles SET lifecycle_state='erasure_pending' WHERE id=$1`, s.anchor(linkCustomerA))
	if res := call("GET", "", reader, ""); res.Code != 403 {
		t.Fatalf("a token for an erasure-pending customer: %d", res.Code)
	}
	s.exec(`UPDATE profiles SET lifecycle_state='active' WHERE id=$1`, s.anchor(linkCustomerA))
	// Other customer resources still accept only the customer's own session.
	req := httptest.NewRequest("GET", "/v1/customer/consent", nil)
	req.Header.Set("Authorization", "Bearer "+writer)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != 401 {
		t.Fatalf("an access token reached customer consent: %d", res.Code)
	}
}
