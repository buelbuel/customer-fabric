package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const oauthSubject = "8d7a991e-0e30-4919-8d5e-a58c75774738"

type signingKey struct {
	key *rsa.PrivateKey
	kid string
}

func newSigningKey(t *testing.T, kid string) signingKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return signingKey{key, kid}
}

func (k signingKey) sign(t *testing.T, claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k.key, KeyID: k.kid}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOAuthVerifier(t *testing.T) {
	current := newSigningKey(t, "k1")
	var published atomic.Pointer[signingKey]
	published.Store(&current)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		k := published.Load()
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.key.PublicKey, KeyID: k.kid, Algorithm: "RS256", Use: "sig"}}})
	}))
	defer jwks.Close()
	v, err := NewOAuthVerifier(jwks.URL, "http://127.0.0.1:4433")
	if err != nil {
		t.Fatal(err)
	}
	claims := func(change func(map[string]any)) map[string]any {
		c := map[string]any{"iss": "http://127.0.0.1:4433", "sub": oauthSubject, "aud": []string{APIAudience}, "client_id": "shop",
			"scp": []string{"openid", "profile.read"}, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix()}
		if change != nil {
			change(c)
		}
		return c
	}
	got, err := v.Verify(t.Context(), current.sign(t, claims(nil)), "profile.read")
	if err != nil || got.Subject != oauthSubject || got.ClientID != "shop" {
		t.Fatalf("valid token: %+v %v", got, err)
	}
	if _, err := v.Verify(t.Context(), current.sign(t, claims(nil)), "profile.write"); !errors.Is(err, ErrInsufficientScope) {
		t.Fatalf("missing scope: %v", err)
	}
	for name, change := range map[string]func(map[string]any){
		"wrong issuer":   func(c map[string]any) { c["iss"] = "http://evil.test" },
		"wrong audience": func(c map[string]any) { c["aud"] = []string{"other-api"} },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"no expiry":      func(c map[string]any) { delete(c, "exp") },
		"bad subject":    func(c map[string]any) { c["sub"] = "operator" },
	} {
		if _, err := v.Verify(t.Context(), current.sign(t, claims(change)), "profile.read"); !errors.Is(err, ErrInvalidSession) {
			t.Errorf("%s: %v", name, err)
		}
	}
	forged := newSigningKey(t, "k1")
	if _, err := v.Verify(t.Context(), forged.sign(t, claims(nil)), "profile.read"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("a token signed by another key under a known kid: %v", err)
	}
	unsigned := strings.Join([]string{"eyJhbGciOiJub25lIiwia2lkIjoiazEifQ", strings.Split(current.sign(t, claims(nil)), ".")[1], ""}, ".")
	if _, err := v.Verify(t.Context(), unsigned, "profile.read"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("alg none: %v", err)
	}
	// A rotated key is picked up after the refresh interval; an unknown key ID before then is refused.
	rotated := newSigningKey(t, "k2")
	published.Store(&rotated)
	if _, err := v.Verify(t.Context(), rotated.sign(t, claims(nil)), "profile.read"); !errors.Is(err, ErrInvalidSession) {
		t.Fatalf("an unknown key inside the refresh interval: %v", err)
	}
	v.fetched, v.attempted = time.Now().Add(-time.Minute), time.Now().Add(-time.Minute)
	if _, err := v.Verify(t.Context(), rotated.sign(t, claims(nil)), "profile.read"); err != nil {
		t.Fatalf("rotated key: %v", err)
	}
	// An unreachable issuer keeps serving the last good keys instead of failing every request.
	jwks.Close()
	v.fetched, v.attempted = time.Now().Add(-10*time.Minute), time.Now().Add(-time.Minute)
	if _, err := v.Verify(t.Context(), rotated.sign(t, claims(nil)), "profile.read"); err != nil {
		t.Fatalf("stale keys during an outage: %v", err)
	}
}
