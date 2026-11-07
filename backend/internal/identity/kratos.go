package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ErrInvalidSession    = errors.New("invalid customer session")
	ErrUnverifiedAddress = errors.New("customer address is unverified")
	ErrUnavailable       = errors.New("customer identity service unavailable")

	subjectID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	token     = regexp.MustCompile(`^ory_st_[A-Za-z0-9_-]{16,256}$`)
)

// Kratos verifies a native session against the configured public API.
// Only the identity ID leaves this adapter.
type Kratos struct {
	whoami string
	client *http.Client
}

func NewKratos(baseURL string) (*Kratos, error) {
	base, err := url.Parse(baseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") ||
		base.Hostname() == "" || base.User != nil || base.RawQuery != "" ||
		base.ForceQuery || strings.Contains(baseURL, "#") || (base.Path != "" && base.Path != "/") {
		return nil, errors.New("invalid Kratos public URL")
	}
	base.Path = "/sessions/whoami"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &Kratos{
		whoami: base.String(),
		client: &http.Client{
			Timeout:   3 * time.Second,
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (k *Kratos) VerifiedSubject(ctx context.Context, sessionToken string) (string, error) {
	return k.verifiedSubject(ctx, sessionToken, nil)
}

func (k *Kratos) VerifiedBrowserSubject(ctx context.Context, cookie *http.Cookie) (string, error) {
	return k.verifiedSubject(ctx, "", cookie)
}

func (k *Kratos) verifiedSubject(ctx context.Context, sessionToken string, cookie *http.Cookie) (string, error) {
	if cookie == nil && !token.MatchString(sessionToken) {
		return "", ErrInvalidSession
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.whoami, nil)
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if cookie == nil {
		req.Header.Set("X-Session-Token", sessionToken)
	} else {
		req.AddCookie(cookie)
	}
	response, err := k.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", ErrInvalidSession
	case http.StatusOK:
	default:
		return "", ErrUnavailable
	}

	var session struct {
		Active    bool      `json:"active"`
		ExpiresAt time.Time `json:"expires_at"`
		Identity  struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Traits struct {
				Email string `json:"email"`
			} `json:"traits"`
			Addresses []struct {
				Via      string `json:"via"`
				Value    string `json:"value"`
				Verified bool   `json:"verified"`
			} `json:"verifiable_addresses"`
		} `json:"identity"`
	}
	const maxSessionBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionBytes+1))
	if err != nil || len(raw) > maxSessionBytes || json.Unmarshal(raw, &session) != nil {
		return "", ErrUnavailable
	}
	identity := session.Identity
	if !session.Active || !session.ExpiresAt.After(time.Now()) || identity.State != "active" ||
		!subjectID.MatchString(identity.ID) || identity.ID == "00000000-0000-0000-0000-000000000000" {
		return "", ErrInvalidSession
	}
	// Kratos normalizes verification addresses while retaining the submitted trait.
	email := strings.ToLower(strings.TrimSpace(identity.Traits.Email))
	for _, address := range identity.Addresses {
		if address.Via == "email" && address.Verified && address.Value != "" && address.Value == email {
			return identity.ID, nil
		}
	}
	return "", ErrUnverifiedAddress
}
