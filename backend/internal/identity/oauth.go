package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// APIAudience is the only audience Customer Fabric's API accepts in an access token (ADR 0012).
const APIAudience = "customer-fabric-api"

var ErrInsufficientScope = errors.New("access token lacks the required scope")

// AccessToken is a verified OAuth access token: the customer (Kratos identity) it acts for, the
// application it was issued to and the scopes the customer granted.
type AccessToken struct {
	Subject  string
	ClientID string
	Scopes   []string
}

// OAuthVerifier checks Hydra-issued JWT access tokens against Hydra's published keys. Keys are cached
// briefly and refetched when a token names an unknown key, so a rotated key is picked up.
type OAuthVerifier struct {
	jwksURL, issuer string
	client          *http.Client
	mu              sync.Mutex
	keys            jose.JSONWebKeySet
	fetched         time.Time // last successful fetch
	attempted       time.Time // last fetch attempt, successful or not
}

func NewOAuthVerifier(publicURL, issuer string) (*OAuthVerifier, error) {
	if !strings.HasPrefix(publicURL, "http://") && !strings.HasPrefix(publicURL, "https://") || issuer == "" {
		return nil, errors.New("OAuth verification needs Hydra's public URL and issuer")
	}
	return &OAuthVerifier{
		jwksURL: strings.TrimSuffix(publicURL, "/") + "/.well-known/jwks.json", issuer: issuer,
		client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}, nil
}

func (v *OAuthVerifier) keySet(ctx context.Context, kid string, refresh bool) ([]jose.JSONWebKey, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !refresh && time.Since(v.fetched) < 5*time.Minute {
		return v.keys.Key(kid), nil
	}
	// At most one fetch every ten seconds, failed or not, so forged key IDs or an unreachable issuer cannot
	// turn into a request flood; meanwhile the last good key set keeps serving.
	if time.Since(v.attempted) < 10*time.Second {
		return v.cached(kid)
	}
	v.attempted = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return v.cached(kid)
	}
	res, err := v.client.Do(req)
	if err != nil {
		return v.cached(kid)
	}
	defer res.Body.Close()
	var set jose.JSONWebKeySet
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 256<<10)).Decode(&set) != nil {
		return v.cached(kid)
	}
	v.keys, v.fetched = set, time.Now()
	return v.keys.Key(kid), nil
}

// cached serves the last good key set, or reports the issuer unavailable if there never was one.
func (v *OAuthVerifier) cached(kid string) ([]jose.JSONWebKey, error) {
	if v.fetched.IsZero() {
		return nil, ErrUnavailable
	}
	return v.keys.Key(kid), nil
}

// Verify checks the token's signature, issuer, audience, expiry and scope. It never calls Hydra per
// request except to refresh keys, so a token stays valid until it expires even after the customer logs
// out of or is deactivated in Kratos; access tokens are therefore short-lived (Hydra's default is one
// hour), and callers additionally check the customer's lifecycle in Customer Fabric.
func (v *OAuthVerifier) Verify(ctx context.Context, raw, scope string) (AccessToken, error) {
	token, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256, jose.ES256, jose.PS256})
	if err != nil || len(token.Headers) != 1 || token.Headers[0].KeyID == "" {
		return AccessToken{}, ErrInvalidSession
	}
	kid := token.Headers[0].KeyID
	keys, err := v.keySet(ctx, kid, false)
	if err == nil && len(keys) == 0 {
		keys, err = v.keySet(ctx, kid, true)
	}
	if err != nil {
		return AccessToken{}, err
	}
	// The key must be a signing key for the token's algorithm, not an encryption key that happens to match.
	if len(keys) != 1 || (keys[0].Use != "" && keys[0].Use != "sig") ||
		(keys[0].Algorithm != "" && keys[0].Algorithm != token.Headers[0].Algorithm) {
		return AccessToken{}, ErrInvalidSession
	}
	var claims jwt.Claims
	var extra struct {
		Scopes   []string `json:"scp"`
		ClientID string   `json:"client_id"`
	}
	if token.Claims(keys[0].Key, &claims, &extra) != nil {
		return AccessToken{}, ErrInvalidSession
	}
	if claims.Validate(jwt.Expected{Issuer: v.issuer, AnyAudience: jwt.Audience{APIAudience}, Time: time.Now()}) != nil ||
		claims.Expiry == nil || !subjectID.MatchString(claims.Subject) {
		return AccessToken{}, ErrInvalidSession
	}
	if !slices.Contains(extra.Scopes, scope) {
		return AccessToken{}, ErrInsufficientScope
	}
	return AccessToken{Subject: claims.Subject, ClientID: extra.ClientID, Scopes: extra.Scopes}, nil
}
